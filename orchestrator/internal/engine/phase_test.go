package engine

import (
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
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/state"
)

func (pe *phaseEnv) assertBlocked(out *PhaseOutcome, class state.Class) {
	pe.t.Helper()
	if out.Status != state.PhaseBlocked || out.RunStatus != state.RunBlocked || out.Reason.Class != class {
		pe.t.Fatalf("outcome %s/%s %+v, want blocked %s", out.Status, out.RunStatus, out.Reason, class)
	}
	if out.Reason.Stage != state.StageExecution || out.Reason.Phase != out.Phase {
		pe.t.Fatalf("reason %+v", out.Reason)
	}
	ph, err := pe.store.GetPhase(pe.ctx, pe.run.ID, out.Phase)
	if err != nil || ph.Status != state.PhaseBlocked || ph.Reason.Class != class {
		pe.t.Fatalf("stored phase %+v %v", ph, err)
	}
	// ACO never force-commits: the run branch is where the phase started.
	commits, _ := pe.store.ListCommits(pe.ctx, pe.run.ID)
	if head := pe.git("rev-parse", "refs/heads/"+pe.branch); len(commits) == 0 || head != commits[len(commits)-1].SHA {
		pe.t.Fatalf("the run branch moved on a blocked phase")
	}
	for _, c := range commits {
		if c.Phase == out.Phase {
			pe.t.Fatalf("a blocked phase has a commit: %+v", c)
		}
	}
	ref := gitops.BlockedRef(pe.run.ID, out.Phase)
	if out.BlockedRef != ref || pe.git("rev-parse", "--verify", ref) == "" {
		pe.t.Fatalf("WIP ref %q missing", ref)
	}
	if out.BlockedReport == "" {
		pe.t.Fatal("no Blocked Report")
	}
	b, err := pe.runDir.Read(out.BlockedReport)
	if err != nil || !strings.Contains(string(b), string(class)) || !strings.Contains(string(b), "What a human must decide") {
		pe.t.Fatalf("Blocked Report: %v\n%s", err, b)
	}
	if n := count(pe.eventTypes(), events.PhaseBlocked); n != 1 {
		pe.t.Fatalf("phase.blocked events: %d", n)
	}
}

