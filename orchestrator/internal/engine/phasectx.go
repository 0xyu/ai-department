package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/apidiff"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/state"
)

// reviewRequest builds the reviewer's context (§11.6) for tree, and the
// lines changed since the last review (scope-creep control).
func (p *phaseRun) reviewRequest(ctx context.Context, tree string, round int) (agent.CodeReviewRequest, ChangedLines, error) {
	repo := p.d.Worktree
	full, err := repo.Patch(ctx, p.base, tree, 3)
	if err != nil {
		return agent.CodeReviewRequest{}, nil, err
	}
	stat, err := repo.DiffStat(ctx, p.base, tree)
	if err != nil {
		return agent.CodeReviewRequest{}, nil, err
	}
	incremental := ""
	if p.att.reviewedTree != "" {
		b, err := repo.Patch(ctx, p.att.reviewedTree, tree, 3)
		if err != nil {
			return agent.CodeReviewRequest{}, nil, err
		}
		incremental = string(b)
	}
	_, _ = p.d.Run.Write(artifacts.DiffFile(p.in.Phase, p.attempt, round, "full.patch"), full)
	_, _ = p.d.Run.Write(artifacts.DiffFile(p.in.Phase, p.attempt, round, "stat.txt"), []byte(stat))
	if incremental != "" {
		_, _ = p.d.Run.Write(artifacts.DiffFile(p.in.Phase, p.attempt, round, "incremental.patch"), []byte(incremental))
	}
	changes, err := repo.Changes(ctx, p.base, tree)
	if err != nil {
		return agent.CodeReviewRequest{}, nil, err
	}
	pol, err := policy.CheckDiff(ctx, repo, p.base, tree, policy.FromConfig(p.d.Config.Policy, p.spec.Scope.Paths, p.d.Redactor))
	if err != nil {
		return agent.CodeReviewRequest{}, nil, err
	}
	phases, err := p.phaseStatuses(ctx)
	if err != nil {
		return agent.CodeReviewRequest{}, nil, err
	}
	impl := ""
	if p.att.lastReport != nil {
		b, _ := json.MarshalIndent(p.att.lastReport, "", "  ")
		impl = string(b)
	}
	var ledger []agent.LedgerEntry
	if round > 1 {
		ledger = p.ledger.ForReview()
	}
	changed := ChangedLines{}
	if round > 1 {
		changed = changedLines(incremental)
	}
	req := agent.CodeReviewRequest{
		RunID: p.in.RunID, Kind: agent.ReviewPhase, Phase: p.in.Phase, Attempt: p.attempt, Round: round,
		Workspace:    agent.Workspace{Dir: p.wt(), ProtectedPaths: p.d.Config.Policy.ProtectedPaths, Network: "deny"},
		PlanOverview: p.overview, Phases: phases, PhaseSpec: p.specText, AgentsMD: p.agentsText, ACIDs: p.acIDs,
		Diff: string(full), DiffStat: stat, IncrementalDiff: incremental,
		GateResults: renderGateResults(p.att.lastRound), Baseline: renderBaseline(p.baseline, p.att.lastRound),
		TestChanges: renderTestChanges(changes), Regressions: p.renderRegressions(), Security: renderSecurity(p.att.lastRound, pol),
		Importers: relatedFiles(p.wt(), changes), ImplReport: impl, Ledger: ledger, BudgetScopes: p.scopes(),
	}
	return req, changed, nil
}

func (p *phaseRun) phaseStatuses(ctx context.Context) ([]agent.PhaseStatus, error) {
	phases, err := p.d.Store.ListPhases(ctx, p.in.RunID)
	if err != nil {
		return nil, err
	}
	var out []agent.PhaseStatus
	for _, ph := range phases {
		st := "future"
		switch {
		case ph.Number == p.in.Phase:
			st = "current"
		case ph.Status == state.PhaseCommitted:
			st = "done"
		}
		out = append(out, agent.PhaseStatus{Number: ph.Number, Title: ph.Title, Status: st})
	}
	return out, nil
}

// changedLines collects the new-side line numbers a diff adds or changes.
func changedLines(diff string) ChangedLines {
	out := ChangedLines{}
	for _, f := range apidiff.Parse(diff) {
		name := strings.TrimPrefix(f.Path(), "b/")
		for _, h := range f.Hunks {
			n := h.NewStart
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					out[name] = append(out[name], n)
					n++
				case strings.HasPrefix(l, " "):
					n++
				}
			}
		}
		if _, ok := out[name]; !ok {
			out[name] = nil // a changed file without added lines (deletions only)
		}
	}
	return out
}

