package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openTest(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mustRun(t *testing.T, s *Store, input InputKind) Run {
	t.Helper()
	r, err := s.CreateRun(context.Background(), NewRun{RepoPath: t.TempDir(), BaseBranch: "main", Mode: ModeFull, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func step(t *testing.T, s *Store, id string, to RunStatus, opt TransitionOptions) Run {
	t.Helper()
	r, err := s.TransitionRun(context.Background(), id, to, opt)
	if err != nil {
		t.Fatalf("-> %s: %v", to, err)
	}
	if r.Status != to {
		t.Fatalf("status %s, want %s", r.Status, to)
	}
	return r
}

func eventTypes(t *testing.T, s *Store, runID string) []events.Type {
	t.Helper()
	evs, err := s.EventsAfter(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []events.Type
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
		out = append(out, e.Type)
	}
	return out
}

func TestOpenMigratesOnceAndRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t, Options{})
	r := mustRun(t, s, InputPlan)
	s.Close()
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetRun(ctx, r.ID); err != nil {
		t.Fatalf("data lost on reopen: %v", err)
	}
	var mode string
	if err := s2.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
	if _, err := s2.db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (99, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("want ErrNewerSchema, got %v", err)
	}
	if SchemaVersion() != 1 {
		t.Errorf("SchemaVersion = %d", SchemaVersion())
	}
}

func TestRunLifecyclePersistsTransitionsAndEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{Lang: i18n.EN})
	r := mustRun(t, s, InputRequirements)
	if r.Status != RunCreated || !strings.HasPrefix(r.ID, "20261") && len(r.ID) < 10 {
		t.Fatalf("new run: %+v", r)
	}
	step(t, s, r.ID, RunPreflight, TransitionOptions{})
	step(t, s, r.ID, RunPlanning, TransitionOptions{})
	step(t, s, r.ID, RunPlanReview, TransitionOptions{})
	step(t, s, r.ID, RunPlanning, TransitionOptions{}) // plan gate FAIL -> revise
	step(t, s, r.ID, RunPlanReview, TransitionOptions{})
	step(t, s, r.ID, RunExecuting, TransitionOptions{})
	resume := time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)
	p := step(t, s, r.ID, RunPaused, TransitionOptions{ResumeAt: resume})
	if p.PausedFrom != RunExecuting || p.ResumeAt == nil || !p.ResumeAt.Equal(resume) {
		t.Errorf("paused: %+v", p)
	}
	// Resuming to the wrong stage is illegal and changes nothing.
	if _, err := s.TransitionRun(ctx, r.ID, RunPlanning, TransitionOptions{}); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("paused -> planning: %v", err)
	}
	step(t, s, r.ID, RunExecuting, TransitionOptions{})
	step(t, s, r.ID, RunFinalVerification, TransitionOptions{})
	done := step(t, s, r.ID, RunCompleted, TransitionOptions{})
	if done.EndedAt == nil || done.PausedFrom != "" || done.ResumeAt != nil {
		t.Errorf("completed: %+v", done)
	}
	if _, err := s.TransitionRun(ctx, r.ID, RunExecuting, TransitionOptions{}); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("completed is terminal: %v", err)
	}
	if _, err := s.ResumeRun(ctx, r.ID, ""); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("completed is not resumable: %v", err)
	}

	want := []events.Type{
		events.RunCreated,
		events.RunPreflightPassed, events.RunStageStarted,
		events.RunStageCompleted, events.RunStageStarted,
		events.RunStageCompleted, events.RunStageStarted,
		events.RunStageCompleted, events.RunStageStarted,
		events.RunStageCompleted, events.RunStageStarted,
		events.RunPaused, events.RunResumed,
		events.RunStageCompleted, events.RunStageStarted,
		events.RunStageCompleted, events.RunCompleted,
	}
	if got := eventTypes(t, s, r.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
	final, _ := s.GetRun(ctx, r.ID)
	if final.LastEventSeq != int64(len(want)) {
		t.Errorf("last_event_seq %d", final.LastEventSeq)
	}
	evs, _ := s.EventsAfter(ctx, r.ID, 0, 1)
	if evs[0].Message != "Run created" {
		t.Errorf("message %q", evs[0].Message)
	}
	stages, err := s.ListStages(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range stages {
		got = append(got, string(e.Stage)+"#"+string(rune('0'+e.Entry))+":"+e.Status)
	}
	wantStages := []string{"planning#1:completed", "plan_review#1:completed", "planning#2:completed",
		"plan_review#2:completed", "execution#1:completed", "final_verification#1:completed"}
	if !reflect.DeepEqual(got, wantStages) {
		t.Errorf("stages:\n got %v\nwant %v", got, wantStages)
	}
}

func TestBlockedAndFailedAreDistinctAndResumable(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	r := mustRun(t, s, InputPlan)
	step(t, s, r.ID, RunPreflight, TransitionOptions{})
	step(t, s, r.ID, RunPlanReview, TransitionOptions{})
	if _, err := s.TransitionRun(ctx, r.ID, RunBlocked, TransitionOptions{}); !errors.Is(err, ErrBadReason) {
		t.Fatalf("blocked without reason: %v", err)
	}
	if got, _ := s.GetRun(ctx, r.ID); got.Status != RunPlanReview {
		t.Fatalf("rejected transition changed the status to %s", got.Status)
	}
	reason := Reason{Class: ClassPlanReviewRounds, Stage: StagePlanReview, Detail: "4 rounds without PASS"}
	b := step(t, s, r.ID, RunBlocked, TransitionOptions{Reason: reason})
	if b.Reason != reason {
		t.Errorf("reason %+v", b.Reason)
	}
	evs, _ := s.EventsAfter(ctx, r.ID, b.LastEventSeq-1, 0)
	if evs[0].Type != events.RunBlocked {
		t.Fatalf("last event %s", evs[0].Type)
	}
	if f, _ := evs[0].Payload.Get("class"); f.String() != string(ClassPlanReviewRounds) {
		t.Errorf("payload %v", evs[0].Payload)
	}
	res, err := s.ResumeRun(ctx, r.ID, "")
	if err != nil || res.Status != RunPlanReview || !res.Reason.IsZero() || res.EndedAt != nil {
		t.Fatalf("resume: %+v %v", res, err)
	}
	step(t, s, r.ID, RunExecuting, TransitionOptions{})
	f := step(t, s, r.ID, RunFailed, TransitionOptions{Reason: Reason{Class: ClassAuth, Stage: StageExecution}})
	if f.Status == RunBlocked || f.Reason.Class != ClassAuth {
		t.Errorf("failed: %+v", f)
	}
	if _, err := s.ResumeRun(ctx, r.ID, RunCompleted); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("resume to completed: %v", err)
	}
	if got, _ := s.ResumeRun(ctx, r.ID, ""); got.Status != RunExecuting {
		t.Errorf("failed in execution resumes to executing, got %s", got.Status)
	}
	stages, _ := s.ListStages(ctx, r.ID)
	last := stages[len(stages)-2]
	if last.Stage != StageExecution || last.Status != "failed" {
		t.Errorf("stage entries %+v", stages)
	}
}

