package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

// TestCLINeverPrintsAFakeCredential runs the CLI with every fake credential
// in every place a user could put one by mistake (flags, --set, positional
// arguments, env vars, config files, the env file) and checks stdout, stderr,
// and debug logs for any encoding of it.
func TestCLINeverPrintsAFakeCredential(t *testing.T) {
	for _, f := range securetest.All {
		type scenario struct {
			name  string
			setup func(h *harness)
			args  []string
		}
		scenarios := []scenario{
			{"set secret field", nil, []string{"doctor", "--set", "auth.api_key=" + f}},
			{"set bare", nil, []string{"doctor", "--set", f}},
			{"set auth mode", nil, []string{"doctor", "--set", "auth.mode=" + f}},
			{"set effort", nil, []string{"doctor", "--set", "planner.effort=" + f}},
			{"set duration", nil, []string{"doctor", "--set", "limits.idle_timeout=" + f}},
			{"set bad yaml", nil, []string{"doctor", "--set", "planner.model=[" + f}},
			{"set unknown key", nil, []string{"doctor", "--set", f + "=1"}},
			{"set unknown nested key", nil, []string{"doctor", "--set", "planner." + f + "=1"}},
			{"set map key", nil, []string{"doctor", "--set", "pricing.codex." + f + ".input_per_mtok=-1"}},
			{"set map key applied", nil, []string{"doctor", "--set", "pricing.codex." + f + ".input_per_mtok=1"}},
			{"auth flag", nil, []string{"doctor", "--auth-mode", f}},
			{"agent auth flag", nil, []string{"doctor", "--claude-auth-mode=" + f}},
			{"int flag", nil, []string{"resume", "id", "--from-phase", f}},
			{"bool flag", nil, []string{"resume", "id", "--retry=" + f}},
			{"unknown flag", nil, []string{"doctor", "--" + f}},
			{"positional", nil, []string{"doctor", f}},
			{"after --", nil, []string{"version", "--", f}},
			{"unknown command", nil, []string{f}},
			{"env auth mode", func(h *harness) { h.env["ACO_AUTH_MODE"] = f }, []string{"doctor"}},
			{"env fallback", func(h *harness) { h.env["ACO_AUTH_FALLBACK"] = f }, []string{"doctor"}},
			{"config value", func(h *harness) {
				h.write(filepath.Join(h.repo, ".aco.yaml"), "auth: {mode: \""+yamlEscape(f)+"\"}\n")
			}, []string{"doctor"}},
			{"config unknown key", func(h *harness) {
				h.write(filepath.Join(h.repo, ".aco.yaml"), "\""+yamlEscape(f)+"\": 1\n")
			}, []string{"doctor"}},
			{"config unknown nested key", func(h *harness) {
				h.write(filepath.Join(h.repo, ".aco.yaml"), "planner:\n  \""+yamlEscape(f)+"\": 1\n")
			}, []string{"doctor"}},
			{"config unknown gate type", func(h *harness) {
				h.write(filepath.Join(h.repo, ".aco.yaml"), "gates:\n  \""+yamlEscape(f)+"\": {}\n")
			}, []string{"doctor"}},
			{"config secret key", func(h *harness) {
				h.write(filepath.Join(h.repo, ".aco.yaml"), "reviewer:\n  api_key: \""+yamlEscape(f)+"\"\n")
			}, []string{"doctor"}},
			{"env file key in api mode", func(h *harness) {
				h.write(filepath.Join(h.home, ".aco", ".env"), "ANTHROPIC_API_KEY='"+f+"'\nACO_AUTH_MODE=api\n")
			}, []string{"doctor"}},
			{"env file bad line", func(h *harness) {
				h.write(filepath.Join(h.home, ".aco", ".env"), "OK=1\n"+f+"\n")
			}, []string{"doctor"}},
			{"process env key", func(h *harness) {
				h.env["ANTHROPIC_API_KEY"] = f
				h.env["ACO_AUTH_MODE"] = "api"
			}, []string{"doctor"}},
		}
		for _, sc := range scenarios {
			for _, format := range []string{"text", "json"} {
				h := newHarness(t)
				h.env["ACO_LOG_LEVEL"] = "debug"
				h.env["ACO_LOG_FORMAT"] = format
				if sc.setup != nil {
					sc.setup(h)
				}
				h.run(sc.args...)
				securetest.AssertNoLeak(t, sc.name+"/"+format, h.stdout.String()+h.stderr.String(), f)
			}
		}
	}
}

// A registered credential shorter than the Redactor's masking threshold is
// never echoed back by a diagnostic.
func TestCLINeverEchoesAShortRegisteredCredential(t *testing.T) {
	const short = "Fk7q2x" // fake six-character OPENAI_API_KEY
	for _, args := range [][]string{
		{short},
		{"x" + short},
		{"doctor", short},
		{"doctor", "--" + short},
		{"doctor", "--set", short + "=1"},
		{"version", "--", short},
	} {
		h := newHarness(t)
		h.env["OPENAI_API_KEY"] = short
		h.env["ACO_LOG_LEVEL"] = "debug"
		h.run(args...)
		out := h.stdout.String() + h.stderr.String()
		if strings.Contains(out, short) {
			t.Errorf("%v: short credential printed:\n%s", args, out)
		}
	}
}

// Diagnostics for an untrusted key still point at it by trusted parts.
func TestCLIUntrustedKeyDiagnosticsStayUseful(t *testing.T) {
	f := securetest.FakeUnregistered
	h := newHarness(t)
	h.run("doctor", "--set", "planner.model=x", "--set", "planner."+f+"=1")
	if out := h.stderr.String(); !strings.Contains(out, "--set #2") || !strings.Contains(out, "planner."+secure.Hidden) {
		t.Errorf("want ordinal and trusted path:\n%s", out)
	}
	h = newHarness(t)
	h.write(filepath.Join(h.repo, ".aco.yaml"), "planner:\n  "+f+": 1\n")
	h.run("doctor")
	if out := h.stderr.String(); !strings.Contains(out, "第 2 行") || !strings.Contains(out, "planner."+secure.Hidden) {
		t.Errorf("want file line and trusted path:\n%s", out)
	}
	h = newHarness(t)
	h.run("doctor", "--set", "planner.colour=1", "--set", "auth.api_key=x")
	if out := h.stderr.String(); !strings.Contains(out, "planner.") || !strings.Contains(out, "--set auth.api_key") {
		t.Errorf("want known secret key name kept:\n%s", out)
	}
}

// yamlEscape escapes a value for a double-quoted YAML scalar.
func yamlEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}
