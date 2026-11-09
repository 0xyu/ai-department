package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Reviewer is the independent reviewer (GPT Sol 6.1 through Codex CLI):
// Stage 2 plan review (§10.3), Stage 3 phase review (§11.6), and Stage 4
// final review (§12.5). Every review is a new, stateless, read-only
// session; it sees only what the request carries.
type Reviewer interface {
	ReviewPlan(ctx context.Context, req PlanReviewRequest) (*PlanReviewResult, error)
	ReviewCode(ctx context.Context, req CodeReviewRequest) (*CodeReviewResult, error)
}

// PackFile is one file placed verbatim in a context pack.
type PackFile struct {
	Path    string
	Content string
}

// PlanFindingRecord is a plan finding from an earlier round with the
// planner's answer (plan re-review).
type PlanFindingRecord struct {
	Finding  PlanFinding
	Response *FindingResponse // nil: the planner did not answer
}

// PlanReviewRequest asks for a Stage 2 review of a plan package.
type PlanReviewRequest struct {
	RunID     string
	Round     int       // 1-based; > 1 with Previous is a re-review
	Workspace Workspace // the repository; Codex reads it with -C, read-only
	Output    Output

	Requirements string     // the requirement document
	PackageFiles []PackFile // every plan file, verbatim (see PackageFiles)
	ReqIDs       []string   // requirement IDs the coverage must list
	RepoOverview string     // tree, languages, existing tests
	AgentsMD     string     // existing or proposed AGENTS.md
	CheckResults string     // deterministic plan checks (§10.4), rendered
	Previous     []PlanFindingRecord
	BudgetScopes []Scope
}

// ReviewKind selects the code review prompt.
type ReviewKind string

// Review kinds.
const (
	ReviewPhase ReviewKind = "phase" // Stage 3 (§11.6)
	ReviewFinal ReviewKind = "final" // Stage 4 (§12.5)
)

// PhaseStatus places a phase in the plan for the reviewer (done, current,
// future), so later-phase work is not demanded.
type PhaseStatus struct {
	Number int
	Title  string
	Status string // done | current | future
}

// LedgerEntry is a finding from an earlier review round (re-review).
type LedgerEntry struct {
	ID             string
	Severity       string
	Category       string
	Location       string
	Title          string
	RequiredChange string
	State          string // open | closed
	Response       string // the implementer's answer: fixed | disputed | ...
	Detail         string
}

// CodeReviewRequest asks for a phase or final code review.
type CodeReviewRequest struct {
	RunID     string
	Kind      ReviewKind
	Phase     int // 0 for the final review
	Attempt   int
	Round     int       // 1-based; > 1 with Ledger is a re-review
	Workspace Workspace // the worktree; Codex reads it with -C, read-only
	Output    Output

	PlanOverview string
	Phases       []PhaseStatus
	PhaseSpec    string // current phases/PNN.md verbatim (final: empty)
	Requirements string // final review: the requirements
	AgentsMD     string
	ACIDs        []string // criteria the review must assess

	Diff            string // full diff from the phase base (final: run base) to the current tree
	DiffStat        string
	IncrementalDiff string // re-review: diff since the last review
	GateResults     string // all six gate results (authoritative)
	Baseline        string // baseline comparison
	TestChanges     string // new / changed / removed tests
	Regressions     string // tests that passed at baseline and fail now
	Security        string // security gate findings, diff-policy and anti-gaming flags
	Importers       string // changed files other modules import; tests touching changed code
	ImplReport      string // the implementer's report: unverified claims
	Ledger          []LedgerEntry
	BudgetScopes    []Scope
}

// AccessViolation is something the reviewer did outside its confinement
// (§16.3): a read outside the worktree and the artifact directory, a write,
// a network or tool call.
type AccessViolation struct {
	Kind    string `json:"kind"` // read | write | tool
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"` // masked, shortened
}

// ReviewCalls is shared by both review results: every CLI call made for
// the review (retries included) and their total usage.
type ReviewCalls struct {
	Calls []Invocation
	Total Usage
	// FallbackUsed is true when reviewer.fallback_model produced the review.
	FallbackUsed bool
	// ContextRef is the context pack of the first call (relative to the run dir).
	ContextRef string
	// ReviewRef is the accepted review JSON.
	ReviewRef string
}

// PlanReviewResult is an accepted plan review.
type PlanReviewResult struct {
	Invocation // the call that produced the review
	ReviewCalls
	Raw    json.RawMessage
	Review *PlanReview
	// Inconsistent lists contradictions between the verdict and the
	// details (the gate decides from the details; review.inconsistent).
	Inconsistent []string
}

// CodeReviewResult is an accepted code review.
type CodeReviewResult struct {
	Invocation
	ReviewCalls
	Raw          json.RawMessage
	Review       *CodeReview
	Inconsistent []string
}

