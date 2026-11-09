package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/state"
)

// An approving review with an in-scope major that the reviewer proposes to
// defer: the deferral is overridden (§11.7), the gate fails,
// review.inconsistent is emitted, and the finding goes to Claude.
func TestPhaseApproveWithDeferredMajor(t *testing.T) {
	for _, cat := range []string{"correctness", "security"} {
		t.Run(cat, func(t *testing.T) {
			pe := newPhaseEnv(t, phaseOpts{})
			pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("F-01-001")}
			pe.reviewer.CodeSteps = reviews(codeReview("approve", ac1, nil, finding("major", cat, ClassDeferred, "Sub overflows")),
				fake.CodeReviewApprove(ac1, "F-01-001"))
			out := pe.runPhase(1)
			if out.Status != state.PhaseCommitted || out.ReviewRounds != 2 {
				t.Fatalf("outcome %+v", out)
			}
			rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1)
			if len(rs) != 2 || rs[0].Verdict != "approve" || rs[0].GatePassed {
				t.Fatalf("the first review gate must fail: %+v", rs)
			}
			if n := count(pe.eventTypes(), events.ReviewInconsistent); n != 1 {
				t.Fatalf("review.inconsistent: %d", n)
			}
			if count(pe.eventTypes(), events.FindingDeferred) != 0 || len(out.Report.Deferred) != 0 {
				t.Fatal("an in-scope major was deferred")
			}
			e := out.Ledger.Get("F-01-001")
			if e.Proposed != ClassDeferred || e.Class != ClassFixable || e.Status != CodeResolved {
				t.Fatalf("ledger %+v", e)
			}
			if c := pe.impl.Continues[0]; c.Kind != agent.ContinueReviewFix || len(c.Findings) == 0 || c.Findings[0].ID != "F-01-001" {
				t.Fatalf("the finding was not sent to Claude: %+v", c)
			}
		})
	}
}

// Two review rounds: a minor finding is deferred in round 1 and comes back
// in round 2 with the same fingerprint as an in-scope major. It keeps its
// ID, is reopened and sent to Claude, and the phase commits only after it
// is resolved; the report does not list it as deferred.
func TestPhaseDeferredMinorEscalates(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("F-01-001"), fixStep("F-01-002")}
	other := finding("major", "correctness", ClassFixable, "Sub has no doc comment")
	minor := finding("minor", "correctness", ClassDeferred, "Sub overflows")
	major := finding("major", "correctness", ClassDeferred, "Sub overflows")
	pe.reviewer.CodeSteps = reviews(
		codeReview("request_changes", ac1, nil, other, minor),
		codeReview("request_changes", ac1, []agent.PreviousStatus{prev("F-01-001", "resolved")}, major),
		fake.CodeReviewApprove(ac1, "F-01-001", "F-01-002"))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.ReviewRounds != 3 {
		t.Fatalf("outcome %+v", out)
	}
	e := out.Ledger.Get("F-01-002")
	if len(out.Ledger.Entries) != 2 || e.Finding.Severity != "major" || e.ReopenCount != 1 || e.OpenedRound != 1 ||
		e.Status != CodeResolved || e.Class != ClassFixable {
		t.Fatalf("ledger %+v", e)
	}
	rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1)
	if len(rs) != 3 || rs[1].GatePassed {
		t.Fatalf("the second review gate must fail: %+v", rs)
	}
	if len(pe.impl.Continues) != 2 {
		t.Fatalf("continues %d", len(pe.impl.Continues))
	}
	var sent []string
	for _, f := range pe.impl.Continues[1].Findings {
		sent = append(sent, f.ID)
	}
	if !slices.Contains(sent, "F-01-002") {
		t.Fatalf("the escalated finding was not sent to Claude: %v", sent)
	}
	if len(out.Report.Deferred) != 0 {
		t.Fatalf("report deferred %+v", out.Report.Deferred)
	}
}

