package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xyu/aco/internal/policy"
)

// Profile is a Claude Code execution profile (§16.2).
type Profile string

// Profiles.
const (
	ProfilePlanner     Profile = Profile(policy.ProfilePlanner)
	ProfileImplementer Profile = Profile(policy.ProfileImplementer)
)

// Tool sets. --tools limits which built-in tools exist at all; the
// permission lists then decide which of those run without a prompt.
var (
	plannerTools     = []string{"Read", "Glob", "Grep"}
	implementerTools = []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash"}
	writeTools       = []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash", "PowerShell"}
)

// BaseAllowedBash are command prefixes the implementer may run without
// further configuration (inspection and file housekeeping). The guard hook
// still checks every command; implementer.extra_allowed_bash adds the
// project's toolchain (go, make, npm, ...).
var BaseAllowedBash = []string{
	"ls", "cat", "head", "tail", "wc", "grep", "rg", "find", "pwd", "echo", "printf", "diff", "sort", "uniq",
	"tree", "which", "test", "mkdir", "touch", "rm", "mv", "cp", "sed", "awk", "cut", "tr", "xargs",
	"git status", "git diff", "git log", "git show", "git ls-files", "git blame", "git grep", "git rev-parse",
}

// DeniedBash are always denied by permission rules, on top of the guard.
var DeniedBash = []string{
	"git commit", "git push", "git reset", "git rebase", "git checkout", "git switch", "git branch", "git tag",
	"git stash", "git clean", "git worktree", "git config", "git notes", "git add", "git merge", "git cherry-pick",
	"git revert", "git restore", "git rm", "git am", "git apply", "git update-ref", "git fetch", "git pull",
	"git clone", "git remote", "git submodule", "git filter-branch", "git gc", "git prune",
	"sudo", "su", "doas", "curl", "wget",
}

// CommandSpec is one invocation's command line.
type CommandSpec struct {
	Profile      Profile
	SessionID    string // the session to create, or to resume when Resume
	Resume       bool
	Model        string
	Effort       string
	MaxTurns     int
	Schema       []byte // JSON Schema for --json-schema
	SystemPrompt string // appended to Claude Code's system prompt
	SettingsPath string // generated settings (hook, deny rules, effort fallback)
	AllowedBash  []string
	Network      string  // policy.network
	MaxBudgetUSD float64 // >0: pass --max-budget-usd (api mode)
}

// ErrMissingFlags reports a CLI without the flags ACO needs.
type ErrMissingFlags struct{ Flags []string }

func (e *ErrMissingFlags) Error() string {
	return "claude CLI lacks required flags: --" + strings.Join(e.Flags, ", --")
}

// ErrEffortUnavailable reports that effort cannot be set and strict_effort is on.
var ErrEffortUnavailable = errors.New("claude CLI cannot set the effort level (no --effort flag) and implementer.strict_effort is true")

func bashRule(prefix string) string {
	return "Bash(" + strings.TrimSpace(prefix) + ":*)"
}

// AllowedTools is the --allowedTools list of a profile.
func AllowedTools(p Profile, extraBash []string) []string {
	if p == ProfilePlanner {
		return append([]string{}, plannerTools...)
	}
	out := []string{"Read", "Edit", "Write", "Glob", "Grep"}
	seen := map[string]bool{}
	for _, b := range append(append([]string{}, BaseAllowedBash...), extraBash...) {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, bashRule(b))
	}
	return out
}

// DisallowedTools is the --disallowedTools list of a profile.
func DisallowedTools(p Profile, network string) []string {
	if p == ProfilePlanner {
		return append(append([]string{}, writeTools...), "WebFetch", "WebSearch", "Task")
	}
	var out []string
	if network != policy.NetworkAllow {
		out = append(out, "WebFetch", "WebSearch")
	}
	out = append(out, "Task", "PowerShell")
	for _, b := range DeniedBash {
		out = append(out, bashRule(b))
	}
	return out
}

// cliSchema prepares a schema for --json-schema: compact, without the
// root "$schema" and "$id". Claude Code's validator does not load the draft
// 2020-12 meta-schema and rejects a schema that names it; the keywords ACO's
// schemas use ($defs, if/then, const) validate the same without it.
func cliSchema(schema []byte) (string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(schema, &root); err != nil {
		return "", err
	}
	if _, has := root["$schema"]; has {
		delete(root, "$schema")
		delete(root, "$id")
		b, err := json.Marshal(root)
		if err != nil {
			return "", err
		}
		schema = b
	}
	var out bytes.Buffer
	if err := json.Compact(&out, schema); err != nil {
		return "", err
	}
	return out.String(), nil
}

// BuildArgs returns the claude arguments for spec. The order is fixed
// (golden-tested). The prompt is not an argument: it goes on stdin.
func BuildArgs(caps *Capabilities, spec CommandSpec) ([]string, error) {
	if m := caps.Missing(); len(m) > 0 {
		return nil, &ErrMissingFlags{Flags: m}
	}
	if spec.SessionID == "" || spec.Model == "" || spec.SettingsPath == "" || len(spec.Schema) == 0 {
		return nil, errors.New("claudecode: session ID, model, settings path, and schema are required")
	}
	schema, err := cliSchema(spec.Schema)
	if err != nil {
		return nil, fmt.Errorf("claudecode: schema: %w", err)
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if spec.Resume {
		args = append(args, "--resume", spec.SessionID)
	} else {
		args = append(args, "--session-id", spec.SessionID)
	}
	args = append(args, "--model", spec.Model)
	if spec.Effort != "" && caps.EffortVia() == EffortFlag {
		args = append(args, "--effort", spec.Effort)
	}
	tools := implementerTools
	mode := "acceptEdits"
	if spec.Profile == ProfilePlanner {
		tools, mode = plannerTools, "dontAsk"
	}
	args = append(args,
		"--permission-mode", mode,
		"--tools", strings.Join(tools, ","),
		"--allowedTools", strings.Join(AllowedTools(spec.Profile, spec.AllowedBash), ","),
		"--disallowedTools", strings.Join(DisallowedTools(spec.Profile, spec.Network), ","),
		// Only the generated settings apply: no user, project, or local
		// settings (a repository's .claude/settings.json could add hooks or
		// permissions), and no MCP servers.
		"--setting-sources", "",
		"--strict-mcp-config",
		"--settings", spec.SettingsPath,
		"--json-schema", schema,
	)
	if spec.MaxTurns > 0 && caps.Has("max-turns") {
		args = append(args, "--max-turns", strconv.Itoa(spec.MaxTurns))
	}
	if spec.MaxBudgetUSD > 0 && caps.Has("max-budget-usd") {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(spec.MaxBudgetUSD, 'f', 2, 64))
	}
	if caps.Has("include-hook-events") {
		args = append(args, "--include-hook-events") // lets the adapter verify that the guard ran
	}
	if caps.Has("permission-prompts") {
		args = append(args, "--permission-prompts", "none") // anything not allowed is denied, never waits
	}
	if spec.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", spec.SystemPrompt)
	}
	return args, nil
}
