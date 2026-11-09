package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/state"
)

// phaseEnv is a run with a frozen two-phase plan, ready for Stage 3.
type phaseEnv struct {
	*env
	freeze string
	impl   *fake.Implementer
	gates  *fakeGates
	rev    agent.Reviewer // e.reviewer unless a test wraps it
}

type phaseOpts struct {
	integration bool
	agentsMD    bool
}

func newPhaseEnv(t *testing.T, o phaseOpts) *phaseEnv {
	t.Helper()
	e := newEnv(t, envOpts{mode: state.ModeFull, agentsMD: o.agentsMD})
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2, integration: o.integration,
		agentsMD: "# Conventions\n\n- Use table tests.\n"}))}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: fake.PlanReviewPass(reqIDs(2))}}
	out := e.plan(PlanningInput{Requirements: requirements})
	if !out.Frozen() || out.Status != state.RunExecuting {
		t.Fatalf("plan not frozen: %+v", out)
	}
	pe := &phaseEnv{env: e, freeze: out.Commit, impl: &fake.Implementer{}, gates: &fakeGates{wt: e.wt}}
	pe.rev = e.reviewer
	return pe
}

func (pe *phaseEnv) phaseDeps() PhaseDeps {
	return PhaseDeps{Store: pe.store, Run: pe.runDir, Implementer: pe.impl, Reviewer: pe.rev, Worktree: pe.wt, Config: pe.cfg,
		Owner: "test-owner", Gates: pe.gates.factory,
		Now: func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }}
}

func (pe *phaseEnv) runPhase(n int) *PhaseOutcome {
	pe.t.Helper()
	out, err := RunPhase(pe.ctx, pe.phaseDeps(), PhaseInput{RunID: pe.run.ID, Phase: n})
	if err != nil {
		pe.t.Fatalf("RunPhase(%d): %v", n, err)
	}
	return out
}

func (pe *phaseEnv) wtGit(args ...string) string { return gitT(pe.t, pe.gitEnv, pe.wt.Dir(), args...) }

// --- fake gate runner ---

// fakeGates returns scripted gate rounds bound to the real working tree
// hash. Script entry i is round i+1 (non-baseline); a nil entry or a round
// past the script passes every gate.
type fakeGates struct {
	mu        sync.Mutex
	wt        *gitops.Repo
	script    []map[string]*gates.Result
	rounds    int
	baselines int
	compared  int // rounds that had a baseline to compare with
	// during runs inside gate i of non-baseline round n (1-based) and may
	// change the worktree; each result then records the tree it started
	// on and, if it moved, TreeAfter (like the real runner).
	during func(round, gate int)
	// baselineScript and duringBaseline are script and hook of baseline
	// round n (1-based).
	baselineScript []map[string]*gates.Result
	duringBaseline func(baseline, gate int)
	// evaluate judges scripted non-baseline results against the baseline
	// with the real comparison (gates.Evaluate) instead of trusting their
	// status.
	evaluate bool
}

func (f *fakeGates) factory(o gates.Options) (GateRunner, error) { return f, nil }

