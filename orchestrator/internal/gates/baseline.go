package gates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/state"
)

// Baseline comparison (§11.3 step 4, §11.5): a gate passes when it has no
// failure beyond the baseline — no new failing test, no new lint issue, no
// new vulnerability or SAST finding at or above security.fail_severity, no
// new unstructured failure, and no blocking policy finding. Pre-existing
// failures are reported but do not block.

// MaxListed bounds every list in a Comparison and in a compact Result.
const MaxListed = 200

// Comparison is a gate result compared with its baseline.
type Comparison struct {
	BaselineTree string `json:"baseline_tree,omitempty"`
	HasBaseline  bool   `json:"has_baseline"`

	// NewFailingTests fail now and did not fail at baseline (including
	// tests that did not exist then). Regressions are the subset that
	// passed at baseline.
	NewFailingTests     []string `json:"new_failing_tests,omitempty"`
	Regressions         []string `json:"regressions,omitempty"`
	PreexistingFailures []string `json:"preexisting_failures,omitempty"`
	FixedTests          []string `json:"fixed_tests,omitempty"`

	NewIssues         []Issue `json:"new_issues,omitempty"`
	BlockingIssues    []Issue `json:"blocking_issues,omitempty"`
	PreexistingIssues int     `json:"preexisting_issues"`

	NewVulns         []Vuln `json:"new_vulns,omitempty"`
	BlockingVulns    []Vuln `json:"blocking_vulns,omitempty"`
	PreexistingVulns []Vuln `json:"preexisting_vulns,omitempty"`

	// NewFailures are commands that fail without structured results and
	// whose failure lines are not all in the baseline's.
	NewFailures         []string `json:"new_failures,omitempty"`
	PreexistingCommands []string `json:"preexisting_command_failures,omitempty"`

	// Counts before the lists were bounded.
	Counts map[string]int `json:"counts,omitempty"`
}

// Blocking reports whether the comparison fails the gate.
func (c *Comparison) Blocking() bool {
	return len(c.NewFailingTests) > 0 || len(c.BlockingIssues) > 0 || len(c.BlockingVulns) > 0 || len(c.NewFailures) > 0
}

// Evaluate sets res.Status, Class, Comparison, and Summary from its commands
// and the baseline result for the same gate (nil: no baseline, every
// failure is new).
func Evaluate(res *Result, base *Result, failSev Severity) {
	if res.Status == NotApplicable || res.Status == Skipped {
		return
	}
	if failSev == "" {
		failSev = SevHigh
	}
	cmp := compare(res, base, failSev)
	res.Comparison = cmp
	for _, c := range res.Commands {
		if c.Status == Timeout {
			res.Status, res.Class = Timeout, ClassTimeout
			res.Summary = fmt.Sprintf("%s: command timed out: %s", res.Gate, c.Command.Run)
			return
		}
	}
	for _, c := range res.Commands {
		if c.Class == ClassEnvironment {
			res.Status, res.Class = Failed, ClassEnvironment
			res.Summary = fmt.Sprintf("%s: environment problem (%s): %s", res.Gate, c.Reason, c.Command.Run)
			return
		}
	}
	for _, c := range res.Commands {
		if c.Status == Errored {
			res.Status, res.Class = Errored, ClassInternal
			res.Summary = fmt.Sprintf("%s: %s: %s", res.Gate, c.Reason, c.Command.Run)
			return
		}
	}
	if !cmp.Blocking() {
		res.Status, res.Class = Passed, ClassNone
		res.Summary = res.Gate + ": passed" + preexistingNote(cmp)
		return
	}
	res.Status, res.Class = Failed, ClassQuality
	policyOnly := len(cmp.NewFailingTests) == 0 && len(cmp.BlockingVulns) == 0 && len(cmp.NewFailures) == 0
	for _, i := range cmp.BlockingIssues {
		if i.Tool != ToolPolicy && i.Tool != ToolSecrets {
			policyOnly = false
		}
	}
	if policyOnly {
		res.Class = ClassPolicy
	}
	var parts []string
	if n := cmp.Counts["new_failing_tests"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d new failing test(s), %d regression(s)", n, cmp.Counts["regressions"]))
	}
	if n := cmp.Counts["blocking_issues"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d new blocking issue(s)", n))
	}
	if n := cmp.Counts["blocking_vulns"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d new vulnerabilit(ies) at or above %s", n, failSev))
	}
	if n := cmp.Counts["new_failures"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d failing command(s)", n))
	}
	res.Summary = res.Gate + ": failed: " + strings.Join(parts, "; ") + preexistingNote(cmp)
}

func preexistingNote(c *Comparison) string {
	n := c.Counts["preexisting_failures"] + c.PreexistingIssues + c.Counts["preexisting_vulns"] + c.Counts["preexisting_command_failures"]
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d pre-existing finding(s) not blocking)", n)
}

