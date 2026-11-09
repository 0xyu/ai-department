// Package engine runs ACO's pipeline stages. This file and its neighbours
// implement Stages 1 and 2 (§10): plan creation, independent plan review,
// the Plan Decision Gate, the revision loop, and the plan freeze.
package engine

import (
	"slices"
	"sort"

	"github.com/0xyu/aco/internal/agent"
)

// PlanGateInput is everything the Plan Decision Gate (§10.4) looks at. It
// never looks at the reviewer's verdict: the decision comes from the
// details.
type PlanGateInput struct {
	// ChecksPassed is the result of the deterministic plan checks.
	ChecksPassed bool
	// Reviewed is false when no review exists for this package (the checks
	// failed, so no GPT call was spent).
	Reviewed bool
	// ReqIDs are the package's requirement IDs.
	ReqIDs   []string
	Coverage []agent.ReqCoverage
	Ratings  []agent.DimensionRating
	// Findings is the plan finding ledger after this review was applied.
	Findings []PlanGateFinding
	// PreviousIDs are the findings sent to the reviewer for a status;
	// Statuses are what came back.
	PreviousIDs []string
	Statuses    []agent.PreviousStatus
}

// PlanGateFinding is a ledger entry as the gate sees it.
type PlanGateFinding struct {
	ID       string
	Severity string // blocker | major | minor
	Open     bool
}

// Plan gate reason codes.
const (
	GateChecksFailed       = "checks_failed"
	GateNotReviewed        = "not_reviewed"
	GateRequirementMissing = "requirement_not_in_review" // the review has no coverage entry for a REQ
	GateRequirementStatus  = "requirement_not_covered"   // coverage status partial or missing
	GateDimensionUnsound   = "dimension_unsound"
	GateOpenFinding        = "open_finding"
	GatePreviousNoStatus   = "previous_finding_without_status"
)

// PlanGateReason is one reason the gate failed.
type PlanGateReason struct {
	Code   string `json:"code"`
	Ref    string `json:"ref,omitempty"` // REQ ID, dimension, or finding ID
	Detail string `json:"detail,omitempty"`
}

// PlanGateDecision is the gate's result.
type PlanGateDecision struct {
	Pass    bool             `json:"pass"`
	Reasons []PlanGateReason `json:"reasons"`
}

// DecidePlanGate is the Plan Decision Gate (§10.4), a pure function:
//
//	PASS ⇔ deterministic plan checks pass
//	     ∧ every REQ has coverage status "covered"
//	     ∧ no dimension rated "unsound"
//	     ∧ no open blocker/major plan finding
//	     ∧ every previous plan finding has a status
//
// Reasons are sorted, so equal inputs give equal decisions.
func DecidePlanGate(in PlanGateInput) PlanGateDecision {
	var rs []PlanGateReason
	if !in.ChecksPassed {
		rs = append(rs, PlanGateReason{Code: GateChecksFailed})
	}
	if !in.Reviewed {
		rs = append(rs, PlanGateReason{Code: GateNotReviewed})
		return finish(rs)
	}
	cov := map[string]string{}
	for _, c := range in.Coverage {
		cov[c.ReqID] = c.Status
	}
	for _, id := range in.ReqIDs {
		switch st, ok := cov[id]; {
		case !ok:
			rs = append(rs, PlanGateReason{Code: GateRequirementMissing, Ref: id})
		case st != "covered":
			rs = append(rs, PlanGateReason{Code: GateRequirementStatus, Ref: id, Detail: st})
		}
	}
	for _, r := range in.Ratings {
		if r.Rating == "unsound" {
			rs = append(rs, PlanGateReason{Code: GateDimensionUnsound, Ref: r.Dimension})
		}
	}
	for _, f := range in.Findings {
		if f.Open && (f.Severity == "blocker" || f.Severity == "major") {
			rs = append(rs, PlanGateReason{Code: GateOpenFinding, Ref: f.ID, Detail: f.Severity})
		}
	}
	for _, id := range in.PreviousIDs {
		if !slices.ContainsFunc(in.Statuses, func(s agent.PreviousStatus) bool { return s.ID == id && s.Status != "" }) {
			rs = append(rs, PlanGateReason{Code: GatePreviousNoStatus, Ref: id})
		}
	}
	return finish(rs)
}

func finish(rs []PlanGateReason) PlanGateDecision {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Code != rs[j].Code {
			return rs[i].Code < rs[j].Code
		}
		return rs[i].Ref < rs[j].Ref
	})
	if rs == nil {
		rs = []PlanGateReason{}
	}
	return PlanGateDecision{Pass: len(rs) == 0, Reasons: rs}
}
