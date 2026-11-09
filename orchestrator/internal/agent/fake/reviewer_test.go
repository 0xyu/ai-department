package fake

import (
	"context"
	"testing"

	"github.com/0xyu/aco/internal/agent"
)

func TestFakeReviewer(t *testing.T) {
	reqs, acs := []string{"REQ-001"}, []string{"AC-01.1"}
	r := &Reviewer{
		PlanSteps: []ReviewStep{{Message: PlanReviewPass(reqs)}, {Message: `{"verdict":"pass"}`}},
		CodeSteps: []ReviewStep{{Message: CodeReviewApprove(acs, "F-01-001")}, {Err: &agent.Error{Class: agent.ClassSubscriptionLimit}}},
	}
	ctx := context.Background()
	pr, err := r.ReviewPlan(ctx, agent.PlanReviewRequest{ReqIDs: reqs})
	if err != nil || pr.Review.Verdict != "pass" || len(pr.Inconsistent) != 0 {
		t.Fatalf("plan: %v %+v", err, pr)
	}
	if _, err := r.ReviewPlan(ctx, agent.PlanReviewRequest{ReqIDs: reqs}); agent.ClassOf(err) != agent.ClassSchemaInvalid {
		t.Errorf("invalid scripted message: %v", err)
	}
	if _, err := r.ReviewPlan(ctx, agent.PlanReviewRequest{}); err != ErrScriptExhausted {
		t.Errorf("exhausted: %v", err)
	}
	cr, err := r.ReviewCode(ctx, agent.CodeReviewRequest{ACIDs: acs, Ledger: []agent.LedgerEntry{{ID: "F-01-001"}}})
	if err != nil || cr.Review.Verdict != "approve" || len(r.CodeReviews) != 1 {
		t.Fatalf("code: %v", err)
	}
	if _, err := r.ReviewCode(ctx, agent.CodeReviewRequest{ACIDs: acs}); agent.ClassOf(err) != agent.ClassSubscriptionLimit {
		t.Errorf("scripted error: %v", err)
	}
}
