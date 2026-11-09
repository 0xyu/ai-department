package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ImplReport is impl-report.schema.json.
type ImplReport struct {
	Version            int               `json:"version"`
	Summary            string            `json:"summary"`
	FilesChanged       []FileChange      `json:"files_changed"`
	DesignDecisions    []string          `json:"design_decisions"`
	Tests              []TestChange      `json:"tests"`
	AcceptanceCriteria []ACAssessment    `json:"acceptance_criteria"`
	FindingResponses   []FindingResponse `json:"finding_responses"`
	KnownGaps          []string          `json:"known_gaps"`
}

// FileChange is one changed file the implementer reports.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
	Reason string `json:"reason"`
}

// TestChange is one added or changed test.
type TestChange struct {
	Path   string   `json:"path"`
	Change string   `json:"change"`
	Covers []string `json:"covers"`
}

// ACAssessment is the implementer's self-assessment of one criterion.
type ACAssessment struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// Enumerations of impl-report.schema.json (a test keeps them in sync).
var (
	FileChangeKinds  = []string{"added", "modified", "deleted", "renamed"}
	TestChangeKinds  = []string{"added", "modified"}
	ACStatuses       = []string{"met", "partial", "not_met", "not_verifiable"}
	FindingResponses = []string{ResponseFixed, ResponseDisputed, ResponseCannotFixWithinSpec, ResponseCannotFixSafely}
)

var acIDRE = regexp.MustCompile(`^AC-[0-9]{2,4}\.[1-9][0-9]*$`)

// ParseImplReport decodes and validates a report against
// impl-report.schema.json (unknown fields are errors). It returns every
// problem found, so the implementer can be asked to fix them all at once.
func ParseImplReport(raw []byte) (*ImplReport, []string) {
	var r ImplReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, []string{"report is not valid JSON for the schema: " + err.Error()}
	}
	var probs []string
	add := func(format string, a ...any) { probs = append(probs, fmt.Sprintf(format, a...)) }
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	for _, k := range []string{"version", "summary", "files_changed", "design_decisions", "tests", "acceptance_criteria", "finding_responses", "known_gaps"} {
		if _, ok := top[k]; !ok {
			add("missing required field %s", k)
		}
	}
	if r.Version != 1 {
		add("version must be 1")
	}
	if strings.TrimSpace(r.Summary) == "" {
		add("summary is empty")
	}
	for i, f := range r.FilesChanged {
		if f.Path == "" || !slices.Contains(FileChangeKinds, f.Change) || f.Reason == "" {
			add("files_changed[%d]: needs path, change (%s), and reason", i, strings.Join(FileChangeKinds, "|"))
		}
	}
	for i, t := range r.Tests {
		if t.Path == "" || !slices.Contains(TestChangeKinds, t.Change) {
			add("tests[%d]: needs path and change (%s)", i, strings.Join(TestChangeKinds, "|"))
		}
		for _, id := range t.Covers {
			if !acIDRE.MatchString(id) {
				add("tests[%d].covers: %q is not an AC ID", i, id)
			}
		}
	}
	for i, a := range r.AcceptanceCriteria {
		if !acIDRE.MatchString(a.ID) || !slices.Contains(ACStatuses, a.Status) {
			add("acceptance_criteria[%d]: needs an AC ID and status (%s)", i, strings.Join(ACStatuses, "|"))
		}
	}
	for i, f := range r.FindingResponses {
		if f.ID == "" || !slices.Contains(FindingResponses, f.Response) || f.Detail == "" {
			add("finding_responses[%d]: needs id, response (%s), and detail", i, strings.Join(FindingResponses, "|"))
		}
	}
	for i, s := range append(append([]string{}, r.DesignDecisions...), r.KnownGaps...) {
		if strings.TrimSpace(s) == "" {
			add("design_decisions/known_gaps entry %d is empty", i)
		}
	}
	if len(probs) > 0 {
		return nil, probs
	}
	return &r, nil
}

// CheckCoverage reports criteria and findings the report does not answer:
// every AC in acIDs needs an assessment, and every finding ID a response.
func (r *ImplReport) CheckCoverage(acIDs, findingIDs []string) []string {
	var probs []string
	seenAC := map[string]bool{}
	for _, a := range r.AcceptanceCriteria {
		seenAC[a.ID] = true
	}
	for _, id := range acIDs {
		if !seenAC[id] {
			probs = append(probs, "acceptance_criteria: no assessment for "+id)
		}
	}
	seenF := map[string]bool{}
	for _, f := range r.FindingResponses {
		seenF[f.ID] = true
	}
	for _, id := range findingIDs {
		if !seenF[id] {
			probs = append(probs, "finding_responses: no answer for "+id)
		}
	}
	return probs
}
