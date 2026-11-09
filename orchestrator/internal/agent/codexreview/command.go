package codexreview

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// DisabledFeatures are switched off for every review when the installed
// CLI has them (`codex features list`): repository hooks, plugins, apps,
// sub-agents, memories, browser and computer use, image generation, goals,
// and tool suggestions. A review needs only reading files and running
// read-only commands; everything else widens what a prompt injection in
// the repository could reach. Unknown names make Codex fail, so only
// detected features are passed.
var DisabledFeatures = []string{
	"hooks", "plugins", "apps", "multi_agent", "memories", "image_generation",
	"browser_use", "browser_use_external", "computer_use", "goals", "tool_suggest", "skill_search",
	"in_app_browser", "remote_plugin",
}

// CommandSpec is one review invocation's command line.
type CommandSpec struct {
	Model           string
	ReasoningEffort string // model_reasoning_effort ("" = CLI default)
	Dir             string // worktree (-C), absolute
	SchemaPath      string // --output-schema, absolute
	LastMessagePath string // -o, absolute
	// WindowsSandbox is windows.sandbox ("unelevated" | "elevated"; "" =
	// not passed). Codex needs it on Windows to run any command in the
	// read-only sandbox.
	WindowsSandbox string
	// SkipGitRepoCheck lets Codex run outside a Git repository (the doctor
	// probe only; reviews always run in the repository or worktree).
	SkipGitRepoCheck bool
}

// Forbidden are arguments a reviewer command may never contain (§16.2):
// anything that makes the sandbox writable, removes it, adds writable
// directories, or creates a worktree.
var Forbidden = []string{
	"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--full-auto", "--approve-for-me",
	"--dangerously-bypass-hook-trust", "--add-dir", "--worktree",
	"workspace-write", "danger-full-access",
}

// ErrForbiddenArg reports a write-capable or sandbox-bypassing argument.
type ErrForbiddenArg struct{ Arg string }

func (e *ErrForbiddenArg) Error() string {
	return "codexreview: refusing a write-capable or sandbox-bypassing argument: " + e.Arg
}

// ErrMissingFlags reports a CLI without the flags ACO needs.
type ErrMissingFlags struct{ Flags []string }

func (e *ErrMissingFlags) Error() string {
	return "codex CLI lacks required flags: " + strings.Join(e.Flags, ", ")
}

// CheckArgs refuses any argument list that could let the reviewer write
// or leave the read-only sandbox: the forbidden flags and values, a
// --sandbox other than read-only, and config overrides of the sandbox or
// approval policy.
func CheckArgs(args []string) error {
	for i, a := range args {
		low := strings.ToLower(a)
		for _, f := range Forbidden {
			if low == f || strings.HasPrefix(low, f+"=") || strings.Contains(low, f) && !strings.HasPrefix(f, "--") {
				return &ErrForbiddenArg{Arg: a}
			}
		}
		switch {
		case low == "--sandbox" || low == "-s":
			if i+1 >= len(args) || args[i+1] != "read-only" {
				return &ErrForbiddenArg{Arg: a}
			}
		case strings.HasPrefix(low, "--sandbox="):
			if a != "--sandbox=read-only" {
				return &ErrForbiddenArg{Arg: a}
			}
		case low == "-c" || low == "--config":
			if i+1 < len(args) {
				k, _, _ := strings.Cut(strings.ToLower(args[i+1]), "=")
				k = strings.TrimSpace(k)
				if k == "sandbox_mode" || strings.HasPrefix(k, "sandbox_workspace_write") || k == "approval_policy" ||
					strings.HasPrefix(k, "sandbox_permissions") || strings.HasPrefix(k, "permissions") ||
					strings.HasPrefix(k, "mcp_servers") || k == "model_provider" || strings.HasPrefix(k, "model_providers") {
					return &ErrForbiddenArg{Arg: a + " " + args[i+1]}
				}
			}
		}
	}
	if !slices.Contains(args, "--ephemeral") || !slices.Contains(args, "--output-schema") {
		return errors.New("codexreview: the command lacks --ephemeral or --output-schema")
	}
	return nil
}

func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// BuildArgs returns the codex arguments for spec. The order is fixed
// (golden-tested). The context pack is not an argument: "-" reads it from
// stdin.
func BuildArgs(caps *Capabilities, spec CommandSpec) ([]string, error) {
	if m := caps.Missing(); len(m) > 0 {
		return nil, &ErrMissingFlags{Flags: m}
	}
	if spec.Model == "" || spec.SchemaPath == "" || spec.LastMessagePath == "" || spec.Dir == "" {
		return nil, errors.New("codexreview: model, worktree, schema path, and last-message path are required")
	}
	for _, p := range []string{spec.Dir, spec.SchemaPath, spec.LastMessagePath} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("codexreview: %s is not absolute", p)
		}
	}
	args := []string{"exec", "-m", spec.Model}
	if spec.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(spec.ReasoningEffort))
	}
	if spec.WindowsSandbox != "" {
		args = append(args, "-c", "windows.sandbox="+tomlString(spec.WindowsSandbox))
	}
	args = append(args, "-c", `web_search="disabled"`)
	for _, f := range DisabledFeatures {
		if caps.HasFeature(f) {
			args = append(args, "--disable", f)
		}
	}
	args = append(args,
		// The user's config.toml (MCP servers, notify hooks, profiles,
		// sandbox settings) and execpolicy rules (user or repository
		// .rules files that could allow commands) do not apply.
		"--ignore-user-config",
		"--ignore-rules",
		"-C", spec.Dir,
		"--sandbox", "read-only",
		"--ephemeral",
		"--json",
	)
	if spec.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if caps.Has("color") {
		args = append(args, "--color", "never")
	}
	args = append(args,
		"--output-schema", spec.SchemaPath,
		"-o", spec.LastMessagePath,
		"-",
	)
	if err := CheckArgs(args); err != nil {
		return nil, err
	}
	return args, nil
}
