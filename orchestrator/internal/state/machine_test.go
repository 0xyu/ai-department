package state

import (
	"errors"
	"reflect"
	"testing"

	"github.com/0xyu/aco/internal/events"
)

// legalRun is the expected run table, written out independently of
// runEdges. Paused is checked separately (it depends on PausedFrom).
var legalRun = map[[2]RunStatus]bool{
	{RunCreated, RunPreflight}: true, {RunCreated, RunFailed}: true, {RunCreated, RunCancelled}: true,

	{RunPreflight, RunPlanning}: true, {RunPreflight, RunPlanReview}: true, {RunPreflight, RunFailed}: true,
	{RunPreflight, RunCancelled}: true,

	{RunPlanning, RunPlanReview}: true, {RunPlanning, RunPaused}: true, {RunPlanning, RunBlocked}: true,
	{RunPlanning, RunFailed}: true, {RunPlanning, RunCancelled}: true,

	{RunPlanReview, RunPlanning}: true, {RunPlanReview, RunExecuting}: true, {RunPlanReview, RunPaused}: true,
	{RunPlanReview, RunBlocked}: true, {RunPlanReview, RunFailed}: true, {RunPlanReview, RunCancelled}: true,

	{RunExecuting, RunFinalVerification}: true, {RunExecuting, RunPaused}: true, {RunExecuting, RunBlocked}: true,
	{RunExecuting, RunFailed}: true, {RunExecuting, RunCancelled}: true,

	{RunFinalVerification, RunCompleted}: true, {RunFinalVerification, RunPaused}: true,
	{RunFinalVerification, RunBlocked}: true, {RunFinalVerification, RunFailed}: true,
	{RunFinalVerification, RunCancelled}: true,
}

func reasonFor(to RunStatus, from RunStatus) Reason {
	stage := StageOf(from)
	if from == RunPaused || from == RunCreated || from == RunPreflight {
		stage = StagePreflight
		if from == RunPaused {
			stage = StageExecution
		}
	}
	switch to {
	case RunBlocked:
		r := Reason{Class: ClassPlanReviewRounds, Stage: stage, Detail: "x"}
		if stage == StageExecution {
			r.Class, r.Phase = ClassIterationLimit, 3
		}
		return r
	case RunFailed:
		r := Reason{Class: ClassInternal, Stage: stage}
		if stage == StageExecution {
			r.Phase = 2
		}
		return r
	}
	return Reason{}
}

// TestRunMachineEveryPair covers every (from, to) pair of the run machine.
func TestRunMachineEveryPair(t *testing.T) {
	for _, from := range RunStatuses {
		if from == RunPaused {
			continue
		}
		for _, to := range RunStatuses {
			err := CheckRunTransition(RunSnapshot{Status: from}, to, reasonFor(to, from))
			want := legalRun[[2]RunStatus{from, to}]
			if want && err != nil {
				t.Errorf("%s -> %s: want legal, got %v", from, to, err)
			}
			if !want {
				if err == nil {
					t.Errorf("%s -> %s: want illegal", from, to)
				} else if !errors.Is(err, ErrIllegalTransition) {
					t.Errorf("%s -> %s: want ErrIllegalTransition, got %v", from, to, err)
				}
			}
		}
	}
}

func TestRunMachinePaused(t *testing.T) {
	stages := []RunStatus{RunPlanning, RunPlanReview, RunExecuting, RunFinalVerification}
	for _, pf := range stages {
		cur := RunSnapshot{Status: RunPaused, PausedFrom: pf}
		for _, to := range RunStatuses {
			err := CheckRunTransition(cur, to, reasonFor(to, RunPaused))
			want := to == pf || to == RunFailed || to == RunCancelled
			if want != (err == nil) {
				t.Errorf("paused(from %s) -> %s: legal=%v, err=%v", pf, to, want, err)
			}
		}
	}
	for _, from := range []RunStatus{RunCreated, RunPreflight} {
		if CheckRunTransition(RunSnapshot{Status: from}, RunPaused, Reason{}) == nil {
			t.Errorf("%s cannot pause", from)
		}
	}
}

