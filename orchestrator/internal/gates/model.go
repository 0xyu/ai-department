// Package gates is the quality gate runner (§11.5, §12): it resolves the six
// gate types (typecheck, lint, unit, integration, build, security) from the
// Phase Specification, the config, or auto-detection; runs them in the
// worktree with an allowlisted environment, timeouts, and process-tree kill;
// parses machine-readable output (go test -json, JUnit XML, ESLint and
// golangci-lint JSON, SARIF, govulncheck, npm audit, pip-audit); and compares
// the results with a baseline so that only failures beyond the baseline fail
// a gate.
//
// Gates run independently of the agents: nothing here calls Claude or Codex.
// Every result is bound to the tree hash it ran on, redacted, size-bounded,
// and JSON-serializable for the reviewer context pack and reports.
package gates

import (
	"fmt"
	"time"

	"github.com/0xyu/aco/internal/config"
)

// Gate types, in execution order (§11.5).
const (
	Typecheck   = "typecheck"
	Lint        = "lint"
	Unit        = "unit"
	Integration = "integration"
	Build       = "build"
	Security    = "security"
)

// Types lists the six gate types in execution order.
var Types = config.GateTypes

// Status is the outcome of a gate or a command.
type Status string

// Statuses. Timeout and Error are failures that are not the code's fault as
// far as ACO can tell; Skipped is a gate after a fail-fast stop.
const (
	Passed        Status = "passed"
	Failed        Status = "failed"
	Timeout       Status = "timeout"
	Errored       Status = "error"
	NotApplicable Status = "not_applicable"
	Skipped       Status = "skipped"
)

// OK reports whether s does not fail a round.
func (s Status) OK() bool { return s == Passed || s == NotApplicable }

// DBStatus maps s onto the gate_results.status column (passed | failed |
// not_applicable | error). Skipped gates are not stored.
func (s Status) DBStatus() string {
	switch s {
	case Passed, Failed, NotApplicable:
		return string(s)
	default:
		return "error"
	}
}

// Class tells triage (§11.7) why a gate did not pass.
type Class string

// Failure classes.
const (
	ClassNone        Class = ""
	ClassQuality     Class = "quality"     // the code fails the gate: fix it
	ClassPolicy      Class = "policy"      // diff policy / anti-gaming / secret: revert or fix
	ClassEnvironment Class = "environment" // command not found, service unreachable: BLOCKED (§14.1)
	ClassTimeout     Class = "timeout"     // killed at its timeout
	ClassInternal    Class = "internal"    // ACO could not run or read the gate
)

// Source says where a gate's commands came from.
type Source string

// Command sources, in precedence order.
const (
	SourcePhase  Source = "phase"  // frozen Phase Specification
	SourceConfig Source = "config" // gates.<type>.commands
	SourceAuto   Source = "auto"   // auto-detected from the repository
	SourceNone   Source = ""
)

// Mode selects how much a gate checks.
type Mode string

// Modes.
const (
	// ModePhase checks one phase: the security gate scans the phase diff.
	ModePhase Mode = "phase"
	// ModeFull is Stage 4 (§12): all packages, every phase's declared
	// commands, and a secret scan of all files and the run's history.
	ModeFull Mode = "full"
)

// Scanner names for the security gate (config.SecurityScanners).
const (
	ScannerSecrets     = "secrets"
	ScannerGosec       = "gosec"
	ScannerGovulncheck = "govulncheck"
	ScannerSemgrep     = "semgrep"
	ScannerNpmAudit    = "npm-audit"
	ScannerPipAudit    = "pip-audit"
	ScannerTrivy       = "trivy"
)

// Command is one resolved gate command.
type Command struct {
	// Run is the shell command line. The placeholder {reports} is replaced
	// by a quoted directory outside the worktree where the command may
	// write report files (JUnit XML, SARIF, JSON); every file left there is
	// parsed.
	Run string `json:"run"`
	// Scanner is set for a security scanner command; its result comes from
	// its parsed output, not from its exit code alone.
	Scanner string `json:"scanner,omitempty"`
}

// Spec is one resolved gate.
type Spec struct {
	Type          string        `json:"type"`
	Required      bool          `json:"required"`
	Justification string        `json:"justification,omitempty"` // not_applicable only
	Source        Source        `json:"source"`
	Commands      []Command     `json:"commands"`
	Scanners      []string      `json:"scanners,omitempty"` // security only
	Timeout       time.Duration `json:"timeout"`            // per command
}

// Severity of a finding. The order is low < medium < high < critical.
type Severity string

// Severities.
const (
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevCritical Severity = "critical"
)

// Rank orders severities; unknown ranks as high (fail closed).
func (s Severity) Rank() int {
	switch s {
	case SevLow:
		return 1
	case SevMedium:
		return 2
	case SevHigh:
		return 3
	case SevCritical:
		return 4
	}
	return 3
}

// AtLeast reports whether s is at or above min.
func (s Severity) AtLeast(min Severity) bool { return s.Rank() >= min.Rank() }

// TestStatus is one test's outcome.
type TestStatus string

// Test outcomes.
const (
	TestPass TestStatus = "pass"
	TestFail TestStatus = "fail"
	TestSkip TestStatus = "skip"
)