func (f *fakeGates) Run(ctx context.Context, specs []gates.Spec, ro gates.RunOptions) (*gates.Round, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree, err := f.wt.WorkingTree(ctx)
	if err != nil {
		return nil, err
	}
	round := &gates.Round{TreeHash: tree, Mode: ro.Mode, Baseline: ro.Baseline}
	var script map[string]*gates.Result
	hook, n := f.during, 0
	if ro.Baseline {
		f.baselines++
		hook, n = f.duringBaseline, f.baselines
		if f.baselines <= len(f.baselineScript) {
			script = f.baselineScript[f.baselines-1]
		}
	} else {
		n = f.rounds + 1
		f.rounds++
		if ro.Compare != nil {
			f.compared++
		}
		if f.rounds <= len(f.script) {
			script = f.script[f.rounds-1]
		}
	}
	stop := false
	for i, s := range specs {
		gateTree, after := tree, ""
		if hook != nil && !stop {
			if gateTree, err = f.wt.WorkingTree(ctx); err != nil {
				return nil, err
			}
			hook(n, i)
			now, err := f.wt.WorkingTree(ctx)
			if err != nil {
				return nil, err
			}
			if now != gateTree {
				after = now
			}
		}
		if stop {
			round.Results = append(round.Results, &gates.Result{Gate: s.Type, Status: gates.Skipped, Required: s.Required, TreeHash: tree})
			continue
		}
		var res *gates.Result
		if r, ok := script[s.Type]; ok {
			c := *r
			res = &c
		} else {
			res = &gates.Result{Gate: s.Type, Status: gates.Passed, Summary: s.Type + ": passed",
				Tests: []gates.TestCase{{Package: "example.com/toy/calc", Name: "TestAdd", Status: gates.TestPass}}}
			if !s.Required {
				res.Status, res.Summary, res.Tests = gates.NotApplicable, s.Type+": not applicable", nil
			}
		}
		res.Required, res.Source, res.TreeHash, res.TreeAfter, res.Duration = s.Required, s.Source, gateTree, after, 1500*time.Millisecond
		if len(res.Commands) == 0 {
			for _, c := range s.Commands {
				res.Commands = append(res.Commands, gates.CommandResult{Command: c, Status: res.Status})
			}
		}
		switch {
		case !ro.Baseline && f.evaluate:
			var base *gates.Result
			if ro.Compare != nil {
				base = ro.Compare.Results[s.Type]
			}
			gates.Evaluate(res, base, "")
		case !ro.Baseline && res.Status != gates.NotApplicable && res.Comparison == nil:
			res.Comparison = &gates.Comparison{HasBaseline: ro.Compare != nil}
		}
		round.Results = append(round.Results, res)
		if ro.FailFast && !ro.Baseline && !res.Status.OK() {
			stop = true
		}
	}
	return round, nil
}

func (f *fakeGates) count() (rounds, baselines int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rounds, f.baselines
}

func lintFail(file string) map[string]*gates.Result {
	return map[string]*gates.Result{gates.Lint: {Gate: gates.Lint, Status: gates.Failed, Class: gates.ClassQuality,
		Summary: "lint: failed: 1 failing command(s)",
		Commands: []gates.CommandResult{{Command: gates.Command{Run: "gofmt -l ."}, Status: gates.Failed, ExitCode: 1,
			Log: file + "\n"}},
		Comparison: &gates.Comparison{HasBaseline: true, NewFailures: []string{"gofmt -l ."}}}}
}

const regressedTest = "example.com/toy/calc.TestIntegrationAdd"

func integrationRegression() map[string]*gates.Result {
	return map[string]*gates.Result{gates.Integration: {Gate: gates.Integration, Status: gates.Failed, Class: gates.ClassQuality,
		Summary: "integration: failed: 1 new failing test(s), 1 regression(s)",
		Tests:   []gates.TestCase{{Package: "example.com/toy/calc", Name: "TestIntegrationAdd", Status: gates.TestFail}},
		Commands: []gates.CommandResult{{Command: gates.Command{Run: "go test -tags=integration ./..."}, Status: gates.Failed,
			ExitCode: 1, Log: "--- FAIL: TestIntegrationAdd\n"}},
		Comparison: &gates.Comparison{HasBaseline: true, NewFailingTests: []string{regressedTest}, Regressions: []string{regressedTest}}}}
}

func environmentFail() map[string]*gates.Result {
	return map[string]*gates.Result{gates.Unit: {Gate: gates.Unit, Status: gates.Failed, Class: gates.ClassEnvironment,
		Summary: "unit: environment problem (command not found): go test ./...",
		Commands: []gates.CommandResult{{Command: gates.Command{Run: "go test ./..."}, Status: gates.Failed, ExitCode: 127,
			Class: gates.ClassEnvironment, Reason: "command not found", Log: "sh: go: command not found\n"}}}}
}

// --- implementer steps ---

var ac1 = []string{"AC-01.1", "AC-01.2"}

func edit(rel, content string) func(dir string) error {
	return func(dir string) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(content), 0o644)
	}
}

// implReport is a report of a change to calc.go and its test.
func implReport(summary string, acIDs []string, findingIDs ...string) *agent.ImplReport {
	r := fake.Report(summary, acIDs, findingIDs...)
	r.FilesChanged = []agent.FileChange{{Path: "calc/calc.go", Change: "modified", Reason: "the operation"},
		{Path: "calc/calc_test.go", Change: "modified", Reason: "table test"}}
	r.DesignDecisions = []string{"Keep the calculator in one package."}
	r.Tests = []agent.TestChange{{Path: "calc/calc_test.go", Change: "modified", Covers: acIDs}}
	return r
}

