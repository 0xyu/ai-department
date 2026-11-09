package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/state"
)

func review(mut func(r *agent.CodeReview)) *agent.CodeReview {
	r := &agent.CodeReview{Verdict: "approve", PlanConformance: agent.StatusNotes{Status: "conforms"},
		SecurityAssessment: agent.StatusNotes{Status: "no_issues"}, RegressionAssessment: agent.RiskNotes{Risk: "low"},
		AcceptanceCriteria: []agent.ACAssessment{{ID: "AC-01.1", Status: "met"}, {ID: "AC-01.2", Status: "met"}}}
	if mut != nil {
		mut(r)
	}
	return r
}

func codes(d PlanGateDecision) string {
	var out []string
	for _, r := range d.Reasons {
		s := r.Code
		if r.Ref != "" {
			s += ":" + r.Ref
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

// Review Decision Gate (§11.7): exhaustive table.
func TestDecideReviewGate(t *testing.T) {
	open := func(id, sev, cat, class string) ReviewGateFinding {
		return ReviewGateFinding{ID: id, Severity: sev, Category: cat, Class: class, Open: true}
	}
	cases := []struct {
		name string
		in   ReviewGateInput
		want string // "" = PASS
	}{
		{"pass", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1}, ""},
		{"approve with failing gate", ReviewGateInput{GatesGreen: false, FailedGates: []string{"unit"}, Review: review(nil), ACIDs: ac1},
			"quality_gate_failed:unit"},
		{"no review", ReviewGateInput{GatesGreen: true, ACIDs: ac1}, "no_review"},
		{"deviates", ReviewGateInput{GatesGreen: true, Review: review(func(r *agent.CodeReview) { r.PlanConformance.Status = "deviates" }), ACIDs: ac1},
			"plan_deviates"},
		{"ac partial", ReviewGateInput{GatesGreen: true, Review: review(func(r *agent.CodeReview) { r.AcceptanceCriteria[1].Status = "partial" }), ACIDs: ac1},
			"ac_not_met:AC-01.2"},
		{"ac missing", ReviewGateInput{GatesGreen: true, Review: review(func(r *agent.CodeReview) { r.AcceptanceCriteria = r.AcceptanceCriteria[:1] }), ACIDs: ac1},
			"ac_not_assessed:AC-01.2"},
		{"not verifiable refused", ReviewGateInput{GatesGreen: true, Review: review(func(r *agent.CodeReview) { r.AcceptanceCriteria[0].Status = "not_verifiable" }), ACIDs: ac1},
			"ac_not_met:AC-01.1"},
		{"not verifiable allowed", ReviewGateInput{GatesGreen: true, AllowUnverifiable: true,
			Review: review(func(r *agent.CodeReview) { r.AcceptanceCriteria[0].Status = "not_verifiable" }), ACIDs: ac1}, ""},
		{"high risk", ReviewGateInput{GatesGreen: true, Review: review(func(r *agent.CodeReview) { r.RegressionAssessment.Risk = "high" }), ACIDs: ac1},
			"regression_risk_high"},
		{"high risk covered", ReviewGateInput{GatesGreen: true, Tests: []gates.TestCase{{Package: "p", Name: "TestA", Status: gates.TestPass}},
			Review: review(func(r *agent.CodeReview) {
				r.RegressionAssessment = agent.RiskNotes{Risk: "high", Items: []agent.RegressionItem{{Risk: "high", Description: "x", Tests: []string{"p.TestA"}}}}
			}), ACIDs: ac1}, ""},
		{"open blocker", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-001", "blocker", "correctness", ClassFixable)}}, "open_finding:F-01-001"},
		{"open security major", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-002", "major", "security", ClassFixable)}}, "open_security_finding:F-01-002"},
		{"out of scope blocker", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-003", "blocker", "out_of_scope", ClassFixable)}}, ""},
		// Deferral never clears an in-scope blocker or major.
		{"deferred major", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-004", "major", "tests", ClassDeferred)}}, "open_finding:F-01-004"},
		{"deferred and closed major", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{{ID: "F-01-004", Severity: "major", Category: "tests", Class: ClassDeferred, Deferred: true}}},
			"open_finding:F-01-004"},
		{"deferred security blocker", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{{ID: "F-01-007", Severity: "blocker", Category: "security", Class: ClassDeferred, Deferred: true}}},
			"open_security_finding:F-01-007"},
		{"deferred minor", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{{ID: "F-01-008", Severity: "minor", Category: "tests", Class: ClassDeferred, Deferred: true}}}, ""},
		{"deferred out of scope major", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{{ID: "F-01-009", Severity: "major", Category: "out_of_scope", Class: ClassDeferred, Deferred: true}}}, ""},
		// A blocking triage class fails the gate at any severity.
		{"minor unsafe", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-010", "minor", "correctness", ClassUnsafe)}}, "open_finding:F-01-010"},
		{"nit spec conflict", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-011", "nit", "correctness", ClassSpecConflict)}}, "open_finding:F-01-011"},
		{"minor environment", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-012", "minor", "tests", ClassEnvironment)}}, "open_finding:F-01-012"},
		{"open minor", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{open("F-01-005", "minor", "tests", ClassFixable)}}, ""},
		{"closed blocker", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1,
			Findings: []ReviewGateFinding{{ID: "F-01-006", Severity: "blocker", Category: "correctness"}}}, ""},
		{"previous without status", ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1, PreviousIDs: []string{"F-01-001"}},
			"previous_finding_without_status:F-01-001"},
		{"previous with status", ReviewGateInput{GatesGreen: true, ACIDs: ac1, PreviousIDs: []string{"F-01-001"},
			Review: review(func(r *agent.CodeReview) {
				r.PreviousFindings = []agent.PreviousStatus{{ID: "F-01-001", Status: "resolved"}}
			})}, ""},
		{"several reasons sorted", ReviewGateInput{GatesGreen: false, Review: review(func(r *agent.CodeReview) {
			r.PlanConformance.Status = "deviates"
			r.AcceptanceCriteria[0].Status = "not_met"
		}), ACIDs: ac1}, "ac_not_met:AC-01.1,plan_deviates,quality_gate_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideReviewGate(tc.in)
			if d.Pass != (tc.want == "") || codes(d) != tc.want {
				t.Fatalf("pass=%v reasons %q, want %q", d.Pass, codes(d), tc.want)
			}
			// Verdict is not an input: the same details give the same decision.
			if tc.in.Review != nil {
				tc.in.Review.Verdict = "request_changes"
				if d2 := DecideReviewGate(tc.in); d2.Pass != d.Pass || codes(d2) != codes(d) {
					t.Fatal("the verdict changed the decision")
				}
			}
		})
	}
}

// High regression risk (§11.7): every high-risk item needs passing test
// evidence by exact test identity in the accepted round.
func TestRegressionEvidence(t *testing.T) {
	tests := []gates.TestCase{
		{Package: "example.com/toy/calc", Name: "TestAdd", Status: gates.TestPass},
		{Package: "example.com/toy/calc", Name: "TestAddOverflow", Status: gates.TestPass},
		{Package: "example.com/toy/calc", Name: "TestSub", Status: gates.TestFail},
		{Package: "example.com/toy/calc", Name: "TestMul", Status: gates.TestSkip},
		{Package: "example.com/toy/calc", Name: "TestDiv", Status: gates.TestPass},
		{Package: "example.com/toy/other", Name: "TestDiv", Status: gates.TestPass},
		{Package: "example.com/toy/calc", Name: "TestFlaky", Status: gates.TestPass},
		{Package: "example.com/toy/calc", Name: "TestFlaky", Status: gates.TestFail}, // same identity in a second gate
		{Package: "example.com/toy/calc", Status: gates.TestPass},                    // package result: not a test
	}
	item := func(risk string, ts ...string) agent.RegressionItem {
		return agent.RegressionItem{Risk: risk, Description: "risky change", Tests: ts}
	}
	cases := []struct {
		name  string
		risk  string
		items []agent.RegressionItem
		notes string
		want  string // "" = covered
	}{
		{"low risk needs nothing", "low", nil, "", ""},
		{"medium item ignored", "medium", []agent.RegressionItem{item("medium")}, "", ""},
		{"complete coverage", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestAdd"),
			item("high", "example.com/toy/calc.TestAddOverflow", "TestAdd"), item("low")}, "", ""},
		{"two high items, one covered", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestAdd"), item("high")},
			"", "regression_risk_high:item-2"},
		{"substring collision", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestAddOver")},
			"", "regression_risk_high:item-1"},
		{"bare name prefix is no match", "high", []agent.RegressionItem{item("high", "TestAddOver")}, "", "regression_risk_high:item-1"},
		{"notes are not evidence", "high", []agent.RegressionItem{item("high")}, "covered by TestAdd and TestAddOverflow",
			"regression_risk_high:item-1"},
		{"high without items", "high", nil, "TestAdd passed", "regression_risk_high"},
		{"high item under medium risk", "medium", []agent.RegressionItem{item("high", "example.com/toy/calc.TestSub")}, "",
			"regression_risk_high:item-1"},
		{"failing test", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestAdd", "example.com/toy/calc.TestSub")},
			"", "regression_risk_high:item-1"},
		{"skipped test", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestMul")}, "", "regression_risk_high:item-1"},
		{"ambiguous bare name", "high", []agent.RegressionItem{item("high", "TestDiv")}, "", "regression_risk_high:item-1"},
		{"full identity resolves the ambiguity", "high", []agent.RegressionItem{item("high", "example.com/toy/other.TestDiv")}, "", ""},
		{"same identity failing elsewhere", "high", []agent.RegressionItem{item("high", "example.com/toy/calc.TestFlaky")}, "",
			"regression_risk_high:item-1"},
		{"package is not a test", "high", []agent.RegressionItem{item("high", "example.com/toy/calc")}, "", "regression_risk_high:item-1"},
		{"blank citation", "high", []agent.RegressionItem{item("high", " ")}, "", "regression_risk_high:item-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideReviewGate(ReviewGateInput{GatesGreen: true, ACIDs: ac1, Tests: tests, Review: review(func(r *agent.CodeReview) {
				r.RegressionAssessment = agent.RiskNotes{Risk: tc.risk, Notes: tc.notes, Items: tc.items}
			})})
			if d.Pass != (tc.want == "") || codes(d) != tc.want {
				t.Fatalf("pass=%v reasons %q (%+v), want %q", d.Pass, codes(d), d.Reasons, tc.want)
			}
		})
	}
	// Without an accepted round there is no evidence at all.
	d := DecideReviewGate(ReviewGateInput{GatesGreen: true, ACIDs: ac1, Review: review(func(r *agent.CodeReview) {
		r.RegressionAssessment = agent.RiskNotes{Risk: "high", Items: []agent.RegressionItem{item("high", "example.com/toy/calc.TestAdd")}}
	})})
	if d.Pass {
		t.Fatal("high risk covered without gate results")
	}
}

