package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/gates"
)

// ReviewGateInput is everything the Review Decision Gate (§11.7) looks at.
// The reviewer's verdict is not an input: it is recorded but not trusted.
type ReviewGateInput struct {
	// GatesGreen: every quality gate passed (vs baseline) on the reviewed
	// tree; FailedGates names the ones that did not.
	GatesGreen  bool
	FailedGates []string
	Review      *agent.CodeReview
	// ACIDs are the phase's criteria; each must be met (not_verifiable
	// only with AllowUnverifiable).
	ACIDs             []string
	AllowUnverifiable bool
	// Tests are the per-test results of the accepted gate round (the round
	// bound to the reviewed tree): the evidence a high-risk regression item
	// must cite.
	Tests []gates.TestCase
	// Findings is the code finding ledger after triage, current attempt.
	Findings []ReviewGateFinding
	// PreviousIDs were sent to the reviewer for a status.
	PreviousIDs []string
}

// ReviewGateFinding is a ledger entry as the gate sees it.
type ReviewGateFinding struct {
	ID       string
	Severity string
	Category string
	Class    string // resolution class after triage
	Open     bool
	// Deferred: triage closed the finding as deferred.
	Deferred bool
}

// blockingClasses are the triage classes that block the phase.
var blockingClasses = map[string]bool{ClassUnsafe: true, ClassSpecConflict: true, ClassEnvironment: true}

// blocking reports whether the finding blocks the gate: an open finding
// with a blocking triage class (any severity), or an open or deferred
// blocker/major that is not out of scope. Deferral never clears an
// in-scope blocker or major: triage only defers out-of-scope, minor/nit,
// and downgraded (now minor) findings, and this check does not trust it.
func (f ReviewGateFinding) blocking() bool {
	if !f.Open && !f.Deferred {
		return false
	}
	if f.Open && blockingClasses[f.Class] {
		return true
	}
	return (f.Severity == "blocker" || f.Severity == "major") && f.Category != "out_of_scope"
}

// Review gate reason codes.
const (
	RGGateFailed       = "quality_gate_failed"
	RGNoReview         = "no_review"
	RGPlanDeviates     = "plan_deviates"
	RGACMissing        = "ac_not_assessed"
	RGACNotMet         = "ac_not_met"
	RGSecurityFinding  = "open_security_finding"
	RGRegressionHigh   = "regression_risk_high"
	RGOpenFinding      = "open_finding"
	RGPreviousNoStatus = "previous_finding_without_status"
)

// DecideReviewGate is the Review Decision Gate (§11.7), a pure function:
//
//	PASS ⇔ all quality gates green (vs baseline)
//	     ∧ plan_conformance = conforms
//	     ∧ every AC = met   (not_verifiable only if policy.allow_unverifiable_ac)
//	     ∧ security_assessment has no open blocker/major finding
//	     ∧ regression_assessment.risk ≠ high, unless every high-risk item cites only tests that passed
//	       in the accepted gate round (exact test identity; see regressionReasons)
//	     ∧ no open blocker/major finding (except category out_of_scope)
//	     ∧ every previous finding has a status
//
// Reasons reuse PlanGateReason and are sorted, so equal inputs give equal
// decisions.
func DecideReviewGate(in ReviewGateInput) PlanGateDecision {
	var rs []PlanGateReason
	if !in.GatesGreen {
		if len(in.FailedGates) == 0 {
			rs = append(rs, PlanGateReason{Code: RGGateFailed})
		}
		for _, g := range in.FailedGates {
			rs = append(rs, PlanGateReason{Code: RGGateFailed, Ref: g})
		}
	}
	r := in.Review
	if r == nil {
		rs = append(rs, PlanGateReason{Code: RGNoReview})
		return finish(rs)
	}
	if r.PlanConformance.Status != "conforms" {
		rs = append(rs, PlanGateReason{Code: RGPlanDeviates, Detail: r.PlanConformance.Status})
	}
	status := map[string]string{}
	for _, a := range r.AcceptanceCriteria {
		status[a.ID] = a.Status
	}
	for _, id := range in.ACIDs {
		st, ok := status[id]
		switch {
		case !ok:
			rs = append(rs, PlanGateReason{Code: RGACMissing, Ref: id})
		case st == "met", st == "not_verifiable" && in.AllowUnverifiable:
		default:
			rs = append(rs, PlanGateReason{Code: RGACNotMet, Ref: id, Detail: st})
		}
	}
	rs = append(rs, regressionReasons(r.RegressionAssessment, in.Tests)...)
	for _, f := range in.Findings {
		if !f.blocking() {
			continue
		}
		code := RGOpenFinding
		if f.Category == "security" {
			code = RGSecurityFinding
		}
		detail := f.Severity
		if f.Open && blockingClasses[f.Class] {
			detail += " " + f.Class
		}
		rs = append(rs, PlanGateReason{Code: code, Ref: f.ID, Detail: detail})
	}
	for _, id := range in.PreviousIDs {
		if !slices.ContainsFunc(r.PreviousFindings, func(s agent.PreviousStatus) bool { return s.ID == id && s.Status != "" }) {
			rs = append(rs, PlanGateReason{Code: RGPreviousNoStatus, Ref: id})
		}
	}
	return finish(rs)
}

// regressionReasons checks the evidence for a high regression risk. The risk
// is high when the assessment says so or any item is high. Then there must
// be at least one high-risk item, and every high-risk item must cite at
// least one test, each of which must name exactly one test of the accepted
// round (by its full identity package.TestName, or by a bare test name that
// identifies one test) whose every result passed. Missing, ambiguous,
// skipped, and failing evidence each give a reason; free-text notes are
// never evidence.
func regressionReasons(ra agent.RiskNotes, tests []gates.TestCase) []PlanGateReason {
	var high []int
	for i, it := range ra.Items {
		if it.Risk == "high" {
			high = append(high, i)
		}
	}
	if ra.Risk != "high" && len(high) == 0 {
		return nil
	}
	if len(high) == 0 {
		return []PlanGateReason{{Code: RGRegressionHigh, Detail: "no high-risk item with test evidence"}}
	}
	var rs []PlanGateReason
	for _, i := range high {
		ref := fmt.Sprintf("item-%d", i+1)
		cited := 0
		for _, name := range ra.Items[i].Tests {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			cited++
			if problem := testEvidence(name, tests); problem != "" {
				rs = append(rs, PlanGateReason{Code: RGRegressionHigh, Ref: ref, Detail: problem})
			}
		}
		if cited == 0 {
			rs = append(rs, PlanGateReason{Code: RGRegressionHigh, Ref: ref, Detail: "no test cited"})
		}
	}
	return rs
}

// testEvidence resolves one cited test against the round's results and
// says what is wrong with it ("" when it passed). Entries without a test
// name (package-level results) are not test identities.
func testEvidence(name string, tests []gates.TestCase) string {
	statuses := map[string][]gates.TestStatus{}
	for _, t := range tests {
		if t.Name == "" {
			continue
		}
		if id := t.ID(); name == id || name == t.Name {
			statuses[id] = append(statuses[id], t.Status)
		}
	}
	switch len(statuses) {
	case 0:
		return fmt.Sprintf("cited test %s is not in the gate results", name)
	case 1:
	default:
		ids := make([]string, 0, len(statuses))
		for id := range statuses {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Sprintf("cited test %s is ambiguous: %s", name, strings.Join(ids, ", "))
	}
	for id, sts := range statuses {
		for _, s := range sts {
			if s != gates.TestPass {
				return fmt.Sprintf("cited test %s: %s", id, s)
			}
		}
	}
	return ""
}