func TestEvidenceTree(t *testing.T) {
	res := func(tree, after string) *gates.Result { return &gates.Result{TreeHash: tree, TreeAfter: after} }
	cases := []struct {
		name string
		r    *gates.Round
		want string
	}{
		{"nil", nil, ""},
		{"one tree", &gates.Round{TreeHash: "t1", Results: []*gates.Result{res("t1", ""), res("t1", "")}}, "t1"},
		{"no tree", &gates.Round{Results: []*gates.Result{res("", "")}}, ""},
		{"result without tree", &gates.Round{TreeHash: "t1", Results: []*gates.Result{res("t1", ""), res("", "")}}, ""},
		{"gate changed the tree", &gates.Round{TreeHash: "t1", Results: []*gates.Result{res("t1", "t2"), res("t2", "")}}, ""},
		{"changed and restored", &gates.Round{TreeHash: "t1", Results: []*gates.Result{res("t1", "t2"), res("t2", "t1"), res("t1", "")}}, ""},
		{"mixed trees", &gates.Round{TreeHash: "t1", Results: []*gates.Result{res("t1", ""), res("t2", "")}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceTree(tc.r); got != tc.want {
				t.Fatalf("evidenceTree %q, want %q", got, tc.want)
			}
		})
	}
}

// gateSteps returns the outcomes of the phase gate steps in order.
func (pe *phaseEnv) gateSteps() []map[string]any {
	pe.t.Helper()
	steps, err := pe.store.ListSteps(pe.ctx, pe.run.ID)
	if err != nil {
		pe.t.Fatal(err)
	}
	var out []map[string]any
	for _, s := range steps {
		if s.Kind != "gates" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(s.Outcome, &m)
		out = append(out, m)
	}
	return out
}