// AC-09.1 scenario 1: happy path → one phase commit with only the reviewed
// paths and the report, all trailers, the tag, and the note.
func TestPhaseHappyPath(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.RunStatus != state.RunExecuting || out.Attempts != 1 || out.ReviewRounds != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if got := pe.git("rev-parse", "refs/heads/"+pe.branch); got != out.Commit {
		t.Fatal("run branch is not at the phase commit")
	}
	if parent := pe.git("rev-parse", out.Commit+"^"); parent != pe.freeze {
		t.Fatal("phase commit is not on top of the freeze commit")
	}
	files := strings.Split(pe.git("diff-tree", "--no-commit-id", "--name-only", "-r", out.Commit), "\n")
	slices.Sort(files)
	want := []string{".aco/reports/phase-01.json", ".aco/reports/phase-01.md", "calc/calc.go", "calc/calc_test.go"}
	if !slices.Equal(files, want) {
		t.Fatalf("commit files %v, want %v", files, want)
	}
	if subj := pe.git("log", "-1", "--format=%s", out.Commit); subj != "feat(phase-01): Operation 1" {
		t.Fatalf("subject %q", subj)
	}
	trailers := pe.git("log", "-1", "--format=%(trailers:only,unfold)", out.Commit)
	lock, _ := pe.store.GetRun(pe.ctx, pe.run.ID)
	for _, w := range []string{"ACO-Run: " + pe.run.ID, "ACO-Phase: 01", "ACO-Phase-Attempt: " + gitops.PhaseAttemptKey(pe.run.ID, 1, 1),
		"ACO-Plan-Hash: " + lock.PlanHash, "ACO-Reviewed-Tree: " + out.ReviewedTree, "ACO-Review-Rounds: 1",
		"Reviewed-by: fake-reviewer", "Co-Authored-By: Claude <noreply@anthropic.com>"} {
		if !strings.Contains(trailers, w) {
			t.Fatalf("trailers lack %q:\n%s", w, trailers)
		}
	}
	// The reviewed tree is the commit minus the two reports.
	if got := pe.git("rev-parse", out.Commit+"^{tree}"); got == out.ReviewedTree {
		t.Fatal("commit tree should include the reports")
	}
	tag := gitops.PhaseTag(pe.run.ID, 1)
	if out.Tag != tag || pe.git("rev-parse", "refs/tags/"+tag+"^{commit}") != out.Commit {
		t.Fatal("phase tag missing or misplaced")
	}
	if note := pe.git("notes", "--ref=aco", "show", out.Commit); !strings.Contains(note, `"kind":"phase"`) || !strings.Contains(note, out.Commit) {
		t.Fatalf("note %s", note)
	}
	if clean := pe.wtGit("status", "--porcelain"); clean != "" {
		t.Fatalf("worktree not clean after commit: %s", clean)
	}

	// The report validates and holds every §11.9 item.
	r, err := reports.ReadPhase(pe.wt.Dir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertReportComplete(t, r)
	if r.ReviewedTree != out.ReviewedTree || r.BaseCommit != pe.freeze || r.PlanHash != lock.PlanHash {
		t.Fatalf("report header %+v", r)
	}
	md, _ := os.ReadFile(filepath.Join(pe.wt.Dir(), ".aco", "reports", "phase-01.md"))
	for _, s := range []string{"# Phase 01: Operation 1", "## Acceptance criteria", "AC-01.1", "## Quality gates", "## Review history",
		"## Usage", "## Digest"} {
		if !strings.Contains(string(md), s) {
			t.Fatalf("markdown lacks %q", s)
		}
	}

	// State: commit, report, baseline for the next phase, steps with checkpoints.
	ph, _ := pe.store.GetPhase(pe.ctx, pe.run.ID, 1)
	if ph.Status != state.PhaseCommitted || ph.CommitSHA != out.Commit || ph.BaseCommit != pe.freeze {
		t.Fatalf("phase %+v", ph)
	}
	if _, err := (gates.BaselineStore{Dir: pe.runDir}).Load(out.Commit); err != nil {
		t.Fatalf("no baseline stored for the phase commit: %v", err)
	}
	if _, b := pe.gates.count(); b != 1 {
		t.Fatalf("baseline rounds %d, want 1 (none recorded at the freeze commit)", b)
	}
	steps, _ := pe.store.ListSteps(pe.ctx, pe.run.ID)
	kinds := map[string]int{}
	for _, s := range steps {
		kinds[s.Kind]++
		if s.Status != state.StepCompleted {
			t.Fatalf("step %s is %s", s.Kind, s.Status)
		}
		if (s.Kind == "implement" || s.Kind == "review") && (s.CheckpointRef == "" || !strings.Contains(string(s.Outcome), "checkpoint_after")) {
			t.Fatalf("agent step %s lacks checkpoints: %+v", s.Kind, s)
		}
	}
	for _, k := range []string{"baseline", "implement", "gates", "review", "commit"} {
		if kinds[k] == 0 {
			t.Fatalf("no %s step: %v", k, kinds)
		}
	}
	evs := pe.eventTypes()
	for _, et := range []events.Type{events.PhaseStarted, events.PhaseInitPassed, events.GateCompleted, events.ReviewCompleted,
		events.CommitGatePassed, events.GitCommit, events.ReportWritten, events.PhaseCompleted} {
		if count(evs, et) == 0 {
			t.Fatalf("no %s event", et)
		}
	}

	// §11.10: phase 2 starts from the phase 1 commit with a new session
	// and the phase 1 digest.
	mul := func(dir string) error {
		if err := edit("calc/mul.go", "package calc\n\nfunc Mul(a, b int) int { return a * b }\n")(dir); err != nil {
			return err
		}
		return edit("calc/mul_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {}\n")(dir)
	}
	pe.impl.Steps = append(pe.impl.Steps, fake.ImplStep{Edit: mul,
		Result: fake.ImplResult(fake.Report("multiplication", []string{"AC-02.1", "AC-02.2"}))})
	pe.reviewer.CodeSteps = append(pe.reviewer.CodeSteps, reviews(fake.CodeReviewApprove([]string{"AC-02.1", "AC-02.2"}))...)
	out2 := pe.runPhase(2)
	if out2.Status != state.PhaseCommitted || pe.git("rev-parse", out2.Commit+"^") != out.Commit {
		t.Fatalf("phase 2: %+v", out2)
	}
	if len(pe.impl.Starts) != 2 || len(pe.impl.Starts[1].Digests) != 1 || pe.impl.Starts[1].Digests[0].Digest != r.Digest {
		t.Fatalf("phase 2 context: %+v", pe.impl.Starts[1].Digests)
	}
	atts1, _ := pe.store.ListAttempts(pe.ctx, pe.run.ID, 1)
	atts2, _ := pe.store.ListAttempts(pe.ctx, pe.run.ID, 2)
	if atts1[0].SessionID == "" || atts1[0].SessionID == atts2[0].SessionID {
		t.Fatalf("sessions %q %q", atts1[0].SessionID, atts2[0].SessionID)
	}
	if rounds, b := pe.gates.count(); b != 1 || rounds != 2 {
		t.Fatalf("phase 2 should reuse the stored baseline: rounds %d baselines %d", rounds, b)
	}
}

// assertReportComplete checks every item of §11.9.
func assertReportComplete(t *testing.T, r *reports.PhaseReport) {
	t.Helper()
	raw, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if probs := reports.ValidatePhaseJSON(raw); len(probs) > 0 {
		t.Fatalf("schema: %v", probs)
	}
	switch {
	case r.Phase != 1 || r.Title == "" || r.Goal == "" || r.BaseCommit == "" || r.Attempts < 1:
		t.Fatalf("header %+v", r)
	case len(r.Sessions) == 0 || r.Sessions[0].SessionID == "" || r.ReviewerModel == "":
		t.Fatalf("sessions/reviewer %+v", r.Sessions)
	case len(r.AcceptanceCriteria) != 2 || r.AcceptanceCriteria[0].Status != "met" || r.AcceptanceCriteria[0].Text == "" ||
		r.AcceptanceCriteria[0].Evidence == "":
		t.Fatalf("AC %+v", r.AcceptanceCriteria)
	case !slices.Contains(r.Requirements, "REQ-001"):
		t.Fatalf("requirements %v", r.Requirements)
	case len(r.Files) == 0 || r.DiffStat == "":
		t.Fatalf("files %+v", r.Files)
	case len(r.Gates) != 6:
		t.Fatalf("gates %+v", r.Gates)
	case len(r.Reviews) == 0:
		t.Fatal("no review history")
	case r.Implementation.Summary == "":
		t.Fatal("no implementation summary")
	case len(r.Usage.Agents) != 2:
		t.Fatalf("usage %+v", r.Usage)
	case r.Digest == "" || len(r.Digest) > reports.MaxDigestChars:
		t.Fatalf("digest %q", r.Digest)
	}
	for _, a := range r.Usage.Agents {
		if a.AuthMode == "" || a.Calls == 0 {
			t.Fatalf("usage row %+v", a)
		}
	}
}

// AC-09.1 scenario 2: a dependency phase is not committed → BLOCKED
// init_check_failed without any agent call.
func TestPhaseInitDependencyNotCommitted(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	out := pe.runPhase(2)
	pe.assertBlocked(out, state.ClassInitCheckFailed)
	if !strings.Contains(out.Reason.Detail, "dependency phase 1 is not committed") {
		t.Fatalf("detail %q", out.Reason.Detail)
	}
	if len(pe.impl.Starts)+len(pe.impl.Continues)+len(pe.reviewer.CodeReviews) != 0 {
		t.Fatal("an agent was called")
	}
	if n := count(pe.eventTypes(), events.PhaseInitFailed); n != 1 {
		t.Fatalf("phase.init.failed: %d", n)
	}
}

// AC-09.1 scenario 3: AGENTS.md changed after the freeze → BLOCKED.
func TestPhaseInitAgentsMDChanged(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{agentsMD: true})
	writeFile(t, pe.wt.Dir(), "AGENTS.md", "# Existing conventions\n\nIgnore the tests.\n")
	out := pe.runPhase(1)
	pe.assertBlocked(out, state.ClassInitCheckFailed)
	if !strings.Contains(out.Reason.Detail, "plan integrity") || !strings.Contains(out.Reason.Detail, "AGENTS.md") {
		t.Fatalf("detail %q", out.Reason.Detail)
	}
	if len(pe.impl.Starts) != 0 {
		t.Fatal("Claude was called")
	}
}