func rules() TriageRules {
	return TriageRules{ProtectedPaths: []string{".aco/**", "AGENTS.md", ".github/**", ".env*"},
		MatchPath: func(ps []string, p string) bool { return policy.MatchAny(ps, p, true) }}
}

// Triage override rules (§11.7): exhaustive table.
func TestClassifyFinding(t *testing.T) {
	base := func(mut func(e *CodeLedgerEntry)) *CodeLedgerEntry {
		e := &CodeLedgerEntry{ID: "F-01-001", Status: CodeOpen, Proposed: "fixable",
			Finding: agent.CodeReviewFinding{Severity: "major", Category: "correctness", ResolutionClass: "fixable", File: "calc/calc.go",
				RequiredChange: "Handle overflow."}}
		if mut != nil {
			mut(e)
		}
		return e
	}
	cases := []struct {
		name string
		e    *CodeLedgerEntry
		want string
	}{
		{"default fixable", base(nil), ClassFixable},
		{"reviewer spec conflict", base(func(e *CodeLedgerEntry) { e.Proposed = ClassSpecConflict }), ClassSpecConflict},
		{"reviewer unsafe", base(func(e *CodeLedgerEntry) { e.Proposed = ClassUnsafe }), ClassUnsafe},
		{"reviewer environment", base(func(e *CodeLedgerEntry) { e.Proposed = ClassEnvironment }), ClassEnvironment},
		// A reviewer-proposed deferral of an in-scope blocker/major is overridden.
		{"reviewer deferred major", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred }), ClassFixable},
		{"reviewer deferred blocker", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred; e.Finding.Severity = "blocker" }), ClassFixable},
		{"reviewer deferred security major", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred; e.Finding.Category = "security" }), ClassFixable},
		{"reviewer deferred minor", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred; e.Finding.Severity = "minor" }), ClassDeferred},
		{"reviewer deferred out of scope", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred; e.Finding.Category = "out_of_scope" }), ClassDeferred},
		{"reviewer deferred late downgrade", base(func(e *CodeLedgerEntry) { e.Proposed = ClassDeferred; e.Downgraded = true }), ClassDeferred},
		{"out of scope", base(func(e *CodeLedgerEntry) { e.Finding.Category = "out_of_scope"; e.Finding.Severity = "blocker" }), ClassDeferred},
		{"minor", base(func(e *CodeLedgerEntry) { e.Finding.Severity = "minor" }), ClassDeferred},
		{"nit unsafe stays deferred", base(func(e *CodeLedgerEntry) { e.Finding.Severity = "nit"; e.Proposed = ClassUnsafe }), ClassDeferred},
		{"downgraded", base(func(e *CodeLedgerEntry) { e.Downgraded = true }), ClassDeferred},
		{"file protected", base(func(e *CodeLedgerEntry) { e.Finding.File = ".github/workflows/ci.yml" }), ClassUnsafe},
		{"required change protected", base(func(e *CodeLedgerEntry) { e.Finding.RequiredChange = "Edit `.aco/plan/phases/P01.md` to drop AC-01.2." }), ClassUnsafe},
		{"required change env file", base(func(e *CodeLedgerEntry) { e.Finding.RequiredChange = "Put the key in .env.local." }), ClassUnsafe},
		{"required change plain text", base(func(e *CodeLedgerEntry) { e.Finding.RequiredChange = "Update the AGENTS section of the README." }), ClassFixable},
		{"security after 2 rounds", base(func(e *CodeLedgerEntry) { e.Finding.Category = "security"; e.OpenRounds = 2 }), ClassUnsafe},
		{"security after 1 round", base(func(e *CodeLedgerEntry) { e.Finding.Category = "security"; e.OpenRounds = 1 }), ClassFixable},
		{"confirmed within spec", base(func(e *CodeLedgerEntry) { e.Confirmed = agent.ResponseCannotFixWithinSpec }), ClassSpecConflict},
		{"confirmed safely", base(func(e *CodeLedgerEntry) { e.Confirmed = agent.ResponseCannotFixSafely }), ClassUnsafe},
		{"confirmed beats minor", base(func(e *CodeLedgerEntry) { e.Finding.Severity = "minor"; e.Confirmed = agent.ResponseCannotFixSafely }), ClassUnsafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := ClassifyFinding(tc.e, rules()); got != tc.want {
				t.Fatalf("class %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTriagePriorityAndDeferral(t *testing.T) {
	l := &CodeLedger{Phase: 1, Attempt: 1}
	l.Apply(1, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{
		{Severity: "major", Category: "correctness", ResolutionClass: "environment", File: "a.go", Title: "a"},
		{Severity: "major", Category: "correctness", ResolutionClass: "unsafe", File: "b.go", Title: "b"},
		{Severity: "minor", Category: "tests", ResolutionClass: "fixable", File: "c.go", Title: "c"},
		{Severity: "blocker", Category: "correctness", ResolutionClass: "fixable", File: "d.go", Title: "d"},
	}}, nil)
	tr := l.Triage(1, rules())
	if tr.Blocked != ClassUnsafe || len(tr.BlockedIDs) != 1 || tr.BlockedIDs[0] != "F-01-002" {
		t.Fatalf("blocked %+v", tr)
	}
	if len(tr.Deferred) != 1 || l.Get("F-01-003").Status != CodeDeferred || len(tr.Fixable) != 1 || tr.Fixable[0] != "F-01-004" {
		t.Fatalf("triage %+v", tr)
	}
}

// Scope-creep control (§11.7).
func TestScopeCreep(t *testing.T) {
	changed := ChangedLines{"calc/calc.go": {10, 11}, "calc/del.go": nil}
	f := func(sev, cat, file string, line int) agent.CodeReviewFinding {
		return agent.CodeReviewFinding{Severity: sev, Category: cat, File: file, LineStart: line, LineEnd: line}
	}
	cases := []struct {
		name   string
		f      agent.CodeReviewFinding
		round  int
		cutoff int
		want   bool
	}{
		{"before cutoff", f("blocker", "correctness", "x.go", 1), 3, 3, false},
		{"after cutoff untouched", f("blocker", "correctness", "x.go", 1), 4, 3, true},
		{"after cutoff changed line", f("major", "correctness", "calc/calc.go", 11), 4, 3, false},
		{"after cutoff other line", f("major", "correctness", "calc/calc.go", 30), 4, 3, true},
		{"changed file without line", f("major", "correctness", "calc/del.go", 0), 4, 3, false},
		{"security exempt", f("blocker", "security", "x.go", 1), 4, 3, false},
		{"regression exempt", f("major", "regression", "x.go", 1), 4, 3, false},
		{"minor not blocking", f("minor", "tests", "x.go", 1), 4, 3, false},
		{"disabled", f("blocker", "correctness", "x.go", 1), 9, 0, false},
	}
	for _, tc := range cases {
		if got := ScopeCreep(tc.f, tc.round, tc.cutoff, changed); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCodeLedgerApply(t *testing.T) {
	l := &CodeLedger{Phase: 2, Attempt: 1}
	f := agent.CodeReviewFinding{Severity: "major", Category: "correctness", File: "a.go", Title: "Off by one"}
	if ch := l.Apply(1, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{f}}, nil); len(ch) != 1 || ch[0].ID != "F-02-001" {
		t.Fatalf("changes %+v", ch)
	}
	l.SetResponses([]agent.FindingResponse{{ID: "F-02-001", Response: agent.ResponseFixed, Detail: "x"}})
	l.Apply(2, 3, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "unresolved"}}}, nil)
	e := l.Get("F-02-001")
	if e.ReopenCount != 1 || !e.Open() || e.OpenRounds != 1 {
		t.Fatalf("reopen after fixed: %+v", e)
	}
	l.Apply(3, 3, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "resolved"}}}, nil)
	if e.Status != CodeResolved || e.ClosedRound != 3 {
		t.Fatalf("resolved %+v", e)
	}
	// A closed finding confirmed again keeps its closing round.
	l.Apply(4, 3, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "resolved"}}}, nil)
	if e.ClosedRound != 3 {
		t.Fatalf("closed round moved: %+v", e)
	}
	// The same finding reported as new reopens it (oscillation).
	ch := l.Apply(5, 9, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{f}}, nil)
	if len(ch) != 1 || ch[0].Kind != "reopened" || e.ReopenCount != 2 || len(l.Entries) != 1 {
		t.Fatalf("re-reported: %+v %+v", ch, e)
	}
	// Disputes: rejected twice is a deadlock.
	l.SetResponses([]agent.FindingResponse{{ID: "F-02-001", Response: agent.ResponseDisputed, Detail: "wrong"}})
	l.Apply(6, 9, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "dispute_rejected"}}}, nil)
	l.SetResponses([]agent.FindingResponse{{ID: "F-02-001", Response: agent.ResponseDisputed, Detail: "still wrong"}})
	l.Apply(7, 9, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "dispute_rejected"}}}, nil)
	if reopen, rej := l.MaxCounts(); rej != 2 || reopen != 2 || e.DisputeCount != 2 {
		t.Fatalf("counts reopen %d rejections %d disputes %d", reopen, rej, e.DisputeCount)
	}
	// An accepted dispute closes it; an accepted cannot_fix confirms it.
	l.SetResponses([]agent.FindingResponse{{ID: "F-02-001", Response: agent.ResponseCannotFixSafely, Detail: "needs .env"}})
	l.Apply(8, 9, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-02-001", Status: "dispute_accepted"}}}, nil)
	if e.Confirmed != agent.ResponseCannotFixSafely || !e.Open() {
		t.Fatalf("confirmed %+v", e)
	}
	// A restart starts a new attempt: earlier entries are history, IDs go on.
	l.Attempt = 2
	if len(l.Current()) != 0 || len(l.ForReview()) != 0 {
		t.Fatal("attempt 1 entries leak into attempt 2")
	}
	l.Apply(1, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{f}}, nil)
	if l.Entries[1].ID != "F-02-002" || l.Entries[1].Attempt != 2 {
		t.Fatalf("attempt 2 entry %+v", l.Entries[1])
	}
}

