// Package agent defines the roles ACO drives through vendor CLIs — Planner
// (Stage 1), Implementer (Stages 3–4), and Reviewer (Stages 2–4) — and the
// types they share: requests, results, usage, budgets, and the error
// classes of §14.1. Adapters (internal/agent/claudecode) and scripted fakes
// (internal/agent/fake) implement the interfaces, so the engine is testable
// offline.
package agent

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/plan"
)

// Planner creates and revises plan packages (Stage 1, §10.2). It is
// read-only: the package comes back as structured data and the
// orchestrator writes the files.
type Planner interface {
	Create(ctx context.Context, req PlanRequest) (*PlanResult, error)
	// Revise resumes the Create session with review findings (§10.4).
	Revise(ctx context.Context, req PlanRevision) (*PlanResult, error)
}

// Implementer implements a phase (Stage 3) or a final fix (Stage 4) in the
// run worktree.
type Implementer interface {
	// Start runs a fresh session (§11.3 step 5).
	Start(ctx context.Context, req ImplementRequest) (*ImplResult, error)
	// Continue resumes a session for gate fixes, review fixes, or to finish
	// an incomplete report (§11.7, §14.1).
	Continue(ctx context.Context, req ContinueRequest) (*ImplResult, error)
}

// Effort levels Claude Code accepts.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// Output says where an invocation stores its artifacts: Prefix is a
// directory inside the run directory (artifacts layout, e.g.
// "phases/P01/a1/step-3-implement").
type Output struct {
	Run    *artifacts.RunDir
	Prefix string
}

// Workspace is the directory an agent works in and the guard policy for it.
type Workspace struct {
	Dir              string   // worktree (implementer) or repository (planner); absolute
	ProtectedPaths   []string // config policy.protected_paths
	ScopePaths       []string // phase scope.paths (implementer)
	ScopeEnforcement string   // flag | block
	Network          string   // deny | allowlist | allow
	NetworkAllow     []string // hosts for WebFetch in allowlist mode
}

// Scope is a budget and usage scope (§14.3).
type Scope string

// Scopes.
const (
	ScopeRun      Scope = "run"
	ScopePlanning Scope = "planning"
	ScopePhase    Scope = "phase"
	ScopeFinal    Scope = "final"
)

// Usage is what one or more invocations consumed, as the CLI reported it.
type Usage struct {
	InputTokens         int64         `json:"input_tokens"`
	OutputTokens        int64         `json:"output_tokens"`
	CacheReadTokens     int64         `json:"cache_read_tokens"`
	CacheCreationTokens int64         `json:"cache_creation_tokens"`
	Turns               int           `json:"turns"`
	CostUSD             float64       `json:"cost_usd"` // total_cost_usd (an estimate in subscription mode)
	Duration            time.Duration `json:"duration"`
	// ReasoningTokens is the part of OutputTokens spent on reasoning (Codex).
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.Turns += o.Turns
	u.CostUSD += o.CostUSD
	u.Duration += o.Duration
	u.ReasoningTokens += o.ReasoningTokens
}

// GuardDenial is one tool call the guard hook denied during an invocation.
type GuardDenial struct {
	Tool string `json:"tool"`
	Rule string `json:"rule"`
	Path string `json:"path,omitempty"`
}

// Invocation is the metadata of one finished CLI call.
type Invocation struct {
	SessionID  string
	Model      string // as reported by the CLI
	CLIVersion string
	AuthMode   string // resolved mode: subscription | api
	AuthSource string // what the CLI reported (Claude: apiKeySource)
	Usage      Usage
	// Charged is true when Usage.CostUSD was counted against the budget (api mode).
	Charged      bool
	Budget       BudgetStatus
	GuardDenials []GuardDenial
	// ResultText is the CLI's final text, masked.
	ResultText string
	// Artifact paths relative to the run directory.
	PromptRef, TranscriptRef, StderrRef, SettingsRef, GuardPolicyRef string
	// OutputRef is the CLI's final message (Codex -o), masked.
	OutputRef string
	// Priced is true when CostUSD was computed from a configured price
	// (Codex in api mode); false means only the token counts are known.
	Priced bool
	// Violations are confinement breaches seen in the reviewer's stream.
	Violations []AccessViolation
}