// TestCase is one test result from machine-readable output. A package or
// suite that failed without a failing test (build error, panic in init) is a
// TestCase named after the package with Package set and Name empty.
type TestCase struct {
	Package string     `json:"package,omitempty"`
	Name    string     `json:"name,omitempty"`
	Status  TestStatus `json:"status"`
}

// ID is the stable test identity used for baseline comparison.
func (t TestCase) ID() string {
	if t.Name == "" {
		return t.Package
	}
	if t.Package == "" {
		return t.Name
	}
	return t.Package + "." + t.Name
}

// Issue is a lint or static-analysis finding.
type Issue struct {
	Tool     string   `json:"tool"`
	Rule     string   `json:"rule,omitempty"`
	Path     string   `json:"path,omitempty"`
	Line     int      `json:"line,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	// Blocking is set for policy findings that block on their own.
	Blocking bool `json:"blocking,omitempty"`
}

// Key is the issue identity for baseline comparison. Line numbers are left
// out so that edits above an existing issue do not make it "new".
// Secret findings keep their line (fail closed: a second secret in the same
// file must not hide behind an existing one).
func (i Issue) Key() string {
	k := i.Tool + "\x00" + i.Rule + "\x00" + i.Path + "\x00" + i.Message
	if i.Tool == ToolSecrets {
		k += fmt.Sprintf("\x00%d", i.Line)
	}
	return k
}

// Vuln is a dependency vulnerability.
type Vuln struct {
	Tool     string   `json:"tool"`
	ID       string   `json:"id"` // GO-…, GHSA-…, CVE-…, PYSEC-…
	Package  string   `json:"package"`
	Version  string   `json:"version,omitempty"`
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary,omitempty"`
}

// Key is the vulnerability identity for baseline comparison: the same
// advisory in the same package, whatever the installed version.
func (v Vuln) Key() string { return v.ID + "\x00" + v.Package }

// CommandResult is one executed command.
type CommandResult struct {
	Command  Command       `json:"command"`
	Status   Status        `json:"status"`
	Class    Class         `json:"class,omitempty"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ns"`
	// Log is the combined stdout and stderr, redacted and trimmed to
	// MaxLogBytes with the first failure kept (see Trim).
	Log string `json:"log"`
	// LogBytes is the size of the full output before trimming.
	LogBytes  int64 `json:"log_bytes"`
	Truncated bool  `json:"truncated,omitempty"`
	// Parsed is set when machine-readable output was found.
	Parsed bool `json:"parsed,omitempty"`
	// Reason explains Errored and environment results.
	Reason string `json:"reason,omitempty"`
	// Signatures are normalized failure lines of a failed command without
	// structured results, used to compare it with the baseline.
	Signatures []string `json:"signatures,omitempty"`
	// LogArtifact is the full redacted log in the run's artifacts, if kept.
	LogArtifact string `json:"log_artifact,omitempty"`
}

// Result is one gate's outcome on one tree.
type Result struct {
	Gate          string          `json:"gate"`
	Status        Status          `json:"status"`
	Class         Class           `json:"class,omitempty"`
	Required      bool            `json:"required"`
	Justification string          `json:"justification,omitempty"`
	Source        Source          `json:"source,omitempty"`
	TreeHash      string          `json:"tree_hash"`
	TreeAfter     string          `json:"tree_after,omitempty"` // the first other tree seen at a command boundary (the gate changed the working state, even if it restored it later)
	Commands      []CommandResult `json:"commands"`
	Duration      time.Duration   `json:"duration_ns"`

	Tests  []TestCase `json:"tests,omitempty"`
	Issues []Issue    `json:"issues,omitempty"`
	Vulns  []Vuln     `json:"vulns,omitempty"`
	// Comparison is set when the result was compared with a baseline.
	Comparison *Comparison `json:"comparison,omitempty"`
	// Summary is a short line in the document language (English) for
	// reports and the reviewer; operator messages come from the i18n
	// catalog in the engine.
	Summary string `json:"summary,omitempty"`
}

// FailedCommand returns the first command that did not pass, or nil.
func (r *Result) FailedCommand() *CommandResult {
	for i := range r.Commands {
		if r.Commands[i].Status != Passed {
			return &r.Commands[i]
		}
	}
	return nil
}

// Round is one ordered run of the gates.
type Round struct {
	TreeHash string    `json:"tree_hash"`
	Mode     Mode      `json:"mode"`
	Baseline bool      `json:"baseline,omitempty"`
	Results  []*Result `json:"results"`
}

// Passed reports whether every gate passed or is not applicable.
func (r *Round) Passed() bool {
	for _, g := range r.Results {
		if !g.Status.OK() {
			return false
		}
	}
	return true
}

// FirstFailure returns the first gate that did not pass, or nil.
func (r *Round) FirstFailure() *Result {
	for _, g := range r.Results {
		if !g.Status.OK() {
			return g
		}
	}
	return nil
}

// Gate returns the result for gate type t, or nil.
func (r *Round) Gate(t string) *Result {
	for _, g := range r.Results {
		if g.Gate == t {
			return g
		}
	}
	return nil
}
