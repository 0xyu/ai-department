package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/exitcode"
)

type fakeExec struct{ tools map[string]string }

func (f fakeExec) LookPath(name string) (string, error) {
	if _, ok := f.tools[name]; ok {
		return name, nil
	}
	return "", errors.New("not found")
}

func (f fakeExec) Output(_ context.Context, _ string, name string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "-C" {
		return args[1], nil
	}
	return f.tools[name], nil
}

type harness struct {
	t      *testing.T
	home   string
	repo   string
	env    map[string]string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	h := &harness{t: t, home: filepath.Join(dir, "home"), repo: filepath.Join(dir, "repo"), env: map[string]string{}}
	for _, d := range []string{h.home, h.repo} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) write(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	sys := System{
		Stdout:    &h.stdout,
		Stderr:    &h.stderr,
		LookupEnv: func(k string) (string, bool) { v, ok := h.env[k]; return v, ok },
		Environ: func() []string {
			var out []string
			for k, v := range h.env {
				out = append(out, k+"="+v)
			}
			return out
		},
		HomeDir: func() (string, error) { return h.home, nil },
		Getwd:   func() (string, error) { return h.repo, nil },
		Exec: fakeExec{tools: map[string]string{
			"git": "git version 2.47.1", "claude": "2.1.311 (Claude Code)", "codex": "codex-cli 0.162.0",
		}},
	}
	return Run(context.Background(), args, sys)
}

var allCommands = []string{"run", "plan", "resume", "status", "pause", "cancel", "report",
	"validate-plan", "doctor", "serve", "mcp", "hook", "version"}

func TestHelpListsAllCommandsInChineseByDefault(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--help"); code != exitcode.Completed {
		t.Fatalf("exit %d", code)
	}
	out := h.stdout.String()
	for _, c := range allCommands {
		if !strings.Contains(out, "\n  "+c+" ") {
			t.Errorf("help does not list %q:\n%s", c, out)
		}
	}
	for _, want := range []string{"用法：", "命令：", "检查运行环境", "退出码"} {
		if !strings.Contains(out, want) {
			t.Errorf("zh help missing %q", want)
		}
	}
}

func TestHelpInEnglishWithRepoConfig(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.repo, ".aco.yaml"), "language:\n  operator: en\n")
	h.run("--help")
	if out := h.stdout.String(); !strings.Contains(out, "Usage:") || !strings.Contains(out, "Commands:") || strings.Contains(out, "用法") {
		t.Errorf("expected English help:\n%s", out)
	}
	h.run("doctor")
	if out := h.stdout.String(); !strings.Contains(out, "ACO doctor") || !strings.Contains(out, "[ OK ] git 2.47.1") {
		t.Errorf("expected English doctor:\n%s", out)
	}
}

func TestHelpInEnglishWithUserConfigAndSet(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.home, ".aco", "config.yaml"), "language: {operator: en}\n")
	h.run("--help")
	if !strings.Contains(h.stdout.String(), "Usage:") {
		t.Errorf("user config language not applied:\n%s", h.stdout.String())
	}
	h2 := newHarness(t)
	h2.run("doctor", "--help", "--set", "language.operator=en")
	if !strings.Contains(h2.stdout.String(), "Flags:") {
		t.Errorf("--set language not applied:\n%s", h2.stdout.String())
	}
}

