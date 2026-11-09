package state

import (
	"errors"
	"fmt"

	"github.com/0xyu/aco/internal/events"
)

// The run (§13) and phase (§11.2) state machines are pure functions over
// statuses. The Store applies every status change through them, so an
// illegal transition is an error and never reaches the database.
// docs/lifecycle.md has the diagrams and full tables.

// RunStatus is the status of a run.
type RunStatus string

// Run statuses.
const (
	RunCreated           RunStatus = "created"
	RunPreflight         RunStatus = "preflight"
	RunPlanning          RunStatus = "planning"
	RunPlanReview        RunStatus = "plan_review"
	RunExecuting         RunStatus = "executing"
	RunFinalVerification RunStatus = "final_verification"
	RunPaused            RunStatus = "paused"
	RunCompleted         RunStatus = "completed"
	RunBlocked           RunStatus = "blocked"
	RunFailed            RunStatus = "failed"
	RunCancelled         RunStatus = "cancelled"
)

// RunStatuses lists every run status.
var RunStatuses = []RunStatus{
	RunCreated, RunPreflight, RunPlanning, RunPlanReview, RunExecuting, RunFinalVerification,
	RunPaused, RunCompleted, RunBlocked, RunFailed, RunCancelled,
}

// Terminal reports whether no further automatic transition exists. Blocked
// and failed runs can still be resumed by a human (ResumeRun).
func (s RunStatus) Terminal() bool {
	return s == RunCompleted || s == RunBlocked || s == RunFailed || s == RunCancelled
}

// Resumable reports whether a human may resume a run in this status.
func (s RunStatus) Resumable() bool { return s == RunBlocked || s == RunFailed }

// stageOf maps the four stage statuses to their stage.
var stageOf = map[RunStatus]Stage{
	RunPlanning:          StagePlanning,
	RunPlanReview:        StagePlanReview,
	RunExecuting:         StageExecution,
	RunFinalVerification: StageFinalVerification,
}

// statusOf is the inverse of stageOf, plus preflight.
var statusOf = map[Stage]RunStatus{
	StagePreflight:         RunPreflight,
	StagePlanning:          RunPlanning,
	StagePlanReview:        RunPlanReview,
	StageExecution:         RunExecuting,
	StageFinalVerification: RunFinalVerification,
}

// StageOf returns the stage a run status belongs to ("" for none).
func StageOf(s RunStatus) Stage { return stageOf[s] }

// runEdges is the transition table. Paused resumes only to the status it
// paused from (checked in CheckRunTransition).
var runEdges = map[RunStatus][]RunStatus{
	RunCreated:           {RunPreflight, RunFailed, RunCancelled},
	RunPreflight:         {RunPlanning, RunPlanReview, RunFailed, RunCancelled},
	RunPlanning:          {RunPlanReview, RunPaused, RunBlocked, RunFailed, RunCancelled},
	RunPlanReview:        {RunPlanning, RunExecuting, RunPaused, RunBlocked, RunFailed, RunCancelled},
	RunExecuting:         {RunFinalVerification, RunPaused, RunBlocked, RunFailed, RunCancelled},
	RunFinalVerification: {RunCompleted, RunPaused, RunBlocked, RunFailed, RunCancelled},
	RunPaused:            {RunPlanning, RunPlanReview, RunExecuting, RunFinalVerification, RunFailed, RunCancelled},
}

// ErrIllegalTransition is wrapped by every rejected transition.
var ErrIllegalTransition = errors.New("illegal state transition")

// TransitionError names the rejected transition.
type TransitionError struct {
	Machine  string // "run" or "phase"
	From, To string
	Why      string
}

func (e *TransitionError) Error() string {
	s := fmt.Sprintf("%s: %s %s -> %s", ErrIllegalTransition, e.Machine, e.From, e.To)
	if e.Why != "" {
		s += " (" + e.Why + ")"
	}
	return s
}

// Unwrap lets errors.Is(err, ErrIllegalTransition) match.
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// RunSnapshot is the part of a run the machine looks at.
type RunSnapshot struct {
	Status     RunStatus
	PausedFrom RunStatus // set while Status is paused
	Reason     Reason    // set while Status is blocked or failed
}

