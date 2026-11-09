package config

import "time"

// Default model and role kinds.
const (
	DefaultClaudeModel = "claude-opus-5-5"
	KindClaudeCode     = "claude-code"
	KindCodexCLI       = "codex-cli"
	KindFake           = "fake"
)

// Default returns the built-in default configuration (lowest layer).
func Default() *Config {
	return &Config{
		Version: CurrentVersion,
		Planner: AgentConfig{
			Kind:   KindClaudeCode,
			Model:  DefaultClaudeModel,
			Effort: "high",
		},
		Implementer: ImplementerConfig{
			Kind:             KindClaudeCode,
			Model:            DefaultClaudeModel,
			Effort:           "medium",
			StrictEffort:     true,
			ExtraAllowedBash: []string{},
		},
		Reviewer: ReviewerConfig{
			Kind:            KindCodexCLI,
			Model:           "", // GPT Sol 6.1 model ID; set by the operator, verified by `aco doctor` (Phase 7)
			FallbackModel:   "",
			ReasoningEffort: "high",
			FinalReview:     true,
		},
		Auth: AuthConfig{
			Mode:     "subscription",
			Claude:   "",
			Codex:    "",
			Fallback: "none",
			EnvFile:  "~/.aco/.env",
		},
		Pricing: PricingConfig{Codex: map[string]Price{}},
		Language: LanguageConfig{
			Operator:  "zh-CN",
			Documents: "en",
		},
		Gates: GatesConfig{
			Security: SecurityGateConfig{
				Scanners:     []string{"secrets"},
				FailSeverity: "high",
			},
			AutoDetect: true,
		},
		Policy: PolicyConfig{
			ProtectedPaths:      []string{".aco/**", "AGENTS.md", ".github/**", ".aco.yaml", "**/*.pem", ".env*"},
			ScopeEnforcement:    "flag",
			TestsMissing:        "fail",
			AntiGaming:          "fail",
			AllowUnverifiableAC: false,
			Network:             "allowlist",
			MaxFileBytes:        5 << 20,
			BinaryFiles:         "warn",
		},
		Git: GitConfig{
			Tag:         true,
			Notes:       true,
			Push:        false,
			OpenPR:      false,
			MergeToBase: "none",
		},
		Sandbox: SandboxConfig{Mode: "host"},
		Limits: LimitsConfig{
			PlanReviewRounds:      4,
			GateFixAttempts:       4,
			ReviewRounds:          5,
			PhaseRestarts:         1,
			FinalFixRounds:        2,
			MaxReopen:             2,
			NewBlockerCutoffRound: 3,
			PolicyViolations:      5,
			ClaudeMaxTurns:        200,
			InvocationTimeout:     Duration(60 * time.Minute),
			IdleTimeout:           Duration(10 * time.Minute),
			PhaseTimeout:          Duration(4 * time.Hour),
			RunTimeout:            Duration(72 * time.Hour),
			AgentCalls:            AgentCalls{Run: 600, Planning: 20, Phase: 40, Final: 20, SoftRatio: 0.8},
			ClaudeTurns:           ClaudeTurns{Run: 20000, Phase: 1500},
			BudgetUSD:             BudgetLimits{Run: 250, Planning: 20, Phase: 40, Final: 30, SoftRatio: 0.8},
			MaxConcurrentRuns:     1,
			MaxPause:              Duration(12 * time.Hour),
		},
	}
}
