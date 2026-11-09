package fake

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/0xyu/aco/internal/agent"
)

// ReviewStep is one scripted reviewer answer: either the final message
// Codex would write with -o (Message, validated exactly like the real
// adapter does), or an error.
type ReviewStep struct {
	Message string
	Err     error
}

// Reviewer is a scripted agent.Reviewer. Plan and code reviews take steps
// from their own scripts. A Message that fails the review schema yields a
// ClassSchemaInvalid error, as the adapter does once its retries are
// exhausted.
type Reviewer struct {
	mu          sync.Mutex
	PlanSteps   []ReviewStep
	CodeSteps   []ReviewStep
	nextPlan    int
	nextCode    int
	PlanReviews []agent.PlanReviewRequest
	CodeReviews []agent.CodeReviewRequest
}

var _ agent.Reviewer = (*Reviewer)(nil)

func take(ctx context.Context, steps []ReviewStep, next *int) (ReviewStep, error) {
	if err := ctx.Err(); err != nil {
		return ReviewStep{}, &agent.Error{Agent: "fake", Class: agent.ClassCancelled, Err: err}
	}
	if *next >= len(steps) {
		return ReviewStep{}, ErrScriptExhausted
	}
	s := steps[*next]
	*next++
	return s, s.Err
}

func calls(msg string) agent.ReviewCalls {
	inv := agent.Invocation{AuthMode: "subscription", Model: "fake-reviewer", ResultText: msg}
	return agent.ReviewCalls{Calls: []agent.Invocation{inv}}
}

// ReviewPlan implements agent.Reviewer.
func (r *Reviewer) ReviewPlan(ctx context.Context, req agent.PlanReviewRequest) (*agent.PlanReviewResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.PlanReviews = append(r.PlanReviews, req)
	s, err := take(ctx, r.PlanSteps, &r.nextPlan)
	if err != nil {
		return nil, err
	}
	var prev []string
	for _, p := range req.Previous {
		prev = append(prev, p.Finding.ID)
	}
	rev, probs := agent.ParsePlanReview([]byte(s.Message), req.ReqIDs, prev)
	if rev == nil {
		return nil, &agent.Error{Agent: "fake", Class: agent.ClassSchemaInvalid, Message: strings.Join(probs, "; ")}
	}
	c := calls(s.Message)
	return &agent.PlanReviewResult{Invocation: c.Calls[0], ReviewCalls: c, Raw: json.RawMessage(s.Message), Review: rev, Inconsistent: rev.Inconsistencies()}, nil
}

// ReviewCode implements agent.Reviewer.
func (r *Reviewer) ReviewCode(ctx context.Context, req agent.CodeReviewRequest) (*agent.CodeReviewResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.CodeReviews = append(r.CodeReviews, req)
	s, err := take(ctx, r.CodeSteps, &r.nextCode)
	if err != nil {
		return nil, err
	}
	var prev []string
	for _, l := range req.Ledger {
		prev = append(prev, l.ID)
	}
	rev, probs := agent.ParseCodeReview([]byte(s.Message), req.ACIDs, prev)
	if rev == nil {
		return nil, &agent.Error{Agent: "fake", Class: agent.ClassSchemaInvalid, Message: strings.Join(probs, "; ")}
	}
	c := calls(s.Message)
	return &agent.CodeReviewResult{Invocation: c.Calls[0], ReviewCalls: c, Raw: json.RawMessage(s.Message), Review: rev, Inconsistent: rev.Inconsistencies()}, nil
}

// PlanReviewPass builds a passing plan review message: every dimension
// sound, every requirement covered, every earlier finding resolved.
func PlanReviewPass(reqIDs []string, previousIDs ...string) string {
	r := agent.PlanReview{Verdict: "pass", Summary: "fake review: the plan is sound",
		DimensionRatings: []agent.DimensionRating{}, RequirementCoverage: []agent.ReqCoverage{},
		PreviousFindings: []agent.PreviousStatus{}, NewFindings: []agent.PlanReviewFinding{}}
	for _, d := range agent.PlanDimensions {
		r.DimensionRatings = append(r.DimensionRatings, agent.DimensionRating{Dimension: d, Rating: "sound", Notes: "ok"})
	}
	for _, id := range reqIDs {
		r.RequirementCoverage = append(r.RequirementCoverage, agent.ReqCoverage{ReqID: id, Status: "covered", Phases: []string{"P01"}, Notes: "ok"})
	}
	for _, id := range previousIDs {
		r.PreviousFindings = append(r.PreviousFindings, agent.PreviousStatus{ID: id, Status: "resolved", Note: "fixed"})
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// CodeReviewApprove builds an approving code review message: every AC
// met, every earlier finding resolved.
func CodeReviewApprove(acIDs []string, previousIDs ...string) string {
	r := agent.CodeReview{Verdict: "approve", Summary: "fake review: looks correct",
		PlanConformance:      agent.StatusNotes{Status: "conforms", Notes: "ok"},
		AcceptanceCriteria:   []agent.ACAssessment{},
		SecurityAssessment:   agent.StatusNotes{Status: "no_issues", Notes: "ok"},
		RegressionAssessment: agent.RiskNotes{Risk: "low", Notes: "ok"},
		TestAssessment:       agent.TestAssessment{Adequate: true, Notes: "ok"},
		PreviousFindings:     []agent.PreviousStatus{}, NewFindings: []agent.CodeReviewFinding{}}
	for _, id := range acIDs {
		r.AcceptanceCriteria = append(r.AcceptanceCriteria, agent.ACAssessment{ID: id, Status: "met", Evidence: "fake test"})
	}
	for _, id := range previousIDs {
		r.PreviousFindings = append(r.PreviousFindings, agent.PreviousStatus{ID: id, Status: "resolved", Note: "fixed"})
	}
	b, _ := json.Marshal(r)
	return string(b)
}
