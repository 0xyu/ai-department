package engine

import (
	"fmt"
	"slices"
	"time"

	"github.com/0xyu/aco/internal/state"
)

// CommitGateInput is everything the Commit Gate (§11.8) checks. All values
// are recomputed by the orchestrator after the review; no agent runs again
// in the phase.
type CommitGateInput struct {
	// ReviewedTree and ReviewedPaths were recorded when the review ran;
	// CurrentTree and CurrentPaths are recomputed now.
	ReviewedTree  string
	ReviewedPaths []string
	CurrentTree   string
	CurrentPaths  []string
	// GateTree is the tree the latest quality gate round ran on.
	GateTree   string
	GatesGreen bool
	// ReviewPassed and ReviewTree: the Review Decision Gate passed for this tree.
	ReviewPassed bool
	ReviewTree   string
	// OpenBlocking counts open blocker/major findings (not out of scope or deferred).
	OpenBlocking int
	// TriageBlocked is a blocking triage class ("" when none).
	TriageBlocked string
	// PolicyBlocking lists blocking diff-policy findings on the final diff
	// (protected paths, secrets, scope with enforcement block, …).
	PolicyBlocking []string
	// PlanIntact: plan.lock.json still matches; PlanProblem says why not.
	PlanIntact  bool
	PlanProblem string
}

// Commit gate actions.
const (
	CommitProceed = "commit"
	CommitReview  = "review" // the tree changed since the review: back to Reviewing
	CommitBlock   = "block"
)

// CommitGateDecision is the Commit Gate result.
type CommitGateDecision struct {
	Action  string           `json:"action"`
	Class   state.Class      `json:"class,omitempty"` // set for block
	Reasons []PlanGateReason `json:"reasons"`
}

// Commit gate reason codes.
const (
	CGTreeChanged   = "tree_changed"
	CGPathsChanged  = "paths_changed"
	CGGatesStale    = "gates_not_for_tree"
	CGGatesRed      = "gates_not_green"
	CGReviewMissing = "review_not_passed_for_tree"
	CGOpenFinding   = "open_blocking_finding"
	CGTriageBlocked = "triage_blocked"
	CGPolicy        = "policy_violation"
	CGPlanChanged   = "plan_changed"
	// CGWorktreeDrift: the Git helper refused to stage because the
	// worktree changed after the gate passed (back to gates and review).
	CGWorktreeDrift = "worktree_drift"
)

// DecideCommitGate is the Commit Gate (§11.8), a pure function. A changed
// tree (or a gate round that is not for the reviewed tree) sends the phase
// back to review; a broken plan lock blocks with plan_integrity; a policy
// violation or any other failed check blocks with commit_gate_blocked.
// Nothing is committed unless every check passes.
func DecideCommitGate(in CommitGateInput) CommitGateDecision {
	var back, block []PlanGateReason
	if in.ReviewedTree == "" || in.CurrentTree != in.ReviewedTree {
		back = append(back, PlanGateReason{Code: CGTreeChanged, Detail: in.CurrentTree})
	}
	cur, rev := slices.Clone(in.CurrentPaths), slices.Clone(in.ReviewedPaths)
	slices.Sort(cur)
	slices.Sort(rev)
	if !slices.Equal(cur, rev) {
		back = append(back, PlanGateReason{Code: CGPathsChanged})
	}
	if in.GateTree != in.ReviewedTree {
		back = append(back, PlanGateReason{Code: CGGatesStale})
	}
	plan := !in.PlanIntact
	if plan {
		block = append(block, PlanGateReason{Code: CGPlanChanged, Detail: in.PlanProblem})
	}
	for _, p := range in.PolicyBlocking {
		block = append(block, PlanGateReason{Code: CGPolicy, Detail: p})
	}
	if !in.GatesGreen {
		block = append(block, PlanGateReason{Code: CGGatesRed})
	}
	if !in.ReviewPassed || in.ReviewTree != in.ReviewedTree {
		block = append(block, PlanGateReason{Code: CGReviewMissing})
	}
	if in.OpenBlocking > 0 {
		block = append(block, PlanGateReason{Code: CGOpenFinding})
	}
	if in.TriageBlocked != "" {
		block = append(block, PlanGateReason{Code: CGTriageBlocked, Ref: in.TriageBlocked})
	}
	switch {
	case plan:
		d := finish(append(block, back...))
		return CommitGateDecision{Action: CommitBlock, Class: state.ClassPlanIntegrity, Reasons: d.Reasons}
	case len(back) > 0:
		// A different tree needs fresh gates and a fresh review; the
		// other checks are repeated after that.
		return CommitGateDecision{Action: CommitReview, Reasons: finish(back).Reasons}
	case len(block) > 0:
		return CommitGateDecision{Action: CommitBlock, Class: state.ClassCommitGateBlocked, Reasons: finish(block).Reasons}
	}
	return CommitGateDecision{Action: CommitProceed, Reasons: []PlanGateReason{}}
}