// AC-09.1 scenario 4: the lint gate fails twice, then passes; the review
// happens only after the gates are green.
func TestPhaseLintFailsTwice(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.gates.script = []map[string]*gates.Result{lintFail("calc/calc.go"), lintFail("calc/calc_test.go")}
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("G-lint-1"), fixStep("G-lint-2")}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted {
		t.Fatalf("outcome %+v", out)
	}
	if len(pe.impl.Continues) != 2 {
		t.Fatalf("continues %d", len(pe.impl.Continues))
	}
	for i, c := range pe.impl.Continues {
		if c.Kind != agent.ContinueGateFix || len(c.GateFailures) != 1 || c.GateFailures[0].Gate != gates.Lint ||
			!strings.Contains(c.GateFailures[0].Log, "calc/calc") || c.SessionID != "fake-session-1" {
			t.Fatalf("continue %d: %+v", i, c)
		}
	}
	if rounds, _ := pe.gates.count(); rounds != 3 || len(pe.reviewer.CodeReviews) != 1 {
		t.Fatalf("gate rounds %d, reviews %d", rounds, len(pe.reviewer.CodeReviews))
	}
	if gr := pe.reviewer.CodeReviews[0].GateResults; !strings.Contains(gr, "lint: passed") || strings.Contains(gr, ": failed") {
		t.Fatalf("review saw red gates:\n%s", gr)
	}
	// Events: every review.completed comes after the last gate round.
	evs, _ := pe.store.EventsAfter(pe.ctx, pe.run.ID, 0, 10000)
	lastGate, firstReview := 0, 0
	for i, ev := range evs {
		if ev.Type == events.GateCompleted {
			lastGate = i
		}
		if ev.Type == events.ReviewCompleted && firstReview == 0 {
			firstReview = i
		}
	}
	if firstReview < lastGate {
		t.Fatal("a review ran before the gates were green")
	}
}

