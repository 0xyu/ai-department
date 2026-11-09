package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

// Child environments for the agent CLIs (§16.5). A CLI process gets an
// allowlist of the parent's variables (system, locale, proxy, toolchain), no
// secret or ACO variable, nothing from the §16.5 removal list, and only the
// credential of its own vendor for its resolved mode.

// ClaudeRemovedVars are never passed to Claude Code as they are in the
// parent: they would switch its provider, endpoint, or billing. In api mode
// ANTHROPIC_API_KEY is then set from the resolved key (env, env file, or
// keychain), never copied from the parent environment.
var ClaudeRemovedVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"OPENAI_API_KEY", "CODEX_API_KEY",
}

// CodexRemovedVars are never passed to Codex CLI from the parent (§16.5):
// API keys and endpoint overrides of both vendors. Every ANTHROPIC_* and
// CLAUDE_* variable is removed as well. In api mode the key reaches Codex
// only through the dedicated CODEX_HOME login, never as a variable.
var CodexRemovedVars = []string{
	"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_BASE", "OPENAI_ORGANIZATION", "OPENAI_PROJECT",
	"CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
}

// EnvCodexHome is Codex CLI's home directory variable.
const EnvCodexHome = "CODEX_HOME"

// EnvClaudeOAuthToken is a subscription credential (`claude setup-token`),
// passed to Claude Code in subscription mode only.
const EnvClaudeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"

// allowedVars are copied from the parent environment (names compared
// case-insensitively, as Windows does).
var allowedVars = map[string]bool{}

func init() {
	for _, v := range []string{
		// System.
		"PATH", "PATHEXT", "HOME", "USER", "USERNAME", "LOGNAME", "SHELL", "TERM", "TZ", "LANG", "LANGUAGE",
		"TMPDIR", "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
		"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)",
		"COMMONPROGRAMW6432", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "OS", "NUMBER_OF_PROCESSORS",
		"PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER", "PUBLIC", "ALLUSERSPROFILE", "COMPUTERNAME",
		// Network and certificates.
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
		// Claude Code location settings (no credentials).
		"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_GIT_BASH_PATH",
		// Toolchains the implementer runs.
		"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOPRIVATE", "GONOSUMDB", "GONOPROXY",
		"GOFLAGS", "GOTOOLCHAIN", "GOBIN", "CGO_ENABLED", "CARGO_HOME", "RUSTUP_HOME", "JAVA_HOME",
		"VIRTUAL_ENV", "CONDA_PREFIX", "PYENV_ROOT", "NVM_DIR", "NVM_HOME", "NVM_SYMLINK", "PNPM_HOME",
		"VOLTA_HOME", "DOTNET_ROOT", "BUN_INSTALL", "DENO_DIR",
	} {
		allowedVars[v] = true
	}
}

var allowedPrefixes = []string{"LC_", "XDG_"}

func allowedName(name string) bool {
	u := strings.ToUpper(name)
	if allowedVars[u] {
		return true
	}
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// ProcessEnviron returns the process environment (the default Base).
func ProcessEnviron() []string { return os.Environ() }

// ChildEnvInput describes the environment of one agent CLI process.
type ChildEnvInput struct {
	Agent Agent
	Mode  Mode
	Env   *Env     // resolves API keys (process env, then env file)
	Keys  KeyStore // optional keychain
	Base  []string // parent variables; nil = ProcessEnviron()
	Extra []string // fixed non-secret additions (applied last)
	// CodexHome is the dedicated CODEX_HOME of Codex in api mode
	// (~/.aco/codex-api); required there, ignored otherwise.
	CodexHome string
}

// KeyMissingError reports an API-mode agent without its key.
type KeyMissingError struct {
	Agent Agent
	Var   string
}

func (e *KeyMissingError) Error() string {
	return fmt.Sprintf("auth: %s runs in api mode but %s is not set", e.Agent, e.Var)
}

// ErrUnsupportedAgent is returned for an unknown agent.
var ErrUnsupportedAgent = errors.New("auth: child environment not supported for this agent")

// removedFor returns the variables never copied from the parent for agent.
func removedFor(a Agent) map[string]bool {
	removed := map[string]bool{}
	list := ClaudeRemovedVars
	if a == Codex {
		list = CodexRemovedVars
	}
	for _, v := range list {
		removed[v] = true
	}
	return removed
}

func removedName(a Agent, removed map[string]bool, name string) bool {
	u := strings.ToUpper(name)
	if removed[u] {
		return true
	}
	// Codex never sees any Anthropic or Claude Code setting (§16.5 ANTHROPIC_*).
	return a == Codex && (strings.HasPrefix(u, "ANTHROPIC_") || strings.HasPrefix(u, "CLAUDE_"))
}

// ChildEnv builds the child environment for an agent CLI.
func ChildEnv(in ChildEnvInput) ([]string, error) {
	if in.Agent != Claude && in.Agent != Codex {
		return nil, ErrUnsupportedAgent
	}
	base := in.Base
	if base == nil {
		base = ProcessEnviron()
	}
	removed := removedFor(in.Agent)
	var out []string
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		u := strings.ToUpper(name)
		if removedName(in.Agent, removed, name) || secure.IsSecretVar(name) || strings.HasPrefix(u, "ACO_") {
			continue
		}
		allowed := allowedName(name)
		if in.Agent == Codex {
			// The user's own CODEX_HOME is kept in subscription mode only;
			// api mode always uses the dedicated home.
			allowed = allowed && !strings.HasPrefix(u, "CLAUDE_")
			if u == EnvCodexHome {
				allowed = in.Mode == Subscription
			}
		}
		if !allowed {
			continue
		}
		out = append(out, kv)
	}

	switch {
	case in.Agent == Claude && in.Mode == API:
		key, ok := lookupKey(in.Env, in.Keys, EnvAnthropicKey)
		if !ok {
			return nil, &KeyMissingError{Agent: in.Agent, Var: EnvAnthropicKey}
		}
		out = append(out, EnvAnthropicKey+"="+key.Reveal())
	case in.Agent == Claude && in.Mode == Subscription:
		if in.Env != nil {
			if tok, _, ok := in.Env.Secret(EnvClaudeOAuthToken); ok {
				out = append(out, EnvClaudeOAuthToken+"="+tok.Reveal())
			}
		}
	case in.Agent == Codex && in.Mode == API:
		// The key never goes into the environment: ACO logs the dedicated
		// CODEX_HOME in once with the key on stdin (CodexLogin).
		if in.CodexHome == "" {
			return nil, errors.New("auth: codex api mode needs the dedicated CODEX_HOME")
		}
		out = append(out, EnvCodexHome+"="+in.CodexHome)
	case in.Agent == Codex && in.Mode == Subscription:
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", in.Mode)
	}
	if in.Agent == Claude {
		out = append(out, "DISABLE_AUTOUPDATER=1")
	} else {
		out = append(out, "NO_COLOR=1")
	}
	for _, kv := range in.Extra {
		name, _, _ := strings.Cut(kv, "=")
		if secure.IsSecretVar(name) || removedName(in.Agent, removed, name) || strings.EqualFold(name, EnvCodexHome) {
			return nil, fmt.Errorf("auth: extra variable %s may not be set", name)
		}
		out = append(out, kv)
	}
	return out, nil
}