func renderGateResults(r *gates.Round) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Tree %s. These results are authoritative (run by the orchestrator).\n", r.TreeHash))
	for _, g := range r.Results {
		var cmds []string
		for _, c := range g.Commands {
			cmds = append(cmds, c.Command.Run)
		}
		req := "required"
		if !g.Required {
			req = "not applicable: " + g.Justification
		}
		b.WriteString(fmt.Sprintf("- %s: %s (%s; source %s; %s) %s\n", g.Gate, g.Status, req, g.Source, g.Duration.Round(1e6), g.Summary))
		if len(cmds) > 0 {
			b.WriteString(fmt.Sprintf("  commands: %s\n", strings.Join(cmds, "; ")))
		}
		passed, failed := 0, 0
		for _, t := range g.Tests {
			switch t.Status {
			case gates.TestPass:
				passed++
			case gates.TestFail:
				failed++
			}
		}
		if passed+failed > 0 {
			b.WriteString(fmt.Sprintf("  tests: %d passed, %d failed\n", passed, failed))
		}
		// The test identities a regression item can cite as evidence.
		named := 0
		for _, t := range g.Tests {
			if t.Name == "" {
				continue
			}
			if named == maxListedTests {
				b.WriteString(fmt.Sprintf("  … %d more test results in the gate artifacts\n", len(g.Tests)-named))
				break
			}
			named++
			b.WriteString(fmt.Sprintf("  test %s: %s\n", t.ID(), t.Status))
		}
	}
	return b.String()
}

// maxListedTests caps the per-gate test list in the review pack.
const maxListedTests = 200