// AC-09.1 scenario 5: an integration test that passed at baseline fails →
// it is fixed, and the regression is listed in the review pack and the
// report.
func TestPhaseIntegrationRegression(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{integration: true})
	pe.gates.script = []map[string]*gates.Result{integrationRegression()}
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("G-integration-1")}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted {
		t.Fatalf("outcome %+v", out)
	}
	if log := pe.impl.Continues[0].GateFailures[0].Log; !strings.Contains(log, regressedTest) {
		t.Fatalf("gate fix log lacks the regression:\n%s", log)
	}
	if reg := pe.reviewer.CodeReviews[0].Regressions; !strings.Contains(reg, regressedTest) {
		t.Fatalf("review pack regressions:\n%s", reg)
	}
	if !slices.Contains(out.Report.RegressionsSeen, regressedTest) || len(out.Report.Regressions) != 0 {
		t.Fatalf("report regressions %v / %v", out.Report.Regressions, out.Report.RegressionsSeen)
	}
	md, _ := os.ReadFile(filepath.Join(pe.wt.Dir(), ".aco", "reports", "phase-01.md"))
	if !strings.Contains(string(md), regressedTest) {
		t.Fatal("markdown report lacks the regression")
	}
}

// AC-09.1 scenario 6: the review requests changes → fix → gates →
// re-review resolves → commit.
func TestPhaseReviewFixReReview(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	f := finding("major", "correctness", "fixable", "Sub overflows")
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("F-01-001")}
	pe.reviewer.CodeSteps = reviews(codeReview("request_changes", ac1, nil, f),
		fake.CodeReviewApprove(ac1, "F-01-001"))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.ReviewRounds != 2 {
		t.Fatalf("outcome %+v", out)
	}
	c := pe.impl.Continues[0]
	if c.Kind != agent.ContinueReviewFix || len(c.Findings) != 1 || c.Findings[0].ID != "F-01-001" || c.Findings[0].Location != "calc/calc.go:5" {
		t.Fatalf("review fix %+v", c)
	}
	rr := pe.reviewer.CodeReviews[1]
	if rr.Round != 2 || len(rr.Ledger) != 1 || rr.Ledger[0].Response != agent.ResponseFixed || rr.IncrementalDiff == "" {
		t.Fatalf("re-review request: round %d ledger %+v incremental %d", rr.Round, rr.Ledger, len(rr.IncrementalDiff))
	}
	if rounds, _ := pe.gates.count(); rounds != 2 {
		t.Fatalf("gates must run before the re-review: %d rounds", rounds)
	}
	e := out.Ledger.Get("F-01-001")
	if e.Status != CodeResolved || e.ClosedRound != 2 {
		t.Fatalf("ledger %+v", e)
	}
	if len(out.Report.Findings) != 1 || out.Report.Findings[0].Status != CodeResolved || out.Report.Findings[0].Response != "fixed" {
		t.Fatalf("report findings %+v", out.Report.Findings)
	}
	if trailers := pe.git("log", "-1", "--format=%(trailers:only,unfold)", out.Commit); !strings.Contains(trailers, "ACO-Review-Rounds: 2") {
		t.Fatal(trailers)
	}
	fs, _ := pe.store.ListFindings(pe.ctx, pe.run.ID, "code")
	if len(fs) != 1 || fs[0].Status != CodeResolved || fs[0].Phase != 1 {
		t.Fatalf("stored findings %+v", fs)
	}
}

