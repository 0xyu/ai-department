package config

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Allowed value sets.
var (
	Efforts            = []string{"low", "medium", "high", "xhigh", "max"}
	ReasoningEfforts   = []string{"minimal", "low", "medium", "high", "xhigh"}
	AuthModes          = []string{"subscription", "api"}
	AuthFallbacks      = []string{"none", "api"}
	OperatorLangs      = []string{"zh-CN", "en"}
	DocumentLangs      = []string{"en", "zh-CN"}
	Severities         = []string{"low", "medium", "high", "critical"}
	SecurityScanners   = []string{"secrets", "gosec", "govulncheck", "semgrep", "npm-audit", "pip-audit", "trivy"}
	ScopeEnforcements  = []string{"flag", "block"}
	WarnFail           = []string{"warn", "fail"}
	NetworkModes       = []string{"deny", "allowlist", "allow"}
	MergeModes         = []string{"none", "ff-only"}
	SandboxModes       = []string{"host", "docker"}
	WindowsSandboxes   = []string{"unelevated", "elevated"}
	plannerKinds       = []string{KindClaudeCode, KindFake}
	reviewerKinds      = []string{KindCodexCLI, KindFake}
	optionalAuthModes  = []string{"", "subscription", "api"}
	optionalAuthLegend = "subscription|api (or empty)"
)

// Validate checks semantic rules on a merged config and returns field-level
// errors (Layer is left empty; Load attributes errors to layers).
func Validate(c *Config) Errors {
	var v validator

	if c.Version != CurrentVersion {
		v.add(FieldError{Field: "version", Code: ErrUnsupportedVersion, Value: fmt.Sprint(c.Version), Allowed: fmt.Sprint(CurrentVersion)})
	}

	v.oneOf("planner.kind", c.Planner.Kind, plannerKinds)
	v.required("planner.model", c.Planner.Model)
	v.oneOf("planner.effort", c.Planner.Effort, Efforts)

	v.oneOf("implementer.kind", c.Implementer.Kind, plannerKinds)
	v.required("implementer.model", c.Implementer.Model)
	v.oneOf("implementer.effort", c.Implementer.Effort, Efforts)

	v.oneOf("reviewer.kind", c.Reviewer.Kind, reviewerKinds)
	v.oneOf("reviewer.reasoning_effort", c.Reviewer.ReasoningEffort, ReasoningEfforts)
	v.required("reviewer.model", c.Reviewer.Model)
	v.oneOf("reviewer.windows_sandbox", c.Reviewer.WindowsSandbox, WindowsSandboxes)
	if c.Reviewer.MaxContextBytes < 64<<10 {
		v.add(FieldError{Field: "reviewer.max_context_bytes", Code: ErrOutOfRange, Value: fmt.Sprint(c.Reviewer.MaxContextBytes), Allowed: ">= 65536"})
	}

	v.oneOf("auth.mode", c.Auth.Mode, AuthModes)
	v.oneOfLegend("auth.claude", c.Auth.Claude, optionalAuthModes, optionalAuthLegend)
	v.oneOfLegend("auth.codex", c.Auth.Codex, optionalAuthModes, optionalAuthLegend)
	v.oneOf("auth.fallback", c.Auth.Fallback, AuthFallbacks)

	models := make([]string, 0, len(c.Pricing.Codex))
	for m := range c.Pricing.Codex {
		models = append(models, m)
	}
	slices.Sort(models)
	for _, model := range models {
		p := c.Pricing.Codex[model]
		v.nonNegF("pricing.codex."+model+".input_per_mtok", p.InputPerMTok)
		v.nonNegF("pricing.codex."+model+".output_per_mtok", p.OutputPerMTok)
	}

	v.oneOf("language.operator", c.Language.Operator, OperatorLangs)
	v.oneOf("language.documents", c.Language.Documents, DocumentLangs)

	cmdGates := []struct {
		name string
		g    GateConfig
	}{
		{"typecheck", c.Gates.Typecheck}, {"lint", c.Gates.Lint}, {"unit", c.Gates.Unit},
		{"integration", c.Gates.Integration}, {"build", c.Gates.Build},
	}
	for _, cg := range cmdGates {
		v.nonNegD("gates."+cg.name+".timeout", cg.g.Timeout)
		v.nonEmptyItems("gates."+cg.name+".commands", cg.g.Commands)
	}
	v.nonNegD("gates.security.timeout", c.Gates.Security.Timeout)
	v.nonEmptyItems("gates.security.commands", c.Gates.Security.Commands)
	v.oneOf("gates.security.fail_severity", c.Gates.Security.FailSeverity, Severities)
	for i, s := range c.Gates.Security.Scanners {
		v.oneOf(fmt.Sprintf("gates.security.scanners[%d]", i), s, SecurityScanners)
	}

	v.oneOf("policy.scope_enforcement", c.Policy.ScopeEnforcement, ScopeEnforcements)
	v.oneOf("policy.tests_missing", c.Policy.TestsMissing, WarnFail)
	v.oneOf("policy.anti_gaming", c.Policy.AntiGaming, WarnFail)
	v.oneOf("policy.network", c.Policy.Network, NetworkModes)
	v.nonEmptyItems("policy.protected_paths", c.Policy.ProtectedPaths)
	v.positive("policy.max_file_bytes", c.Policy.MaxFileBytes)
	v.oneOf("policy.binary_files", c.Policy.BinaryFiles, WarnFail)

	v.oneOf("git.merge_to_base", c.Git.MergeToBase, MergeModes)
	v.oneOf("sandbox.mode", c.Sandbox.Mode, SandboxModes)

	l := c.Limits
	v.positive("limits.plan_review_rounds", l.PlanReviewRounds)
	v.positive("limits.gate_fix_attempts", l.GateFixAttempts)
	v.positive("limits.review_rounds", l.ReviewRounds)
	v.nonNeg("limits.phase_restarts", l.PhaseRestarts)
	v.nonNeg("limits.final_fix_rounds", l.FinalFixRounds)
	v.positive("limits.max_reopen", l.MaxReopen)
	v.positive("limits.new_blocker_cutoff_round", l.NewBlockerCutoffRound)
	v.positive("limits.policy_violations", l.PolicyViolations)
	v.positive("limits.claude_max_turns", l.ClaudeMaxTurns)
	v.positiveD("limits.invocation_timeout", l.InvocationTimeout)
	v.positiveD("limits.idle_timeout", l.IdleTimeout)
	v.positiveD("limits.phase_timeout", l.PhaseTimeout)
	v.positiveD("limits.run_timeout", l.RunTimeout)
	v.positive("limits.agent_calls.run", l.AgentCalls.Run)
	v.positive("limits.agent_calls.planning", l.AgentCalls.Planning)
	v.positive("limits.agent_calls.phase", l.AgentCalls.Phase)
	v.positive("limits.agent_calls.final", l.AgentCalls.Final)
	v.ratio("limits.agent_calls.soft_ratio", l.AgentCalls.SoftRatio)
	v.positive("limits.claude_turns.run", l.ClaudeTurns.Run)
	v.positive("limits.claude_turns.phase", l.ClaudeTurns.Phase)
	v.nonNegF("limits.budget_usd.run", l.BudgetUSD.Run)
	v.nonNegF("limits.budget_usd.planning", l.BudgetUSD.Planning)
	v.nonNegF("limits.budget_usd.phase", l.BudgetUSD.Phase)
	v.nonNegF("limits.budget_usd.final", l.BudgetUSD.Final)
	v.ratio("limits.budget_usd.soft_ratio", l.BudgetUSD.SoftRatio)
	v.positive("limits.max_concurrent_runs", l.MaxConcurrentRuns)
	v.positiveD("limits.max_pause", l.MaxPause)

	return v.errs
}

