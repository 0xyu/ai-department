package agent

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/schemas"
)

const goodReport = `{"version":1,"summary":"s","files_changed":[{"path":"a.go","change":"added","reason":"r"}],"design_decisions":["d"],"tests":[{"path":"a_test.go","change":"added","covers":["AC-01.1"]}],"acceptance_criteria":[{"id":"AC-01.1","status":"met","evidence":"e"}],"finding_responses":[{"id":"F-1","response":"disputed","detail":"why"}],"known_gaps":[]}`

func TestParseImplReport(t *testing.T) {
	r, probs := ParseImplReport([]byte(goodReport))
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	if p := r.CheckCoverage([]string{"AC-01.1", "AC-01.2"}, []string{"F-1", "F-2"}); len(p) != 2 ||
		!strings.Contains(p[0], "AC-01.2") || !strings.Contains(p[1], "F-2") {
		t.Errorf("coverage: %v", p)
	}
	bad := map[string]string{
		"unknown field":   strings.Replace(goodReport, `"known_gaps":[]`, `"known_gaps":[],"x":1`, 1),
		"missing field":   strings.Replace(goodReport, `,"known_gaps":[]`, ``, 1),
		"bad change":      strings.Replace(goodReport, `"change":"added","reason"`, `"change":"created","reason"`, 1),
		"bad AC id":       strings.Replace(goodReport, `"id":"AC-01.1","status"`, `"id":"AC-1","status"`, 1),
		"bad status":      strings.Replace(goodReport, `"status":"met"`, `"status":"done"`, 1),
		"bad response":    strings.Replace(goodReport, `"disputed"`, `"ignored"`, 1),
		"empty summary":   strings.Replace(goodReport, `"summary":"s"`, `"summary":" "`, 1),
		"wrong version":   strings.Replace(goodReport, `"version":1`, `"version":2`, 1),
		"not json":        `{"version":`,
		"bad covers":      strings.Replace(goodReport, `"covers":["AC-01.1"]`, `"covers":["REQ-001"]`, 1),
		"empty detail":    strings.Replace(goodReport, `"detail":"why"`, `"detail":""`, 1),
		"empty gap entry": strings.Replace(goodReport, `"known_gaps":[]`, `"known_gaps":[""]`, 1),
	}
	for name, raw := range bad {
		if r, probs := ParseImplReport([]byte(raw)); r != nil || len(probs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

type node struct {
	Properties map[string]*node `json:"properties"`
	Required   []string         `json:"required"`
	Items      *node            `json:"items"`
	Enum       []string         `json:"enum"`
}

func jsonTags(v any) []string {
	var out []string
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		out = append(out, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(out)
	return out
}

func keys(m map[string]*node) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// impl-report.schema.json and the Go types describe the same document.
func TestImplReportSchemaMatchesTypes(t *testing.T) {
	var root node
	if err := json.Unmarshal(schemas.ImplReport, &root); err != nil {
		t.Fatal(err)
	}
	if got, want := keys(root.Properties), jsonTags(ImplReport{}); !slices.Equal(got, want) {
		t.Errorf("top level: schema %v, Go %v", got, want)
	}
	if r := append([]string{}, root.Required...); !slices.Equal(sortS(r), jsonTags(ImplReport{})) {
		t.Errorf("all top-level fields are required: %v", root.Required)
	}
	for field, typ := range map[string]any{"files_changed": FileChange{}, "tests": TestChange{}, "acceptance_criteria": ACAssessment{}, "finding_responses": FindingResponse{}} {
		if got, want := keys(root.Properties[field].Items.Properties), jsonTags(typ); !slices.Equal(got, want) {
			t.Errorf("%s: schema %v, Go %v", field, got, want)
		}
	}
	enums := map[string][]string{
		"files_changed.change":       FileChangeKinds,
		"tests.change":               TestChangeKinds,
		"acceptance_criteria.status": ACStatuses,
		"finding_responses.response": FindingResponses,
	}
	for path, want := range enums {
		f, p, _ := strings.Cut(path, ".")
		if got := root.Properties[f].Items.Properties[p].Enum; !slices.Equal(got, want) {
			t.Errorf("%s enum: schema %v, Go %v", path, got, want)
		}
	}
}

func sortS(s []string) []string { sort.Strings(s); return s }

func TestBudget(t *testing.T) {
	b := NewBudget(config.BudgetLimits{Run: 10, Phase: 2, Planning: 0, SoftRatio: 0.8})
	if !math.IsInf(b.Remaining(ScopePlanning), 1) {
		t.Error("a zero limit is unlimited")
	}
	if st := b.Charge(1.0, ScopeRun, ScopePhase); len(st.Warn)+len(st.Exceeded) != 0 {
		t.Errorf("1/2: %+v", st)
	}
	if st := b.Charge(0.7, ScopeRun, ScopePhase); !slices.Equal(st.Warn, []Scope{ScopePhase}) {
		t.Errorf("1.7/2 should warn: %+v", st)
	}
	if st := b.Charge(0.5, ScopeRun, ScopePhase); !slices.Equal(st.Exceeded, []Scope{ScopePhase}) {
		t.Errorf("2.2/2 should be exceeded: %+v", st)
	}
	if r := b.Remaining(ScopeRun, ScopePhase); r > 0 {
		t.Errorf("remaining %v", r)
	}
	b.ResetScope(ScopePhase)
	if r := b.Remaining(ScopeRun, ScopePhase); math.Abs(r-2) > 1e-9 {
		t.Errorf("after phase reset: %v", r)
	}
	if b.Charge(math.NaN(), ScopeRun); b.Spent(ScopeRun) != 2.2 {
		t.Errorf("NaN charged: %v", b.Spent(ScopeRun))
	}
}

func TestErrors(t *testing.T) {
	e := &Error{Agent: "claude", Class: ClassSubscriptionLimit, Message: "limit"}
	wrapped := errors.Join(errors.New("ctx"), e)
	if ClassOf(wrapped) != ClassSubscriptionLimit || ClassOf(errors.New("x")) != "" {
		t.Error("ClassOf")
	}
	if !ClassTransient.Retryable() || ClassAuth.Retryable() || ClassAPIQuota.Retryable() || ClassBudget.Retryable() {
		t.Error("Retryable")
	}
	var u Usage
	u.Add(Usage{InputTokens: 1, Turns: 2, CostUSD: 0.5})
	u.Add(Usage{InputTokens: 1, Turns: 1, CostUSD: 0.25})
	if u.InputTokens != 2 || u.Turns != 3 || u.CostUSD != 0.75 {
		t.Errorf("usage add: %+v", u)
	}
}