// AC-09.1 scenario 7: approve with a blocker → the gate fails and
// review.inconsistent is emitted.
func TestPhaseApproveWithBlocker(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("F-01-001")}
	pe.reviewer.CodeSteps = reviews(codeReview("approve", ac1, nil, finding("blocker", "correctness", "fixable", "Sub is wrong")),
		fake.CodeReviewApprove(ac1, "F-01-001"))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.ReviewRounds != 2 {
		t.Fatalf("outcome %+v", out)
	}
	if n := count(pe.eventTypes(), events.ReviewInconsistent); n != 1 {
		t.Fatalf("review.inconsistent: %d", n)
	}
	rs, _ := pe.store.ListReviews(pe.ctx, pe.run.ID, 1)
	if len(rs) != 2 || rs[0].Verdict != "approve" || rs[0].GatePassed || !rs[1].GatePassed {
		t.Fatalf("reviews %+v", rs)
	}
}

// AC-09.1 scenario 8: a finding classified unsafe → BLOCKED at once, no
// commit, WIP ref.
func TestPhaseUnsafeFinding(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(codeReview("request_changes", ac1, nil, finding("blocker", "security", "unsafe", "Needs the CI secret")))
	out := pe.runPhase(1)
	pe.assertBlocked(out, state.ClassUnsafe)
	if len(pe.impl.Continues) != 0 {
		t.Fatal("the unsafe finding was sent to Claude")
	}
	if n := count(pe.eventTypes(), events.TriageCompleted); n != 1 {
		t.Fatalf("triage.completed %d", n)
	}
	// The WIP ref holds the implementation.
	if got := pe.git("show", gitops.BlockedRef(pe.run.ID, 1)+":calc/calc.go"); !strings.Contains(got, "Sub") {
		t.Fatalf("WIP ref content %q", got)
	}
}

// AC-09.1 scenario 9: Claude reports cannot_fix_within_spec, the reviewer
// confirms → BLOCKED spec_conflict.
func TestPhaseSpecConflictConfirmed(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{startStep(), answer("F-01-001", agent.ResponseCannotFixWithinSpec)}
	pe.reviewer.CodeSteps = reviews(codeReview("request_changes", ac1, nil, finding("major", "spec_mismatch", "fixable", "AC-01.2 contradicts AC-01.1")),
		codeReview("request_changes", ac1, []agent.PreviousStatus{prev("F-01-001", "dispute_accepted")}))
	out := pe.runPhase(1)
	pe.assertBlocked(out, state.ClassSpecConflict)
	if e := out.Ledger.Get("F-01-001"); e.Confirmed != agent.ResponseCannotFixWithinSpec || e.Class != ClassSpecConflict {
		t.Fatalf("ledger %+v", e)
	}
}