var calcV = 0

// calcEdit changes calc.go and its test (a new version each call).
func calcEdit() func(dir string) error {
	calcV++
	v := calcV
	return func(dir string) error {
		if err := edit("calc/calc.go", fmt.Sprintf("package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b } // v%d\n", v))(dir); err != nil {
			return err
		}
		return edit("calc/calc_test.go", fmt.Sprintf("package calc\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) { // v%d\n\tif Sub(3, 1) != 2 {\n\t\tt.Fatal()\n\t}\n}\n", v))(dir)
	}
}

func startStep() fake.ImplStep {
	return fake.ImplStep{Edit: calcEdit(), Result: fake.ImplResult(implReport("implemented subtraction", ac1))}
}

func fixStep(ids ...string) fake.ImplStep {
	return fake.ImplStep{Edit: calcEdit(), Result: fake.ImplResult(implReport("fixed", ac1, ids...))}
}

// noEditFix answers "fixed" without changing anything.
func noEditFix(ids ...string) fake.ImplStep {
	return fake.ImplStep{Result: fake.ImplResult(implReport("fixed", ac1, ids...))}
}

func answer(id, response string) fake.ImplStep {
	r := implReport("answered", ac1)
	r.FindingResponses = []agent.FindingResponse{{ID: id, Response: response, Detail: "the frozen AC-01.2 contradicts AC-01.1"}}
	return fake.ImplStep{Result: fake.ImplResult(r)}
}

// --- code reviews ---

func finding(sev, cat, class, title string) agent.CodeReviewFinding {
	return agent.CodeReviewFinding{Severity: sev, Category: cat, ResolutionClass: class, File: "calc/calc.go", LineStart: 5, LineEnd: 5,
		Title: title, Description: "Sub ignores overflow.", Evidence: "calc.go:5", RequiredChange: "Handle overflow in Sub."}
}

// codeReview builds a review message: verdict, every AC met, the given
// previous statuses and new findings.
func codeReview(verdict string, acIDs []string, prev []agent.PreviousStatus, nf ...agent.CodeReviewFinding) string {
	r := agent.CodeReview{Verdict: verdict, Summary: "fake review",
		PlanConformance:      agent.StatusNotes{Status: "conforms", Notes: "ok"},
		SecurityAssessment:   agent.StatusNotes{Status: "no_issues", Notes: "ok"},
		RegressionAssessment: agent.RiskNotes{Risk: "low", Notes: "ok"},
		TestAssessment:       agent.TestAssessment{Adequate: true, Notes: "ok"},
		AcceptanceCriteria:   []agent.ACAssessment{}, PreviousFindings: []agent.PreviousStatus{}, NewFindings: []agent.CodeReviewFinding{}}
	for _, id := range acIDs {
		r.AcceptanceCriteria = append(r.AcceptanceCriteria, agent.ACAssessment{ID: id, Status: "met", Evidence: "TestSub"})
	}
	r.PreviousFindings = append(r.PreviousFindings, prev...)
	r.NewFindings = append(r.NewFindings, nf...)
	b, _ := json.Marshal(r)
	return string(b)
}

func prev(id, status string) agent.PreviousStatus {
	return agent.PreviousStatus{ID: id, Status: status, Note: "checked"}
}

func reviews(msgs ...string) []fake.ReviewStep {
	var out []fake.ReviewStep
	for _, m := range msgs {
		out = append(out, fake.ReviewStep{Message: m})
	}
	return out
}

// touchingReviewer changes a file after its first review: the tree the
// Commit Gate sees differs from the reviewed one.
type touchingReviewer struct {
	*fake.Reviewer
	dir   string
	calls int
}

func (r *touchingReviewer) ReviewCode(ctx context.Context, req agent.CodeReviewRequest) (*agent.CodeReviewResult, error) {
	res, err := r.Reviewer.ReviewCode(ctx, req)
	r.calls++
	if r.calls == 1 {
		_ = edit("calc/extra.go", "package calc\n\nconst Extra = 1\n")(r.dir)
	}
	return res, err
}

func hasString(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
