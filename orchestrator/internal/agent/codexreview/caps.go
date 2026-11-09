// Package codexreview drives Codex CLI headlessly (`codex exec --json`) as
// ACO's independent reviewer (§10.3, §11.6, §12.5, §16.2): GPT Sol 6.1 in a
// new ephemeral, read-only session per review, the context pack on stdin,
// the answer constrained by --output-schema and read from -o. The adapter
// verifies the login against the resolved auth mode before every review,
// runs the CLI as a killable process tree with an allowlisted environment,
// parses the JSONL event stream (commands, file changes, errors, token
// usage), flags reads outside the worktree and the artifact directory as
// policy violations (§16.3), validates the review, retries a schema-invalid
// answer with the validation errors (max 2) and then the fallback model,
// and classifies failures (§14.1).
package codexreview

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

// Capabilities are what the installed Codex CLI supports.
type Capabilities struct {
	Version   string          // e.g. "0.162.0"
	Flags     map[string]bool // long flags of `codex exec` without "--"
	Sandboxes map[string]bool // values of --sandbox
	// Features are the names `codex features list` reports, with their
	// stage; nil when the list could not be read.
	Features map[string]string
}

// RequiredFlags must all be present in `codex exec --help`.
var RequiredFlags = []string{
	"model", "config", "cd", "sandbox", "ephemeral", "json", "output-schema", "output-last-message",
	"ignore-user-config", "ignore-rules", "disable",
}

var (
	flagRE     = regexp.MustCompile(`--([A-Za-z][A-Za-z0-9-]*)`)
	sandboxRE  = regexp.MustCompile(`(?s)--sandbox\b.*?possible values:\s*([^\]]+)\]`)
	versionRE  = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)
	featureRE  = regexp.MustCompile(`^([a-z0-9_.]+)\s+(stable|experimental|under development|deprecated|removed|beta)\s+(true|false)\s*$`)
	knownStage = map[string]bool{"stable": true, "experimental": true, "beta": true, "deprecated": true}
)

// ParseCapabilities reads `codex --version`, `codex exec --help`, and
// `codex features list` output (features may be empty).
func ParseCapabilities(versionOut, execHelp, features string) *Capabilities {
	c := &Capabilities{Flags: map[string]bool{}, Sandboxes: map[string]bool{}}
	if m := versionRE.FindString(versionOut); m != "" {
		c.Version = m
	}
	for _, m := range flagRE.FindAllStringSubmatch(execHelp, -1) {
		c.Flags[m[1]] = true
	}
	if m := sandboxRE.FindStringSubmatch(execHelp); m != nil {
		for _, v := range strings.Split(m[1], ",") {
			c.Sandboxes[strings.TrimSpace(v)] = true
		}
	}
	if strings.TrimSpace(features) != "" {
		c.Features = map[string]string{}
		for _, l := range strings.Split(features, "\n") {
			if m := featureRE.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
				c.Features[m[1]] = m[2]
			}
		}
	}
	return c
}

// Has reports whether `codex exec` has flag (without "--").
func (c *Capabilities) Has(flag string) bool { return c != nil && c.Flags[flag] }

// Missing lists the required flags (and the read-only sandbox) the CLI lacks.
func (c *Capabilities) Missing() []string {
	var out []string
	for _, f := range RequiredFlags {
		if !c.Has(f) {
			out = append(out, "--"+f)
		}
	}
	if c == nil || !c.Sandboxes["read-only"] {
		out = append(out, "--sandbox read-only")
	}
	sort.Strings(out)
	return out
}

// HasFeature reports whether a feature exists and can be switched (not
// removed or still under development).
func (c *Capabilities) HasFeature(name string) bool {
	if c == nil || c.Features == nil {
		return false
	}
	return knownStage[c.Features[name]]
}

// detectTimeout bounds each detection command.
const detectTimeout = 30 * time.Second

// Detect runs the CLI to read its version, flags, and features. env is the
// child environment (no credential is needed for these calls).
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
			return "", fmt.Errorf("codex %s: %w", strings.Join(args, " "), cctx.Err())
		}
	}
	v, err := run("--version")
	if err != nil {
		return nil, fmt.Errorf("codex --version: %w", err)
	}
	h, err := run("exec", "--help")
	if err != nil {
		return nil, fmt.Errorf("codex exec --help: %w", err)
	}
	f, _ := run("features", "list") // optional: without it no feature is switched off
	return ParseCapabilities(v, h, f), nil
}

// ParseVersion returns the numeric version (for display and comparisons).
func ParseVersion(s string) ([3]int, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}
