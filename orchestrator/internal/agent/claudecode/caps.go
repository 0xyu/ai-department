// Package claudecode drives Claude Code headlessly (`claude -p --output-format
// stream-json`) as ACO's planner (read-only, Stage 1) and implementer
// (Stages 3–4) — §16.2. It builds the command for each profile, generates a
// settings file whose PreToolUse hook runs `aco hook pre-tool-use`, starts
// the CLI as a killable process tree with an allowlisted environment and
// only the credential of the resolved auth mode, parses the stream, verifies
// the auth source from the init message, enforces idle/total timeouts and
// turn limits, captures the masked transcript, extracts the structured
// output, and classifies failures (§14.1).
package claudecode

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/proc"
)

// Capabilities are the flags the installed CLI supports.
type Capabilities struct {
	Version string          // e.g. "2.1.311"
	Flags   map[string]bool // long flag names without "--"
}

// RequiredFlags must all be present; doctor and the adapter fail otherwise.
var RequiredFlags = []string{
	"print", "output-format", "verbose", "session-id", "resume", "model", "permission-mode",
	"tools", "allowedTools", "disallowedTools", "settings", "setting-sources", "strict-mcp-config",
	"json-schema", "append-system-prompt",
}

// hiddenFlags are supported but not listed by --help in the versions ACO was
// verified with (2.1.311). They are assumed from major version 2 on.
var hiddenFlags = []string{"max-turns"}

var flagRE = regexp.MustCompile(`--([A-Za-z][A-Za-z0-9-]*)`)

// ParseCapabilities reads `claude --version` and `claude --help` output.
func ParseCapabilities(versionOut, helpOut string) *Capabilities {
	c := &Capabilities{Flags: map[string]bool{}}
	if v, vs, ok := parseVersion(versionOut); ok {
		c.Version = vs
		if v[0] >= 2 {
			for _, f := range hiddenFlags {
				c.Flags[f] = true
			}
		}
	}
	for _, m := range flagRE.FindAllStringSubmatch(helpOut, -1) {
		c.Flags[m[1]] = true
	}
	return c
}

// Has reports whether flag (without "--") is supported.
func (c *Capabilities) Has(flag string) bool { return c != nil && c.Flags[flag] }

// Missing lists required flags the CLI lacks.
func (c *Capabilities) Missing() []string {
	var out []string
	for _, f := range RequiredFlags {
		if !c.Has(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Effort mechanisms.
const (
	EffortFlag     = "flag"     // --effort <level>
	EffortSettings = "settings" // "effortLevel" in the generated settings file
)

// EffortVia says how effort is set: the --effort flag when the CLI has it,
// else the settings key.
func (c *Capabilities) EffortVia() string {
	if c.Has("effort") {
		return EffortFlag
	}
	return EffortSettings
}

// detectTimeout bounds `claude --version` and `claude --help`.
const detectTimeout = 30 * time.Second

// Detect runs the CLI to read its version and flags. env is the child
// environment (no credentials are needed for these calls).
func Detect(ctx context.Context, bin string, prefix []string, env []string) (*Capabilities, error) {
	run := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, detectTimeout)
		defer cancel()
		cmd := exec.Command(bin, append(append([]string{}, prefix...), args...)...)
		cmd.Env = env
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		t, err := proc.Start(cmd)
		if err != nil {
			return "", err
		}
		defer t.Close()
		done := make(chan error, 1)
		go func() { done <- t.Wait() }()
		select {
		case err := <-done:
			return out.String(), err
		case <-cctx.Done():
			_ = t.Kill()
			<-done
			return "", fmt.Errorf("claude %s: %w", strings.Join(args, " "), cctx.Err())
		}
	}
	v, err := run("--version")
	if err != nil {
		return nil, fmt.Errorf("claude --version: %w", err)
	}
	h, err := run("--help")
	if err != nil {
		return nil, fmt.Errorf("claude --help: %w", err)
	}
	return ParseCapabilities(v, h), nil
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func parseVersion(s string) ([3]int, string, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, "", false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, m[0], true
}