func renderBaseline(base *gates.Baseline, r *gates.Round) string {
	if base == nil {
		return "No baseline."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Baseline: commit %s (tree %s), recorded %s.\n", base.Commit, base.TreeHash, base.CreatedAt.Format("2006-01-02 15:04 UTC")))
	if r == nil {
		return b.String()
	}
	for _, g := range r.Results {
		c := g.Comparison
		if c == nil {
			continue
		}
		appendf(&b, "- %s: new failing tests %d, fixed %d, pre-existing failures %d; new issues %d, pre-existing %d; new vulnerabilities %d, pre-existing %d\n",
			g.Gate, len(c.NewFailingTests), len(c.FixedTests), len(c.PreexistingFailures), len(c.NewIssues), c.PreexistingIssues,
			len(c.NewVulns), len(c.PreexistingVulns))
	}
	return b.String()
}

func renderTestChanges(cs []gitops.Change) string {
	var b strings.Builder
	for _, c := range cs {
		if !policy.IsTestFile(c.Path) && !(c.OldPath != "" && policy.IsTestFile(c.OldPath)) {
			continue
		}
		kind := map[string]string{gitops.Added: "added", gitops.Deleted: "removed", gitops.Renamed: "renamed"}[c.Status]
		if kind == "" {
			kind = "changed"
		}
		b.WriteString(fmt.Sprintf("- %s %s (+%d -%d)\n", kind, c.Path, c.Added, c.Deleted))
	}
	if b.Len() == 0 {
		return "No test files were added, changed, or removed."
	}
	return b.String()
}

func (p *phaseRun) renderRegressions() string {
	var cur []string
	if r := p.att.lastRound; r != nil {
		for _, g := range r.Results {
			if g.Comparison != nil {
				cur = append(cur, g.Comparison.Regressions...)
			}
		}
	}
	var b strings.Builder
	if len(cur) == 0 {
		b.WriteString("No test that passed at baseline fails on this tree.\n")
	} else {
		b.WriteString("Tests that passed at baseline and fail now:\n")
		for _, t := range cur {
			b.WriteString("- " + t + "\n")
		}
	}
	if len(p.seenRegr) > 0 {
		b.WriteString("Regressions detected in earlier gate rounds of this phase (check that the fixes are real and not test changes):\n")
		for _, t := range p.seenRegr {
			b.WriteString("- " + t + "\n")
		}
	}
	return b.String()
}

func renderSecurity(r *gates.Round, pol policy.Result) string {
	var b strings.Builder
	if r != nil {
		if g := r.Gate(gates.Security); g != nil {
			b.WriteString(fmt.Sprintf("Security gate: %s. %s\n", g.Status, g.Summary))
			for i, is := range g.Issues {
				if i == 50 {
					b.WriteString(fmt.Sprintf("… %d more issues\n", len(g.Issues)-i))
					break
				}
				b.WriteString(fmt.Sprintf("- issue %s %s %s:%d %s %s\n", is.Severity, is.Tool, is.Path, is.Line, is.Rule, is.Message))
			}
			for _, v := range g.Vulns {
				b.WriteString(fmt.Sprintf("- vulnerability %s %s in %s %s\n", v.Severity, v.ID, v.Package, v.Version))
			}
		}
	}
	if len(pol.Findings) == 0 {
		b.WriteString("Diff policy and anti-gaming checks: no findings.\n")
	} else {
		b.WriteString("Diff policy and anti-gaming flags:\n")
		for _, f := range pol.Findings {
			blocking := ""
			if f.Blocking {
				blocking = " (blocking)"
			}
			b.WriteString(fmt.Sprintf("- %s%s\n", f.String(), blocking))
		}
	}
	return b.String()
}

// --- related files (§11.6 regression data) --------------------------------------

const (
	maxRelatedWalk = 5000
	maxRelated     = 60
)

var moduleRE = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// relatedFiles lists files that import the changed Go packages and test
// files next to changed source files: the code a regression would show in.
func relatedFiles(root string, cs []gitops.Change) string {
	changedDirs := map[string]bool{}
	changedFile := map[string]bool{}
	for _, c := range cs {
		changedFile[c.Path] = true
		if policy.IsSource(c.Path) && !policy.IsTestFile(c.Path) {
			changedDirs[path.Dir(c.Path)] = true
		}
	}
	if len(changedDirs) == 0 {
		return "No changed source files."
	}
	module := ""
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		if m := moduleRE.FindSubmatch(b); m != nil {
			module = string(m[1])
		}
	}
	imports := map[string]string{} // quoted import path -> dir
	if module != "" {
		for d := range changedDirs {
			ip := module
			if d != "." {
				ip += "/" + d
			}
			imports[`"`+ip+`"`] = d
		}
	}
	var importers, tests []string
	walked := 0
	_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if rel != "." && (skipDirs[e.Name()] || strings.HasPrefix(rel, ".aco")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() {
			return nil
		}
		walked++
		if walked > maxRelatedWalk || len(importers)+len(tests) >= maxRelated {
			return filepath.SkipAll
		}
		if policy.IsTestFile(rel) && changedDirs[path.Dir(rel)] {
			tests = append(tests, rel)
		}
		if len(imports) == 0 || !strings.HasSuffix(rel, ".go") || changedFile[rel] {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 1<<20 {
			return nil
		}
		for q, d := range imports {
			if path.Dir(rel) != d && strings.Contains(string(b), q) {
				importers = append(importers, rel)
				break
			}
		}
		return nil
	})
	sort.Strings(importers)
	sort.Strings(tests)
	var b strings.Builder
	b.WriteString("Files that import changed packages:\n")
	if len(importers) == 0 {
		b.WriteString("- none found\n")
	}
	for _, f := range importers {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("Tests next to changed code:\n")
	if len(tests) == 0 {
		b.WriteString("- none found\n")
	}
	for _, f := range tests {
		b.WriteString("- " + f + "\n")
	}
	return b.String()
}

// --- lessons (§11.7 phase restart) ----------------------------------------------

const maxLessons = 4000

// buildLessons is the brief for the next attempt: why the attempt ended,
// what the reviewer kept finding, and what was failing.
func (p *phaseRun) buildLessons(trip *GuardTrip) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Attempt %d ended: %s (%s).\n", p.attempt, trip.Detail, trip.Guard))
	if open := p.ledger.OpenEntries(); len(open) > 0 {
		b.WriteString("Open review findings at the end of the attempt:\n")
		for _, e := range FixOrder(open) {
			appendf(&b, "- [%s/%s] %s%s — required: %s\n", e.Finding.Severity, e.Finding.Category, oneLine(e.Finding.Title),
				locSuffix(e.Location()), oneLine(e.Finding.RequiredChange))
		}
	}
	if r := p.att.lastRound; r != nil && !r.Passed() {
		b.WriteString("Failing quality gates:\n")
		for _, g := range r.Results {
			if !g.Status.OK() && g.Status != gates.Skipped {
				b.WriteString(fmt.Sprintf("- %s\n", oneLine(g.Summary)))
			}
		}
	}
	if rep := p.att.lastReport; rep != nil && len(rep.KnownGaps) > 0 {
		b.WriteString("Known gaps reported by the previous attempt:\n")
		for _, g := range rep.KnownGaps {
			b.WriteString(fmt.Sprintf("- %s\n", oneLine(g)))
		}
	}
	s := p.d.Redactor.String(b.String())
	if len(s) > maxLessons {
		s = s[:maxLessons] + "\n…"
	}
	return s
}