// GateEnv builds the environment for quality gate commands (§11.5): the same
// allowlist of parent variables as the agent CLIs, never a credential of
// either agent, a secret-looking variable, or an ACO variable. Gates run
// non-interactively (CI=true, no git prompts). extra holds fixed non-secret
// additions; a secret-looking name in extra is an error.
func GateEnv(base, extra []string) ([]string, error) {
	if base == nil {
		base = ProcessEnviron()
	}
	removed := map[string]bool{}
	for _, v := range append(append([]string{}, ClaudeRemovedVars...), CodexRemovedVars...) {
		removed[v] = true
	}
	var out []string
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		u := strings.ToUpper(name)
		if removed[u] || secure.IsSecretVar(name) || strings.HasPrefix(u, "ACO_") || !allowedName(name) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "CI=true", "GIT_TERMINAL_PROMPT=0", "NO_COLOR=1", "NO_UPDATE_NOTIFIER=1", "PIP_DISABLE_PIP_VERSION_CHECK=1")
	for _, kv := range extra {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" || secure.IsSecretVar(name) || removed[strings.ToUpper(name)] {
			return nil, fmt.Errorf("auth: gate variable %s may not be set", name)
		}
		out = append(out, kv)
	}
	return out, nil
}

func lookupKey(env *Env, ks KeyStore, name string) (secure.Secret, bool) {
	if env != nil {
		if s, _, ok := env.Secret(name); ok {
			return s, true
		}
	}
	if ks != nil {
		if s, ok := ks.Lookup(name); ok && !s.IsZero() {
			return s, true
		}
	}
	return secure.Secret{}, false
}

// Claude Code reports how it authenticated in the stream-json init message
// (apiKeySource). Subscription (OAuth) logins report "none".
const (
	ClaudeSourceSubscription = "none"
	ClaudeSourceAPIKey       = "ANTHROPIC_API_KEY"
)

// SourceMismatchError reports a CLI that authenticated differently from the
// resolved mode (§16.5: never bill the other way silently).
type SourceMismatchError struct {
	Agent    Agent
	Expected Mode
	Got      string // the CLI's reported source
}

func (e *SourceMismatchError) Error() string {
	return fmt.Sprintf("auth: %s should authenticate in %s mode but reports source %q", e.Agent, e.Expected, e.Got)
}

// CheckClaudeSource verifies the init message's apiKeySource against the
// resolved mode.
func CheckClaudeSource(mode Mode, apiKeySource string) error {
	want := ClaudeSourceSubscription
	if mode == API {
		want = ClaudeSourceAPIKey
	}
	if apiKeySource != want {
		return &SourceMismatchError{Agent: Claude, Expected: mode, Got: apiKeySource}
	}
	return nil
}

// CodexKeyHint is how Codex masks an API key in `codex login status`: the
// first 8 and the last 5 characters (keys of 13 characters or fewer are not
// compared). ACO compares it to find out whether the dedicated home is
// logged in with the current key, without storing anything about the key.
func CodexKeyHint(key secure.Secret) string {
	k := key.Reveal()
	if len(k) <= 13 {
		return ""
	}
	return k[:8] + "***" + k[len(k)-5:]
}

// CodexLoginStdin is the stdin of `codex login --with-api-key`: the key and
// a newline. The key goes nowhere else (no variable, file, or argument).
func CodexLoginStdin(key secure.Secret) io.Reader {
	return strings.NewReader(key.Reveal() + string('\n'))
}
