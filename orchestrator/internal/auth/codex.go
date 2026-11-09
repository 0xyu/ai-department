package auth

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

// Codex CLI authentication (§16.5). Codex reports no auth source in its
// event stream, so the mode is verified with `codex login status`
// (verified with codex-cli 0.162.0):
//
//	Logged in using ChatGPT                        subscription
//	Logged in using an API key - sk-dummy***0abcd  api
//	Not logged in                                  (exit 1)
//
// In api mode ACO never touches the user's ~/.codex: it logs a dedicated
// CODEX_HOME (~/.aco/codex-api) in once with `codex login --with-api-key`,
// the key piped on stdin, and runs every review with that home.

// CodexLogin is what `codex login status` reports.
type CodexLogin string

// Codex login kinds.
const (
	CodexLoginChatGPT CodexLogin = "chatgpt"
	CodexLoginAPIKey  CodexLogin = "api_key"
	CodexLoginNone    CodexLogin = "none"
	CodexLoginUnknown CodexLogin = "unknown"
)

// CodexAPIHomeDir is the dedicated CODEX_HOME below ~/.aco.
const CodexAPIHomeDir = "codex-api"

// CodexAPIHome returns ~/.aco/codex-api.
func CodexAPIHome(acoHome string) string { return filepath.Join(acoHome, CodexAPIHomeDir) }

// CodexStatus is a parsed `codex login status`.
type CodexStatus struct {
	Login CodexLogin
	// KeyHint is the masked key Codex shows for an API-key login
	// ("sk-dummy***0abcd"); it is not a secret.
	KeyHint string
	Line    string // the status line, for diagnostics
}

// ParseCodexLoginStatus reads the output (stdout and stderr) of
// `codex login status`.
func ParseCodexLoginStatus(out string) CodexStatus {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		ll := strings.ToLower(l)
		switch {
		case strings.HasPrefix(ll, "not logged in"):
			return CodexStatus{Login: CodexLoginNone, Line: l}
		case strings.HasPrefix(ll, "logged in using chatgpt"):
			return CodexStatus{Login: CodexLoginChatGPT, Line: l}
		case strings.HasPrefix(ll, "logged in using an api key"), strings.HasPrefix(ll, "logged in using api key"):
			st := CodexStatus{Login: CodexLoginAPIKey}
			if _, hint, ok := strings.Cut(l, " - "); ok {
				st.KeyHint = strings.TrimSpace(hint)
			}
			st.Line = strings.TrimSpace(strings.SplitN(l, " - ", 2)[0])
			return st
		}
	}
	return CodexStatus{Login: CodexLoginUnknown, Line: firstNonEmpty(out)}
}

func firstNonEmpty(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

// CodexLoginMismatchError reports a Codex login that does not match the
// resolved mode (§16.5: never bill the other way silently).
type CodexLoginMismatchError struct {
	Expected Mode
	Got      CodexLogin
	Home     string // CODEX_HOME that was checked ("" = the default ~/.codex)
}

func (e *CodexLoginMismatchError) Error() string {
	where := "~/.codex"
	if e.Home != "" {
		where = e.Home
	}
	return fmt.Sprintf("auth: codex should be logged in for %s mode but %s reports %s", e.Expected, where, e.Got)
}

// CheckCodexLogin verifies a login status against the resolved mode:
// subscription needs a ChatGPT login, api an API-key login.
func CheckCodexLogin(mode Mode, st CodexStatus, home string) error {
	want := CodexLoginChatGPT
	if mode == API {
		want = CodexLoginAPIKey
	}
	if st.Login != want {
		return &CodexLoginMismatchError{Expected: mode, Got: st.Login, Home: home}
	}
	return nil
}

// LookupCodexKey returns OPENAI_API_KEY from the environment, the env
// file, or the key store.
func LookupCodexKey(env *Env, ks KeyStore) (secure.Secret, bool) {
	return lookupKey(env, ks, EnvOpenAIKey)
}
