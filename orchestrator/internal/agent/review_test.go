package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/0xyu/aco/schemas"
)

// strictNode checks the parts of a JSON Schema that strict structured
// output needs: every object lists all properties as required and forbids
// others.
type strictNode struct {
	Type                 any                    `json:"type"`
	Properties           map[string]*strictNode `json:"properties"`
	Required             []string               `json:"required"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
	Items                *strictNode            `json:"items"`
	Enum                 []string               `json:"enum"`
}

func walkStrict(t *testing.T, path string, n *strictNode) {
	if n == nil {
		return
	}
	if n.Properties != nil {
		if n.AdditionalProperties == nil || *n.AdditionalProperties {
			t.Errorf("%s: additionalProperties must be false", path)
		}
		var keys []string
		for k := range n.Properties {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		req := slices.Clone(n.Required)
		slices.Sort(req)
		if !slices.Equal(keys, req) {
			t.Errorf("%s: required %v != properties %v", path, req, keys)
		}
		for k, c := range n.Properties {
			walkStrict(t, path+"."+k, c)
		}
	}
	walkStrict(t, path+"[]", n.Items)
}

func loadStrict(t *testing.T, b []byte) *strictNode {
	t.Helper()
	var n strictNode
	if err := json.Unmarshal(b, &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func fieldTags(v any) []string {
	var out []string
	ty := reflect.TypeOf(v)
	for i := 0; i < ty.NumField(); i++ {
		out = append(out, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	slices.Sort(out)
	return out
}

func propKeys(n *strictNode) []string {
	var out []string
	for k := range n.Properties {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sameEnum(t *testing.T, what string, schema, gov []string) {
	t.Helper()
	a, b := slices.Clone(schema), slices.Clone(gov)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Errorf("%s: schema %v, Go %v", what, schema, gov)
	}
}

func TestReviewSchemasMatchGoTypes(t *testing.T) {
	pr := loadStrict(t, schemas.PlanReview)
	walkStrict(t, "plan-review", pr)
	if !slices.Equal(propKeys(pr), fieldTags(PlanReview{})) {
		t.Errorf("plan review fields: %v vs %v", propKeys(pr), fieldTags(PlanReview{}))
	}
	for name, v := range map[string]any{"dimension_ratings": DimensionRating{}, "requirement_coverage": ReqCoverage{},
		"previous_findings": PreviousStatus{}, "new_findings": PlanReviewFinding{}} {
		if !slices.Equal(propKeys(pr.Properties[name].Items), fieldTags(v)) {
			t.Errorf("plan review %s fields differ", name)
		}
	}
	sameEnum(t, "verdict", pr.Properties["verdict"].Enum, PlanVerdicts)
	sameEnum(t, "dimension", pr.Properties["dimension_ratings"].Items.Properties["dimension"].Enum, PlanDimensions)
	sameEnum(t, "rating", pr.Properties["dimension_ratings"].Items.Properties["rating"].Enum, DimensionRatingValues)
	sameEnum(t, "coverage", pr.Properties["requirement_coverage"].Items.Properties["status"].Enum, CoverageStatuses)
	sameEnum(t, "plan previous", pr.Properties["previous_findings"].Items.Properties["status"].Enum, PlanPreviousStatuses)
	sameEnum(t, "plan severity", pr.Properties["new_findings"].Items.Properties["severity"].Enum, PlanSeverities)
	sameEnum(t, "finding dimension", pr.Properties["new_findings"].Items.Properties["dimension"].Enum, PlanDimensions)

	cr := loadStrict(t, schemas.CodeReview)
	walkStrict(t, "code-review", cr)
	if !slices.Equal(propKeys(cr), fieldTags(CodeReview{})) {
		t.Errorf("code review fields: %v vs %v", propKeys(cr), fieldTags(CodeReview{}))
	}
	if !slices.Equal(propKeys(cr.Properties["new_findings"].Items), fieldTags(CodeReviewFinding{})) {
		t.Error("code review finding fields differ")
	}
	sameEnum(t, "code verdict", cr.Properties["verdict"].Enum, CodeVerdicts)
	sameEnum(t, "conformance", cr.Properties["plan_conformance"].Properties["status"].Enum, ConformanceStatuses)
	sameEnum(t, "ac", cr.Properties["acceptance_criteria"].Items.Properties["status"].Enum, ACStatuses)
	sameEnum(t, "security", cr.Properties["security_assessment"].Properties["status"].Enum, SecurityStatuses)
	sameEnum(t, "risk", cr.Properties["regression_assessment"].Properties["risk"].Enum, RegressionRisks)
	sameEnum(t, "code previous", cr.Properties["previous_findings"].Items.Properties["status"].Enum, CodePreviousStatuses)
	nf := cr.Properties["new_findings"].Items.Properties
	sameEnum(t, "code severity", nf["severity"].Enum, CodeSeverities)
	sameEnum(t, "category", nf["category"].Enum, CodeCategories)
	sameEnum(t, "resolution", nf["resolution_class"].Enum, ResolutionClasses)
}

const goodCode = `{"verdict":"approve","summary":"ok","plan_conformance":{"status":"conforms","notes":""},
"acceptance_criteria":[{"id":"AC-01.1","status":"met","evidence":"TestA"}],
"security_assessment":{"status":"no_issues","notes":""},"regression_assessment":{"risk":"low","notes":""},
"test_assessment":{"adequate":true,"notes":""},"previous_findings":[{"id":"F-01-001","status":"resolved","note":""}],
"new_findings":[{"severity":"nit","category":"maintainability","resolution_class":"deferred","file":"a.go","line_start":1,"line_end":2,"title":"t","description":"d","evidence":"e","required_change":"r"}]}`

func TestParseCodeReview(t *testing.T) {
	r, probs := ParseCodeReview([]byte(goodCode), []string{"AC-01.1"}, []string{"F-01-001"})
	if r == nil {
		t.Fatal(probs)
	}
	if len(r.Inconsistencies()) != 0 {
		t.Errorf("consistent review: %v", r.Inconsistencies())
	}
	bad := map[string]struct {
		raw       string
		ac, prev  []string
		wantProbe string
	}{
		"unknown field":  {strings.Replace(goodCode, `"summary":"ok"`, `"summary":"ok","x":1`, 1), nil, []string{"F-01-001"}, "unknown field"},
		"missing field":  {strings.Replace(goodCode, `"test_assessment":{"adequate":true,"notes":""},`, ``, 1), nil, []string{"F-01-001"}, "test_assessment"},
		"bad verdict":    {strings.Replace(goodCode, `"approve"`, `"lgtm"`, 1), nil, []string{"F-01-001"}, "verdict"},
		"missing AC":     {goodCode, []string{"AC-01.1", "AC-01.2"}, []string{"F-01-001"}, "AC-01.2"},
		"foreign AC":     {goodCode, []string{"AC-02.1"}, []string{"F-01-001"}, "not a criterion"},
		"missing status": {goodCode, nil, []string{"F-01-001", "F-01-002"}, "no status for F-01-002"},
		"unknown prev":   {goodCode, nil, nil, "not a finding of an earlier round"},
		"abs file":       {strings.Replace(goodCode, `"file":"a.go"`, `"file":"/etc/x"`, 1), nil, []string{"F-01-001"}, "relative"},
		"bad lines":      {strings.Replace(goodCode, `"line_end":2`, `"line_end":-1`, 1), nil, []string{"F-01-001"}, "line_start"},
		"bad class":      {strings.Replace(goodCode, `"deferred"`, `"later"`, 1), nil, []string{"F-01-001"}, "resolution_class"},
		"trailing data":  {goodCode + `{}`, nil, []string{"F-01-001"}, "after the JSON"},
	}
	for name, c := range bad {
		r, probs := ParseCodeReview([]byte(c.raw), c.ac, c.prev)
		if r != nil || !strings.Contains(strings.Join(probs, "\n"), c.wantProbe) {
			t.Errorf("%s: %v", name, probs)
		}
	}
	// approve + not_met + major finding + unresolved: inconsistent.
	inc := strings.Replace(goodCode, `"status":"met"`, `"status":"not_met"`, 1)
	inc = strings.Replace(inc, `"severity":"nit","category":"maintainability"`, `"severity":"major","category":"correctness"`, 1)
	inc = strings.Replace(inc, `"status":"resolved"`, `"status":"unresolved"`, 1)
	r, _ = ParseCodeReview([]byte(inc), nil, []string{"F-01-001"})
	if got := r.Inconsistencies(); len(got) != 3 {
		t.Errorf("inconsistencies: %v", got)
	}
	// An out_of_scope major is not inconsistent with approve (§11.7).
	oos := strings.Replace(goodCode, `"severity":"nit","category":"maintainability"`, `"severity":"major","category":"out_of_scope"`, 1)
	r, _ = ParseCodeReview([]byte(oos), nil, []string{"F-01-001"})
	if got := r.Inconsistencies(); len(got) != 0 {
		t.Errorf("out_of_scope: %v", got)
	}
}

func planReviewJSON(verdict string, cov map[string]string, ratings map[string]string) string {
	r := PlanReview{Verdict: verdict, Summary: "s", DimensionRatings: []DimensionRating{}, RequirementCoverage: []ReqCoverage{},
		PreviousFindings: []PreviousStatus{}, NewFindings: []PlanReviewFinding{}}
	for _, d := range PlanDimensions {
		rt := "sound"
		if v, ok := ratings[d]; ok {
			rt = v
		}
		r.DimensionRatings = append(r.DimensionRatings, DimensionRating{Dimension: d, Rating: rt, Notes: ""})
	}
	for _, id := range []string{"REQ-001", "REQ-002"} {
		r.RequirementCoverage = append(r.RequirementCoverage, ReqCoverage{ReqID: id, Status: cov[id], Phases: []string{}, Notes: ""})
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func TestParsePlanReview(t *testing.T) {
	reqs := []string{"REQ-001", "REQ-002"}
	ok := planReviewJSON("pass", map[string]string{"REQ-001": "covered", "REQ-002": "covered"}, nil)
	r, probs := ParsePlanReview([]byte(ok), reqs, nil)
	if r == nil || len(r.Inconsistencies()) != 0 {
		t.Fatalf("%v %v", probs, r)
	}
	inc := planReviewJSON("pass", map[string]string{"REQ-001": "covered", "REQ-002": "missing"}, map[string]string{"risks": "unsound"})
	r, _ = ParsePlanReview([]byte(inc), reqs, nil)
	if got := r.Inconsistencies(); len(got) != 2 {
		t.Errorf("plan inconsistencies: %v", got)
	}
	if _, probs := ParsePlanReview([]byte(ok), append(reqs, "REQ-003"), nil); !strings.Contains(strings.Join(probs, " "), "REQ-003") {
		t.Errorf("missing requirement: %v", probs)
	}
	if _, probs := ParsePlanReview([]byte(ok), reqs, []string{"PF-001"}); !strings.Contains(strings.Join(probs, " "), "PF-001") {
		t.Errorf("missing previous status: %v", probs)
	}
	// A dimension rated twice and one not at all.
	dup := strings.Replace(ok, `"dimension":"quality_gates"`, `"dimension":"risks"`, 1)
	if _, probs := ParsePlanReview([]byte(dup), reqs, nil); !strings.Contains(strings.Join(probs, " "), "quality_gates must be rated") {
		t.Errorf("dimensions: %v", probs)
	}
}

func TestFullSchemaValidation(t *testing.T) {
	if _, _, err := reviewSchemas(); err != nil {
		t.Fatalf("embedded schemas do not compile: %v", err)
	}
	ac, prev := []string{"AC-01.1"}, []string{"F-01-001"}
	for name, c := range map[string]struct{ raw, want string }{
		"empty nested object":   {strings.Replace(goodCode, `"test_assessment":{"adequate":true,"notes":""}`, `"test_assessment":{}`, 1), "$.test_assessment.adequate: missing required field"},
		"null required array":   {strings.Replace(goodCode, `"acceptance_criteria":[`, `"acceptance_criteria":null,"x_ignored":[`, 1), "$.acceptance_criteria: must be array, not null"},
		"null new_findings":     {strings.Replace(goodCode, `"new_findings":[{`, `"new_findings":null,"y":[{`, 1), "$.new_findings: must be array, not null"},
		"null string":           {strings.Replace(goodCode, `"summary":"ok"`, `"summary":null`, 1), "$.summary: must be string, not null"},
		"nested item missing":   {strings.Replace(goodCode, `"evidence":"TestA"`, `"evidence_x":"TestA"`, 1), "$.acceptance_criteria[0].evidence: missing required field"},
		"fractional line":       {strings.Replace(goodCode, `"line_start":1`, `"line_start":1.5`, 1), "$.new_findings[0].line_start: must be an integer"},
		"wrong type boolean":    {strings.Replace(goodCode, `"adequate":true`, `"adequate":"yes"`, 1), "$.test_assessment.adequate: must be a boolean"},
		"nested unknown field":  {strings.Replace(goodCode, `"risk":"low"`, `"risk":"low","extra":1`, 1), "$.regression_assessment.extra: unknown field"},
		"nested enum":           {strings.Replace(goodCode, `"risk":"low"`, `"risk":"none"`, 1), `$.regression_assessment.risk: "none" is not one of`},
		"array of wrong things": {strings.Replace(goodCode, `"previous_findings":[{"id":"F-01-001","status":"resolved","note":""}]`, `"previous_findings":["F-01-001"]`, 1), "$.previous_findings[0]: must be an object"},
	} {
		r, probs := ParseCodeReview([]byte(c.raw), ac, prev)
		if r != nil || !strings.Contains(strings.Join(probs, "\n"), c.want) {
			t.Errorf("%s: %v", name, probs)
		}
	}
	plan := planReviewJSON("pass", map[string]string{"REQ-001": "covered", "REQ-002": "covered"}, nil)
	for name, c := range map[string]struct{ raw, want string }{
		"null phases":        {strings.Replace(plan, `"phases":[]`, `"phases":null`, 1), "$.requirement_coverage[0].phases: must be array, not null"},
		"missing nested":     {strings.Replace(plan, `"rating":"sound",`, ``, 1), "$.dimension_ratings[0].rating: missing required field"},
		"null top-level arr": {strings.Replace(plan, `"new_findings":[]`, `"new_findings":null`, 1), "$.new_findings: must be array, not null"},
	} {
		r, probs := ParsePlanReview([]byte(c.raw), []string{"REQ-001", "REQ-002"}, nil)
		if r != nil || !strings.Contains(strings.Join(probs, "\n"), c.want) {
			t.Errorf("plan %s: %v", name, probs)
		}
	}
	// Schema-valid but inconsistent reviews stay available to the gates.
	inc := strings.Replace(goodCode, `"status":"met"`, `"status":"not_met"`, 1)
	if r, probs := ParseCodeReview([]byte(inc), ac, prev); r == nil || len(r.Inconsistencies()) == 0 {
		t.Errorf("inconsistent review rejected: %v", probs)
	}
}

func TestCompileSchemaRejectsUnsupportedKeywords(t *testing.T) {
	for _, s := range []string{`{"type":"string","minLength":1}`, `{"type":["string","null"]}`, `{"type":"object","additionalProperties":true}`, `{"anyOf":[]}`} {
		if _, err := compileSchema([]byte(s), "$"); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
