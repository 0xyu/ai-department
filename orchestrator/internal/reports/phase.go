// Package reports writes ACO's reports into the target repository: the
// Phase Completion Report (§11.9, .aco/reports/phase-NN.{md,json}) and,
// from Phase 11, the Final Development Report. The JSON form follows
// schemas/phase-report.schema.json and is what later phases and Stage 4
// read back; the Markdown form is for people.
//
// Reports are documents in the document language (English). They hold no
// secrets: every string that came from an agent or a gate is masked by the
// engine before it is put into a report.
package reports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/schemas"
)

// PhaseFormat is the "format" value of a Phase Completion Report.
const PhaseFormat = "aco.phase-report/v1"

// Dir is the repository directory reports are committed to.
const Dir = ".aco/reports"

// MaxDigestChars bounds the phase digest (about 1.5k tokens).
const MaxDigestChars = 6000

// PhaseReport is phase-report.schema.json.
type PhaseReport struct {
	Format             string         `json:"format"`
	RunID              string         `json:"run_id"`
	Phase              int            `json:"phase"`
	Title              string         `json:"title"`
	Goal               string         `json:"goal"`
	PlanVersion        string         `json:"plan_version"`
	PlanHash           string         `json:"plan_hash"`
	BaseCommit         string         `json:"base_commit"`
	ReviewedTree       string         `json:"reviewed_tree"`
	Attempt            int            `json:"attempt"`
	Attempts           int            `json:"attempts"`
	Sessions           []Session      `json:"sessions"`
	ImplementerModel   string         `json:"implementer_model"`
	ReviewerModel      string         `json:"reviewer_model"`
	AcceptanceCriteria []AC           `json:"acceptance_criteria"`
	Requirements       []string       `json:"requirements"`
	Files              []FileChange   `json:"files"`
	DiffStat           string         `json:"diff_stat"`
	Gates              []Gate         `json:"gates"`
	Regressions        []string       `json:"regressions"`
	RegressionsSeen    []string       `json:"regressions_seen"`
	Security           Security       `json:"security"`
	Reviews            []ReviewRound  `json:"reviews"`
	Findings           []Finding      `json:"findings"`
	Deferred           []Deferred     `json:"deferred"`
	Implementation     Implementation `json:"implementation"`
	Usage              Usage          `json:"usage"`
	Digest             string         `json:"digest"`
	GeneratedAt        string         `json:"generated_at"`
}

// Session is a Claude session of one attempt.
type Session struct {
	Attempt   int    `json:"attempt"`
	SessionID string `json:"session_id"`
}

// AC is one acceptance criterion with its final status.
type AC struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Status   string   `json:"status"`
	Evidence string   `json:"evidence"`
	Tests    []string `json:"tests"`
}

// FileChange is one changed path of the phase diff.
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path"`
	Status  string `json:"status"` // added | modified | deleted | renamed | copied | type_changed
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

// Gate is one quality gate result on the committed tree.
type Gate struct {
	Gate       string        `json:"gate"`
	Status     string        `json:"status"`
	Required   bool          `json:"required"`
	Source     string        `json:"source"`
	Commands   []string      `json:"commands"`
	DurationMS int64         `json:"duration_ms"`
	Summary    string        `json:"summary"`
	Baseline   BaselineDelta `json:"baseline"`
}

// BaselineDelta is a gate result compared with the baseline.
type BaselineDelta struct {
	HasBaseline         bool `json:"has_baseline"`
	NewFailingTests     int  `json:"new_failing_tests"`
	Regressions         int  `json:"regressions"`
	FixedTests          int  `json:"fixed_tests"`
	PreexistingFailures int  `json:"preexisting_failures"`
	NewIssues           int  `json:"new_issues"`
	PreexistingIssues   int  `json:"preexisting_issues"`
	NewVulns            int  `json:"new_vulns"`
	PreexistingVulns    int  `json:"preexisting_vulns"`
}

// Security summarizes the security gate and the diff policy.
type Security struct {
	Scanners    []string        `json:"scanners"`
	NewFindings []string        `json:"new_findings"`
	Preexisting []string        `json:"preexisting_findings"`
	Policy      []PolicyFinding `json:"policy"`
}