func compare(res, base *Result, failSev Severity) *Comparison {
	c := &Comparison{Counts: map[string]int{}}
	if base != nil {
		c.HasBaseline, c.BaselineTree = true, base.TreeHash
	}

	// Tests.
	baseStatus := map[string]TestStatus{}
	if base != nil {
		for _, t := range base.Tests {
			baseStatus[t.ID()] = t.Status
		}
	}
	nowFailing := map[string]bool{}
	for _, t := range res.Tests {
		if t.Status != TestFail {
			continue
		}
		id := t.ID()
		if nowFailing[id] {
			continue
		}
		nowFailing[id] = true
		switch baseStatus[id] {
		case TestFail:
			c.PreexistingFailures = append(c.PreexistingFailures, id)
		case TestPass:
			c.NewFailingTests = append(c.NewFailingTests, id)
			c.Regressions = append(c.Regressions, id)
		default:
			c.NewFailingTests = append(c.NewFailingTests, id)
		}
	}
	if base != nil {
		seen := map[string]bool{}
		for _, t := range base.Tests {
			if t.Status == TestFail && !nowFailing[t.ID()] && !seen[t.ID()] {
				seen[t.ID()] = true
				c.FixedTests = append(c.FixedTests, t.ID())
			}
		}
	}

	// Issues.
	baseIssues := map[string]bool{}
	if base != nil {
		for _, i := range base.Issues {
			baseIssues[i.Key()] = true
		}
	}
	for _, i := range res.Issues {
		if i.Tool == ToolPolicy || (i.Tool == ToolSecrets && i.Blocking) {
			// Policy findings judge this diff; the baseline does not apply.
			if i.Blocking {
				c.BlockingIssues = append(c.BlockingIssues, i)
			} else {
				c.NewIssues = append(c.NewIssues, i)
			}
			continue
		}
		if baseIssues[i.Key()] {
			c.PreexistingIssues++
			continue
		}
		c.NewIssues = append(c.NewIssues, i)
		if issueBlocks(res.Gate, i, failSev) {
			c.BlockingIssues = append(c.BlockingIssues, i)
		}
	}

	// Vulnerabilities.
	baseVulns := map[string]bool{}
	if base != nil {
		for _, v := range base.Vulns {
			baseVulns[v.Key()] = true
		}
	}
	seenV := map[string]bool{}
	for _, v := range res.Vulns {
		if seenV[v.Key()] {
			continue
		}
		seenV[v.Key()] = true
		if baseVulns[v.Key()] {
			c.PreexistingVulns = append(c.PreexistingVulns, v)
			continue
		}
		c.NewVulns = append(c.NewVulns, v)
		if v.Severity.AtLeast(failSev) {
			c.BlockingVulns = append(c.BlockingVulns, v)
		}
	}

	// Unstructured command failures.
	for _, cr := range res.Commands {
		if cr.Status != Failed || cr.Class != ClassQuality {
			continue
		}
		if len(cr.Signatures) == 0 && len(nowFailing) > 0 {
			continue // explained by the failing tests above
		}
		if bc := baseCommand(base, cr.Command.Run); bc != nil && bc.Status == Failed && subset(cr.Signatures, bc.Signatures) {
			c.PreexistingCommands = append(c.PreexistingCommands, cr.Command.Run)
			continue
		}
		c.NewFailures = append(c.NewFailures, cr.Command.Run)
	}

	c.bound()
	return c
}

// issueBlocks reports whether a new issue fails its gate: in the lint and
// typecheck gates every new issue above low (warnings stay advisory); in
// the security gate a new finding at or above fail_severity.
func issueBlocks(gate string, i Issue, failSev Severity) bool {
	if gate == Security {
		return i.Severity.AtLeast(failSev)
	}
	return i.Severity.AtLeast(SevMedium)
}

func baseCommand(base *Result, run string) *CommandResult {
	if base == nil {
		return nil
	}
	for i := range base.Commands {
		if base.Commands[i].Command.Run == run {
			return &base.Commands[i]
		}
	}
	return nil
}

func subset(a, b []string) bool {
	m := make(map[string]bool, len(b))
	for _, s := range b {
		m[s] = true
	}
	for _, s := range a {
		if !m[s] {
			return false
		}
	}
	return true
}

func (c *Comparison) bound() {
	str := func(name string, l *[]string) {
		c.Counts[name] = len(*l)
		if len(*l) > MaxListed {
			*l = (*l)[:MaxListed]
		}
	}
	iss := func(name string, l *[]Issue) {
		c.Counts[name] = len(*l)
		if len(*l) > MaxListed {
			*l = (*l)[:MaxListed]
		}
	}
	vul := func(name string, l *[]Vuln) {
		c.Counts[name] = len(*l)
		if len(*l) > MaxListed {
			*l = (*l)[:MaxListed]
		}
	}
	str("new_failing_tests", &c.NewFailingTests)
	str("regressions", &c.Regressions)
	str("preexisting_failures", &c.PreexistingFailures)
	str("fixed_tests", &c.FixedTests)
	iss("new_issues", &c.NewIssues)
	iss("blocking_issues", &c.BlockingIssues)
	vul("new_vulns", &c.NewVulns)
	vul("blocking_vulns", &c.BlockingVulns)
	vul("preexisting_vulns", &c.PreexistingVulns)
	str("new_failures", &c.NewFailures)
	str("preexisting_command_failures", &c.PreexistingCommands)
}

