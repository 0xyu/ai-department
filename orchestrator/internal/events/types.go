// Package events defines ACO's public, versioned run events (§13), their
// typed payloads, and a pub/sub bus that replays persisted events from any
// sequence number.
//
// Events are stored by internal/state (the Log); this package does not import
// it. Payloads hold only typed values (strings, integers, booleans, string
// lists) so nothing is serialized by reflection, and every string is masked
// with the run's secure.Redactor before it is stored.
package events

// Type is an event type such as "run.created". The set is closed: Known
// reports whether a type is part of the v1 contract.
type Type string

// Schema is the version tag written into every encoded event.
const Schema = "aco.event/v1"

// Run lifecycle.
const (
	RunCreated         Type = "run.created"
	RunPreflightPassed Type = "run.preflight.passed"
	RunPreflightFailed Type = "run.preflight.failed"
	RunStageStarted    Type = "run.stage.started"
	RunStageCompleted  Type = "run.stage.completed"
	RunPaused          Type = "run.paused"
	RunResumed         Type = "run.resumed"
	RunCompleted       Type = "run.completed"
	RunBlocked         Type = "run.blocked"
	RunFailed          Type = "run.failed"
	RunCancelled       Type = "run.cancelled"
)

// Stages 1 and 2: plan creation, review, and freeze.
const (
	PlanCreated         Type = "plan.created"
	PlanRevised         Type = "plan.revised"
	PlanReviewCompleted Type = "plan.review.completed"
	PlanGatePassed      Type = "plan.gate.passed"
	PlanGateFailed      Type = "plan.gate.failed"
	PlanFrozen          Type = "plan.frozen"
)

// Stage 3: phases, attempts, steps, gates, reviews, findings, commits.
const (
	PhaseStarted          Type = "phase.started"
	PhaseInitPassed       Type = "phase.init.passed"
	PhaseInitFailed       Type = "phase.init.failed"
	PhaseAttemptStarted   Type = "phase.attempt.started"
	PhaseAttemptExhausted Type = "phase.attempt.exhausted"
	StepStarted           Type = "step.started"
	StepCompleted         Type = "step.completed"
	GateStarted           Type = "gate.started"
	GateCompleted         Type = "gate.completed"
	ReviewCompleted       Type = "review.completed"
	ReviewInconsistent    Type = "review.inconsistent"
	TriageCompleted       Type = "triage.completed"
	FindingOpened         Type = "finding.opened"
	FindingResolved       Type = "finding.resolved"
	FindingReopened       Type = "finding.reopened"
	FindingDisputed       Type = "finding.disputed"
	FindingDeferred       Type = "finding.deferred"
	GuardTripped          Type = "guard.tripped"
	PolicyViolation       Type = "policy.violation"
	CommitGatePassed      Type = "commit_gate.passed"
	CommitGateFailed      Type = "commit_gate.failed"
	GitCommit             Type = "git.commit"
	PhaseCompleted        Type = "phase.completed"
	PhaseBlocked          Type = "phase.blocked"
)

// Stage 4 and reports.
const (
	FinalCheckCompleted Type = "final.check.completed"
	FinalGatePassed     Type = "final.gate.passed"
	FinalGateFailed     Type = "final.gate.failed"
	ReportWritten       Type = "report.written"
)

// Usage, budget, auth, and agent invocations.
const (
	UsageWarning             Type = "usage.warning"
	UsageExceeded            Type = "usage.exceeded"
	BudgetWarning            Type = "budget.warning"
	BudgetExceeded           Type = "budget.exceeded"
	SubscriptionLimitReached Type = "subscription.limit_reached"
	SubscriptionReset        Type = "subscription.reset"
	AuthFallback             Type = "auth.fallback"
	AuthModeChanged          Type = "auth.mode_changed"
	AgentInvocationStarted   Type = "agent.invocation.started"
	AgentInvocationCompleted Type = "agent.invocation.completed"
	AgentInvocationFailed    Type = "agent.invocation.failed"
)

// All lists every v1 event type in catalog order.
var All = []Type{
	RunCreated, RunPreflightPassed, RunPreflightFailed, RunStageStarted, RunStageCompleted,
	PlanCreated, PlanRevised, PlanReviewCompleted, PlanGatePassed, PlanGateFailed, PlanFrozen,
	PhaseStarted, PhaseInitPassed, PhaseInitFailed, PhaseAttemptStarted, PhaseAttemptExhausted,
	StepStarted, StepCompleted, GateStarted, GateCompleted,
	ReviewCompleted, ReviewInconsistent, TriageCompleted,
	FindingOpened, FindingResolved, FindingReopened, FindingDisputed, FindingDeferred,
	GuardTripped, PolicyViolation, CommitGatePassed, CommitGateFailed, GitCommit,
	PhaseCompleted, PhaseBlocked,
	FinalCheckCompleted, FinalGatePassed, FinalGateFailed, ReportWritten,
	RunPaused, RunResumed, RunCompleted, RunBlocked, RunFailed, RunCancelled,
	UsageWarning, UsageExceeded, BudgetWarning, BudgetExceeded,
	SubscriptionLimitReached, SubscriptionReset, AuthFallback, AuthModeChanged,
	AgentInvocationStarted, AgentInvocationCompleted, AgentInvocationFailed,
}

var known = func() map[Type]bool {
	m := make(map[Type]bool, len(All))
	for _, t := range All {
		m[t] = true
	}
	return m
}()

// Known reports whether t is a v1 event type.
func Known(t Type) bool { return known[t] }