// A gate changes the tree and a later gate restores it: the round ends on
// the tree it started on, but its evidence is mixed, so it is stale. It
// authorizes neither review nor commit; the gates run again first.
func TestPhaseGateChangesTreeThenRestores(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	calc := filepath.Join(pe.wt.Dir(), "calc", "calc.go")
	var orig []byte
	pe.gates.during = func(round, gate int) {
		if round != 1 {
			return
		}
		switch gate {
		case 1:
			orig, _ = os.ReadFile(calc)
			_ = os.WriteFile(calc, append(slices.Clone(orig), []byte("// generated by a gate\n")...), 0o644)
		case 2:
			_ = os.WriteFile(calc, orig, 0o644)
		}
	}
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.ReviewRounds != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if rounds, _ := pe.gates.count(); rounds != 2 {
		t.Fatalf("the stale round must be rerun: %d rounds", rounds)
	}
	gs := pe.gateSteps()
	if len(gs) != 2 || gs[0]["stale"] != true || gs[1]["stale"] != false {
		t.Fatalf("gate steps %+v", gs)
	}
	if gs[0]["tree"] != gs[0]["tree_after"] {
		t.Fatalf("the first round must end on its own tree: %+v", gs[0])
	}
	// The review starts only after a round whose evidence is bound to one tree.
	evs, err := pe.store.EventsAfter(pe.ctx, pe.run.ID, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	freshBeforeReview, staleSeen := false, false
	for _, ev := range evs {
		if ev.Type == events.ReviewCompleted {
			break
		}
		if ev.Type != events.GateCompleted {
			continue
		}
		stale, _ := ev.Payload.Get("stale")
		round, _ := ev.Payload.Get("round")
		switch {
		case stale.BoolValue():
			staleSeen = true
		case round.IntValue() == 2:
			freshBeforeReview = true
		}
	}
	if !staleSeen || !freshBeforeReview {
		t.Fatalf("stale %v, fresh round before the review %v", staleSeen, freshBeforeReview)
	}
	if pe.git("log", "-1", "--format=%(trailers:key=ACO-Reviewed-Tree,valueonly)", out.Commit) != out.ReviewedTree {
		t.Fatal("reviewed tree trailer")
	}
}

// A gate that keeps changing the tree never yields evidence: the stale
// rounds count as gate-fix attempts, so the attempt ends, restarts, and
// the phase blocks.
func TestPhaseGateKeepsChangingTreeBlocks(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.cfg.Limits.GateFixAttempts = 2
	pe.cfg.Limits.PhaseRestarts = 1
	n := 0
	pe.gates.during = func(_, gate int) {
		if gate == 1 {
			n++
			_ = edit("calc/gen.go", "package calc\n\n// generated "+strings.Repeat("x", n)+"\n")(pe.wt.Dir())
		}
	}
	pe.impl.Steps = []fake.ImplStep{startStep(), startStep()}
	out := pe.runPhase(1)
	pe.assertBlocked(out, state.ClassIterationLimit)
	if len(pe.reviewer.CodeReviews) != 0 {
		t.Fatal("a stale round authorized a review")
	}
}

// checkingReviewer runs check before each review (n counts from 1).
type checkingReviewer struct {
	*fake.Reviewer
	check func(n int)
	calls int
}

func (r *checkingReviewer) ReviewCode(ctx context.Context, req agent.CodeReviewRequest) (*agent.CodeReviewResult, error) {
	r.calls++
	r.check(r.calls)
	return r.Reviewer.ReviewCode(ctx, req)
}

// The worktree drifts after the Commit Gate passes but before staging: the
// Git helper refuses the candidate (tree mismatch or unreviewed change),
// the commit step is finished as failed, the attempt's report files are
// discarded, and the phase goes back through gates and an independent
// review. The commit holds only the freshly reviewed changes and reports.
func TestPhaseCommitDriftReReviewed(t *testing.T) {
	cases := []struct {
		name, file, content string
	}{
		{"reviewed path changed", "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b } // drift\n"},
		{"unreviewed new file", "calc/doc.go", "// Package calc is a toy calculator.\npackage calc\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := newPhaseEnv(t, phaseOpts{})
			dir := pe.wt.Dir()
			mdRel, jsonRel := ".aco/reports/phase-01.md", ".aco/reports/phase-01.json"
			hooks := 0
			beforeStageHook = func() {
				hooks++
				if hooks == 1 {
					if err := edit(tc.file, tc.content)(dir); err != nil {
						t.Error(err)
					}
				}
			}
			t.Cleanup(func() { beforeStageHook = nil })
			pe.rev = &checkingReviewer{Reviewer: pe.reviewer, check: func(n int) {
				if n != 2 {
					return
				}
				for _, rel := range []string{mdRel, jsonRel} {
					if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s of the failed commit is still in the worktree (%v)", rel, err)
					}
				}
			}}
			pe.impl.Steps = []fake.ImplStep{startStep()}
			pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1), fake.CodeReviewApprove(ac1))
			out := pe.runPhase(1)
			if out.Status != state.PhaseCommitted || out.ReviewRounds != 2 || hooks != 2 {
				t.Fatalf("outcome %+v, staging hooks %d", out, hooks)
			}
			if rounds, _ := pe.gates.count(); rounds != 2 {
				t.Fatalf("the drifted candidate needs fresh gates: %d rounds", rounds)
			}
			if !strings.Contains(pe.reviewer.CodeReviews[1].Diff, tc.file) {
				t.Fatal("the second review did not see the drift")
			}
			rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1)
			if len(rs) != 2 || rs[0].TreeHash == rs[1].TreeHash || rs[1].TreeHash != out.ReviewedTree {
				t.Fatalf("reviews %+v, reviewed tree %s", rs, out.ReviewedTree)
			}
			// Only the freshly reviewed paths and the reports are committed.
			files := strings.Fields(pe.git("diff", "--name-only", out.Commit+"^", out.Commit))
			want := []string{jsonRel, mdRel, "calc/calc.go", "calc/calc_test.go"}
			if tc.file != "calc/calc.go" {
				want = append(want, tc.file)
			}
			slices.Sort(files)
			slices.Sort(want)
			if !slices.Equal(files, want) {
				t.Fatalf("commit files %v, want %v", files, want)
			}
			if got := pe.git("show", out.Commit+":"+tc.file); got != strings.TrimRight(tc.content, "\n") {
				t.Fatalf("committed %s = %q", tc.file, got)
			}
			if pe.git("log", "-1", "--format=%(trailers:key=ACO-Reviewed-Tree,valueonly)", out.Commit) != out.ReviewedTree {
				t.Fatal("reviewed tree trailer")
			}
			// The drift itself stays in the worktree (it was reviewed and committed).
			if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tc.file))); err != nil || string(b) != tc.content {
				t.Fatalf("worktree %s: %q %v", tc.file, b, err)
			}
			// Two commit steps: the refused one is finished as failed and retryable.
			steps, _ := pe.store.ListSteps(pe.ctx, pe.run.ID)
			var commits []state.Step
			for _, s := range steps {
				if s.Kind == "commit" {
					commits = append(commits, s)
				}
			}
			if len(commits) != 2 || commits[0].Status != state.StepFailed || !strings.Contains(string(commits[0].Outcome), "worktree_drift") ||
				commits[1].Status != state.StepCompleted || commits[0].Key == commits[1].Key {
				t.Fatalf("commit steps %+v", commits)
			}
			drift, phaseCommits := 0, 0
			evs, _ := pe.store.EventsAfter(pe.ctx, pe.run.ID, 0, 10000)
			for _, ev := range evs {
				if r, ok := ev.Payload.Get("reasons"); ev.Type == events.CommitGateFailed && ok && slices.Contains(r.Strings(), CGWorktreeDrift) {
					drift++
				}
				if ev.Type == events.GitCommit && ev.Phase == 1 {
					phaseCommits++
				}
			}
			if drift != 1 || phaseCommits != 1 {
				t.Fatalf("drift events %d, phase commits %d", drift, phaseCommits)
			}
		})
	}
}