func executingRun(t *testing.T, s *Store, phases int) Run {
	t.Helper()
	r := mustRun(t, s, InputPlan)
	step(t, s, r.ID, RunPreflight, TransitionOptions{})
	step(t, s, r.ID, RunPlanReview, TransitionOptions{})
	defs := make([]PhaseDef, phases)
	for i := range defs {
		defs[i] = PhaseDef{Number: i + 1, Title: "Phase"}
	}
	if err := s.CreatePhases(context.Background(), r.ID, defs); err != nil {
		t.Fatal(err)
	}
	return step(t, s, r.ID, RunExecuting, TransitionOptions{})
}

func phaseTo(t *testing.T, s *Store, runID string, n int, to PhaseStatus, r Reason) Phase {
	t.Helper()
	p, err := s.TransitionPhase(context.Background(), runID, n, to, r)
	if err != nil {
		t.Fatalf("phase %d -> %s: %v", n, to, err)
	}
	return p
}

func TestPhaseLifecycleAttemptsAndEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	r := executingRun(t, s, 2)
	if err := s.CreatePhases(ctx, r.ID, []PhaseDef{{Number: 1}}); err == nil {
		t.Error("phases registered twice")
	}
	seq0 := r.LastEventSeq
	phaseTo(t, s, r.ID, 1, PhaseInitializing, Reason{})
	phaseTo(t, s, r.ID, 1, PhaseImplementing, Reason{})
	phaseTo(t, s, r.ID, 1, PhaseQualityGates, Reason{})
	phaseTo(t, s, r.ID, 1, PhaseFixing, Reason{})
	phaseTo(t, s, r.ID, 1, PhaseAttemptExhausted, Reason{})
	p := phaseTo(t, s, r.ID, 1, PhaseInitializing, Reason{}) // restart
	if p.Attempts != 2 {
		t.Errorf("attempts %d", p.Attempts)
	}
	if _, err := s.TransitionPhase(ctx, r.ID, 1, PhaseCommitted, Reason{}); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("initializing -> committed: %v", err)
	}
	for _, to := range []PhaseStatus{PhaseImplementing, PhaseQualityGates, PhaseReviewing, PhaseCommitGate, PhaseCommitted} {
		p = phaseTo(t, s, r.ID, 1, to, Reason{})
	}
	if p.EndedAt == nil || p.StartedAt == nil {
		t.Errorf("committed phase times: %+v", p)
	}
	atts, _ := s.ListAttempts(ctx, r.ID, 1)
	if len(atts) != 2 || atts[0].Status != "exhausted" || atts[1].Status != "committed" {
		t.Errorf("attempts %+v", atts)
	}
	if err := s.SetAttemptSession(ctx, r.ID, 1, 2, "sess-1"); err != nil {
		t.Error(err)
	}
	if err := s.SetPhaseCommits(ctx, r.ID, 1, "base1", "c0ffee"); err != nil {
		t.Error(err)
	}

	// Phase 2 blocks; the run blocks; a human resumes both.
	phaseTo(t, s, r.ID, 2, PhaseInitializing, Reason{})
	b := phaseTo(t, s, r.ID, 2, PhaseBlocked, Reason{Class: ClassInitCheckFailed, Detail: "HEAD moved"})
	if b.Reason.Stage != StageExecution || b.Reason.Phase != 2 {
		t.Errorf("phase reason %+v", b.Reason)
	}
	run := step(t, s, r.ID, RunBlocked, TransitionOptions{Reason: b.Reason})
	if run.CurrentPhase != 2 {
		t.Errorf("current phase %d", run.CurrentPhase)
	}
	if _, err := s.ResumePhase(ctx, r.ID, 2); err == nil {
		t.Error("phase resumed while the run is blocked")
	}
	if _, err := s.ResumeRun(ctx, r.ID, ""); err != nil {
		t.Fatal(err)
	}
	p2, err := s.ResumePhase(ctx, r.ID, 2)
	if err != nil || p2.Status != PhaseInitializing || p2.Attempts != 2 || !p2.Reason.IsZero() || p2.EndedAt != nil {
		t.Fatalf("resume phase: %+v %v", p2, err)
	}
	if _, err := s.ResumePhase(ctx, r.ID, 1); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("committed phase resumed: %v", err)
	}

	evs, _ := s.EventsAfter(ctx, r.ID, seq0, 0)
	var phaseEvents []string
	for _, e := range evs {
		if strings.HasPrefix(string(e.Type), "phase.") {
			phaseEvents = append(phaseEvents, string(e.Type)+"@"+string(rune('0'+e.Phase)))
		}
	}
	want := []string{"phase.started@1", "phase.attempt.started@1", "phase.init.passed@1", "phase.attempt.exhausted@1",
		"phase.attempt.started@1", "phase.init.passed@1", "phase.completed@1",
		"phase.started@2", "phase.attempt.started@2", "phase.init.failed@2", "phase.blocked@2",
		"phase.attempt.started@2"}
	if !reflect.DeepEqual(phaseEvents, want) {
		t.Errorf("phase events:\n got %v\nwant %v", phaseEvents, want)
	}
}