// Planning <-> PlanReview loops any number of times, then freezes.
func TestRunMachinePlanReviewLoop(t *testing.T) {
	cur := RunSnapshot{Status: RunPreflight}
	path := []RunStatus{RunPlanning, RunPlanReview, RunPlanning, RunPlanReview, RunPlanning, RunPlanReview, RunExecuting}
	for _, to := range path {
		if err := CheckRunTransition(cur, to, Reason{}); err != nil {
			t.Fatalf("%s -> %s: %v", cur.Status, to, err)
		}
		cur.Status = to
	}
	// Plan given: preflight goes straight to review.
	if err := CheckRunTransition(RunSnapshot{Status: RunPreflight}, RunPlanReview, Reason{}); err != nil {
		t.Error(err)
	}
	if err := CheckRunTransition(RunSnapshot{Status: RunPlanning}, RunExecuting, Reason{}); err == nil {
		t.Error("planning must not skip plan review")
	}
}

// BLOCKED is reachable from every stage and only with a blocked reason.
func TestRunMachineBlockedFromEveryStage(t *testing.T) {
	for status, stage := range stageOf {
		r := Reason{Class: ClassSpecConflict, Stage: stage}
		if stage == StageExecution {
			r.Phase = 4
		}
		if err := CheckRunTransition(RunSnapshot{Status: status}, RunBlocked, r); err != nil {
			t.Errorf("%s -> blocked: %v", status, err)
		}
		if err := CheckRunTransition(RunSnapshot{Status: status}, RunBlocked, Reason{}); !errors.Is(err, ErrBadReason) {
			t.Errorf("%s -> blocked without reason: want ErrBadReason, got %v", status, err)
		}
		if err := CheckRunTransition(RunSnapshot{Status: status}, RunBlocked,
			Reason{Class: ClassAuth, Stage: stage, Phase: r.Phase}); !errors.Is(err, ErrBadReason) {
			t.Errorf("%s -> blocked with a failed class: want ErrBadReason, got %v", status, err)
		}
	}
	for _, s := range []RunStatus{RunCreated, RunPreflight, RunPaused} {
		if err := CheckRunTransition(RunSnapshot{Status: s, PausedFrom: RunExecuting}, RunBlocked,
			Reason{Class: ClassUnsafe, Stage: StagePlanning}); err == nil {
			t.Errorf("%s -> blocked must be illegal", s)
		}
	}
}

func TestReasonRules(t *testing.T) {
	cases := []struct {
		name    string
		blocked bool
		r       Reason
		ok      bool
	}{
		{"blocked ok", true, Reason{Class: ClassUnsafe, Stage: StageExecution, Phase: 2}, true},
		{"blocked execution needs phase", true, Reason{Class: ClassUnsafe, Stage: StageExecution}, false},
		{"blocked preflight", true, Reason{Class: ClassEnvironment, Stage: StagePreflight}, false},
		{"phase outside execution", true, Reason{Class: ClassPlanReviewRounds, Stage: StagePlanReview, Phase: 1}, false},
		{"failed with blocked class", false, Reason{Class: ClassUnsafe, Stage: StagePlanning}, false},
		{"failed preflight", false, Reason{Class: ClassPreflightFailed, Stage: StagePreflight}, true},
		{"failed execution no phase", false, Reason{Class: ClassAuth, Stage: StageExecution}, true},
		{"bad stage", false, Reason{Class: ClassAuth, Stage: "nowhere"}, false},
		{"negative phase", false, Reason{Class: ClassAuth, Stage: StageExecution, Phase: -1}, false},
	}
	for _, c := range cases {
		if err := c.r.validateFor(c.blocked); (err == nil) != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
	}
	// A reason on a non-terminal transition is rejected.
	if err := CheckRunTransition(RunSnapshot{Status: RunPlanning}, RunPlanReview,
		Reason{Class: ClassUnsafe, Stage: StagePlanning}); !errors.Is(err, ErrBadReason) {
		t.Errorf("reason on planning -> plan_review: %v", err)
	}
	// Blocked and failed are distinct: no class is in both lists.
	for _, c := range BlockedClasses {
		if hasClass(FailedClasses, c) {
			t.Errorf("class %s is both blocked and failed", c)
		}
	}
}