// Compact returns a size-bounded copy of r for the reviewer context pack
// and reports: only failing tests (at most MaxListed), at most MaxListed
// issues and vulnerabilities, and command logs cut to maxLog bytes each.
func (r *Result) Compact(maxLog int) *Result {
	if maxLog <= 0 {
		maxLog = 64 << 10
	}
	q := *r
	q.Tests = nil
	for _, t := range r.Tests {
		if t.Status == TestFail && len(q.Tests) < MaxListed {
			q.Tests = append(q.Tests, t)
		}
	}
	if len(r.Issues) > MaxListed {
		q.Issues = r.Issues[:MaxListed]
	}
	if len(r.Vulns) > MaxListed {
		q.Vulns = r.Vulns[:MaxListed]
	}
	q.Commands = make([]CommandResult, len(r.Commands))
	for i, c := range r.Commands {
		if len(c.Log) > maxLog {
			b, _ := Trim([]byte(c.Log), maxLog)
			c.Log = string(b)
			c.Truncated = true
		}
		if len(c.Signatures) > 20 {
			c.Signatures = c.Signatures[:20]
		}
		q.Commands[i] = c
	}
	return &q
}

// --- baseline store -----------------------------------------------------------

// BaselineVersion is the baseline file format version.
const BaselineVersion = 1

// Baseline is the stored result of all gates at one commit: the regression
// reference for the next phase (§11.3 step 4) and, for the run's first
// baseline, for Stage 4 (§12.2).
type Baseline struct {
	Version   int                `json:"version"`
	Commit    string             `json:"commit"`
	TreeHash  string             `json:"tree_hash"`
	Mode      Mode               `json:"mode"`
	CreatedAt time.Time          `json:"created_at"`
	Results   map[string]*Result `json:"results"`
}

// NewBaseline records a round as the baseline for commit. Full test lists
// are kept (comparison needs them); logs are compacted.
func NewBaseline(r *Round, commit string, now time.Time) *Baseline {
	b := &Baseline{Version: BaselineVersion, Commit: commit, TreeHash: r.TreeHash, Mode: r.Mode, CreatedAt: now.UTC(),
		Results: map[string]*Result{}}
	for _, g := range r.Results {
		c := g.Compact(16 << 10)
		c.Tests = g.Tests
		c.Issues = g.Issues
		c.Vulns = g.Vulns
		b.Results[g.Gate] = c
	}
	return b
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// ErrNoBaseline is returned by Load when no baseline exists for a commit.
var ErrNoBaseline = errors.New("gates: no baseline for this commit")

// BaselineStore keeps baselines in a run's artifact directory, one file
// per commit (baselines/<sha>.json).
type BaselineStore struct{ Dir *artifacts.RunDir }

func baselinePath(commit string) (string, error) {
	if !shaRE.MatchString(commit) {
		return "", fmt.Errorf("gates: baseline key must be a full commit SHA")
	}
	return "baselines/" + commit + ".json", nil
}

// Save stores b under its commit.
func (s BaselineStore) Save(b *Baseline) (artifacts.Ref, error) {
	p, err := baselinePath(b.Commit)
	if err != nil {
		return artifacts.Ref{}, err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return artifacts.Ref{}, err
	}
	return s.Dir.Write(p, data)
}

// Load returns the baseline for commit, or ErrNoBaseline.
func (s BaselineStore) Load(commit string) (*Baseline, error) {
	p, err := baselinePath(commit)
	if err != nil {
		return nil, err
	}
	data, err := s.Dir.Read(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoBaseline
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("gates: baseline %s: %w", commit, err)
	}
	if b.Version != BaselineVersion {
		return nil, fmt.Errorf("gates: baseline %s has unsupported version %d", commit, b.Version)
	}
	return &b, nil
}

// StateRecords converts a round into gate_results rows. Skipped gates are
// not stored. The summary is the compact result as JSON.
func StateRecords(r *Round, runID string, phase, attempt, roundNo int) ([]state.GateResult, error) {
	var out []state.GateResult
	for _, g := range r.Results {
		if g.Status == Skipped {
			continue
		}
		sum, err := json.Marshal(g.Compact(8 << 10))
		if err != nil {
			return nil, err
		}
		rec := state.GateResult{RunID: runID, Phase: phase, Attempt: attempt, Round: roundNo, Gate: g.Gate,
			IsBaseline: r.Baseline, Status: g.Status.DBStatus(), Duration: g.Duration, TreeHash: g.TreeHash, Summary: sum}
		var runs []string
		for _, c := range g.Commands {
			runs = append(runs, c.Command.Run)
		}
		rec.Command = strings.Join(runs, "\n")
		if fc := g.FailedCommand(); fc != nil {
			rec.ExitCode = fc.ExitCode
			rec.LogArtifact = fc.LogArtifact
		} else if len(g.Commands) > 0 {
			rec.LogArtifact = g.Commands[0].LogArtifact
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return gateIndex(out[i].Gate) < gateIndex(out[j].Gate) })
	return out, nil
}

func gateIndex(t string) int {
	for i, x := range Types {
		if x == t {
			return i
		}
	}
	return len(Types)
}