func TestFixOrder(t *testing.T) {
	mk := func(id, sev, cat string) *CodeLedgerEntry {
		return &CodeLedgerEntry{ID: id, Finding: agent.CodeReviewFinding{Severity: sev, Category: cat}}
	}
	got := FixOrder([]*CodeLedgerEntry{mk("F-5", "major", "tests"), mk("F-4", "major", "regression"), mk("F-3", "major", "security"),
		mk("F-2", "blocker", "tests"), mk("F-1", "blocker", "security")})
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "F-1,F-2,F-3,F-4,F-5" {
		t.Fatalf("order %v", ids)
	}
}

// Commit Gate (§11.8): exhaustive table.
func TestDecideCommitGate(t *testing.T) {
	ok := func(mut func(in *CommitGateInput)) CommitGateInput {
		in := CommitGateInput{ReviewedTree: "t1", ReviewedPaths: []string{"a.go", "b.go"}, CurrentTree: "t1",
			CurrentPaths: []string{"b.go", "a.go"}, GateTree: "t1", GatesGreen: true, ReviewPassed: true, ReviewTree: "t1", PlanIntact: true}
		if mut != nil {
			mut(&in)
		}
		return in
	}
	cases := []struct {
		name   string
		in     CommitGateInput
		action string
		class  state.Class
		reason string
	}{
		{"commit", ok(nil), CommitProceed, "", ""},
		{"tree changed", ok(func(in *CommitGateInput) { in.CurrentTree = "t2" }), CommitReview, "", CGTreeChanged},
		{"paths changed", ok(func(in *CommitGateInput) { in.CurrentPaths = []string{"a.go"} }), CommitReview, "", CGPathsChanged},
		{"gates for another tree", ok(func(in *CommitGateInput) { in.GateTree = "t0" }), CommitReview, "", CGGatesStale},
		{"no reviewed tree", ok(func(in *CommitGateInput) { in.ReviewedTree = "" }), CommitReview, "", CGTreeChanged},
		{"plan changed", ok(func(in *CommitGateInput) { in.PlanIntact, in.PlanProblem = false, "AGENTS.md (modified)" }), CommitBlock,
			state.ClassPlanIntegrity, CGPlanChanged},
		{"plan changed and tree changed", ok(func(in *CommitGateInput) { in.PlanIntact = false; in.CurrentTree = "t2" }), CommitBlock,
			state.ClassPlanIntegrity, CGPlanChanged},
		{"secret", ok(func(in *CommitGateInput) { in.PolicyBlocking = []string{"secret_detected a.go:3"} }), CommitBlock,
			state.ClassCommitGateBlocked, CGPolicy},
		{"gates red", ok(func(in *CommitGateInput) { in.GatesGreen = false }), CommitBlock, state.ClassCommitGateBlocked, CGGatesRed},
		{"review failed", ok(func(in *CommitGateInput) { in.ReviewPassed = false }), CommitBlock, state.ClassCommitGateBlocked, CGReviewMissing},
		{"review for other tree", ok(func(in *CommitGateInput) { in.ReviewTree = "t0" }), CommitBlock, state.ClassCommitGateBlocked, CGReviewMissing},
		{"open blocker", ok(func(in *CommitGateInput) { in.OpenBlocking = 1 }), CommitBlock, state.ClassCommitGateBlocked, CGOpenFinding},
		{"triage blocked", ok(func(in *CommitGateInput) { in.TriageBlocked = ClassUnsafe }), CommitBlock, state.ClassCommitGateBlocked, CGTriageBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideCommitGate(tc.in)
			if d.Action != tc.action || d.Class != tc.class {
				t.Fatalf("decision %+v, want %s %s", d, tc.action, tc.class)
			}
			if tc.reason != "" && !strings.Contains(codes(PlanGateDecision{Reasons: d.Reasons}), tc.reason) {
				t.Fatalf("reasons %+v lack %s", d.Reasons, tc.reason)
			}
			// No code path commits when anything failed.
			if d.Action == CommitProceed && len(d.Reasons) != 0 {
				t.Fatal("commit with reasons")
			}
		})
	}
}

