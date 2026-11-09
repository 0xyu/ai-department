// Package config defines the ACO configuration schema, its defaults, layered
// loading, and validation.
//
// Layers, lowest to highest precedence:
//
//	defaults ← ~/.aco/config.yaml ← <repo>/.aco.yaml ← --config files ← plan front matter ← --set flags
//
// Config files never hold secrets: unknown keys are errors and key names that
// look like credentials (api_key, token, secret, …) get a dedicated error.
package config

// CurrentVersion is the only supported config schema version.
const CurrentVersion = 1

// Config is the full ACO configuration (§18 of the implementation plan).
type Config struct {
	Version     int               `yaml:"version"`
	Planner     AgentConfig       `yaml:"planner"`
	Implementer ImplementerConfig `yaml:"implementer"`
	Reviewer    ReviewerConfig    `yaml:"reviewer"`
	Auth        AuthConfig        `yaml:"auth"`
	Pricing     PricingConfig     `yaml:"pricing"`
	Language    LanguageConfig    `yaml:"language"`
	Gates       GatesConfig       `yaml:"gates"`
	Policy      PolicyConfig      `yaml:"policy"`
	Git         GitConfig         `yaml:"git"`
	Sandbox     SandboxConfig     `yaml:"sandbox"`
	Limits      LimitsConfig      `yaml:"limits"`
}

// AgentConfig configures a Claude Code agent role (planner).
type AgentConfig struct {
	Kind   string `yaml:"kind"`   // claude-code | fake
	Model  string `yaml:"model"`  // e.g. claude-opus-5-5
	Effort string `yaml:"effort"` // low | medium | high | xhigh | max
}

// ImplementerConfig configures the Stage 3/4 implementer.
type ImplementerConfig struct {
	Kind             string   `yaml:"kind"`
	Model            string   `yaml:"model"`
	Effort           string   `yaml:"effort"`
	StrictEffort     bool     `yaml:"strict_effort"`
	ExtraAllowedBash []string `yaml:"extra_allowed_bash"`
}