func TestStepsIntentOutcome(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	r := executingRun(t, s, 1)
	phaseTo(t, s, r.ID, 1, PhaseInitializing, Reason{})
	in := StepIntent{RunID: r.ID, Phase: 1, Attempt: 1, Kind: "implement", Key: "P01/a1/implement", Owner: "o1",
		Intent: []byte(`{"session":"new"}`), CheckpointRef: "refs/aco/x/P01/a1/s1"}
	st, err := s.BeginStep(ctx, in)
	if err != nil || st.Status != StepStarted || string(st.Intent) != `{"session":"new"}` {
		t.Fatalf("begin: %+v %v", st, err)
	}
	again, err := s.BeginStep(ctx, in)
	if !errors.Is(err, ErrStepExists) || again.ID != st.ID {
		t.Errorf("duplicate key: %+v %v", again, err)
	}
	done, err := s.CompleteStep(ctx, st.ID, []byte(`{"files":2}`))
	if err != nil || done.Status != StepCompleted || done.EndedAt == nil {
		t.Fatalf("complete: %+v %v", done, err)
	}
	if _, err := s.CompleteStep(ctx, st.ID, nil); !errors.Is(err, ErrStepState) {
		t.Errorf("complete twice: %v", err)
	}
	f, _ := s.BeginStep(ctx, StepIntent{RunID: r.ID, Phase: 1, Attempt: 1, Kind: "gates", Key: "k2", Owner: "o1"})
	if f2, err := s.FailStep(ctx, f.ID, "exit 1", nil); err != nil || f2.Status != StepFailed || f2.Error != "exit 1" {
		t.Errorf("fail: %+v %v", f2, err)
	}
	bad := []StepIntent{
		{RunID: r.ID, Kind: "Bad Kind", Key: "k", Owner: "o"},
		{RunID: r.ID, Kind: "k", Key: "has space", Owner: "o"},
		{RunID: r.ID, Kind: "k", Key: "k3"},
		{RunID: r.ID, Phase: 1, Kind: "k", Key: "k4", Owner: "o"},
		{RunID: r.ID, Phase: 1, Attempt: 9, Kind: "k", Key: "k5", Owner: "o"},
		{RunID: r.ID, Kind: "k", Key: "k6", Owner: "o", Intent: []byte("{not json")},
		{RunID: "no-such-run", Kind: "k", Key: "k7", Owner: "o"},
	}
	for i, b := range bad {
		if _, err := s.BeginStep(ctx, b); err == nil {
			t.Errorf("bad intent #%d accepted", i)
		}
	}

	// A started step of a vanished owner needs reconciliation.
	lost, _ := s.BeginStep(ctx, StepIntent{RunID: r.ID, Kind: "commit", Key: "commit-1", Owner: "dead"})
	mine, _ := s.BeginStep(ctx, StepIntent{RunID: r.ID, Kind: "commit", Key: "commit-2", Owner: "me"})
	marked, err := s.MarkInterrupted(ctx, r.ID, "me")
	if err != nil || len(marked) != 1 || marked[0].ID != lost.ID {
		t.Fatalf("mark: %+v %v", marked, err)
	}
	if got, _ := s.GetStep(ctx, mine.ID); got.Status != StepStarted {
		t.Errorf("own step marked: %s", got.Status)
	}
	if _, err := s.CompleteStep(ctx, lost.ID, nil); !errors.Is(err, ErrStepState) {
		t.Errorf("complete an interrupted step: %v", err)
	}
	if _, err := s.ResolveStep(ctx, lost.ID, "whatever", nil); err == nil {
		t.Error("unknown resolution accepted")
	}
	rs, err := s.ResolveStep(ctx, lost.ID, ResolutionAccepted, []byte(`{"commit":"abc"}`))
	if err != nil || rs.Status != StepReconciled || rs.Resolution != ResolutionAccepted {
		t.Errorf("resolve: %+v %v", rs, err)
	}
	if n, _ := s.ListSteps(ctx, r.ID, StepNeedsReconciliation); len(n) != 0 {
		t.Errorf("left: %+v", n)
	}
	all, _ := s.ListSteps(ctx, r.ID)
	if len(all) != 4 {
		t.Errorf("steps %d", len(all))
	}
}