func TestStubsExitTwoNotImplemented(t *testing.T) {
	stubs := map[string][]string{
		"run": nil, "plan": nil, "resume": {"r1"}, "status": nil, "pause": {"r1"}, "cancel": {"r1"},
		"report": {"r1"}, "serve": nil, "mcp": nil, "hook": {"pre-tool-use"},
	}
	for name, args := range stubs {
		h := newHarness(t)
		code := h.run(append([]string{name}, args...)...)
		if code != exitcode.Usage {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		if !strings.Contains(h.stderr.String(), "尚未实现") {
			t.Errorf("%s: stderr %q", name, h.stderr.String())
		}
		h.write(filepath.Join(h.repo, ".aco.yaml"), "language: {operator: en}\n")
		h.run(append([]string{name}, args...)...)
		if !strings.Contains(h.stderr.String(), "not implemented") {
			t.Errorf("%s (en): stderr %q", name, h.stderr.String())
		}
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range allCommands {
		h := newHarness(t)
		if code := h.run(c, "--help"); code != exitcode.Completed {
			t.Errorf("%s --help: exit %d", c, code)
		}
		if !strings.Contains(h.stdout.String(), "aco "+c) || !strings.Contains(h.stdout.String(), "--help") {
			t.Errorf("%s --help output:\n%s", c, h.stdout.String())
		}
		h.run("help", c)
		if !strings.Contains(h.stdout.String(), "aco "+c) {
			t.Errorf("help %s output:\n%s", c, h.stdout.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != exitcode.Usage {
		t.Errorf("no args: exit %d", code)
	}
	if code := h.run("frobnicate"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "未知命令：frobnicate") {
		t.Errorf("unknown command: %d %q", code, h.stderr.String())
	}
	if code := h.run("doctor", "--bogus"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "未知参数") {
		t.Errorf("unknown flag: %d %q", code, h.stderr.String())
	}
	if code := h.run("doctor", "--repo"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "需要一个值") {
		t.Errorf("missing value: %d %q", code, h.stderr.String())
	}
	if code := h.run("resume", "r1", "--from-phase", "x"); code != exitcode.Usage {
		t.Errorf("bad int: %d", code)
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code := h.run("version"); code != 0 || !strings.HasPrefix(h.stdout.String(), "aco dev") {
		t.Errorf("version: %d %q", code, h.stdout.String())
	}
	if code := h.run("--version"); code != 0 || !strings.HasPrefix(h.stdout.String(), "aco ") {
		t.Errorf("--version: %d %q", code, h.stdout.String())
	}
}

func TestDoctorDefault(t *testing.T) {
	h := newHarness(t)
	code := h.run("doctor")
	out := h.stdout.String()
	if code != exitcode.Completed {
		t.Fatalf("exit %d\n%s%s", code, out, h.stderr.String())
	}
	for _, want := range []string{
		"ACO 环境检查",
		"Claude Code 认证方式：订阅模式（subscription）（来源：默认值）",
		"Codex CLI 认证方式：订阅模式（subscription）（来源：默认值）",
		"[通过] git 2.47.1", "[通过] claude 2.1.311", "[通过] codex 0.162.0",
		"可写",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorAPIModeWithoutKeyFailsInChinese(t *testing.T) {
	h := newHarness(t)
	h.env["ACO_AUTH_MODE"] = "api"
	code := h.run("doctor")
	out := h.stdout.String()
	if code != exitcode.PreflightFailed {
		t.Fatalf("exit %d, want 3\n%s", code, out)
	}
	for _, want := range []string{
		"[失败] Claude Code 处于 API 模式，但缺少环境变量 ANTHROPIC_API_KEY",
		"[失败] Codex CLI 处于 API 模式，但缺少环境变量 OPENAI_API_KEY",
		"来源：环境变量 ACO_AUTH_MODE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorMixedModesFromEnvFile(t *testing.T) {
	h := newHarness(t)
	envFile := filepath.Join(h.home, ".aco", ".env")
	h.write(envFile, "ACO_CLAUDE_AUTH_MODE=subscription\nACO_CODEX_AUTH_MODE=api\nOPENAI_API_KEY=sk-proj-FAKEFAKEFAKEFAKE12345\n")
	code := h.run("doctor")
	out := h.stdout.String()
	if code != exitcode.Completed {
		t.Fatalf("exit %d\n%s%s", code, out, h.stderr.String())
	}
	if !strings.Contains(out, "Codex CLI 认证方式：API 模式（api）（来源：环境文件中的 ACO_CODEX_AUTH_MODE") {
		t.Errorf("codex mode line:\n%s", out)
	}
	if !strings.Contains(out, "Claude Code 认证方式：订阅模式（subscription）（来源：环境文件中的 ACO_CLAUDE_AUTH_MODE") {
		t.Errorf("claude mode line:\n%s", out)
	}
	if !strings.Contains(out, "[通过] Codex CLI 所需的 OPENAI_API_KEY 已设置") {
		t.Errorf("key line:\n%s", out)
	}
	if strings.Contains(out+h.stderr.String(), "FAKEFAKE") {
		t.Errorf("key value leaked")
	}

	// A flag beats the env file.
	code = h.run("doctor", "--codex-auth-mode", "subscription")
	if code != 0 || !strings.Contains(h.stdout.String(), "Codex CLI 认证方式：订阅模式（subscription）（来源：参数 --codex-auth-mode）") {
		t.Errorf("flag precedence: %d\n%s", code, h.stdout.String())
	}

	// Explicit --env-file.
	other := filepath.Join(h.repo, "ci.env")
	h.write(other, "ACO_AUTH_MODE=api\n")
	if code := h.run("doctor", "--env-file", other); code != exitcode.PreflightFailed {
		t.Errorf("--env-file api without keys: exit %d\n%s", code, h.stdout.String())
	}
	if code := h.run("doctor", "--env-file", filepath.Join(h.repo, "missing.env")); code != exitcode.Usage ||
		!strings.Contains(h.stderr.String(), "找不到环境文件") {
		t.Errorf("missing --env-file: %d %q", code, h.stderr.String())
	}
}

func TestDoctorInvalidEnvValueNamesVariable(t *testing.T) {
	for _, v := range []string{"ACO_AUTH_MODE", "ACO_CLAUDE_AUTH_MODE", "ACO_CODEX_AUTH_MODE", "ACO_AUTH_FALLBACK"} {
		h := newHarness(t)
		h.env[v] = "bogus"
		code := h.run("doctor")
		if code != exitcode.Usage || !strings.Contains(h.stderr.String(), "环境变量 "+v+" 的值 \"bogus\" 无效") {
			t.Errorf("%s: exit %d stderr %q", v, code, h.stderr.String())
		}
	}
}

func TestConfigWithAPIKeyRejected(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.repo, ".aco.yaml"), "version: 1\nreviewer:\n  api_key: sk-ant-FAKEFAKEFAKEFAKE\n")
	code := h.run("doctor")
	errOut := h.stderr.String()
	if code != exitcode.Usage || !strings.Contains(errOut, "reviewer.api_key") || !strings.Contains(errOut, "第 3 行") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "FAKEFAKE") {
		t.Errorf("secret echoed: %q", errOut)
	}
}

func TestConfigInvalidValueNamesField(t *testing.T) {
	h := newHarness(t)
	code := h.run("doctor", "--set", "implementer.effort=extreme")
	if code != exitcode.Usage || !strings.Contains(h.stderr.String(), "implementer.effort") || !strings.Contains(h.stderr.String(), "low|medium|high|max") {
		t.Errorf("exit %d stderr %q", code, h.stderr.String())
	}
}

func TestDoctorRepoFlag(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--repo", h.repo); code != 0 || !strings.Contains(h.stdout.String(), "Git 仓库：") {
		t.Errorf("exit %d\n%s", code, h.stdout.String())
	}
}

func TestDebugLogsAreRedacted(t *testing.T) {
	h := newHarness(t)
	h.env["ACO_LOG_LEVEL"] = "debug"
	h.env["ANTHROPIC_API_KEY"] = "sk-ant-api03-FAKEFAKEFAKEFAKE00"
	h.env["ACO_AUTH_MODE"] = "api"
	h.env["OPENAI_API_KEY"] = "sk-proj-FAKEFAKEFAKEFAKE00"
	h.run("doctor")
	if !strings.Contains(h.stderr.String(), "doctor starting") {
		t.Errorf("expected debug log on stderr: %q", h.stderr.String())
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), "FAKEFAKE") {
		t.Errorf("secret leaked")
	}
}