// PlanReview is plan-review.schema.json.
type PlanReview struct {
	Verdict             string              `json:"verdict"`
	Summary             string              `json:"summary"`
	DimensionRatings    []DimensionRating   `json:"dimension_ratings"`
	RequirementCoverage []ReqCoverage       `json:"requirement_coverage"`
	PreviousFindings    []PreviousStatus    `json:"previous_findings"`
	NewFindings         []PlanReviewFinding `json:"new_findings"`
}

// DimensionRating rates one plan review dimension.
type DimensionRating struct {
	Dimension string `json:"dimension"`
	Rating    string `json:"rating"`
	Notes     string `json:"notes"`
}

// ReqCoverage is the reviewer's coverage status of one requirement.
type ReqCoverage struct {
	ReqID  string   `json:"req_id"`
	Status string   `json:"status"`
	Phases []string `json:"phases"`
	Notes  string   `json:"notes"`
}

// PreviousStatus is the reviewer's status of an earlier finding.
type PreviousStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// PlanReviewFinding is a new plan finding (the engine assigns its ID).
type PlanReviewFinding struct {
	Severity       string `json:"severity"`
	Dimension      string `json:"dimension"`
	Location       string `json:"location"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	RequiredChange string `json:"required_change"`
}

// CodeReview is code-review.schema.json.
type CodeReview struct {
	Verdict              string              `json:"verdict"`
	Summary              string              `json:"summary"`
	PlanConformance      StatusNotes         `json:"plan_conformance"`
	AcceptanceCriteria   []ACAssessment      `json:"acceptance_criteria"`
	SecurityAssessment   StatusNotes         `json:"security_assessment"`
	RegressionAssessment RiskNotes           `json:"regression_assessment"`
	TestAssessment       TestAssessment      `json:"test_assessment"`
	PreviousFindings     []PreviousStatus    `json:"previous_findings"`
	NewFindings          []CodeReviewFinding `json:"new_findings"`
}

// StatusNotes is a status with notes.
type StatusNotes struct {
	Status string `json:"status"`
	Notes  string `json:"notes"`
}

// RiskNotes is the regression assessment.
type RiskNotes struct {
	Risk  string `json:"risk"`
	Notes string `json:"notes"`
}

// TestAssessment says whether the tests cover the criteria.
type TestAssessment struct {
	Adequate bool   `json:"adequate"`
	Notes    string `json:"notes"`
}

// CodeReviewFinding is a new code finding (the engine assigns its ID).
type CodeReviewFinding struct {
	Severity        string `json:"severity"`
	Category        string `json:"category"`
	ResolutionClass string `json:"resolution_class"`
	File            string `json:"file"`
	LineStart       int    `json:"line_start"`
	LineEnd         int    `json:"line_end"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	Evidence        string `json:"evidence"`
	RequiredChange  string `json:"required_change"`
}

// Enumerations of the review schemas (a test keeps them in sync).
var (
	PlanVerdicts          = []string{"pass", "revise"}
	PlanDimensions        = []string{"architecture", "technical_approach", "coverage", "dependencies", "risks", "phase_quality", "acceptance_criteria", "quality_gates"}
	DimensionRatingValues = []string{"sound", "concerns", "unsound"}
	CoverageStatuses      = []string{"covered", "partial", "missing"}
	PlanPreviousStatuses  = []string{"resolved", "unresolved", "dispute_accepted", "dispute_rejected"}
	PlanSeverities        = []string{"blocker", "major", "minor"}

	CodeVerdicts         = []string{"approve", "request_changes"}
	ConformanceStatuses  = []string{"conforms", "deviates"}
	SecurityStatuses     = []string{"no_issues", "issues_found"}
	RegressionRisks      = []string{"low", "medium", "high"}
	CodePreviousStatuses = []string{"resolved", "unresolved", "partially_resolved", "dispute_accepted", "dispute_rejected"}
	CodeSeverities       = []string{"blocker", "major", "minor", "nit"}
	CodeCategories       = []string{"correctness", "security", "regression", "tests", "spec_mismatch", "scope_violation", "agents_md_violation", "maintainability", "performance", "out_of_scope", "prompt_injection"}
	ResolutionClasses    = []string{"fixable", "spec_conflict", "unsafe", "environment", "deferred"}
)

// decodeStrict decodes raw into v rejecting unknown fields and checks that
// every top-level key in required is present.
func decodeStrict(raw []byte, v any, required []string) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return []string{"the review is not valid JSON for the schema: " + err.Error()}
	}
	if dec.More() {
		return []string{"the review has data after the JSON object"}
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	var probs []string
	for _, k := range required {
		if _, ok := top[k]; !ok {
			probs = append(probs, "missing required field "+k)
		}
	}
	return probs
}