// AC-09.1 scenario 10: oscillation, no progress, and the round limit each
// end the attempt; the phase restarts once and then blocks.
func TestPhaseGuardsRestartThenBlock(t *testing.T) {
	f := finding("major", "correctness", "fixable", "Sub overflows")
	cases := []struct {
		name   string
		class  state.Class
		limits func(pe *phaseEnv)
		impl   func() []fake.ImplStep
		review func() []string
	}{
		{name: "oscillation", class: state.ClassOscillation,
			impl: func() []fake.ImplStep { return []fake.ImplStep{startStep(), fixStep("F"), fixStep("F")} },
			review: func() []string {
				return []string{codeReview("request_changes", ac1, nil, f), "unresolved", "unresolved"}
			}},
		{name: "no_progress", class: state.ClassNoProgress,
			impl: func() []fake.ImplStep { return []fake.ImplStep{startStep(), noEditFix("F"), noEditFix("F")} },
			review: func() []string {
				return []string{codeReview("request_changes", ac1, nil, f), "unresolved"}
			}},
		{name: "round_limit", class: state.ClassIterationLimit,
			limits: func(pe *phaseEnv) { pe.cfg.Limits.ReviewRounds = 2 },
			impl:   func() []fake.ImplStep { return []fake.ImplStep{startStep(), fixStep("F")} },
			review: func() []string {
				return []string{codeReview("request_changes", ac1, nil, f), "resolved+new"}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := newPhaseEnv(t, phaseOpts{})
			if tc.limits != nil {
				tc.limits(pe)
			}
			// Two attempts with the same script; finding IDs continue
			// across attempts (F-01-001 in attempt 1, F-01-00N in attempt 2).
			next := 1
			for a := 0; a < 2; a++ {
				id := "F-01-00" + string(rune('0'+next))
				for _, s := range tc.impl() {
					if s.Result != nil && len(s.Result.Report.FindingResponses) > 0 {
						s.Result.Report.FindingResponses[0].ID = id
					}
					pe.impl.Steps = append(pe.impl.Steps, s)
				}
				for _, m := range tc.review() {
					switch m {
					case "unresolved":
						m = codeReview("request_changes", ac1, []agent.PreviousStatus{prev(id, "unresolved")})
					case "resolved+new":
						g := finding("major", "tests", "fixable", "Missing negative case")
						m = codeReview("request_changes", ac1, []agent.PreviousStatus{prev(id, "resolved")}, g)
						next++
					}
					pe.reviewer.CodeSteps = append(pe.reviewer.CodeSteps, fake.ReviewStep{Message: m})
				}
				next++
			}
			out := pe.runPhase(1)
			pe.assertBlocked(out, tc.class)
			if out.Attempts != 2 || len(pe.impl.Starts) != 2 || out.Trip == nil || out.Trip.Class != tc.class {
				t.Fatalf("attempts %d starts %d trip %+v", out.Attempts, len(pe.impl.Starts), out.Trip)
			}
			// The restart is a fresh session with a lessons brief, on a
			// worktree reset to the phase base.
			second := pe.impl.Starts[1]
			if second.Attempt != 2 || !strings.Contains(second.Lessons, "Attempt 1 ended") {
				t.Fatalf("restart request: attempt %d lessons %q", second.Attempt, second.Lessons)
			}
			if count(pe.eventTypes(), events.GuardTripped) != 2 || count(pe.eventTypes(), events.PhaseAttemptExhausted) != 2 {
				t.Fatal("guard events")
			}
			if pe.impl.Remaining() != 0 {
				t.Fatalf("%d implementer steps unused", pe.impl.Remaining())
			}
		})
	}
}

// AC-09.1 scenario 11: a file changes after the review → the Commit Gate
// sends the phase back to review; the new tree is reviewed and committed.
func TestPhaseTreeChangedAfterReview(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	tr := &touchingReviewer{Reviewer: pe.reviewer, dir: pe.wt.Dir()}
	pe.rev = tr
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1), fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted || out.ReviewRounds != 2 {
		t.Fatalf("outcome %+v", out)
	}
	if n := count(pe.eventTypes(), events.CommitGateFailed); n != 1 {
		t.Fatalf("commit_gate.failed: %d", n)
	}
	if rounds, _ := pe.gates.count(); rounds != 2 {
		t.Fatalf("the changed tree needs fresh gates: %d rounds", rounds)
	}
	if !strings.Contains(pe.reviewer.CodeReviews[1].Diff, "calc/extra.go") {
		t.Fatal("the second review did not see the change")
	}
	files := pe.git("diff-tree", "--no-commit-id", "--name-only", "-r", out.Commit)
	if !strings.Contains(files, "calc/extra.go") {
		t.Fatalf("commit files %s", files)
	}
	if pe.git("log", "-1", "--format=%(trailers:key=ACO-Reviewed-Tree,valueonly)", out.Commit) != out.ReviewedTree {
		t.Fatal("reviewed tree trailer")
	}
}