// --- convergence guards (§11.7) -----------------------------------------------

// GuardInput holds the counters of the current attempt.
type GuardInput struct {
	ReviewRounds, ReviewLimit int // review rounds that failed the gate so far
	GateFixes, GateFixLimit   int // consecutive gate-fix attempts
	MaxReopen, ReopenLimit    int // highest finding reopen count
	Rejections                int // most rejected disputes of one finding (deadlock at 2)
	NoProgress                int // consecutive fix rounds without changes or accepted disputes
	PolicyViolations          int // guard blocks in this attempt
	PolicyLimit               int
	AgentCalls, CallLimit     int // agent invocations in this attempt
	Turns, TurnLimit          int // Claude turns in this attempt
	Elapsed, Timeout          time.Duration
	IncompleteReports         int // implementer reports still missing after the continue limit
}

// GuardTrip is a tripped guard: the attempt ends.
type GuardTrip struct {
	Guard  string      `json:"guard"`
	Class  state.Class `json:"class"`
	Detail string      `json:"detail"`
}

// Guard names.
const (
	GuardPolicy      = "policy_violations"
	GuardOscillation = "oscillation"
	GuardDeadlock    = "dispute_deadlock"
	GuardNoProgress  = "no_progress"
	GuardReviews     = "review_rounds"
	GuardGateFixes   = "gate_fix_attempts"
	GuardUsage       = "usage"
	GuardIncomplete  = "agent_incomplete"
)

// NoProgressLimit is the number of fix rounds in a row without progress
// that ends an attempt.
const NoProgressLimit = 2

// DeadlockLimit is how often the same dispute may be rejected.
const DeadlockLimit = 2

// CheckGuards returns the first tripped guard, or nil. Limits <= 0 are off.
func CheckGuards(in GuardInput) *GuardTrip {
	trip := func(g string, c state.Class, format string, a ...any) *GuardTrip {
		return &GuardTrip{Guard: g, Class: c, Detail: fmt.Sprintf(format, a...)}
	}
	switch {
	case in.PolicyLimit > 0 && in.PolicyViolations >= in.PolicyLimit:
		return trip(GuardPolicy, state.ClassPolicyViolations, "%d guard blocks in this attempt (limits.policy_violations %d)", in.PolicyViolations, in.PolicyLimit)
	case in.ReopenLimit > 0 && in.MaxReopen >= in.ReopenLimit:
		return trip(GuardOscillation, state.ClassOscillation, "a finding was reopened %d times (limits.max_reopen %d)", in.MaxReopen, in.ReopenLimit)
	case in.Rejections >= DeadlockLimit:
		return trip(GuardDeadlock, state.ClassDisputeDeadlock, "the same finding was disputed and rejected %d times", in.Rejections)
	case in.NoProgress >= NoProgressLimit:
		return trip(GuardNoProgress, state.ClassNoProgress, "%d fix rounds in a row without changes or accepted disputes", in.NoProgress)
	case in.ReviewLimit > 0 && in.ReviewRounds >= in.ReviewLimit:
		return trip(GuardReviews, state.ClassIterationLimit, "the review gate still fails after %d review rounds (limits.review_rounds)", in.ReviewRounds)
	case in.GateFixLimit > 0 && in.GateFixes >= in.GateFixLimit:
		return trip(GuardGateFixes, state.ClassIterationLimit, "the quality gates still fail after %d consecutive fix attempts (limits.gate_fix_attempts)", in.GateFixes)
	case in.IncompleteReports > 0:
		return trip(GuardIncomplete, state.ClassIterationLimit, "the implementer did not return a valid report")
	case in.CallLimit > 0 && in.AgentCalls >= in.CallLimit:
		return trip(GuardUsage, state.ClassPhaseUsageLimit, "%d agent calls in this attempt (limits.agent_calls.phase %d)", in.AgentCalls, in.CallLimit)
	case in.TurnLimit > 0 && in.Turns >= in.TurnLimit:
		return trip(GuardUsage, state.ClassPhaseUsageLimit, "%d Claude turns in this attempt (limits.claude_turns.phase %d)", in.Turns, in.TurnLimit)
	case in.Timeout > 0 && in.Elapsed >= in.Timeout:
		return trip(GuardUsage, state.ClassPhaseUsageLimit, "the attempt ran %s (limits.phase_timeout %s)", in.Elapsed.Round(time.Second), in.Timeout)
	}
	return nil
}