type validator struct{ errs Errors }

func (v *validator) add(e FieldError) { v.errs = append(v.errs, e) }

func (v *validator) oneOf(field, val string, allowed []string) {
	v.oneOfLegend(field, val, allowed, strings.Join(allowed, "|"))
}

func (v *validator) oneOfLegend(field, val string, allowed []string, legend string) {
	if !slices.Contains(allowed, val) {
		v.add(FieldError{Field: field, Code: ErrInvalidValue, Value: val, Allowed: legend})
	}
}

func (v *validator) required(field, val string) {
	if strings.TrimSpace(val) == "" {
		v.add(FieldError{Field: field, Code: ErrRequired})
	}
}

func (v *validator) nonEmptyItems(field string, items []string) {
	for i, s := range items {
		if strings.TrimSpace(s) == "" {
			v.add(FieldError{Field: fmt.Sprintf("%s[%d]", field, i), Code: ErrRequired})
		}
	}
}

func (v *validator) positive(field string, n int) {
	if n < 1 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: fmt.Sprint(n), Allowed: ">= 1"})
	}
}

func (v *validator) nonNeg(field string, n int) {
	if n < 0 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: fmt.Sprint(n), Allowed: ">= 0"})
	}
}

// finite reports whether f is a real number (not NaN or ±Inf). YAML accepts
// .nan, .inf, and -.inf as floats; none of them is a valid price, budget, or ratio.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func (v *validator) nonNegF(field string, f float64) {
	if !finite(f) || f < 0 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: fmt.Sprint(f), Allowed: ">= 0"})
	}
}

func (v *validator) ratio(field string, f float64) {
	if !finite(f) || f <= 0 || f > 1 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: fmt.Sprint(f), Allowed: "(0, 1]"})
	}
}

func (v *validator) positiveD(field string, d Duration) {
	if d <= 0 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: d.String(), Allowed: "> 0"})
	}
}

func (v *validator) nonNegD(field string, d Duration) {
	if d < 0 {
		v.add(FieldError{Field: field, Code: ErrOutOfRange, Value: d.String(), Allowed: ">= 0"})
	}
}
