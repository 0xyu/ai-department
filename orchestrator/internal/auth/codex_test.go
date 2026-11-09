package auth

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure"
)

func TestParseCodexLoginStatus(t *testing.T) {
	for _, c := range []struct {
		out  string
		want CodexLogin
		hint string
	}{
		{"Logged in using ChatGPT\n", CodexLoginChatGPT, ""},
		{"Logged in using an API key - sk-dummy***0abcd\n", CodexLoginAPIKey, "sk-dummy***0abcd"},
		{"WARNING: something\nNot logged in\n", CodexLoginNone, ""},
		{"weird\n", CodexLoginUnknown, ""},
	} {
		st := ParseCodexLoginStatus(c.out)
		if st.Login != c.want || st.KeyHint != c.hint {
			t.Errorf("%q: %+v", c.out, st)
		}
	}
	if h := CodexKeyHint(secure.NewSecret("sk-dummy-not-a-real-key-1234567890abcd")); h != "sk-dummy***0abcd" {
		t.Errorf("hint %q (must match codex login status, verified with 0.162.0)", h)
	}
}

func TestCheckCodexLogin(t *testing.T) {
	sub, api := ParseCodexLoginStatus("Logged in using ChatGPT"), ParseCodexLoginStatus("Logged in using an API key - sk-proj-***12345")
	if CheckCodexLogin(Subscription, sub, "") != nil || CheckCodexLogin(API, api, "/h") != nil {
		t.Fatal("matching logins rejected")
	}
	var me *CodexLoginMismatchError
	if err := CheckCodexLogin(Subscription, api, ""); !errors.As(err, &me) || me.Got != CodexLoginAPIKey {
		t.Errorf("api key in subscription mode: %v", err)
	}
	if err := CheckCodexLogin(API, sub, "/h"); !errors.As(err, &me) || me.Home != "/h" {
		t.Errorf("chatgpt in api mode: %v", err)
	}
	if CheckCodexLogin(API, ParseCodexLoginStatus("Not logged in"), "/h") == nil {
		t.Error("logged out accepted")
	}
}

func TestCodexChildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/u", "OPENAI_API_KEY=sk-parent", "CODEX_API_KEY=x", "OPENAI_BASE_URL=https://e",
		"ANTHROPIC_API_KEY=a", "ANTHROPIC_MODEL=m", "CLAUDE_CONFIG_DIR=/c", "CODEX_HOME=/home/u/custom-codex", "ACO_AUTH_MODE=api",
		"GITHUB_TOKEN=ghp_x", "LANG=C"}
	env, err := ChildEnv(ChildEnvInput{Agent: Codex, Mode: Subscription, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(env, "\n")
	for _, bad := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_", "CLAUDE_", "ACO_", "GITHUB_TOKEN"} {
		if strings.Contains(got, bad) {
			t.Errorf("subscription env has %s:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "CODEX_HOME=/home/u/custom-codex") || !strings.Contains(got, "PATH=/bin") {
		t.Errorf("subscription keeps the user's CODEX_HOME and the allowlist:\n%s", got)
	}

	env, err = ChildEnv(ChildEnvInput{Agent: Codex, Mode: API, Base: base, CodexHome: "/home/u/.aco/codex-api",
		Env: NewEnv(func(k string) (string, bool) { return "sk-proj-key", k == EnvOpenAIKey }, nil)})
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(env, "\n")
	if strings.Contains(got, "sk-") || strings.Contains(got, "custom-codex") || !strings.Contains(got, "CODEX_HOME=/home/u/.aco/codex-api") {
		t.Errorf("api env:\n%s", got)
	}
	if _, err := ChildEnv(ChildEnvInput{Agent: Codex, Mode: API, Base: base}); err == nil {
		t.Error("api mode without the dedicated home accepted")
	}
	if _, err := ChildEnv(ChildEnvInput{Agent: Codex, Mode: Subscription, Base: base, Extra: []string{"CODEX_HOME=/x"}}); err == nil {
		t.Error("CODEX_HOME as extra accepted")
	}
	if _, err := ChildEnv(ChildEnvInput{Agent: Codex, Mode: Subscription, Base: base, Extra: []string{"ANTHROPIC_BASE_URL=x"}}); err == nil {
		t.Error("ANTHROPIC_* as extra accepted")
	}
	b, _ := io.ReadAll(CodexLoginStdin(secure.NewSecret("sk-k")))
	if string(b) != "sk-k\n" {
		t.Errorf("login stdin %q", b)
	}
}

func TestGateEnvDropsCodexVars(t *testing.T) {
	env, err := GateEnv([]string{"PATH=/bin", "OPENAI_BASE_URL=https://e", "CODEX_ACCESS_TOKEN=t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env, "\n"); strings.Contains(got, "OPENAI_BASE_URL") || strings.Contains(got, "CODEX_ACCESS_TOKEN") {
		t.Errorf("gate env:\n%s", got)
	}
}