// ReviewerConfig configures the GPT reviewer (Codex CLI).
type ReviewerConfig struct {
	Kind            string `yaml:"kind"` // codex-cli | fake
	Model           string `yaml:"model"`
	FallbackModel   string `yaml:"fallback_model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	FinalReview     bool   `yaml:"final_review"`
}

// AuthConfig holds auth-mode defaults. Environment variables and flags
// override it (§16.5). It intentionally has no credential fields.
type AuthConfig struct {
	Mode     string `yaml:"mode"`     // subscription | api
	Claude   string `yaml:"claude"`   // "" | subscription | api
	Codex    string `yaml:"codex"`    // "" | subscription | api
	Fallback string `yaml:"fallback"` // none | api
	EnvFile  string `yaml:"env_file"`
}

// PricingConfig holds per-model token prices used for API-mode budgets.
type PricingConfig struct {
	Codex map[string]Price `yaml:"codex"`
}

// Price is a token price in USD per million tokens.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// LanguageConfig selects languages for operator text and documents.
type LanguageConfig struct {
	Operator  string `yaml:"operator"`  // zh-CN | en
	Documents string `yaml:"documents"` // en | zh-CN
}

// GatesConfig configures the six quality gate types. No other gate types exist.
type GatesConfig struct {
	Typecheck   GateConfig         `yaml:"typecheck"`
	Lint        GateConfig         `yaml:"lint"`
	Unit        GateConfig         `yaml:"unit"`
	Integration GateConfig         `yaml:"integration"`
	Build       GateConfig         `yaml:"build"`
	Security    SecurityGateConfig `yaml:"security"`
	AutoDetect  bool               `yaml:"auto_detect"`
}

// GateTypes lists the six gate type names in execution order.
var GateTypes = []string{"typecheck", "lint", "unit", "integration", "build", "security"}

// GateConfig configures one command-based gate.
type GateConfig struct {
	Commands []string `yaml:"commands"`
	Timeout  Duration `yaml:"timeout"`
}

// SecurityGateConfig configures the security gate.
type SecurityGateConfig struct {
	Commands     []string `yaml:"commands"`
	Scanners     []string `yaml:"scanners"`
	FailSeverity string   `yaml:"fail_severity"`
	Timeout      Duration `yaml:"timeout"`
}

// PolicyConfig configures safety policy.
type PolicyConfig struct {
	ProtectedPaths      []string `yaml:"protected_paths"`
	ScopeEnforcement    string   `yaml:"scope_enforcement"` // flag | block
	TestsMissing        string   `yaml:"tests_missing"`     // warn | fail
	AntiGaming          string   `yaml:"anti_gaming"`       // warn | fail
	AllowUnverifiableAC bool     `yaml:"allow_unverifiable_ac"`
	Network             string   `yaml:"network"`        // deny | allowlist | allow
	MaxFileBytes        int      `yaml:"max_file_bytes"` // largest file a phase diff may add or change
	BinaryFiles         string   `yaml:"binary_files"`   // warn | fail: binary files in a phase diff
}

// GitConfig configures Git behaviour.
type GitConfig struct {
	Tag         bool   `yaml:"tag"`
	Notes       bool   `yaml:"notes"`
	Push        bool   `yaml:"push"`
	OpenPR      bool   `yaml:"open_pr"`
	MergeToBase string `yaml:"merge_to_base"` // none | ff-only
}

// SandboxConfig selects the execution profile.
type SandboxConfig struct {
	Mode string `yaml:"mode"` // host | docker
}

// LimitsConfig holds iteration, usage, time, and budget limits (§14.3).
type LimitsConfig struct {
	PlanReviewRounds      int          `yaml:"plan_review_rounds"`
	GateFixAttempts       int          `yaml:"gate_fix_attempts"`
	ReviewRounds          int          `yaml:"review_rounds"`
	PhaseRestarts         int          `yaml:"phase_restarts"`
	FinalFixRounds        int          `yaml:"final_fix_rounds"`
	MaxReopen             int          `yaml:"max_reopen"`
	NewBlockerCutoffRound int          `yaml:"new_blocker_cutoff_round"`
	PolicyViolations      int          `yaml:"policy_violations"`
	ClaudeMaxTurns        int          `yaml:"claude_max_turns"`
	InvocationTimeout     Duration     `yaml:"invocation_timeout"`
	IdleTimeout           Duration     `yaml:"idle_timeout"`
	PhaseTimeout          Duration     `yaml:"phase_timeout"`
	RunTimeout            Duration     `yaml:"run_timeout"`
	AgentCalls            AgentCalls   `yaml:"agent_calls"`
	ClaudeTurns           ClaudeTurns  `yaml:"claude_turns"`
	BudgetUSD             BudgetLimits `yaml:"budget_usd"`
	MaxConcurrentRuns     int          `yaml:"max_concurrent_runs"`
	MaxPause              Duration     `yaml:"max_pause"`
}

// AgentCalls limits Claude + Codex invocations.
type AgentCalls struct {
	Run       int     `yaml:"run"`
	Planning  int     `yaml:"planning"`
	Phase     int     `yaml:"phase"`
	Final     int     `yaml:"final"`
	SoftRatio float64 `yaml:"soft_ratio"`
}

// ClaudeTurns limits Claude agent turns.
type ClaudeTurns struct {
	Run   int `yaml:"run"`
	Phase int `yaml:"phase"`
}

// BudgetLimits are USD budgets, enforced only for agents in api mode.
type BudgetLimits struct {
	Run       float64 `yaml:"run"`
	Planning  float64 `yaml:"planning"`
	Phase     float64 `yaml:"phase"`
	Final     float64 `yaml:"final"`
	SoftRatio float64 `yaml:"soft_ratio"`
}