// A gate that fails for an environment reason blocks at once (§11.7).
func TestPhaseEnvironmentGateBlocks(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.gates.script = []map[string]*gates.Result{environmentFail()}
	pe.impl.Steps = []fake.ImplStep{startStep()}
	out := pe.runPhase(1)
	pe.assertBlocked(out, state.ClassEnvironment)
	if len(pe.impl.Continues) != 0 || len(pe.reviewer.CodeReviews) != 0 {
		t.Fatal("environment failures are not sent to agents")
	}
}

// A blocker raised late outside the latest changes is downgraded and
// deferred (scope-creep control); the phase commits with it deferred.
func TestPhaseScopeCreepDeferred(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.cfg.Limits.NewBlockerCutoffRound = 1
	late := finding("blocker", "maintainability", "fixable", "Rename the package")
	late.File, late.LineStart, late.LineEnd = "README.md", 1, 1
	pe.impl.Steps = []fake.ImplStep{startStep(), fixStep("F-01-001")}
	pe.reviewer.CodeSteps = reviews(codeReview("request_changes", ac1, nil, finding("major", "correctness", "fixable", "Sub overflows")),
		codeReview("request_changes", ac1, []agent.PreviousStatus{prev("F-01-001", "resolved")}, late))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted {
		t.Fatalf("outcome %+v", out)
	}
	e := out.Ledger.Get("F-01-002")
	if !e.Downgraded || e.Finding.Severity != "minor" || e.Status != CodeDeferred {
		t.Fatalf("late finding %+v", e)
	}
	if len(out.Report.Deferred) != 1 || out.Report.Deferred[0].ID != "F-01-002" {
		t.Fatalf("deferred %+v", out.Report.Deferred)
	}
	if count(pe.eventTypes(), events.FindingDeferred) != 1 {
		t.Fatal("finding.deferred")
	}
}

// Agent errors that stop the run: a subscription limit pauses it.
func TestPhaseSubscriptionLimitPauses(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	pe.impl.Steps = []fake.ImplStep{{Err: &agent.Error{Agent: "claude", Class: agent.ClassSubscriptionLimit}}}
	out := pe.runPhase(1)
	if out.RunStatus != state.RunPaused || out.ResumeAt.IsZero() || out.Status != "" {
		t.Fatalf("outcome %+v", out)
	}
}

// An incomplete report is continued in the same session (§14.1).
func TestPhaseIncompleteReportContinued(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{})
	inc := &agent.ImplResult{Invocation: agent.Invocation{SessionID: "s-1", AuthMode: "subscription"}}
	pe.impl.Steps = []fake.ImplStep{{Edit: calcEdit(), Result: inc, Err: &agent.Error{Agent: "claude", Class: agent.ClassIncomplete,
		Invocation: inc.Invocation}}, {Result: fake.ImplResult(implReport("done", ac1))}}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	out := pe.runPhase(1)
	if out.Status != state.PhaseCommitted {
		t.Fatalf("outcome %+v", out)
	}
	if len(pe.impl.Continues) != 1 || pe.impl.Continues[0].Kind != agent.ContinueFinish || pe.impl.Continues[0].SessionID != "s-1" {
		t.Fatalf("continue %+v", pe.impl.Continues)
	}
}

// RunPhase refuses a run that is not executing.
func TestPhaseWrongState(t *testing.T) {
	e := newEnv(t, envOpts{})
	_, err := RunPhase(e.ctx, PhaseDeps{Store: e.store, Run: e.runDir, Implementer: &fake.Implementer{}, Reviewer: e.reviewer,
		Worktree: e.wt, Config: e.cfg, Owner: "o"}, PhaseInput{RunID: e.run.ID, Phase: 1})
	if err == nil || (!errors.Is(err, ErrPhaseState) && !strings.Contains(err.Error(), "not found")) {
		t.Fatalf("err %v", err)
	}
}
