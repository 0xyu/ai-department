package gates

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
)

// Built-in security checks (§11.5): the diff policy (protected and scope
// paths, binary and oversized files, anti-gaming, missing tests) and the
// built-in secret scanner. In ModePhase both run on the phase diff (Base →
// working state). In ModeFull the secret scanner reads every file of the
// working state and every commit of the run's history (Base..HEAD); the
// diff policy, which judges one phase's changes, does not run.

// Tools reported for built-in findings.
const (
	ToolPolicy  = "policy"
	ToolSecrets = "secrets"
)

var secretCodes = map[string]bool{policy.CodeSecret: true, policy.CodeSecretFile: true}

func (r *Runner) builtinScans(ctx context.Context, s Spec, mode Mode, tree string) ([]Issue, *CommandResult, error) {
	repo := r.opts.Repo
	if repo == nil || tree == "" {
		return nil, nil, nil
	}
	secrets := slices.Contains(s.Scanners, ScannerSecrets)
	start := r.opts.Now()
	var issues []Issue
	label := "aco: built-in diff policy"
	switch mode {
	case ModeFull:
		if !secrets {
			return nil, nil, nil
		}
		label = "aco: built-in secret scan (all files and run history)"
		found, err := r.fullSecretScan(ctx, tree)
		if err != nil {
			return nil, nil, err
		}
		issues = found
	default:
		if r.opts.Base == "" {
			return nil, nil, nil
		}
		if secrets {
			label += " + secret scan"
		}
		res, err := policy.CheckDiff(ctx, repo, r.opts.Base, tree, r.opts.Policy)
		if err != nil {
			return nil, nil, fmt.Errorf("gates: diff policy: %w", err)
		}
		for _, f := range res.Findings {
			if secretCodes[f.Code] && !secrets {
				continue
			}
			issues = append(issues, policyIssue(f, ""))
		}
	}
	cr := &CommandResult{Command: Command{Run: label, Scanner: ScannerSecrets}, Status: Passed, Parsed: true,
		Duration: r.opts.Now().Sub(start)}
	var lines []string
	for _, is := range issues {
		lines = append(lines, fmt.Sprintf("%s %s:%d %s: %s", is.Tool, is.Path, is.Line, is.Rule, is.Message))
	}
	cr.Log = r.opts.Redactor.String(strings.Join(lines, "\n"))
	return issues, cr, nil
}

func policyIssue(f policy.Finding, prefix string) Issue {
	tool := ToolPolicy
	sev := SevMedium
	if secretCodes[f.Code] {
		tool = ToolSecrets
	}
	if f.Blocking {
		sev = SevHigh
		if tool == ToolSecrets {
			sev = SevCritical
		}
	}
	rule := f.Code
	if f.Rule != "" {
		rule = f.Code + "/" + f.Rule
	}
	return Issue{Tool: tool, Rule: rule, Path: prefix + f.Path, Line: f.Line, Severity: sev, Message: f.Message, Blocking: f.Blocking}
}

// secretOnlyPolicy runs policy.Check for its secret findings only.
func (r *Runner) secretOnlyPolicy() policy.Config {
	return policy.Config{TestsMissing: "warn", AntiGaming: "warn", BinaryFiles: "warn", MaxFileBytes: 1 << 62,
		Redactor: r.opts.Policy.Redactor}
}

func (r *Runner) fullSecretScan(ctx context.Context, tree string) ([]Issue, error) {
	repo := r.opts.Repo
	cfg := r.secretOnlyPolicy()
	var out []Issue
	keep := func(res policy.Result, prefix string) {
		for _, f := range res.Findings {
			if secretCodes[f.Code] || f.Code == policy.CodeScanIncomplete {
				is := policyIssue(f, prefix)
				if prefix == "" && is.Tool == ToolSecrets {
					// A secret in the current files may predate the run:
					// judged against the baseline by severity, not blocking
					// on its own. Secrets in the run's commits always block.
					is.Blocking = false
				}
				out = append(out, is)
			}
		}
	}
	res, err := policy.CheckDiff(ctx, repo, gitops.EmptyTree, tree, cfg)
	if err != nil {
		return nil, fmt.Errorf("gates: secret scan: %w", err)
	}
	keep(res, "")
	head, err := repo.Head(ctx)
	if err != nil {
		return nil, fmt.Errorf("gates: secret scan: %w", err)
	}
	commits, err := repo.RevList(ctx, r.opts.Base, head)
	if err != nil {
		return nil, fmt.Errorf("gates: secret scan history: %w", err)
	}
	for _, c := range commits {
		parent := c.Parent
		if parent == "" {
			parent = gitops.EmptyTree
		}
		res, err := policy.CheckDiff(ctx, repo, parent, c.Commit, cfg)
		if err != nil {
			return nil, fmt.Errorf("gates: secret scan history: %w", err)
		}
		keep(res, "commit "+shortSHA(c.Commit)+": ")
	}
	return out, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