// Convergence guards (§11.7).
func TestCheckGuards(t *testing.T) {
	cases := []struct {
		name  string
		in    GuardInput
		guard string
		class state.Class
	}{
		{"none", GuardInput{ReviewRounds: 4, ReviewLimit: 5, GateFixes: 3, GateFixLimit: 4, MaxReopen: 1, ReopenLimit: 2}, "", ""},
		{"review rounds", GuardInput{ReviewRounds: 5, ReviewLimit: 5}, GuardReviews, state.ClassIterationLimit},
		{"gate fixes", GuardInput{GateFixes: 4, GateFixLimit: 4}, GuardGateFixes, state.ClassIterationLimit},
		{"oscillation", GuardInput{MaxReopen: 2, ReopenLimit: 2}, GuardOscillation, state.ClassOscillation},
		{"no progress", GuardInput{NoProgress: 2}, GuardNoProgress, state.ClassNoProgress},
		{"deadlock", GuardInput{Rejections: 2}, GuardDeadlock, state.ClassDisputeDeadlock},
		{"policy", GuardInput{PolicyViolations: 5, PolicyLimit: 5}, GuardPolicy, state.ClassPolicyViolations},
		{"calls", GuardInput{AgentCalls: 40, CallLimit: 40}, GuardUsage, state.ClassPhaseUsageLimit},
		{"turns", GuardInput{Turns: 1500, TurnLimit: 1500}, GuardUsage, state.ClassPhaseUsageLimit},
		{"timeout", GuardInput{Elapsed: 5 * time.Hour, Timeout: 4 * time.Hour}, GuardUsage, state.ClassPhaseUsageLimit},
		{"incomplete", GuardInput{IncompleteReports: 1}, GuardIncomplete, state.ClassIterationLimit},
		{"policy first", GuardInput{PolicyViolations: 5, PolicyLimit: 5, NoProgress: 2}, GuardPolicy, state.ClassPolicyViolations},
		{"limits off", GuardInput{ReviewRounds: 99, GateFixes: 99, MaxReopen: 99, AgentCalls: 99}, "", ""},
	}
	for _, tc := range cases {
		trip := CheckGuards(tc.in)
		switch {
		case tc.guard == "" && trip != nil:
			t.Errorf("%s: tripped %+v", tc.name, trip)
		case tc.guard != "" && (trip == nil || trip.Guard != tc.guard || trip.Class != tc.class):
			t.Errorf("%s: %+v, want %s/%s", tc.name, trip, tc.guard, tc.class)
		}
	}
}

