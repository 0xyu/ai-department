package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/i18n"
)

// FakeExec is a scripted Exec: tools maps a program to its --version output.
type FakeExec struct {
	Tools map[string]string
	Fail  map[string]error
}

func (f FakeExec) LookPath(name string) (string, error) {
	if _, ok := f.Tools[name]; ok {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f FakeExec) Output(_ context.Context, _ string, name string, args ...string) (string, error) {
	if err := f.Fail[name]; err != nil {
		return "", err
	}
	if len(args) > 0 && args[0] == "-C" {
		return args[1] + "\n", nil
	}
	return f.Tools[name], nil
}

func goodTools() map[string]string {
	return map[string]string{
		"git":    "git version 2.47.1.windows.2\n",
		"claude": "2.1.311 (Claude Code)\n",
		"codex":  "codex-cli 0.162.0\n",
	}
}

func byID(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func baseInput(t *testing.T, ex Exec, flags auth.Flags, env map[string]string) Input {
	t.Helper()
	l, err := config.Load(config.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e := auth.NewEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &auth.EnvFile{Path: "/h/.aco/.env"})
	res, err := auth.Resolve(flags, e, l.Config.Auth, l.Origin)
	if err != nil {
		t.Fatal(err)
	}
	return Input{Exec: ex, Config: l, Resolution: res, Env: e, Keys: auth.CheckKeys(res, e, nil), ACOHome: t.TempDir()}
}

func TestDoctorHappyPath(t *testing.T) {
	cs := Run(context.Background(), baseInput(t, FakeExec{Tools: goodTools()}, auth.Flags{}, nil))
	if Failed(cs) {
		t.Fatalf("unexpected failure: %+v", cs)
	}
	m := byID(cs)
	for _, id := range []string{"tool.git", "tool.claude", "tool.codex", "aco_home", "config.layers"} {
		if m[id].Status != OK {
			t.Errorf("%s = %+v", id, m[id])
		}
	}
	if got := i18n.T(i18n.EN, m["tool.claude"].Msg.Key, m["tool.claude"].Msg.Args...); got != "claude 2.1.311" {
		t.Errorf("claude version line = %q", got)
	}
	if m["config.reviewer_model"].Status != Warn {
		t.Errorf("empty reviewer.model should warn")
	}
	if m["auth.mode.claude"].Status != Info || m["auth.mode.codex"].Status != Info {
		t.Errorf("auth mode lines missing")
	}
}

func TestDoctorFailures(t *testing.T) {
	tools := goodTools()
	delete(tools, "codex")
	tools["git"] = "git version 2.20.1\n"
	in := baseInput(t, FakeExec{Tools: tools, Fail: map[string]error{"claude": errors.New("exit status 1")}},
		auth.Flags{ClaudeAuthMode: "api"}, nil)
	in.Repo = "/some/repo"
	m := byID(Run(context.Background(), in))
	if m["tool.git"].Status != Fail || m["tool.git"].Msg.Key != i18n.DoctorToolTooOld {
		t.Errorf("old git: %+v", m["tool.git"])
	}
	if m["tool.codex"].Status != Fail || m["tool.codex"].Msg.Key != i18n.DoctorToolMissing {
		t.Errorf("missing codex: %+v", m["tool.codex"])
	}
	if m["tool.claude"].Status != Fail || m["tool.claude"].Msg.Key != i18n.DoctorToolError {
		t.Errorf("claude error: %+v", m["tool.claude"])
	}
	k := m["auth.key.claude"]
	if k.Status != Fail || k.Msg.Key != i18n.DoctorKeyMissingAPI {
		t.Fatalf("missing key: %+v", k)
	}
	if got := i18n.T(i18n.ZhCN, k.Msg.Key, k.Msg.Args...); !contains(got, "ANTHROPIC_API_KEY") || !contains(got, "API 模式") {
		t.Errorf("zh message = %q", got)
	}
	if m["repo"].Status != OK {
		t.Errorf("repo: %+v", m["repo"])
	}
}

func TestDoctorFallbackKeys(t *testing.T) {
	in := baseInput(t, FakeExec{Tools: goodTools()}, auth.Flags{},
		map[string]string{"ACO_AUTH_FALLBACK": "api", "OPENAI_API_KEY": "sk-test-0123456789abcdef"})
	m := byID(Run(context.Background(), in))
	if m["auth.key.claude"].Msg.Key != i18n.DoctorKeyMissingFallback || m["auth.key.claude"].Status != Fail {
		t.Errorf("claude fallback key: %+v", m["auth.key.claude"])
	}
	if m["auth.key.codex"].Status != OK {
		t.Errorf("codex fallback key: %+v", m["auth.key.codex"])
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"git version 2.47.1.windows.2": {2, 47, 1},
		"2.1.311 (Claude Code)":        {2, 1, 311},
		"codex-cli 0.162.0":            {0, 162, 0},
		"tool 3.4":                     {3, 4, 0},
	} {
		got, _, ok := ParseVersion(in)
		if !ok || got != want {
			t.Errorf("ParseVersion(%q) = %v", in, got)
		}
	}
	if _, _, ok := ParseVersion("no version"); ok {
		t.Error("expected no version")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