func contains[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// CheckRunTransition reports whether cur may move to `to` in normal
// operation. reason must be set (and valid) exactly when `to` is blocked or
// failed.
func CheckRunTransition(cur RunSnapshot, to RunStatus, reason Reason) error {
	illegal := func(why string) error {
		return &TransitionError{Machine: "run", From: string(cur.Status), To: string(to), Why: why}
	}
	if !contains(runEdges[cur.Status], to) {
		if cur.Status.Terminal() {
			return illegal("terminal status")
		}
		return illegal("")
	}
	if cur.Status == RunPaused && to != RunFailed && to != RunCancelled && to != cur.PausedFrom {
		return illegal("a paused run resumes to the status it paused from (" + string(cur.PausedFrom) + ")")
	}
	return checkReason(to == RunBlocked, to == RunFailed, reason)
}

func checkReason(blocked, failed bool, reason Reason) error {
	switch {
	case blocked || failed:
		return reason.validateFor(blocked)
	case !reason.IsZero():
		return fmt.Errorf("%w: a reason is only recorded for blocked or failed", ErrBadReason)
	}
	return nil
}

// RunResumeTarget is where a blocked or failed run resumes by default: the
// stage named in its reason (preflight for a preflight failure).
func RunResumeTarget(cur RunSnapshot) (RunStatus, error) {
	if !cur.Status.Resumable() {
		return "", &TransitionError{Machine: "run", From: string(cur.Status), To: "resume", Why: "only blocked or failed runs are resumed"}
	}
	to, ok := statusOf[cur.Reason.Stage]
	if !ok {
		return "", fmt.Errorf("%w: stored reason has no stage", ErrBadReason)
	}
	return to, nil
}

// CheckRunResume reports whether a human may resume cur to `to`: cur must be
// blocked or failed and `to` a stage status or preflight (for example
// planning for --revise-plan, executing for --from-phase).
func CheckRunResume(cur RunSnapshot, to RunStatus) error {
	if !cur.Status.Resumable() {
		return &TransitionError{Machine: "run", From: string(cur.Status), To: string(to), Why: "only blocked or failed runs are resumed"}
	}
	if to != RunPreflight && StageOf(to) == "" {
		return &TransitionError{Machine: "run", From: string(cur.Status), To: string(to), Why: "resume target must be preflight or a stage"}
	}
	return nil
}

// Emit is one event a transition produces.
type Emit struct {
	Type  events.Type
	Stage Stage
}

// RunTransitionEvents lists the events for a legal run transition, in order.
// resumed is true for a human resume from blocked or failed.
func RunTransitionEvents(from, to RunStatus, resumed bool) []Emit {
	var out []Emit
	if resumed {
		out = append(out, Emit{Type: events.RunResumed, Stage: StageOf(to)})
		if s := StageOf(to); s != "" {
			out = append(out, Emit{Type: events.RunStageStarted, Stage: s})
		}
		return out
	}
	if from == RunPaused {
		if to == RunFailed || to == RunCancelled {
			return terminalEmit(to, "")
		}
		return []Emit{{Type: events.RunResumed, Stage: StageOf(to)}}
	}
	fromStage := StageOf(from)
	switch {
	case from == RunPreflight && to == RunFailed:
		out = append(out, Emit{Type: events.RunPreflightFailed, Stage: StagePreflight})
	case from == RunPreflight && StageOf(to) != "":
		out = append(out, Emit{Type: events.RunPreflightPassed, Stage: StagePreflight})
	case fromStage != "" && (StageOf(to) != "" || to == RunCompleted):
		out = append(out, Emit{Type: events.RunStageCompleted, Stage: fromStage})
	}
	if to == RunPaused {
		return append(out, Emit{Type: events.RunPaused, Stage: fromStage})
	}
	if s := StageOf(to); s != "" {
		return append(out, Emit{Type: events.RunStageStarted, Stage: s})
	}
	return append(out, terminalEmit(to, fromStage)...)
}

func terminalEmit(to RunStatus, stage Stage) []Emit {
	switch to {
	case RunCompleted:
		return []Emit{{Type: events.RunCompleted, Stage: stage}}
	case RunBlocked:
		return []Emit{{Type: events.RunBlocked, Stage: stage}}
	case RunFailed:
		return []Emit{{Type: events.RunFailed, Stage: stage}}
	case RunCancelled:
		return []Emit{{Type: events.RunCancelled, Stage: stage}}
	}
	return nil
}

// PhaseStatus is the status of one phase of a run (§11.2).
type PhaseStatus string

// Phase statuses.
const (
	PhasePending          PhaseStatus = "pending"
	PhaseInitializing     PhaseStatus = "initializing"
	PhaseImplementing     PhaseStatus = "implementing"
	PhaseQualityGates     PhaseStatus = "quality_gates"
	PhaseReviewing        PhaseStatus = "reviewing"
	PhaseTriage           PhaseStatus = "triage"
	PhaseFixing           PhaseStatus = "fixing"
	PhaseAttemptExhausted PhaseStatus = "attempt_exhausted"
	PhaseCommitGate       PhaseStatus = "commit_gate"
	PhaseCommitted        PhaseStatus = "committed"
	PhaseBlocked          PhaseStatus = "blocked"
)

// PhaseStatuses lists every phase status.
var PhaseStatuses = []PhaseStatus{
	PhasePending, PhaseInitializing, PhaseImplementing, PhaseQualityGates, PhaseReviewing,
	PhaseTriage, PhaseFixing, PhaseAttemptExhausted, PhaseCommitGate, PhaseCommitted, PhaseBlocked,
}

// Terminal reports whether the phase is committed or blocked.
func (s PhaseStatus) Terminal() bool { return s == PhaseCommitted || s == PhaseBlocked }

// phaseEdges is the §11.2 table plus the edges the stage descriptions
// require (marked): attempt-ending guards can trip while implementing or
// reviewing (§11.7, §14.1), an environment gate failure blocks (§11.7), and
// the commit gate blocks on a plan-integrity or policy failure (§11.8).
var phaseEdges = map[PhaseStatus][]PhaseStatus{
	PhasePending:          {PhaseInitializing},
	PhaseInitializing:     {PhaseImplementing, PhaseBlocked},
	PhaseImplementing:     {PhaseQualityGates, PhaseAttemptExhausted /* + */},
	PhaseQualityGates:     {PhaseFixing, PhaseReviewing, PhaseBlocked /* + */},
	PhaseReviewing:        {PhaseCommitGate, PhaseTriage, PhaseAttemptExhausted /* + */},
	PhaseTriage:           {PhaseFixing, PhaseBlocked},
	PhaseFixing:           {PhaseQualityGates, PhaseAttemptExhausted},
	PhaseAttemptExhausted: {PhaseInitializing, PhaseBlocked},
	PhaseCommitGate:       {PhaseCommitted, PhaseReviewing, PhaseBlocked /* + */},
}

// CheckPhaseTransition reports whether a phase may move from `from` to `to`.
// reason must be set (and a valid blocked reason) exactly when `to` is
// blocked.
func CheckPhaseTransition(from, to PhaseStatus, reason Reason) error {
	if !contains(phaseEdges[from], to) {
		why := ""
		if from.Terminal() {
			why = "terminal status"
		}
		return &TransitionError{Machine: "phase", From: string(from), To: string(to), Why: why}
	}
	if to == PhaseBlocked && reason.Stage != StageExecution {
		return fmt.Errorf("%w: a phase is blocked in the execution stage", ErrBadReason)
	}
	return checkReason(to == PhaseBlocked, false, reason)
}

// CheckPhaseResume reports whether a human may restart a phase: only a
// blocked phase, and always into a new attempt (initializing).
func CheckPhaseResume(from PhaseStatus) error {
	if from != PhaseBlocked {
		return &TransitionError{Machine: "phase", From: string(from), To: string(PhaseInitializing), Why: "only blocked phases are resumed"}
	}
	return nil
}

// PhaseTransitionEvents lists the events for a legal phase transition or a
// human resume (blocked -> initializing). Entering initializing always
// starts a new attempt.
func PhaseTransitionEvents(from, to PhaseStatus) []events.Type {
	var out []events.Type
	switch {
	case from == PhasePending && to == PhaseInitializing:
		out = append(out, events.PhaseStarted, events.PhaseAttemptStarted)
	case to == PhaseInitializing: // restart or human resume
		out = append(out, events.PhaseAttemptStarted)
	case from == PhaseInitializing && to == PhaseImplementing:
		out = append(out, events.PhaseInitPassed)
	case from == PhaseInitializing && to == PhaseBlocked:
		out = append(out, events.PhaseInitFailed)
	}
	switch to {
	case PhaseAttemptExhausted:
		out = append(out, events.PhaseAttemptExhausted)
	case PhaseCommitted:
		out = append(out, events.PhaseCompleted)
	case PhaseBlocked:
		out = append(out, events.PhaseBlocked)
	}
	return out
}
