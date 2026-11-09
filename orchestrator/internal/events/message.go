package events

import "github.com/0xyu/aco/internal/i18n"

// messageKeys maps each event type to its operator-language message.
var messageKeys = map[Type]i18n.Key{
	Type("run.created"):                i18n.EventRunCreated,
	Type("run.preflight.passed"):       i18n.EventRunPreflightPassed,
	Type("run.preflight.failed"):       i18n.EventRunPreflightFailed,
	Type("run.stage.started"):          i18n.EventRunStageStarted,
	Type("run.stage.completed"):        i18n.EventRunStageCompleted,
	Type("plan.created"):               i18n.EventPlanCreated,
	Type("plan.revised"):               i18n.EventPlanRevised,
	Type("plan.review.completed"):      i18n.EventPlanReviewCompleted,
	Type("plan.gate.passed"):           i18n.EventPlanGatePassed,
	Type("plan.gate.failed"):           i18n.EventPlanGateFailed,
	Type("plan.frozen"):                i18n.EventPlanFrozen,
	Type("phase.started"):              i18n.EventPhaseStarted,
	Type("phase.init.passed"):          i18n.EventPhaseInitPassed,
	Type("phase.init.failed"):          i18n.EventPhaseInitFailed,
	Type("phase.attempt.started"):      i18n.EventPhaseAttemptStarted,
	Type("phase.attempt.exhausted"):    i18n.EventPhaseAttemptExhausted,
	Type("step.started"):               i18n.EventStepStarted,
	Type("step.completed"):             i18n.EventStepCompleted,
	Type("gate.started"):               i18n.EventGateStarted,
	Type("gate.completed"):             i18n.EventGateCompleted,
	Type("review.completed"):           i18n.EventReviewCompleted,
	Type("review.inconsistent"):        i18n.EventReviewInconsistent,
	Type("triage.completed"):           i18n.EventTriageCompleted,
	Type("finding.opened"):             i18n.EventFindingOpened,
	Type("finding.resolved"):           i18n.EventFindingResolved,
	Type("finding.reopened"):           i18n.EventFindingReopened,
	Type("finding.disputed"):           i18n.EventFindingDisputed,
	Type("finding.deferred"):           i18n.EventFindingDeferred,
	Type("guard.tripped"):              i18n.EventGuardTripped,
	Type("policy.violation"):           i18n.EventPolicyViolation,
	Type("commit_gate.passed"):         i18n.EventCommitGatePassed,
	Type("commit_gate.failed"):         i18n.EventCommitGateFailed,
	Type("git.commit"):                 i18n.EventGitCommit,
	Type("phase.completed"):            i18n.EventPhaseCompleted,
	Type("phase.blocked"):              i18n.EventPhaseBlocked,
	Type("final.check.completed"):      i18n.EventFinalCheckCompleted,
	Type("final.gate.passed"):          i18n.EventFinalGatePassed,
	Type("final.gate.failed"):          i18n.EventFinalGateFailed,
	Type("report.written"):             i18n.EventReportWritten,
	Type("run.paused"):                 i18n.EventRunPaused,
	Type("run.resumed"):                i18n.EventRunResumed,
	Type("run.completed"):              i18n.EventRunCompleted,
	Type("run.blocked"):                i18n.EventRunBlocked,
	Type("run.failed"):                 i18n.EventRunFailed,
	Type("run.cancelled"):              i18n.EventRunCancelled,
	Type("usage.warning"):              i18n.EventUsageWarning,
	Type("usage.exceeded"):             i18n.EventUsageExceeded,
	Type("budget.warning"):             i18n.EventBudgetWarning,
	Type("budget.exceeded"):            i18n.EventBudgetExceeded,
	Type("subscription.limit_reached"): i18n.EventSubscriptionLimitReached,
	Type("subscription.reset"):         i18n.EventSubscriptionReset,
	Type("auth.fallback"):              i18n.EventAuthFallback,
	Type("auth.mode_changed"):          i18n.EventAuthModeChanged,
	Type("agent.invocation.started"):   i18n.EventAgentInvocationStarted,
	Type("agent.invocation.completed"): i18n.EventAgentInvocationCompleted,
	Type("agent.invocation.failed"):    i18n.EventAgentInvocationFailed,
}

// Message renders the operator-language message for t ("" for an unknown
// type).
func Message(lang i18n.Lang, t Type) string {
	k, ok := messageKeys[t]
	if !ok {
		return ""
	}
	return i18n.T(lang, k)
}
