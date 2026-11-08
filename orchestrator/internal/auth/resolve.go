// Package auth resolves how each vendor CLI authenticates: subscription
// (the CLI's own login) or api (an API key), selected by environment (§16.5).
//
// Phase 1 covers env-file loading, mode resolution, and API-key presence.
// Credential injection, environment scrubbing, and auth-source verification
// for each CLI are added with the Claude (Phase 5) and Codex (Phase 7) adapters.
package auth

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xyu/aco/internal/config"
)

// Mode is an auth mode.
type Mode string

// Auth modes.
const (
	Subscription Mode = "subscription"
	API          Mode = "api"
)

// Fallback is the action when a subscription hits its usage limit.
type Fallback string

// Fallback values.
const (
	FallbackNone Fallback = "none"
	FallbackAPI  Fallback = "api"
)

// Agent is a model CLI driven by ACO.
type Agent string

// Agents.
const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

// Agents lists all agents in display order.
var Agents = []Agent{Claude, Codex}

// Environment variable names (§16.5).
const (
	EnvAuthMode       = "ACO_AUTH_MODE"
	EnvClaudeAuthMode = "ACO_CLAUDE_AUTH_MODE"
	EnvCodexAuthMode  = "ACO_CODEX_AUTH_MODE"
	EnvAuthFallback   = "ACO_AUTH_FALLBACK"
	EnvAnthropicKey   = "ANTHROPIC_API_KEY"
	EnvOpenAIKey      = "OPENAI_API_KEY"
)

// Flag names.
const (
	FlagAuthMode       = "auth-mode"
	FlagClaudeAuthMode = "claude-auth-mode"
	FlagCodexAuthMode  = "codex-auth-mode"
)

// SourceKind says where a resolved value came from.
type SourceKind string

// Source kinds, highest precedence first.
const (
	SourceFlag     SourceKind = "flag"
	SourceEnv      SourceKind = "env"      // process environment
	SourceEnvFile  SourceKind = "env_file" // ACO env file
	SourceConfig   SourceKind = "config"
	SourceDefault  SourceKind = "default"
	SourceKeychain SourceKind = "keychain"
)

// Source locates a resolved value.
type Source struct {
	Kind SourceKind
	Name string // flag name, variable name, or config field
	Path string // env file path (env_file only)
}

func (s Source) String() string {
	switch s.Kind {
	case SourceFlag:
		return "flag --" + s.Name
	case SourceEnv:
		return "env " + s.Name
	case SourceEnvFile:
		return "env file " + s.Path + " (" + s.Name + ")"
	case SourceConfig:
		if s.Path != "" {
			return "config " + s.Name + " (" + s.Path + ")"
		}
		return "config " + s.Name
	case SourceKeychain:
		return "keychain " + s.Name
	default:
		return "default"
	}
}

// Env is the environment ACO reads: the process environment layered over
// the env file (process wins for the same variable). Empty values count as unset.
type Env struct {
	lookup func(string) (string, bool)
	file   *EnvFile
}

// NewEnv combines a process lookup function with an optional env file.
func NewEnv(lookup func(string) (string, bool), file *EnvFile) *Env {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	return &Env{lookup: lookup, file: file}
}

// File returns the env file (may be nil).
func (e *Env) File() *EnvFile { return e.file }

// Lookup returns a non-empty value and its source.
func (e *Env) Lookup(name string) (string, Source, bool) {
	if v, ok := e.lookup(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), Source{Kind: SourceEnv, Name: name}, true
	}
	if e.file != nil {
		if v, ok := e.file.Values[name]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), Source{Kind: SourceEnvFile, Name: name, Path: e.file.Path}, true
		}
	}
	return "", Source{}, false
}

// Flags are the auth-mode command-line flags ("" = not given).
type Flags struct {
	AuthMode       string
	ClaudeAuthMode string
	CodexAuthMode  string
}

// AgentMode is the resolved mode of one agent.
type AgentMode struct {
	Agent  Agent
	Mode   Mode
	Source Source
}

// Resolution is the resolved auth setup of a run.
type Resolution struct {
	Claude         AgentMode
	Codex          AgentMode
	Fallback       Fallback
	FallbackSource Source
}

// For returns the resolved mode of agent.
func (r *Resolution) For(a Agent) AgentMode {
	if a == Codex {
		return r.Codex
	}
	return r.Claude
}

// ValueError is an invalid auth value from a flag, env var, or config field.
type ValueError struct {
	Source  Source
	Value   string
	Allowed string
}

func (e *ValueError) Error() string {
	return fmt.Sprintf("%s: invalid value %q (allowed: %s)", e.Source, e.Value, e.Allowed)
}

// Errors is a list of ValueErrors.
type Errors []*ValueError

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

var (
	modeValues     = []string{string(Subscription), string(API)}
	fallbackValues = []string{string(FallbackNone), string(FallbackAPI)}
)

type candidate struct {
	value  string
	source Source
}

