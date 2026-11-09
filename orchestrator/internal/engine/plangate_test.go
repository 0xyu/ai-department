package engine

import (
	"reflect"
	"testing"

	"github.com/0xyu/aco/internal/agent"
)

func sound() []agent.DimensionRating {
	var out []agent.DimensionRating
	for _, d := range agent.PlanDimensions {
		out = append(out, agent.DimensionRating{Dimension: d, Rating: "sound"})
	}
	return out
}

func covered(ids ...string) []agent.ReqCoverage {
	var out []agent.ReqCoverage
	for _, id := range ids {
		out = append(out, agent.ReqCoverage{ReqID: id, Status: "covered"})
	}
	return out
}

func TestDecidePlanGate(t *testing.T) {
	base := func() PlanGateInput {
		return PlanGateInput{ChecksPassed: true, Reviewed: true, ReqIDs: []string{"REQ-001", "REQ-002"},
			Coverage: covered("REQ-001", "REQ-002"), Ratings: sound()}
	}
	tests := []struct {
		name string
		mod  func(*PlanGateInput)
		want []PlanGateReason
	}{
		{"all good", func(*PlanGateInput) {}, nil},
		{"checks failed", func(in *PlanGateInput) { in.ChecksPassed = false }, []PlanGateReason{{Code: GateChecksFailed}}},
		{"not reviewed", func(in *PlanGateInput) { in.ChecksPassed, in.Reviewed = false, false },
			[]PlanGateReason{{Code: GateChecksFailed}, {Code: GateNotReviewed}}},
		{"coverage partial", func(in *PlanGateInput) { in.Coverage[1].Status = "partial" },
			[]PlanGateReason{{Code: GateRequirementStatus, Ref: "REQ-002", Detail: "partial"}}},
		{"coverage missing", func(in *PlanGateInput) { in.Coverage[0].Status = "missing" },
			[]PlanGateReason{{Code: GateRequirementStatus, Ref: "REQ-001", Detail: "missing"}}},
		{"requirement absent from review", func(in *PlanGateInput) { in.Coverage = in.Coverage[:1] },
			[]PlanGateReason{{Code: GateRequirementMissing, Ref: "REQ-002"}}},
		{"unsound dimension", func(in *PlanGateInput) { in.Ratings[3].Rating = "unsound" },
			[]PlanGateReason{{Code: GateDimensionUnsound, Ref: in3()}}},
		{"concerns are fine", func(in *PlanGateInput) { in.Ratings[0].Rating = "concerns" }, nil},
		{"open blocker", func(in *PlanGateInput) {
			in.Findings = []PlanGateFinding{{ID: "PF-001", Severity: "blocker", Open: true}}
		}, []PlanGateReason{{Code: GateOpenFinding, Ref: "PF-001", Detail: "blocker"}}},
		{"open major", func(in *PlanGateInput) {
			in.Findings = []PlanGateFinding{{ID: "PF-002", Severity: "major", Open: true}}
		}, []PlanGateReason{{Code: GateOpenFinding, Ref: "PF-002", Detail: "major"}}},
		{"open minor does not block", func(in *PlanGateInput) {
			in.Findings = []PlanGateFinding{{ID: "PF-003", Severity: "minor", Open: true}}
		}, nil},
		{"closed blocker does not block", func(in *PlanGateInput) {
			in.Findings = []PlanGateFinding{{ID: "PF-004", Severity: "blocker", Open: false}}
		}, nil},
		{"previous finding without status", func(in *PlanGateInput) {
			in.PreviousIDs = []string{"PF-001", "PF-002"}
			in.Statuses = []agent.PreviousStatus{{ID: "PF-001", Status: "resolved"}}
		}, []PlanGateReason{{Code: GatePreviousNoStatus, Ref: "PF-002"}}},
		{"everything wrong, sorted", func(in *PlanGateInput) {
			in.ChecksPassed = false
			in.Coverage[1].Status = "missing"
			in.Ratings[0].Rating = "unsound"
			in.Findings = []PlanGateFinding{{ID: "PF-009", Severity: "major", Open: true}}
		}, []PlanGateReason{{Code: GateChecksFailed}, {Code: GateDimensionUnsound, Ref: "architecture"},
			{Code: GateOpenFinding, Ref: "PF-009", Detail: "major"}, {Code: GateRequirementStatus, Ref: "REQ-002", Detail: "missing"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mod(&in)
			got := DecidePlanGate(in)
			want := tt.want
			if want == nil {
				want = []PlanGateReason{}
			}
			if !reflect.DeepEqual(got.Reasons, want) || got.Pass != (len(want) == 0) {
				t.Fatalf("got %+v, want reasons %+v", got, want)
			}
			// Pure: the same input gives the same decision.
			if again := DecidePlanGate(in); !reflect.DeepEqual(again, got) {
				t.Fatal("decision is not deterministic")
			}
		})
	}
}

func in3() string { return agent.PlanDimensions[3] }

// The verdict is never consulted: a "pass" verdict with a missing
// requirement fails, a "revise" verdict with clean details passes.
func TestPlanGateIgnoresVerdict(t *testing.T) {
	in := PlanGateInput{ChecksPassed: true, Reviewed: true, ReqIDs: []string{"REQ-001"},
		Coverage: []agent.ReqCoverage{{ReqID: "REQ-001", Status: "missing"}}, Ratings: sound()}
	if DecidePlanGate(in).Pass {
		t.Fatal("missing coverage passed")
	}
	in.Coverage[0].Status = "covered"
	if !DecidePlanGate(in).Pass {
		t.Fatal("clean details failed")
	}
}

func TestPlanLedger(t *testing.T) {
	l := &PlanLedger{}
	r1 := &agent.PlanReview{NewFindings: []agent.PlanReviewFinding{
		{Severity: "blocker", Dimension: "coverage", Location: "phases/P01.md", Title: "REQ-002 uncovered"},
		{Severity: "major", Dimension: "dependencies", Location: "phases/P02.md", Title: "forward dependency"},
		{Severity: "minor", Dimension: "risks", Location: "IMPLEMENTATION_PLAN.md", Title: "risk list thin"},
	}}
	ch := l.Apply(1, r1)
	if len(ch) != 3 || ch[0] != (LedgerChange{"PF-001", "opened"}) || ch[2].ID != "PF-003" {
		t.Fatalf("changes %v", ch)
	}
	if len(l.Previous()) != 3 {
		t.Fatal("all new findings are open")
	}
	disputed := l.SetResponses([]agent.FindingResponse{
		{ID: "PF-001", Response: agent.ResponseFixed, Detail: "added AC"},
		{ID: "PF-002", Response: agent.ResponseFixed, Detail: "reordered"},
		{ID: "PF-003", Response: agent.ResponseDisputed, Detail: "enough"},
		{ID: "PF-404", Response: agent.ResponseFixed, Detail: "unknown, ignored"},
	})
	if !reflect.DeepEqual(disputed, []string{"PF-003"}) || l.Get("PF-003").DisputeCount != 1 {
		t.Fatalf("disputed %v", disputed)
	}
	if prev := l.Previous(); prev[0].Response == nil || prev[0].Response.Detail != "added AC" {
		t.Fatal("previous records lack the planner's answer")
	}
	r2 := &agent.PlanReview{PreviousFindings: []agent.PreviousStatus{
		{ID: "PF-001", Status: "resolved"}, {ID: "PF-002", Status: "unresolved"}, {ID: "PF-003", Status: "dispute_accepted"},
	}}
	ch = l.Apply(2, r2)
	want := []LedgerChange{{"PF-001", "resolved"}, {"PF-002", "reopened"}, {"PF-003", "dispute_accepted"}}
	if !reflect.DeepEqual(ch, want) {
		t.Fatalf("changes %v", ch)
	}
	if e := l.Get("PF-001"); e.Open() || e.ClosedRound != 2 {
		t.Fatal("PF-001 should be closed in round 2")
	}
	if e := l.Get("PF-002"); !e.Open() || e.ReopenCount != 1 {
		t.Fatal("PF-002 should be open and reopened once")
	}
	open := l.OpenEntries()
	if len(open) != 1 || open[0].Finding.ID != "PF-002" {
		t.Fatalf("open %v", open)
	}
	gf := l.GateFindings()
	if len(gf) != 3 || !gf[1].Open || gf[0].Open {
		t.Fatalf("gate findings %v", gf)
	}
	// Dispute rejected stays open; answers to closed findings are ignored.
	l.SetResponses([]agent.FindingResponse{{ID: "PF-002", Response: agent.ResponseDisputed, Detail: "x"}, {ID: "PF-001", Response: agent.ResponseDisputed, Detail: "y"}})
	l.Apply(3, &agent.PlanReview{PreviousFindings: []agent.PreviousStatus{{ID: "PF-002", Status: "dispute_rejected"}}})
	if e := l.Get("PF-002"); !e.Open() || e.Status != FindingDisputeRejected {
		t.Fatal("rejected dispute must stay open")
	}
	if l.Get("PF-001").DisputeCount != 0 {
		t.Fatal("closed finding took a response")
	}
	if l.Get("PF-001").Fingerprint() == l.Get("PF-002").Fingerprint() {
		t.Fatal("fingerprints collide")
	}
}
