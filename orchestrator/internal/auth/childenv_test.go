package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure/securetest"
)

func envMap(kvs []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		m[strings.ToUpper(k)] = v
	}
	return m
}

var parentEnv = []string{
	"PATH=/usr/bin", "HOME=/home/u", "LANG=C.UTF-8", "LC_ALL=C", "HTTPS_PROXY=http://proxy:3128", "GOPATH=/go",
	"ANTHROPIC_API_KEY=" + securetest.FakeAnthropic, "ANTHROPIC_AUTH_TOKEN=tok-abcdefgh12345678",
	"ANTHROPIC_BASE_URL=https://evil.example", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1",
	"OPENAI_API_KEY=" + securetest.FakeOpenAI, "CODEX_API_KEY=codex-abcdefgh12345678",
	"ACO_API_TOKEN=aco-service-token-123456", "ACO_AUTH_MODE=api", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123",
	"AWS_SECRET_ACCESS_KEY=aws-abcdefgh12345678", "MY_DB_PASSWORD=pw-abcdefgh12345678",
	"RANDOM_APP_SETTING=1", "Path=C:\\Windows",
}

func lookupFrom(kvs []string) func(string) (string, bool) {
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// The §16.5 removal list, ACO's own token, and every other secret never
// reach the Claude process, in either mode (AC-05.3, AC-05.4).
func TestChildEnvRemovesForbiddenVars(t *testing.T) {
	env := NewEnv(lookupFrom(parentEnv), nil)
	for _, mode := range []Mode{Subscription, API} {
		out, err := ChildEnv(ChildEnvInput{Agent: Claude, Mode: mode, Env: env, Base: parentEnv})
		if err != nil {
			t.Fatal(err)
		}
		m := envMap(out)
		for _, v := range append(append([]string{}, ClaudeRemovedVars...), "ACO_API_TOKEN", "ACO_AUTH_MODE", "GITHUB_TOKEN",
			"AWS_SECRET_ACCESS_KEY", "MY_DB_PASSWORD", "RANDOM_APP_SETTING") {
			if v == EnvAnthropicKey && mode == API {
				continue
			}
			if _, ok := m[v]; ok {
				t.Errorf("%s mode: %s reached the child", mode, v)
			}
		}
		for _, v := range []string{"PATH", "HOME", "LANG", "LC_ALL", "HTTPS_PROXY", "GOPATH"} {
			if _, ok := m[v]; !ok {
				t.Errorf("%s mode: allowlisted %s missing", mode, v)
			}
		}
		joined := strings.Join(out, "\n")
		for _, bad := range []string{securetest.FakeOpenAI, "aco-service-token", "evil.example"} {
			if strings.Contains(joined, bad) {
				t.Errorf("%s mode: child env contains %q", mode, bad)
			}
		}
		if mode == Subscription && strings.Contains(joined, securetest.FakeAnthropic) {
			t.Error("subscription mode: Anthropic key reached the child")
		}
		if mode == API && m[EnvAnthropicKey] != securetest.FakeAnthropic {
			t.Error("api mode: key not injected")
		}
	}
}

func TestChildEnvAPIKeyFromEnvFileAndKeychain(t *testing.T) {
	ef := NewEnvFile("/x/.env", map[string]string{EnvAnthropicKey: "sk-ant-fromfile-abcdefgh"})
	out, err := ChildEnv(ChildEnvInput{Agent: Claude, Mode: API, Env: NewEnv(nil, ef), Base: []string{"PATH=/bin"}})
	if err != nil || envMap(out)[EnvAnthropicKey] != "sk-ant-fromfile-abcdefgh" {
		t.Fatalf("env file key not used: %v", err)
	}
	_, err = ChildEnv(ChildEnvInput{Agent: Claude, Mode: API, Env: NewEnv(nil, nil), Base: []string{"PATH=/bin"}})
	var km *KeyMissingError
	if !errors.As(err, &km) || km.Var != EnvAnthropicKey {
		t.Fatalf("want KeyMissingError, got %v", err)
	}
}

func TestChildEnvSubscriptionOAuthToken(t *testing.T) {
	env := NewEnv(lookupFrom([]string{EnvClaudeOAuthToken + "=oauth-abcdefgh12345678"}), nil)
	out, _ := ChildEnv(ChildEnvInput{Agent: Claude, Mode: Subscription, Env: env, Base: []string{}})
	if envMap(out)[EnvClaudeOAuthToken] != "oauth-abcdefgh12345678" {
		t.Error("subscription OAuth token not passed")
	}
	out, _ = ChildEnv(ChildEnvInput{Agent: Claude, Mode: API, Env: NewEnv(lookupFrom([]string{
		EnvClaudeOAuthToken + "=oauth-abcdefgh12345678", EnvAnthropicKey + "=sk-ant-k-abcdefgh"}), nil), Base: []string{}})
	if _, ok := envMap(out)[EnvClaudeOAuthToken]; ok {
		t.Error("api mode must not pass the subscription token")
	}
}

func TestChildEnvExtraRejectsSecrets(t *testing.T) {
	if _, err := ChildEnv(ChildEnvInput{Agent: Claude, Mode: Subscription, Env: NewEnv(nil, nil), Base: []string{},
		Extra: []string{"ANTHROPIC_BASE_URL=x"}}); err == nil {
		t.Error("removal-list var accepted as extra")
	}
	if _, err := ChildEnv(ChildEnvInput{Agent: Claude, Mode: Subscription, Env: NewEnv(nil, nil), Base: []string{},
		Extra: []string{"SOME_TOKEN=x"}}); err == nil {
		t.Error("secret var accepted as extra")
	}
	if _, err := ChildEnv(ChildEnvInput{Agent: Codex, Mode: Subscription}); !errors.Is(err, ErrUnsupportedAgent) {
		t.Error("codex child env is Phase 7")
	}
}

func TestCheckClaudeSource(t *testing.T) {
	if CheckClaudeSource(Subscription, "none") != nil || CheckClaudeSource(API, "ANTHROPIC_API_KEY") != nil {
		t.Fatal("matching sources rejected")
	}
	for _, c := range []struct {
		m   Mode
		src string
	}{{Subscription, "ANTHROPIC_API_KEY"}, {API, "none"}, {API, "apiKeyHelper"}, {Subscription, ""}} {
		var sm *SourceMismatchError
		if err := CheckClaudeSource(c.m, c.src); !errors.As(err, &sm) {
			t.Errorf("%s/%q: want mismatch", c.m, c.src)
		}
	}
}

// Gate commands get the same allowlist and never an agent credential, an
// ACO variable, or another secret (§11.5).
func TestGateEnv(t *testing.T) {
	out, err := GateEnv(parentEnv, []string{"GOTOOLCHAIN=local"})
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(out)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY",
		"CODEX_API_KEY", "ACO_API_TOKEN", "ACO_AUTH_MODE", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "MY_DB_PASSWORD",
		"RANDOM_APP_SETTING", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s leaked into the gate environment", k)
		}
	}
	for k, v := range map[string]string{"HOME": "/home/u", "GOPATH": "/go", "CI": "true", "GIT_TERMINAL_PROMPT": "0", "GOTOOLCHAIN": "local"} {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	securetest.AssertNoLeak(t, "gate env", strings.Join(out, "\n"), securetest.FakeAnthropic, securetest.FakeOpenAI)
	for _, bad := range []string{"OPENAI_API_KEY=x", "DB_PASSWORD=x", "ANTHROPIC_BASE_URL=x", "=x"} {
		if _, err := GateEnv(parentEnv, []string{bad}); err == nil {
			t.Errorf("extra %q accepted", bad)
		}
	}
}