// PlanFinding is a plan-review finding sent back for revision (§10.3).
type PlanFinding struct {
	ID             string `json:"id"`
	Severity       string `json:"severity"`
	Dimension      string `json:"dimension"`
	Location       string `json:"location"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	RequiredChange string `json:"required_change"`
}

// PlanRequest starts Stage 1.
type PlanRequest struct {
	RunID           string
	Workspace       Workspace // the repository, read-only
	Output          Output
	Requirements    string // requirement document text
	SuppliedPlan    string // a plan the user gave (--plan), to be converted and improved; optional
	RepoOverview    string // tree, languages, existing tests
	AgentsMD        string // existing AGENTS.md ("" = none: the planner proposes one)
	PlanFormatGuide string
	GateCatalogue   string
	BudgetScopes    []Scope
}

// PlanRevision resumes the planning session with review results.
type PlanRevision struct {
	RunID        string
	SessionID    string
	Workspace    Workspace
	Output       Output
	Round        int
	Findings     []PlanFinding
	CheckResults string // deterministic plan checks, rendered
	BudgetScopes []Scope
}

// FindingResponse answers one finding (plan or code).
type FindingResponse struct {
	ID       string `json:"id"`
	Response string `json:"response"` // fixed | disputed | cannot_fix_within_spec | cannot_fix_safely
	Detail   string `json:"detail"`
}

// Finding responses.
const (
	ResponseFixed               = "fixed"
	ResponseDisputed            = "disputed"
	ResponseCannotFixWithinSpec = "cannot_fix_within_spec"
	ResponseCannotFixSafely     = "cannot_fix_safely"
)

// PlanResult is a plan package returned by the planner.
type PlanResult struct {
	Invocation
	// Raw is the structured plan package (plan.schema.json) as returned.
	Raw json.RawMessage
	// Package is Raw parsed; its Diagnostics hold parse problems. The
	// deterministic plan checks are the engine's job.
	Package *plan.Package
	// Responses answer the revision's findings (Revise only).
	Responses []FindingResponse
}

// ImplementKind selects the implementer prompt.
type ImplementKind string

// Implement kinds.
const (
	KindPhase    ImplementKind = "phase"
	KindFinalFix ImplementKind = "final_fix"
)

// PhaseDigest is the hand-off of a completed phase (§11.9).
type PhaseDigest struct {
	Phase  int
	Title  string
	Digest string
}

// GateSpec is a quality gate the implementation will be judged by.
type GateSpec struct {
	Type          string
	Commands      []string
	NotApplicable bool
}

// ImplementRequest starts a fresh implementer session.
type ImplementRequest struct {
	RunID        string
	Kind         ImplementKind
	Phase        int // 0 for final fix
	PhaseTitle   string
	Attempt      int
	Workspace    Workspace
	Output       Output
	PlanOverview string
	PhaseSpec    string // verbatim Phase Specification
	AgentsMD     string
	Digests      []PhaseDigest
	Gates        []GateSpec
	ACIDs        []string
	Lessons      string // brief from a previous attempt (phase restart)
	Failures     string // final fix: failing Stage 4 results
	BudgetScopes []Scope
}

// ContinueKind selects the continuation prompt.
type ContinueKind string

// Continue kinds.
const (
	ContinueGateFix   ContinueKind = "gate_fix"
	ContinueReviewFix ContinueKind = "review_fix"
	ContinueFinish    ContinueKind = "continue"
)

// GateFailure is a failed quality gate sent to the implementer.
type GateFailure struct {
	ID      string // finding-style ID the report answers, e.g. "G-unit-1"
	Gate    string
	Command string
	Log     string // trimmed and masked
}

// CodeFinding is a review finding sent to the implementer (§11.7).
type CodeFinding struct {
	ID             string
	Severity       string
	Category       string
	Location       string
	Title          string
	Description    string
	RequiredChange string
}

// ContinueRequest resumes an implementer session.
type ContinueRequest struct {
	RunID        string
	SessionID    string
	Kind         ContinueKind
	Phase        int
	Workspace    Workspace
	Output       Output
	GateFailures []GateFailure
	Findings     []CodeFinding
	Reason       string // ContinueFinish: why the last invocation stopped
	ACIDs        []string
	BudgetScopes []Scope
}

// ImplResult is what an implementer invocation returns.
type ImplResult struct {
	Invocation
	Raw    json.RawMessage
	Report *ImplReport
}

// PlanPackageFiles returns the files of a plan package for a context pack:
// the source files byte-for-byte when the package was loaded from disk,
// otherwise the rendered package (structured planner output).
func PlanPackageFiles(p *plan.Package) []PackFile {
	var out []PackFile
	if len(p.Files) > 0 {
		for _, f := range p.Files {
			out = append(out, PackFile{Path: f.Path, Content: string(f.Raw)})
		}
		return out
	}
	for path, b := range plan.RenderFiles(p) {
		out = append(out, PackFile{Path: path, Content: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