// PolicyFinding is one diff-policy finding on the final diff.
type PolicyFinding struct {
	Code     string `json:"code"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Blocking bool   `json:"blocking"`
	Message  string `json:"message"`
}

// ReviewRound is one review of the phase.
type ReviewRound struct {
	Attempt     int      `json:"attempt"`
	Round       int      `json:"round"`
	Verdict     string   `json:"verdict"`
	GatePassed  bool     `json:"gate_passed"`
	TreeHash    string   `json:"tree_hash"`
	NewFindings []string `json:"new_findings"`
	Reasons     []string `json:"reasons"`
}

// Finding is the final state of one code finding.
type Finding struct {
	ID              string `json:"id"`
	Attempt         int    `json:"attempt"`
	Severity        string `json:"severity"`
	Category        string `json:"category"`
	ResolutionClass string `json:"resolution_class"`
	Status          string `json:"status"`
	Title           string `json:"title"`
	File            string `json:"file"`
	LineStart       int    `json:"line_start"`
	LineEnd         int    `json:"line_end"`
	OpenedRound     int    `json:"opened_round"`
	ClosedRound     int    `json:"closed_round"`
	ReopenCount     int    `json:"reopen_count"`
	DisputeCount    int    `json:"dispute_count"`
	Response        string `json:"response"`
	ResponseDetail  string `json:"response_detail"`
	Ruling          string `json:"ruling"`
}

// Deferred is an item recorded for a later phase.
type Deferred struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	Title    string `json:"title"`
	File     string `json:"file"`
	Reason   string `json:"reason"`
}

// Implementation is the implementer's report, checked against the diff.
type Implementation struct {
	Summary    string       `json:"summary"`
	Decisions  []string     `json:"decisions"`
	KnownGaps  []string     `json:"known_gaps"`
	Tests      []TestChange `json:"tests"`
	Unverified []string     `json:"unverified"`
}

// TestChange is a test the implementer reported.
type TestChange struct {
	Path   string   `json:"path"`
	Change string   `json:"change"`
	Covers []string `json:"covers"`
}

// Usage is what the phase consumed.
type Usage struct {
	Agents             []AgentUsage `json:"agents"`
	DurationMS         int64        `json:"duration_ms"`
	SubscriptionPauses int          `json:"subscription_pauses"`
	APIFallbacks       int          `json:"api_fallbacks"`
}

// AgentUsage is the usage of one agent role.
type AgentUsage struct {
	Agent        string  `json:"agent"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	AuthMode     string  `json:"auth_mode"`
	Calls        int     `json:"calls"`
	Turns        int     `json:"turns"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Charged      bool    `json:"charged"`
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// normalize replaces nil lists with empty ones (the schema requires arrays).
func (r *PhaseReport) normalize() {
	r.Sessions = nonNil(r.Sessions)
	r.AcceptanceCriteria = nonNil(r.AcceptanceCriteria)
	for i := range r.AcceptanceCriteria {
		r.AcceptanceCriteria[i].Tests = nonNil(r.AcceptanceCriteria[i].Tests)
	}
	r.Requirements = nonNil(r.Requirements)
	r.Files = nonNil(r.Files)
	r.Gates = nonNil(r.Gates)
	for i := range r.Gates {
		r.Gates[i].Commands = nonNil(r.Gates[i].Commands)
	}
	r.Regressions = nonNil(r.Regressions)
	r.RegressionsSeen = nonNil(r.RegressionsSeen)
	r.Security.Scanners = nonNil(r.Security.Scanners)
	r.Security.NewFindings = nonNil(r.Security.NewFindings)
	r.Security.Preexisting = nonNil(r.Security.Preexisting)
	r.Security.Policy = nonNil(r.Security.Policy)
	r.Reviews = nonNil(r.Reviews)
	for i := range r.Reviews {
		r.Reviews[i].NewFindings = nonNil(r.Reviews[i].NewFindings)
		r.Reviews[i].Reasons = nonNil(r.Reviews[i].Reasons)
	}
	r.Findings = nonNil(r.Findings)
	r.Deferred = nonNil(r.Deferred)
	r.Implementation.Decisions = nonNil(r.Implementation.Decisions)
	r.Implementation.KnownGaps = nonNil(r.Implementation.KnownGaps)
	r.Implementation.Tests = nonNil(r.Implementation.Tests)
	for i := range r.Implementation.Tests {
		r.Implementation.Tests[i].Covers = nonNil(r.Implementation.Tests[i].Covers)
	}
	r.Implementation.Unverified = nonNil(r.Implementation.Unverified)
	r.Usage.Agents = nonNil(r.Usage.Agents)
	if r.Format == "" {
		r.Format = PhaseFormat
	}
}

// JSON returns the report as indented JSON that validates against the
// schema (nil lists become empty lists).
func (r *PhaseReport) JSON() ([]byte, error) {
	r.normalize()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ValidatePhaseJSON checks raw against phase-report.schema.json and
// returns every violation.
func ValidatePhaseJSON(raw []byte) []string {
	probs, err := agent.ValidateSchema(schemas.PhaseReport, raw)
	if err != nil {
		return []string{"phase report schema: " + err.Error()}
	}
	return probs
}

// ErrInvalidReport is wrapped by ParsePhase for a report that does not
// match its schema.
var ErrInvalidReport = errors.New("reports: invalid phase report")

// ParsePhase validates and decodes a Phase Completion Report.
func ParsePhase(raw []byte) (*PhaseReport, error) {
	if probs := ValidatePhaseJSON(raw); len(probs) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidReport, strings.Join(probs, "; "))
	}
	var r PhaseReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	return &r, nil
}

// PhasePaths returns the repository-relative paths of phase n's report.
func PhasePaths(n int) (md, js string) {
	base := fmt.Sprintf("%s/phase-%02d", Dir, n)
	return base + ".md", base + ".json"
}

// maxReportBytes bounds a report read back from the repository.
const maxReportBytes = 8 << 20

// ReadPhase reads and parses phase n's JSON report from repoRoot. Links and
// other non-regular files are refused (never followed).
func ReadPhase(repoRoot string, n int) (*PhaseReport, error) {
	_, js := PhasePaths(n)
	cur := repoRoot
	segs := strings.Split(js, "/")
	var fi fs.FileInfo
	for i, seg := range segs {
		cur = filepath.Join(cur, seg)
		var err error
		if fi, err = os.Lstat(cur); err != nil {
			return nil, err
		}
		last := i == len(segs)-1
		if (!last && fi.Mode().Type() != fs.ModeDir) || (last && !fi.Mode().IsRegular()) {
			return nil, fmt.Errorf("reports: %s: %s is a link or not a plain file or directory", js, seg)
		}
	}
	if fi.Size() > maxReportBytes {
		return nil, fmt.Errorf("reports: %s is too large", js)
	}
	raw, err := os.ReadFile(cur)
	if err != nil {
		return nil, err
	}
	r, err := ParsePhase(raw)
	if err != nil {
		return nil, err
	}
	if r.Phase != n {
		return nil, fmt.Errorf("%w: %s reports phase %d", ErrInvalidReport, js, r.Phase)
	}
	return r, nil
}

// --- digest -------------------------------------------------------------------

// BuildDigest is the hand-off of a completed phase to later phases
// (§11.9): what was built, where, the key decisions, and what was left for
// later. It is bounded by MaxDigestChars.
func BuildDigest(r *PhaseReport) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Phase %02d — %s (committed, attempt %d of %d)\n", r.Phase, oneLine(r.Title), r.Attempt, r.Attempts))
	if g := oneLine(r.Goal); g != "" {
		b.WriteString(fmt.Sprintf("Goal: %s\n", g))
	}
	if s := oneLine(r.Implementation.Summary); s != "" {
		b.WriteString(fmt.Sprintf("Summary: %s\n", s))
	}
	if len(r.Requirements) > 0 {
		b.WriteString(fmt.Sprintf("Requirements: %s\n", strings.Join(r.Requirements, ", ")))
	}
	if len(r.Files) > 0 {
		b.WriteString("Files:\n")
		for i, f := range r.Files {
			if i == 40 {
				b.WriteString(fmt.Sprintf("- … and %d more\n", len(r.Files)-i))
				break
			}
			b.WriteString(fmt.Sprintf("- %s %s\n", f.Status, f.Path))
		}
	}
	if len(r.Implementation.Decisions) > 0 {
		b.WriteString("Key decisions:\n")
		for i, d := range r.Implementation.Decisions {
			if i == 10 {
				break
			}
			b.WriteString(fmt.Sprintf("- %s\n", clip(oneLine(d), 300)))
		}
	}
	if len(r.Deferred) > 0 {
		b.WriteString("Deferred to later phases:\n")
		for i, d := range r.Deferred {
			if i == 15 {
				break
			}
			b.WriteString(fmt.Sprintf("- %s [%s/%s] %s\n", d.ID, d.Severity, d.Category, clip(oneLine(d.Title), 200)))
		}
	}
	if len(r.Implementation.KnownGaps) > 0 {
		b.WriteString("Known gaps:\n")
		for i, g := range r.Implementation.KnownGaps {
			if i == 10 {
				break
			}
			b.WriteString(fmt.Sprintf("- %s\n", clip(oneLine(g), 300)))
		}
	}
	return clip(b.String(), MaxDigestChars)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip shortens s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