func TestRunTerminalAndResume(t *testing.T) {
	for _, s := range []RunStatus{RunCompleted, RunBlocked, RunFailed, RunCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []RunStatus{RunCompleted, RunCancelled, RunExecuting, RunPaused} {
		if _, err := RunResumeTarget(RunSnapshot{Status: s}); err == nil {
			t.Errorf("%s must not be resumable", s)
		}
	}
	cases := []struct {
		r    Reason
		want RunStatus
	}{
		{Reason{Class: ClassPlanReviewRounds, Stage: StagePlanReview}, RunPlanReview},
		{Reason{Class: ClassIterationLimit, Stage: StageExecution, Phase: 3}, RunExecuting},
		{Reason{Class: ClassFinalFixRounds, Stage: StageFinalVerification}, RunFinalVerification},
		{Reason{Class: ClassPlanInvalid, Stage: StagePlanning}, RunPlanning},
	}
	for _, c := range cases {
		got, err := RunResumeTarget(RunSnapshot{Status: RunBlocked, Reason: c.r})
		if err != nil || got != c.want {
			t.Errorf("blocked at %s: got %s, %v; want %s", c.r.Stage, got, err, c.want)
		}
	}
	got, err := RunResumeTarget(RunSnapshot{Status: RunFailed, Reason: Reason{Class: ClassPreflightFailed, Stage: StagePreflight}})
	if err != nil || got != RunPreflight {
		t.Errorf("failed at preflight resumes to preflight, got %s %v", got, err)
	}
	blocked := RunSnapshot{Status: RunBlocked, Reason: Reason{Class: ClassSpecConflict, Stage: StageExecution, Phase: 2}}
	for _, to := range RunStatuses {
		err := CheckRunResume(blocked, to)
		want := to == RunPreflight || StageOf(to) != ""
		if want != (err == nil) {
			t.Errorf("resume blocked -> %s: legal=%v err=%v", to, want, err)
		}
	}
	if err := CheckRunResume(RunSnapshot{Status: RunCompleted}, RunExecuting); err == nil {
		t.Error("completed must not be resumed")
	}
}

func types(es []Emit) []events.Type {
	var out []events.Type
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func TestRunTransitionEvents(t *testing.T) {
	cases := []struct {
		from, to RunStatus
		resumed  bool
		want     []events.Type
	}{
		{RunCreated, RunPreflight, false, nil},
		{RunPreflight, RunPlanning, false, []events.Type{events.RunPreflightPassed, events.RunStageStarted}},
		{RunPreflight, RunFailed, false, []events.Type{events.RunPreflightFailed, events.RunFailed}},
		{RunPlanning, RunPlanReview, false, []events.Type{events.RunStageCompleted, events.RunStageStarted}},
		{RunPlanReview, RunBlocked, false, []events.Type{events.RunBlocked}},
		{RunExecuting, RunPaused, false, []events.Type{events.RunPaused}},
		{RunPaused, RunExecuting, false, []events.Type{events.RunResumed}},
		{RunPaused, RunCancelled, false, []events.Type{events.RunCancelled}},
		{RunFinalVerification, RunCompleted, false, []events.Type{events.RunStageCompleted, events.RunCompleted}},
		{RunBlocked, RunExecuting, true, []events.Type{events.RunResumed, events.RunStageStarted}},
	}
	for _, c := range cases {
		if got := types(RunTransitionEvents(c.from, c.to, c.resumed)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s -> %s: got %v want %v", c.from, c.to, got, c.want)
		}
	}
	// Every legal transition emits only known event types.
	for pair := range legalRun {
		for _, e := range RunTransitionEvents(pair[0], pair[1], false) {
			if !events.Known(e.Type) {
				t.Errorf("%v: unknown event %s", pair, e.Type)
			}
		}
	}
}

// legalPhase is the expected phase table (§11.2 plus documented edges).
var legalPhase = map[[2]PhaseStatus]bool{
	{PhasePending, PhaseInitializing}:          true,
	{PhaseInitializing, PhaseImplementing}:     true,
	{PhaseInitializing, PhaseBlocked}:          true,
	{PhaseImplementing, PhaseQualityGates}:     true,
	{PhaseImplementing, PhaseAttemptExhausted}: true,
	{PhaseQualityGates, PhaseFixing}:           true,
	{PhaseQualityGates, PhaseReviewing}:        true,
	{PhaseQualityGates, PhaseBlocked}:          true,
	{PhaseReviewing, PhaseCommitGate}:          true,
	{PhaseReviewing, PhaseTriage}:              true,
	{PhaseReviewing, PhaseAttemptExhausted}:    true,
	{PhaseTriage, PhaseFixing}:                 true,
	{PhaseTriage, PhaseBlocked}:                true,
	{PhaseFixing, PhaseQualityGates}:           true,
	{PhaseFixing, PhaseAttemptExhausted}:       true,
	{PhaseAttemptExhausted, PhaseInitializing}: true,
	{PhaseAttemptExhausted, PhaseBlocked}:      true,
	{PhaseCommitGate, PhaseCommitted}:          true,
	{PhaseCommitGate, PhaseReviewing}:          true,
	{PhaseCommitGate, PhaseBlocked}:            true,
}

func TestPhaseMachineEveryPair(t *testing.T) {
	blocked := Reason{Class: ClassUnsafe, Stage: StageExecution, Phase: 1}
	for _, from := range PhaseStatuses {
		for _, to := range PhaseStatuses {
			r := Reason{}
			if to == PhaseBlocked {
				r = blocked
			}
			err := CheckPhaseTransition(from, to, r)
			want := legalPhase[[2]PhaseStatus{from, to}]
			if want != (err == nil) {
				t.Errorf("%s -> %s: legal=%v err=%v", from, to, want, err)
			}
			if !want && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("%s -> %s: want ErrIllegalTransition, got %v", from, to, err)
			}
		}
	}
	if err := CheckPhaseTransition(PhaseTriage, PhaseBlocked, Reason{}); !errors.Is(err, ErrBadReason) {
		t.Errorf("phase blocked without reason: %v", err)
	}
	if err := CheckPhaseTransition(PhaseTriage, PhaseBlocked,
		Reason{Class: ClassBudgetExceeded, Stage: StageExecution, Phase: 1}); !errors.Is(err, ErrBadReason) {
		t.Errorf("phase blocked with a failed class: %v", err)
	}
}

// The full happy path and the fix and restart loops of §11.2.
func TestPhaseMachineLoops(t *testing.T) {
	walk := func(path ...PhaseStatus) {
		t.Helper()
		for i := 1; i < len(path); i++ {
			r := Reason{}
			if path[i] == PhaseBlocked {
				r = Reason{Class: ClassIterationLimit, Stage: StageExecution, Phase: 1}
			}
			if err := CheckPhaseTransition(path[i-1], path[i], r); err != nil {
				t.Fatalf("%s -> %s: %v", path[i-1], path[i], err)
			}
		}
	}
	walk(PhasePending, PhaseInitializing, PhaseImplementing, PhaseQualityGates, PhaseFixing, PhaseQualityGates,
		PhaseReviewing, PhaseTriage, PhaseFixing, PhaseQualityGates, PhaseReviewing, PhaseCommitGate, PhaseReviewing,
		PhaseCommitGate, PhaseCommitted)
	walk(PhaseFixing, PhaseAttemptExhausted, PhaseInitializing, PhaseImplementing, PhaseAttemptExhausted, PhaseBlocked)
	if err := CheckPhaseResume(PhaseBlocked); err != nil {
		t.Error(err)
	}
	for _, s := range PhaseStatuses {
		if s != PhaseBlocked && CheckPhaseResume(s) == nil {
			t.Errorf("resume from %s must be illegal", s)
		}
	}
}

func TestPhaseTransitionEvents(t *testing.T) {
	cases := []struct {
		from, to PhaseStatus
		want     []events.Type
	}{
		{PhasePending, PhaseInitializing, []events.Type{events.PhaseStarted, events.PhaseAttemptStarted}},
		{PhaseInitializing, PhaseImplementing, []events.Type{events.PhaseInitPassed}},
		{PhaseInitializing, PhaseBlocked, []events.Type{events.PhaseInitFailed, events.PhaseBlocked}},
		{PhaseFixing, PhaseAttemptExhausted, []events.Type{events.PhaseAttemptExhausted}},
		{PhaseAttemptExhausted, PhaseInitializing, []events.Type{events.PhaseAttemptStarted}},
		{PhaseCommitGate, PhaseCommitted, []events.Type{events.PhaseCompleted}},
		{PhaseQualityGates, PhaseReviewing, nil},
		{PhaseBlocked, PhaseInitializing, []events.Type{events.PhaseAttemptStarted}},
	}
	for _, c := range cases {
		if got := PhaseTransitionEvents(c.from, c.to); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s -> %s: got %v want %v", c.from, c.to, got, c.want)
		}
	}
}