// baselineSteps returns the outcomes of the baseline gate steps in order.
func (pe *phaseEnv) baselineSteps() []map[string]any {
	pe.t.Helper()
	steps, err := pe.store.ListSteps(pe.ctx, pe.run.ID)
	if err != nil {
		pe.t.Fatal(err)
	}
	var out []map[string]any
	for _, s := range steps {
		if s.Kind == "baseline" {
			var m map[string]any
			_ = json.Unmarshal(s.Outcome, &m)
			out = append(out, m)
		}
	}
	return out
}

func unitResult(status gates.Status, tests map[string]gates.TestStatus) map[string]*gates.Result {
	r := &gates.Result{Gate: gates.Unit, Status: status, Summary: "unit: " + string(status)}
	for _, name := range []string{"TestAdd", "TestSub"} {
		if st, ok := tests[name]; ok {
			r.Tests = append(r.Tests, gates.TestCase{Package: "example.com/toy/calc", Name: name, Status: st})
		}
	}
	return map[string]*gates.Result{gates.Unit: r}
}

// The first baseline round runs while another writer breaks calc.go and
// puts it back: it sees TestSub failing. That round is stale and is not
// saved; the retry on the clean base tree sees TestSub pass, so when the
// phase breaks TestSub it is a regression, not a pre-existing failure.
func TestPhaseStaleBaselineCannotMaskFailure(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	calc := filepath.Join(pe.wt.Dir(), "calc", "calc.go")
	var orig []byte
	pe.gates.duringBaseline = func(n, gate int) {
		if n != 1 {
			return
		}
		switch gate {
		case 0:
			orig, _ = os.ReadFile(calc)
			_ = os.WriteFile(calc, append(slices.Clone(orig), []byte("\nfunc broken( {\n")...), 0o644)
		case 1:
			_ = os.WriteFile(calc, orig, 0o644)
		}
	}
	pe.gates.baselineScript = []map[string]*gates.Result{
		unitResult(gates.Failed, map[string]gates.TestStatus{"TestAdd": gates.TestPass, "TestSub": gates.TestFail}),
		unitResult(gates.Passed, map[string]gates.TestStatus{"TestAdd": gates.TestPass, "TestSub": gates.TestPass}),
	}
	pe.gates.evaluate = true
	pe.gates.script = []map[string]*gates.Result{
		unitResult(gates.Failed, map[string]gates.TestStatus{"TestAdd": gates.TestPass, "TestSub": gates.TestFail}),
	}
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted {
		t.Fatalf("outcome %+v", out)
	}
	if _, baselines := pe.gates.count(); baselines != 2 {
		t.Fatalf("baseline rounds %d, want 2", baselines)
	}
	bs := pe.baselineSteps()
	if len(bs) != 2 || bs[0]["stale"] != true || bs[1]["stale"] != false {
		t.Fatalf("baseline steps %+v", bs)
	}
	if !slices.Contains(out.Report.RegressionsSeen, "example.com/toy/calc.TestSub") {
		t.Fatalf("the new TestSub failure was masked: regressions %v", out.Report.RegressionsSeen)
	}
	if len(pe.impl.Continues) == 0 || pe.impl.Continues[0].Kind != agent.ContinueGateFix {
		t.Fatalf("the failing gate was not sent to Claude: %+v", pe.impl.Continues)
	}
	first, err := pe.wt.ResolveCommit(pe.ctx, out.Commit+"^")
	if err != nil {
		t.Fatal(err)
	}
	b, err := gates.BaselineStore{Dir: pe.runDir}.Load(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range b.Results[gates.Unit].Tests {
		if tc.Name == "TestSub" && tc.Status != gates.TestPass {
			t.Fatalf("the saved baseline has TestSub %s", tc.Status)
		}
	}
}

// The working tree keeps moving while the baseline gates run: after the
// bounded retries the phase blocks before the implementer is called, no
// baseline is saved, and the other writer's file is left alone.
func TestPhaseBaselineDriftBlocks(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	notes := filepath.Join(pe.wt.Dir(), "notes.txt")
	pe.gates.duringBaseline = func(n, gate int) {
		if gate == 0 {
			_ = os.WriteFile(notes, []byte(strings.Repeat("x", n)), 0o644)
		}
	}
	pe.impl.Steps = []fake.ImplStep{startStep()}
	out := pe.runPhase(1)
	if out.Status != state.PhaseBlocked || out.Reason.Class != state.ClassInitCheckFailed ||
		!strings.Contains(out.Reason.Detail, "baseline gates") {
		t.Fatalf("outcome %+v", out)
	}
	if _, baselines := pe.gates.count(); baselines != baselineTries {
		t.Fatalf("baseline rounds %d, want %d", baselines, baselineTries)
	}
	if len(pe.impl.Starts) != 0 || len(pe.impl.Continues) != 0 {
		t.Fatal("the implementer was called")
	}
	if _, err := (gates.BaselineStore{Dir: pe.runDir}).Load(pe.freeze); !errors.Is(err, gates.ErrNoBaseline) {
		t.Fatalf("a baseline was saved: %v", err)
	}
	if b, err := os.ReadFile(notes); err != nil || string(b) != strings.Repeat("x", baselineTries) {
		t.Fatalf("the other writer's file: %q %v", b, err)
	}
}

// commitSteps returns the commit steps of the run in order.
func (pe *phaseEnv) commitSteps() []state.Step {
	pe.t.Helper()
	steps, err := pe.store.ListSteps(pe.ctx, pe.run.ID)
	if err != nil {
		pe.t.Fatal(err)
	}
	var out []state.Step
	for _, s := range steps {
		if s.Kind == "commit" {
			out = append(out, s)
		}
	}
	return out
}

// A report file is replaced after the engine validated and wrote it, right
// before staging. Git would re-read it, so the commit is checked against the
// generated bytes: it is refused before the branch moves, the failed try
// discards only the report file it still owns (the replaced one is someone
// else's edit and stays), and the phase goes back through gates and review.
// The replaced report is a change to a protected path, so the Commit Gate
// then blocks: no commit ever holds the replaced bytes.
func TestPhaseCommitReportTampered(t *testing.T) {
	const tampered = "{\"tampered\":true}\n"
	srcDrift := "package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b } // drift\n"
	for _, withSource := range []bool{false, true} {
		name := "report only"
		if withSource {
			name = "report and source drift"
		}
		t.Run(name, func(t *testing.T) {
			pe := newPhaseEnv(t, phaseOpts{})
			dir := pe.wt.Dir()
			mdRel, jsonRel := ".aco/reports/phase-01.md", ".aco/reports/phase-01.json"
			jsonPath := filepath.Join(dir, filepath.FromSlash(jsonRel))
			hooks := 0
			beforeStageHook = func() {
				hooks++
				if hooks != 1 {
					return
				}
				if err := os.WriteFile(jsonPath, []byte(tampered), 0o644); err != nil {
					t.Error(err)
				}
				if withSource {
					if err := edit("calc/calc.go", srcDrift)(dir); err != nil {
						t.Error(err)
					}
				}
			}
			t.Cleanup(func() { beforeStageHook = nil })
			pe.rev = &checkingReviewer{Reviewer: pe.reviewer, check: func(n int) {
				if n != 2 {
					return
				}
				// The rollback removed the report it still owned and kept
				// the replaced one.
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(mdRel))); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s of the failed commit is still in the worktree (%v)", mdRel, err)
				}
				if b, err := os.ReadFile(jsonPath); err != nil || string(b) != tampered {
					t.Errorf("the replaced %s was not preserved: %q %v", jsonRel, b, err)
				}
			}}
			pe.impl.Steps = []fake.ImplStep{startStep()}
			pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1), fake.CodeReviewApprove(ac1))
			head := pe.git("rev-parse", pe.wt.Branch())
			out := pe.runPhase(1)
			if out.Status != state.PhaseBlocked || out.Reason.Class != state.ClassCommitGateBlocked ||
				!strings.Contains(out.Reason.Detail, jsonRel) || hooks != 1 {
				t.Fatalf("outcome %+v, staging hooks %d", out, hooks)
			}
			if got := pe.git("rev-parse", pe.wt.Branch()); got != head {
				t.Fatalf("the run branch moved to %s", got)
			}
			if rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1); len(rs) != 2 {
				t.Fatalf("reviews %d: the candidate was not reviewed again", len(rs))
			}
			if withSource && !strings.Contains(pe.reviewer.CodeReviews[1].Diff, "// drift") {
				t.Fatal("the second review did not see the source drift")
			}
			commits := pe.commitSteps()
			if len(commits) != 1 || commits[0].Status != state.StepFailed || !strings.Contains(string(commits[0].Outcome), "worktree_drift") {
				t.Fatalf("commit steps %+v", commits)
			}
			rows, _ := pe.store.ListReports(pe.ctx, pe.run.ID)
			for _, r := range rows {
				if r.Kind == "phase" {
					t.Fatalf("phase report row %+v", r)
				}
			}
			if cs, _ := pe.store.ListCommits(pe.ctx, pe.run.ID); len(cs) != 1 || cs[0].Kind == "phase" {
				t.Fatalf("commit rows %+v", cs)
			}
			evs, _ := pe.store.EventsAfter(pe.ctx, pe.run.ID, 0, 10000)
			for _, ev := range evs {
				if ev.Type == events.GitCommit && ev.Phase == 1 {
					t.Fatal("a phase commit event was emitted")
				}
			}
			// Other edits are left as they are.
			if b, err := os.ReadFile(jsonPath); err != nil || string(b) != tampered {
				t.Fatalf("worktree %s: %q %v", jsonRel, b, err)
			}
			if withSource {
				if b, err := os.ReadFile(filepath.Join(dir, "calc", "calc.go")); err != nil || string(b) != srcDrift {
					t.Fatalf("worktree calc.go: %q %v", b, err)
				}
			}
		})
	}
}