type problems []string

func (p *problems) add(format string, a ...any) { *p = append(*p, fmt.Sprintf(format, a...)) }

func (p *problems) enum(field, v string, allowed []string) {
	if !slices.Contains(allowed, v) {
		p.add("%s: %q is not one of %s", field, v, strings.Join(allowed, "|"))
	}
}

func (p *problems) text(field, v string) {
	if strings.TrimSpace(v) == "" {
		p.add("%s is empty", field)
	}
}

// checkPrevious requires exactly one status per earlier finding ID.
func (p *problems) checkPrevious(prev []PreviousStatus, ids []string, allowed []string) {
	seen := map[string]int{}
	for i, s := range prev {
		p.enum(fmt.Sprintf("previous_findings[%d].status", i), s.Status, allowed)
		if !slices.Contains(ids, s.ID) {
			p.add("previous_findings[%d]: %q is not a finding of an earlier round", i, s.ID)
		}
		seen[s.ID]++
	}
	for _, id := range ids {
		switch seen[id] {
		case 0:
			p.add("previous_findings: no status for %s", id)
		case 1:
		default:
			p.add("previous_findings: %s has more than one status", id)
		}
	}
}

// ParsePlanReview validates a plan review against the whole embedded
// schema, then decodes it and applies the request checks. reqIDs are the
// requirements the coverage must list (each exactly once); previousIDs the
// earlier findings that each need a status. It returns every problem, so
// the reviewer can be asked to fix them all at once.
func ParsePlanReview(raw []byte, reqIDs, previousIDs []string) (*PlanReview, []string) {
	ps, _, err := reviewSchemas()
	if err != nil {
		return nil, []string{"plan review schema: " + err.Error()}
	}
	if probs := validateJSON(ps, raw); len(probs) > 0 {
		return nil, probs
	}
	var r PlanReview
	if probs := decodeStrict(raw, &r, []string{"verdict", "summary", "dimension_ratings", "requirement_coverage", "previous_findings", "new_findings"}); len(probs) > 0 {
		return nil, probs
	}
	var p problems
	p.enum("verdict", r.Verdict, PlanVerdicts)
	p.text("summary", r.Summary)
	rated := map[string]int{}
	for i, d := range r.DimensionRatings {
		p.enum(fmt.Sprintf("dimension_ratings[%d].dimension", i), d.Dimension, PlanDimensions)
		p.enum(fmt.Sprintf("dimension_ratings[%d].rating", i), d.Rating, DimensionRatingValues)
		rated[d.Dimension]++
	}
	for _, d := range PlanDimensions {
		if rated[d] != 1 {
			p.add("dimension_ratings: %s must be rated exactly once", d)
		}
	}
	covered := map[string]int{}
	for i, c := range r.RequirementCoverage {
		p.enum(fmt.Sprintf("requirement_coverage[%d].status", i), c.Status, CoverageStatuses)
		if len(reqIDs) > 0 && !slices.Contains(reqIDs, c.ReqID) {
			p.add("requirement_coverage[%d]: %q is not a requirement of the plan", i, c.ReqID)
		}
		covered[c.ReqID]++
	}
	for _, id := range reqIDs {
		if covered[id] != 1 {
			p.add("requirement_coverage: %s must be listed exactly once", id)
		}
	}
	p.checkPrevious(r.PreviousFindings, previousIDs, PlanPreviousStatuses)
	for i, f := range r.NewFindings {
		p.enum(fmt.Sprintf("new_findings[%d].severity", i), f.Severity, PlanSeverities)
		p.enum(fmt.Sprintf("new_findings[%d].dimension", i), f.Dimension, PlanDimensions)
		p.text(fmt.Sprintf("new_findings[%d].title", i), f.Title)
		p.text(fmt.Sprintf("new_findings[%d].description", i), f.Description)
		p.text(fmt.Sprintf("new_findings[%d].required_change", i), f.RequiredChange)
	}
	if len(p) > 0 {
		return nil, p
	}
	return &r, nil
}

// Inconsistencies lists where the verdict contradicts the details. The
// Plan Decision Gate (§10.4) uses the details, never the verdict.
func (r *PlanReview) Inconsistencies() []string {
	var out []string
	if r.Verdict != "pass" {
		return nil
	}
	for _, c := range r.RequirementCoverage {
		if c.Status != "covered" {
			out = append(out, fmt.Sprintf("verdict pass but %s is %s", c.ReqID, c.Status))
		}
	}
	for _, d := range r.DimensionRatings {
		if d.Rating == "unsound" {
			out = append(out, "verdict pass but "+d.Dimension+" is rated unsound")
		}
	}
	for _, f := range r.NewFindings {
		if f.Severity == "blocker" || f.Severity == "major" {
			out = append(out, "verdict pass with a new "+f.Severity+" finding: "+f.Title)
		}
	}
	for _, s := range r.PreviousFindings {
		if s.Status == "unresolved" || s.Status == "dispute_rejected" {
			out = append(out, "verdict pass but "+s.ID+" is "+s.Status)
		}
	}
	return out
}