func TestChangedLines(t *testing.T) {
	diff := "diff --git a/calc/calc.go b/calc/calc.go\n--- a/calc/calc.go\n+++ b/calc/calc.go\n@@ -1,3 +1,4 @@\n package calc\n+// new\n \n-func A() {}\n+func B() {}\n"
	got := changedLines(diff)
	if l := got["calc/calc.go"]; len(l) != 2 || l[0] != 2 || l[1] != 4 {
		t.Fatalf("changed lines %v", got)
	}
}

// A finding that reappears with the same fingerprint keeps its ID and
// history but is judged at its current severity: a deferred minor that
// comes back as an in-scope major is reopened and fixable, and a late
// downgrade is decided again (§11.7).
func TestCodeLedgerSeverityEscalation(t *testing.T) {
	l := &CodeLedger{Phase: 1, Attempt: 1}
	minor := agent.CodeReviewFinding{Severity: "minor", Category: "correctness", ResolutionClass: ClassDeferred, File: "a.go",
		LineStart: 3, LineEnd: 3, Title: "Sub overflows"}
	l.Apply(1, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{minor}}, nil)
	if tr := l.Triage(1, rules()); len(tr.Deferred) != 1 {
		t.Fatalf("round 1 triage %+v", tr)
	}
	e := l.Get("F-01-001")
	if e.Status != CodeDeferred || e.DeferReason == "" {
		t.Fatalf("round 1 %+v", e)
	}
	major := minor
	major.Severity, major.ResolutionClass, major.Description = "major", ClassDeferred, "Sub overflows on MinInt."
	ch := l.Apply(2, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{major}}, nil)
	if len(ch) != 1 || ch[0] != (LedgerChange{"F-01-001", "reopened"}) || len(l.Entries) != 1 {
		t.Fatalf("round 2 changes %+v, entries %d", ch, len(l.Entries))
	}
	if e.Finding.Severity != "major" || e.Proposed != ClassDeferred || e.OpenedRound != 1 || e.ReopenCount != 1 ||
		e.DeferReason != "" || e.Finding.Description != major.Description {
		t.Fatalf("round 2 entry %+v", e)
	}
	tr := l.Triage(2, rules())
	if len(tr.Fixable) != 1 || tr.Fixable[0] != "F-01-001" || len(tr.Deferred) != 0 || e.Class != ClassFixable {
		t.Fatalf("round 2 triage %+v", tr)
	}
	d := DecideReviewGate(ReviewGateInput{GatesGreen: true, Review: review(nil), ACIDs: ac1, Findings: l.GateFindings()})
	if d.Pass || codes(d) != "open_finding:F-01-001" {
		t.Fatalf("review gate %+v", d)
	}

	// A late finding downgraded by scope-creep control is not downgraded
	// again when it comes back on the lines just changed.
	l = &CodeLedger{Phase: 1, Attempt: 1}
	late := major
	l.Apply(5, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{late}}, nil)
	e = l.Get("F-01-001")
	l.Triage(5, rules())
	if !e.Downgraded || e.Status != CodeDeferred {
		t.Fatalf("late finding %+v", e)
	}
	l.Apply(6, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{late}}, ChangedLines{"a.go": {3}})
	if e.Downgraded || e.Finding.Severity != "major" || !e.Open() {
		t.Fatalf("late finding on changed lines %+v", e)
	}
	if tr := l.Triage(6, rules()); len(tr.Fixable) != 1 {
		t.Fatalf("late triage %+v", tr)
	}
	// Still outside the latest changes: it stays downgraded.
	l.Apply(7, 3, &agent.CodeReview{PreviousFindings: []agent.PreviousStatus{{ID: "F-01-001", Status: "resolved"}}}, nil)
	l.Apply(8, 3, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{late}}, ChangedLines{"b.go": {1}})
	if !e.Downgraded || e.Finding.Severity != "minor" {
		t.Fatalf("late finding outside the changes %+v", e)
	}
	// A severity that drops is taken too.
	l.Apply(9, 0, &agent.CodeReview{NewFindings: []agent.CodeReviewFinding{minor}}, nil)
	if e.Downgraded || e.Finding.Severity != "minor" || e.Proposed != ClassDeferred {
		t.Fatalf("lowered %+v", e)
	}
}