// Another commit lands on the run branch between the Commit Gate and
// staging, with the same tree. The phase commit is pinned to the phase's
// validated base, so it is refused as a moved branch: no phase commit, no
// worktree-drift retry.
func TestPhaseCommitBranchMovedBeforeStaging(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	hooks := 0
	beforeStageHook = func() {
		hooks++
		pe.wtGit("commit", "-q", "--allow-empty", "--no-verify", "-m", "external")
	}
	t.Cleanup(func() { beforeStageHook = nil })
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1), fake.CodeReviewApprove(ac1))
	_, err := RunPhase(pe.ctx, pe.phaseDeps(), PhaseInput{RunID: pe.run.ID, Phase: 1})
	if !errors.Is(err, gitops.ErrBranchMoved) || errors.Is(err, errWorktreeDrift) {
		t.Fatalf("RunPhase: %v, want a moved branch", err)
	}
	if hooks != 1 {
		t.Fatalf("staging hooks %d: a moved branch is not retried", hooks)
	}
	tip := pe.git("rev-parse", pe.wt.Branch())
	if pe.git("log", "-1", "--format=%s", tip) != "external" || pe.git("rev-parse", tip+"^") != pe.freeze {
		t.Fatal("the run branch holds more than the external commit")
	}
	if dup, _ := pe.wt.FindCommitByTrailer(pe.ctx, tip, "ACO-Phase-Attempt", gitops.PhaseAttemptKey(pe.run.ID, 1, 1)); dup != "" {
		t.Fatalf("phase commit %s", dup)
	}
	commits := pe.commitSteps()
	if len(commits) != 1 || commits[0].Status != state.StepFailed || strings.Contains(string(commits[0].Outcome), "worktree_drift") {
		t.Fatalf("commit steps %+v", commits)
	}
	if count(pe.eventTypes(), events.CommitGateFailed) != 0 {
		t.Fatal("a moved branch was handled as worktree drift")
	}
	if rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1); len(rs) != 1 {
		t.Fatalf("reviews %d", len(rs))
	}
}