// ParseCodeReview validates a code review against the whole embedded
// schema, then decodes it and applies the request checks. acIDs are the
// criteria that each need exactly one assessment; previousIDs the earlier
// findings that each need a status.
func ParseCodeReview(raw []byte, acIDs, previousIDs []string) (*CodeReview, []string) {
	_, cs, err := reviewSchemas()
	if err != nil {
		return nil, []string{"code review schema: " + err.Error()}
	}
	if probs := validateJSON(cs, raw); len(probs) > 0 {
		return nil, probs
	}
	var r CodeReview
	if probs := decodeStrict(raw, &r, []string{"verdict", "summary", "plan_conformance", "acceptance_criteria", "security_assessment", "regression_assessment", "test_assessment", "previous_findings", "new_findings"}); len(probs) > 0 {
		return nil, probs
	}
	var p problems
	p.enum("verdict", r.Verdict, CodeVerdicts)
	p.text("summary", r.Summary)
	p.enum("plan_conformance.status", r.PlanConformance.Status, ConformanceStatuses)
	p.enum("security_assessment.status", r.SecurityAssessment.Status, SecurityStatuses)
	p.enum("regression_assessment.risk", r.RegressionAssessment.Risk, RegressionRisks)
	seen := map[string]int{}
	for i, a := range r.AcceptanceCriteria {
		p.enum(fmt.Sprintf("acceptance_criteria[%d].status", i), a.Status, ACStatuses)
		if len(acIDs) > 0 && !slices.Contains(acIDs, a.ID) {
			p.add("acceptance_criteria[%d]: %q is not a criterion of this review", i, a.ID)
		}
		p.text(fmt.Sprintf("acceptance_criteria[%d].evidence", i), a.Evidence)
		seen[a.ID]++
	}
	for _, id := range acIDs {
		if seen[id] != 1 {
			p.add("acceptance_criteria: %s must be assessed exactly once", id)
		}
	}
	p.checkPrevious(r.PreviousFindings, previousIDs, CodePreviousStatuses)
	for i, f := range r.NewFindings {
		pre := fmt.Sprintf("new_findings[%d]", i)
		p.enum(pre+".severity", f.Severity, CodeSeverities)
		p.enum(pre+".category", f.Category, CodeCategories)
		p.enum(pre+".resolution_class", f.ResolutionClass, ResolutionClasses)
		p.text(pre+".title", f.Title)
		p.text(pre+".description", f.Description)
		p.text(pre+".required_change", f.RequiredChange)
		if f.LineStart < 0 || f.LineEnd < 0 || (f.LineEnd > 0 && f.LineEnd < f.LineStart) {
			p.add("%s: line_start/line_end must be 0 or a valid range", pre)
		}
		if strings.Contains(f.File, "..") || strings.HasPrefix(f.File, "/") || (len(f.File) > 1 && f.File[1] == ':') {
			p.add("%s.file must be a path relative to the repository root", pre)
		}
	}
	if len(p) > 0 {
		return nil, p
	}
	return &r, nil
}

// Inconsistencies lists where the verdict contradicts the details. The
// Review Decision Gate (§11.7) uses the details, never the verdict.
func (r *CodeReview) Inconsistencies() []string {
	if r.Verdict != "approve" {
		return nil
	}
	var out []string
	if r.PlanConformance.Status != "conforms" {
		out = append(out, "verdict approve but the plan conformance is "+r.PlanConformance.Status)
	}
	for _, a := range r.AcceptanceCriteria {
		if a.Status != "met" {
			out = append(out, fmt.Sprintf("verdict approve but %s is %s", a.ID, a.Status))
		}
	}
	if r.RegressionAssessment.Risk == "high" {
		out = append(out, "verdict approve with a high regression risk")
	}
	if !r.TestAssessment.Adequate {
		out = append(out, "verdict approve but the tests are rated inadequate")
	}
	for _, f := range r.NewFindings {
		if (f.Severity == "blocker" || f.Severity == "major") && f.Category != "out_of_scope" {
			out = append(out, "verdict approve with a new "+f.Severity+" finding: "+f.Title)
		}
	}
	for _, s := range r.PreviousFindings {
		if s.Status == "unresolved" || s.Status == "partially_resolved" || s.Status == "dispute_rejected" {
			out = append(out, "verdict approve but "+s.ID+" is "+s.Status)
		}
	}
	return out
}