func TestRecords(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	r := executingRun(t, s, 1)
	if err := s.AddPlanReview(ctx, PlanReview{RunID: r.ID, Round: 1, ReviewerModel: "gpt", Verdict: "pass",
		GatePassed: true, Review: []byte(`{"verdict":"pass"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPlanReview(ctx, PlanReview{RunID: r.ID, Round: 1, Review: []byte(`{}`)}); err == nil {
		t.Error("duplicate plan review round accepted")
	}
	prs, _ := s.ListPlanReviews(ctx, r.ID)
	if len(prs) != 1 || !prs[0].GatePassed {
		t.Errorf("plan reviews %+v", prs)
	}
	id, err := s.StartInvocation(ctx, Invocation{RunID: r.ID, Agent: "claude", Role: "implementer", Model: "claude-opus-5-5",
		AuthMode: "subscription"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishInvocation(ctx, id, InvocationResult{SessionID: "s1", Turns: 12, InputTokens: 100, CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishInvocation(ctx, id, InvocationResult{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish twice: %v", err)
	}
	if _, err := s.StartInvocation(ctx, Invocation{RunID: r.ID, Agent: "gemini", Role: "reviewer", AuthMode: "api"}); err == nil {
		t.Error("unknown agent accepted")
	}
	invs, _ := s.ListInvocations(ctx, r.ID)
	if len(invs) != 1 || invs[0].Status != "completed" || invs[0].Turns != 12 || invs[0].SessionID != "s1" {
		t.Errorf("invocations %+v", invs)
	}
	if _, err := s.AddGateResult(ctx, GateResult{RunID: r.ID, Phase: 1, Attempt: 1, Round: 1, Gate: "unit",
		Status: "failed", Command: "go test", ExitCode: 1, Duration: 1500 * time.Millisecond,
		Summary: []byte(`{"failed":["TestX"]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGateResult(ctx, GateResult{RunID: r.ID, Gate: "e2e", Status: "passed"}); err == nil {
		t.Error("unknown gate accepted")
	}
	gs, _ := s.ListGateResults(ctx, r.ID, 1)
	if len(gs) != 1 || gs[0].Duration != 1500*time.Millisecond {
		t.Errorf("gates %+v", gs)
	}
	if err := s.AddReview(ctx, Review{RunID: r.ID, Kind: "code", Phase: 1, Attempt: 1, Round: 1, ReviewerModel: "gpt",
		Verdict: "approve", TreeHash: "t1", Review: []byte(`{"verdict":"approve"}`)}); err != nil {
		t.Fatal(err)
	}
	rvs, _ := s.ListReviews(ctx, r.ID, 1)
	if len(rvs) != 1 || rvs[0].TreeHash != "t1" {
		t.Errorf("reviews %+v", rvs)
	}
	f := Finding{RunID: r.ID, ID: "F-01-001", Scope: "code", Phase: 1, Fingerprint: "fp1", Severity: "major",
		Category: "correctness", Status: "open", Title: "nil deref", OpenedRound: 1}
	if err := s.UpsertFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	f.Status, f.ClosedRound = "resolved", 2
	if err := s.UpsertFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	fs, _ := s.ListFindings(ctx, r.ID, "code")
	if len(fs) != 1 || fs[0].Status != "resolved" || fs[0].ClosedRound != 2 {
		t.Errorf("findings %+v", fs)
	}
	if err := s.AddCommit(ctx, Commit{RunID: r.ID, SHA: "abc", Kind: "phase", Phase: 1, Attempt: 1, TreeHash: "t1",
		ReviewedTree: "t1", Tag: "aco/x/phase-01"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.ListCommits(ctx, r.ID)
	if len(cs) != 1 || cs[0].Tag != "aco/x/phase-01" {
		t.Errorf("commits %+v", cs)
	}
	for i := 0; i < 2; i++ {
		if err := s.AddReport(ctx, Report{RunID: r.ID, Kind: "blocked", Path: "reports/blocked.md",
			SHA256: strings.Repeat(string(rune('a'+i)), 64)}); err != nil {
			t.Fatal(err)
		}
	}
	reps, _ := s.ListReports(ctx, r.ID)
	if len(reps) != 1 || reps[0].SHA256[0] != 'b' {
		t.Errorf("reports %+v", reps)
	}
}

func TestAppendEventValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	r := mustRun(t, s, InputPlan)
	bad := []events.Event{
		{RunID: r.ID, Type: "run.exploded"},
		{RunID: r.ID, Type: events.GateStarted, Stage: "nowhere"},
		{RunID: r.ID, Type: events.GateStarted, Phase: -1},
		{RunID: r.ID, Type: events.GateStarted, Payload: events.Payload{events.Str("Bad Key", "x")}},
		{RunID: r.ID, Type: events.GateStarted, Payload: events.Payload{{Key: "empty"}}},
		{RunID: "missing-run", Type: events.GateStarted},
	}
	for i, e := range bad {
		if _, err := s.AppendEvent(ctx, e); err == nil {
			t.Errorf("bad event #%d accepted", i)
		}
	}
	if got, _ := s.GetRun(ctx, r.ID); got.LastEventSeq != 1 {
		t.Errorf("rejected events moved the sequence to %d", got.LastEventSeq)
	}
	e, err := s.AppendEvent(ctx, events.Event{RunID: r.ID, Type: events.GateCompleted, Stage: "execution", Phase: 1,
		Payload: events.Payload{events.Str("gate", "unit"), events.Bool("passed", true), events.Int("ms", 12),
			events.Strs("failed")}})
	if err != nil || e.Seq != 2 {
		t.Fatalf("append: %+v %v", e, err)
	}
	evs, _ := s.EventsAfter(ctx, r.ID, 1, 0)
	if !reflect.DeepEqual(evs[0].Payload, e.Payload) || evs[0].Message == "" {
		t.Errorf("round trip: %+v vs %+v", evs[0], e)
	}
}

// Every text column of every table must be free of the fake credentials
// after they were written through every write API.
func TestStoreNeverStoresSecrets(t *testing.T) {
	ctx := context.Background()
	var secrets []secure.Secret
	for _, f := range securetest.All {
		secrets = append(secrets, secure.NewSecret(f))
	}
	red := secure.NewRedactor(secrets...)
	s, path := openTest(t, Options{Redactor: red})
	all := strings.Join(securetest.All, " | ")
	jsonDoc := []byte(`{"note":` + quoteJSON(all) + `}`)

	r, err := s.CreateRun(ctx, NewRun{RepoPath: t.TempDir(), BaseBranch: "main " + all, Mode: ModeFull, Input: InputPlan})
	if err != nil {
		t.Fatal(err)
	}
	step(t, s, r.ID, RunPreflight, TransitionOptions{})
	step(t, s, r.ID, RunPlanReview, TransitionOptions{})
	if err := s.CreatePhases(ctx, r.ID, []PhaseDef{{Number: 1, Title: all}}); err != nil {
		t.Fatal(err)
	}
	step(t, s, r.ID, RunExecuting, TransitionOptions{})
	branch, hash := "aco/"+all, all
	if _, err := s.UpdateRunInfo(ctx, r.ID, RunInfo{RunBranch: &branch, PlanHash: &hash}); err != nil {
		t.Fatal(err)
	}
	phaseTo(t, s, r.ID, 1, PhaseInitializing, Reason{})
	if err := s.SetAttemptSession(ctx, r.ID, 1, 1, all); err != nil {
		t.Fatal(err)
	}
	st, err := s.BeginStep(ctx, StepIntent{RunID: r.ID, Phase: 1, Attempt: 1, Kind: "implement", Key: "k1", Owner: "o",
		Intent: jsonDoc, CheckpointRef: all})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailStep(ctx, st.ID, all, jsonDoc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, events.Event{RunID: r.ID, Type: events.PolicyViolation, Message: all,
		Payload: events.Payload{events.Str("detail", all), events.Strs("list", securetest.All...)}}); err != nil {
		t.Fatal(err)
	}
	b := phaseTo(t, s, r.ID, 1, PhaseBlocked, Reason{Class: ClassUnsafe, Detail: all})
	step(t, s, r.ID, RunBlocked, TransitionOptions{Reason: b.Reason})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.AddPlanReview(ctx, PlanReview{RunID: r.ID, Round: 1, ReviewerModel: all, Verdict: all, Review: jsonDoc, Artifact: all}))
	inv, err := s.StartInvocation(ctx, Invocation{RunID: r.ID, Agent: "codex", Role: "reviewer", Model: all, AuthMode: "api", SessionID: all})
	must(err)
	must(s.FinishInvocation(ctx, inv, InvocationResult{Failed: true, SessionID: all, Transcript: all, Error: all}))
	_, err = s.AddGateResult(ctx, GateResult{RunID: r.ID, Gate: "security", Status: "failed", Command: all, Summary: jsonDoc, LogArtifact: all})
	must(err)
	must(s.AddReview(ctx, Review{RunID: r.ID, Kind: "final", Round: 1, ReviewerModel: all, Verdict: all, Review: jsonDoc, Artifact: all}))
	must(s.UpsertFinding(ctx, Finding{RunID: r.ID, ID: "PF-001", Scope: "plan", Fingerprint: "fp", Severity: "major",
		Category: "risks", Status: "open", Title: all, File: all, OpenedRound: 1, Data: jsonDoc}))
	must(s.AddCommit(ctx, Commit{RunID: r.ID, SHA: "abc", Kind: "plan_freeze", TreeHash: "t", Tag: all}))
	must(s.AddReport(ctx, Report{RunID: r.ID, Kind: "blocked", Path: all, SHA256: "x"}))
	s.Close()

	// Read every text value back with a plain connection.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	var dump strings.Builder
	for _, tbl := range names {
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				switch x := v.(type) {
				case string:
					dump.WriteString(x + "\n")
				case []byte:
					dump.Write(x)
					dump.WriteString("\n")
				}
			}
		}
		rows.Close()
	}
	if !strings.Contains(dump.String(), secure.Mask) {
		t.Fatal("nothing was masked; the test wrote no secrets?")
	}
	securetest.AssertNoLeak(t, "state database", dump.String())
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestRepoKey(t *testing.T) {
	dir := t.TempDir()
	a, err := RepoKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RepoKey(filepath.Join(dir, "sub", ".."))
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	if strings.Contains(a, `\`) {
		t.Errorf("key not slash-separated: %q", a)
	}
}

func TestRunIDValidation(t *testing.T) {
	s, _ := openTest(t, Options{})
	for _, id := range []string{"ab", "Has-Upper", "../x", "a b c", strings.Repeat("a", 65)} {
		if _, err := s.CreateRun(context.Background(), NewRun{ID: id, RepoPath: t.TempDir(), Mode: ModeFull,
			Input: InputPlan}); err == nil {
			t.Errorf("run ID %q accepted", id)
		}
	}
	if id := NewRunID(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); !runIDRE.MatchString(id) || !strings.HasPrefix(id, "20260102-") {
		t.Errorf("NewRunID %q", id)
	}
}
