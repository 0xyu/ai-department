package state

import (
	"errors"
	"fmt"
)

// Stage is a pipeline stage. Preflight is not a numbered stage but can be
// where a run fails.
type Stage string

// Stages.
const (
	StagePreflight         Stage = "preflight"
	StagePlanning          Stage = "planning"
	StagePlanReview        Stage = "plan_review"
	StageExecution         Stage = "execution"
	StageFinalVerification Stage = "final_verification"
)

// Valid reports whether s is a known stage.
func (s Stage) Valid() bool {
	switch s {
	case StagePreflight, StagePlanning, StagePlanReview, StageExecution, StageFinalVerification:
		return true
	}
	return false
}

// Class is the machine-readable cause of a blocked or failed run or phase.
type Class string

// Blocked classes: ACO could not reach the required quality safely; a human
// must decide (§11.7, §13).
const (
	ClassInitCheckFailed   Class = "init_check_failed"   // phase baseline / dependency / plan-hash check
	ClassPlanIntegrity     Class = "plan_integrity"      // plan.lock.json mismatch at the commit gate
	ClassPlanInvalid       Class = "plan_invalid"        // planner cannot produce a valid plan package
	ClassPlanReviewRounds  Class = "plan_review_rounds"  // plan gate still FAIL after limits.plan_review_rounds
	ClassSpecConflict      Class = "spec_conflict"       // fix needs a change to the frozen plan
	ClassUnsafe            Class = "unsafe"              // fix needs protected paths, secrets, or disabling checks
	ClassEnvironment       Class = "environment"         // missing tool, service, or credential for a gate
	ClassIterationLimit    Class = "iteration_limit"     // review rounds / gate-fix attempts after restarts
	ClassOscillation       Class = "oscillation"         // same finding reopened too often
	ClassNoProgress        Class = "no_progress"         // fix rounds without changes
	ClassDisputeDeadlock   Class = "dispute_deadlock"    // same dispute rejected twice
	ClassPolicyViolations  Class = "policy_violations"   // too many guard blocks (possible injection)
	ClassPhaseUsageLimit   Class = "phase_usage_limit"   // phase agent calls / turns / timeout after restarts
	ClassFinalFixRounds    Class = "final_fix_rounds"    // final gate still FAIL after limits.final_fix_rounds
	ClassCommitGateBlocked Class = "commit_gate_blocked" // final policy re-check failed
)

// Failed classes: a system problem (§13, §14.1).
const (
	ClassPreflightFailed    Class = "preflight_failed"
	ClassSystemEnvironment  Class = "system_environment" // Git corrupt, disk full, DB error
	ClassInternal           Class = "internal"
	ClassUsageLimitExceeded Class = "usage_limit_exceeded"
	ClassBudgetExceeded     Class = "budget_exceeded"
	ClassAuth               Class = "auth"
	ClassAPIQuota           Class = "api_quota"
	ClassMaxPauseExceeded   Class = "max_pause_exceeded"
	ClassRunTimeout         Class = "run_timeout"
)

// BlockedClasses and FailedClasses list the classes allowed for each state.
var (
	BlockedClasses = []Class{
		ClassInitCheckFailed, ClassPlanIntegrity, ClassPlanInvalid, ClassPlanReviewRounds,
		ClassSpecConflict, ClassUnsafe, ClassEnvironment, ClassIterationLimit, ClassOscillation,
		ClassNoProgress, ClassDisputeDeadlock, ClassPolicyViolations, ClassPhaseUsageLimit,
		ClassFinalFixRounds, ClassCommitGateBlocked,
	}
	FailedClasses = []Class{
		ClassPreflightFailed, ClassSystemEnvironment, ClassInternal, ClassUsageLimitExceeded,
		ClassBudgetExceeded, ClassAuth, ClassAPIQuota, ClassMaxPauseExceeded, ClassRunTimeout,
	}
)

func hasClass(list []Class, c Class) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

// Reason explains a blocked or failed run or phase. Detail is English text
// for the report; it is masked before it is stored.
type Reason struct {
	Class  Class
	Stage  Stage
	Phase  int // 0 when not phase-specific
	Detail string
}

// IsZero reports whether r is empty.
func (r Reason) IsZero() bool { return r == Reason{} }

// ErrBadReason is wrapped by every reason validation error.
var ErrBadReason = errors.New("invalid reason")

// validateFor checks r for a run or phase entering status blocked or failed.
func (r Reason) validateFor(blocked bool) error {
	list, what := FailedClasses, "failed"
	if blocked {
		list, what = BlockedClasses, "blocked"
	}
	if !hasClass(list, r.Class) {
		return fmt.Errorf("%w: class %q is not a %s class", ErrBadReason, r.Class, what)
	}
	if !r.Stage.Valid() {
		return fmt.Errorf("%w: stage %q", ErrBadReason, r.Stage)
	}
	if blocked && r.Stage == StagePreflight {
		return fmt.Errorf("%w: preflight problems fail a run, they do not block it", ErrBadReason)
	}
	if r.Phase < 0 {
		return fmt.Errorf("%w: phase %d", ErrBadReason, r.Phase)
	}
	if r.Phase > 0 && r.Stage != StageExecution {
		return fmt.Errorf("%w: phase set outside the execution stage", ErrBadReason)
	}
	if blocked && r.Stage == StageExecution && r.Phase == 0 {
		return fmt.Errorf("%w: a run blocked in execution must name the blocked phase", ErrBadReason)
	}
	return nil
}