// Resolve applies the precedence from §16.5, per agent:
//
//	--<agent>-auth-mode > --auth-mode > ACO_<AGENT>_AUTH_MODE > ACO_AUTH_MODE >
//	auth.<agent> > auth.mode > default (subscription)
//
// Every given value is validated, even if a higher-precedence one wins,
// so a typo in any variable is reported with that variable's name.
// The fallback is ACO_AUTH_FALLBACK > auth.fallback > none.
//
// origin reports which config layer set a field ("auth.mode", …); fields it
// reports as unset are built-in defaults and are skipped, so the source shows
// "default". A nil origin treats every non-empty config field as set.
func Resolve(flags Flags, env *Env, cfg config.AuthConfig, origin func(field string) (config.LayerInfo, bool)) (*Resolution, error) {
	var errs Errors
	check := func(c candidate, allowed []string) bool {
		if c.value == "" {
			return false
		}
		if !slices.Contains(allowed, c.value) {
			errs = append(errs, &ValueError{Source: c.source, Value: c.value, Allowed: strings.Join(allowed, "|")})
			return false
		}
		return true
	}
	fromEnv := func(name string) candidate {
		if v, src, ok := env.Lookup(name); ok {
			return candidate{v, src}
		}
		return candidate{}
	}
	flag := func(name, v string) candidate {
		return candidate{strings.TrimSpace(v), Source{Kind: SourceFlag, Name: name}}
	}
	conf := func(field, v string) candidate {
		src := Source{Kind: SourceConfig, Name: field}
		if origin != nil {
			li, ok := origin(field)
			if !ok {
				return candidate{} // built-in default
			}
			src.Path = li.Source
		}
		return candidate{strings.TrimSpace(v), src}
	}

	globalFlag := flag(FlagAuthMode, flags.AuthMode)
	globalEnv := fromEnv(EnvAuthMode)
	globalConf := conf("auth.mode", cfg.Mode)

	resolveAgent := func(a Agent, agentFlag, agentEnv, agentConf candidate) AgentMode {
		chain := []candidate{agentFlag, globalFlag, agentEnv, globalEnv, agentConf, globalConf}
		var winner *candidate
		for i := range chain {
			if check(chain[i], modeValues) && winner == nil {
				winner = &chain[i]
			}
		}
		if winner == nil {
			return AgentMode{Agent: a, Mode: Subscription, Source: Source{Kind: SourceDefault}}
		}
		return AgentMode{Agent: a, Mode: Mode(winner.value), Source: winner.source}
	}

	res := &Resolution{
		Claude: resolveAgent(Claude,
			flag(FlagClaudeAuthMode, flags.ClaudeAuthMode), fromEnv(EnvClaudeAuthMode), conf("auth.claude", cfg.Claude)),
		Codex: resolveAgent(Codex,
			flag(FlagCodexAuthMode, flags.CodexAuthMode), fromEnv(EnvCodexAuthMode), conf("auth.codex", cfg.Codex)),
		Fallback:       FallbackNone,
		FallbackSource: Source{Kind: SourceDefault},
	}

	for _, c := range []candidate{fromEnv(EnvAuthFallback), conf("auth.fallback", cfg.Fallback)} {
		if check(c, fallbackValues) && res.FallbackSource.Kind == SourceDefault {
			res.Fallback, res.FallbackSource = Fallback(c.value), c.source
		}
	}

	if len(errs) > 0 {
		return nil, dedupe(errs)
	}
	return res, nil
}

// dedupe drops repeated errors for the same source (the global candidates
// appear in both agents' chains).
func dedupe(errs Errors) Errors {
	seen := map[Source]bool{}
	var out Errors
	for _, e := range errs {
		if !seen[e.Source] {
			seen[e.Source] = true
			out = append(out, e)
		}
	}
	return out
}

// KeyVar returns the API-key environment variable for agent.
func KeyVar(a Agent) string {
	if a == Codex {
		return EnvOpenAIKey
	}
	return EnvAnthropicKey
}

// KeyReason says why an API key is required.
type KeyReason string

// Key reasons.
const (
	ReasonAPIMode  KeyReason = "api_mode"
	ReasonFallback KeyReason = "fallback"
)

// KeyStatus reports whether the API key an agent needs is present.
type KeyStatus struct {
	Agent   Agent
	Var     string
	Reason  KeyReason
	Present bool
	Source  Source // where the key was found (when Present)
}

// KeyStore looks up secrets outside the environment (OS keychain).
// Phase 1 ships no implementation; Lookup returns false.
type KeyStore interface {
	Lookup(name string) (string, bool)
}

// CheckKeys lists the API keys required by the resolution: one per agent in
// api mode, and one per subscription agent when the fallback is api.
// Keys are read only from the environment, the env file, or the key store.
func CheckKeys(res *Resolution, env *Env, ks KeyStore) []KeyStatus {
	var out []KeyStatus
	for _, a := range Agents {
		am := res.For(a)
		var reason KeyReason
		switch {
		case am.Mode == API:
			reason = ReasonAPIMode
		case res.Fallback == FallbackAPI:
			reason = ReasonFallback
		default:
			continue
		}
		st := KeyStatus{Agent: a, Var: KeyVar(a), Reason: reason}
		if _, src, ok := env.Lookup(st.Var); ok {
			st.Present, st.Source = true, src
		} else if ks != nil {
			if v, ok := ks.Lookup(st.Var); ok && v != "" {
				st.Present, st.Source = true, Source{Kind: SourceKeychain, Name: st.Var}
			}
		}
		out = append(out, st)
	}
	return out
}