func locSuffix(l string) string {
	if l == "" {
		return ""
	}
	return " (" + l + ")"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- blocked report (§11.7) -----------------------------------------------------

var humanDecision = map[state.Class]string{
	state.ClassInitCheckFailed:   "Restore the expected run state (plan files, run branch, dependency phases), then `aco resume <run> --retry`, or restart from an earlier phase with `--from-phase`.",
	state.ClassEnvironment:       "Install or start the missing tool or service (see the gate log), then `aco resume <run> --retry`.",
	state.ClassSpecConflict:      "The frozen plan cannot be satisfied as written. Decide how the plan should change and run `aco resume <run> --revise-plan`.",
	state.ClassUnsafe:            "A fix would need protected paths, secrets, destructive operations, or disabled checks. Decide whether to change the plan (`--revise-plan`) or fix it by hand, then resume.",
	state.ClassIterationLimit:    "The implementation did not converge. Review the open findings and gate logs; adjust the plan (`--revise-plan`) or retry (`--retry`).",
	state.ClassOscillation:       "The same finding kept coming back. Decide which side is right (implementer or reviewer) and clarify the plan, then resume.",
	state.ClassNoProgress:        "Fix rounds made no changes. Check whether the findings are actionable within the phase scope.",
	state.ClassDisputeDeadlock:   "Implementer and reviewer disagree on a finding. Rule on it (see the ledger), then resume.",
	state.ClassPolicyViolations:  "The implementer or reviewer repeatedly tried forbidden actions (possible prompt injection). Inspect the transcripts before resuming.",
	state.ClassPhaseUsageLimit:   "The phase used up its agent-call, turn, or time limit. Raise the limit or split the phase, then resume.",
	state.ClassPlanIntegrity:     "The frozen plan or AGENTS.md changed during the phase. Restore them from the freeze commit, then `--retry`.",
	state.ClassCommitGateBlocked: "The final policy re-check failed (protected path, secret, or scope). Inspect the WIP ref and decide, then resume.",
}

func (p *phaseRun) writeBlockedReport(ctx context.Context, class state.Class, detail string, cp *gitops.Checkpoint) (string, error) {
	var b strings.Builder
	title := ""
	if p.spec != nil {
		title = p.spec.Title
	}
	b.WriteString(fmt.Sprintf("# Phase %02d blocked: %s\n\n", p.in.Phase, oneLine(title)))
	appendf(&b, "- Run: %s\n- Class: %s\n- Attempt: %d\n- Detail: %s\n- Work in progress: `%s` (commit %s); the run branch was not changed\n",
		p.in.RunID, class, p.attempt, oneLine(detail), cp.Ref, cp.Commit)
	if p.base != "" {
		b.WriteString(fmt.Sprintf("- Phase base: %s\n", p.base))
	}
	b.WriteString(fmt.Sprintf("\n## What a human must decide\n\n%s\n", humanDecision[class]))
	b.WriteString("\n## Open findings\n\n")
	open := p.ledger.OpenEntries()
	if len(open) == 0 {
		b.WriteString("None.\n")
	}
	for _, e := range FixOrder(open) {
		resp := ""
		if e.Response != nil {
			resp = " — implementer: " + e.Response.Response + ": " + oneLine(e.Response.Detail)
		}
		appendf(&b, "- %s [%s/%s, %s] %s%s — required: %s%s\n", e.ID, e.Finding.Severity, e.Finding.Category, e.Class,
			oneLine(e.Finding.Title), locSuffix(e.Location()), oneLine(e.Finding.RequiredChange), resp)
	}
	b.WriteString("\n## Last quality gate round\n\n")
	if r := p.att.lastRound; r == nil {
		b.WriteString("No gate round ran in this attempt.\n")
	} else {
		for _, g := range r.Results {
			logRef := ""
			if fc := g.FailedCommand(); fc != nil && fc.LogArtifact != "" {
				logRef = " (log: " + fc.LogArtifact + ")"
			}
			b.WriteString(fmt.Sprintf("- %s: %s — %s%s\n", g.Gate, g.Status, oneLine(g.Summary), logRef))
		}
	}
	if t := p.att.lastTrip; t != nil {
		b.WriteString(fmt.Sprintf("\n## Guard\n\n%s: %s\n", t.Guard, t.Detail))
	}
	b.WriteString("\n## Resume options\n\n- `aco resume <run> --retry`\n- `aco resume <run> --revise-plan`\n- `aco resume <run> --from-phase N`\n")
	text := p.d.Redactor.String(b.String())
	rel := artifacts.ReportFile(fmt.Sprintf("phase-%02d-blocked.md", p.in.Phase))
	ref, err := p.d.Run.Write(rel, []byte(text))
	if err != nil {
		return "", err
	}
	if err := p.d.Store.AddReport(ctx, state.Report{RunID: p.in.RunID, Kind: "blocked", Phase: p.in.Phase, Path: ref.Path,
		SHA256: ref.SHA256}); err != nil {
		return "", err
	}
	return ref.Path, nil
}

// appendf appends formatted document text (English, not operator output).
func appendf(b *strings.Builder, format string, a ...any) { b.WriteString(fmt.Sprintf(format, a...)) }
