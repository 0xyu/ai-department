package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/secure/securetest"
)

func hookPolicy(t *testing.T, h *harness) (string, string, string) {
	t.Helper()
	wt := filepath.Join(h.repo, "wt")
	if err := os.MkdirAll(filepath.Join(wt, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	denyLog := filepath.Join(h.home, "denials.jsonl")
	g := &policy.GuardPolicy{Version: policy.GuardVersion, RunID: "r-1", Profile: policy.ProfileImplementer, Root: wt,
		ProtectedPaths: config.Default().Policy.ProtectedPaths, ScopeEnforcement: "flag", Network: policy.NetworkDeny, DenyLog: denyLog}
	p := filepath.Join(h.home, "guard.json")
	if err := policy.WriteGuardPolicy(p, g); err != nil {
		t.Fatal(err)
	}
	return p, wt, denyLog
}

func toolJSON(t *testing.T, tool string, input map[string]any, cwd string) string {
	b, err := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The hook command end to end: allowed calls exit 0 silently, denied ones
// exit 2 with the reason on stderr and a line in the deny log.
func TestHookPreToolUse(t *testing.T) {
	h := newHarness(t)
	p, wt, denyLog := hookPolicy(t, h)
	hook := func(stdin string, extra ...string) int {
		h.stdin = stdin
		return h.run(append([]string{"hook", "pre-tool-use", "--run", "r-1", "--policy", p}, extra...)...)
	}
	if code := hook(toolJSON(t, "Bash", map[string]any{"command": "go test ./..."}, wt)); code != 0 || h.stderr.Len() > 0 || h.stdout.Len() > 0 {
		t.Fatalf("allowed call: exit %d, stdout %q, stderr %q", code, h.stdout.String(), h.stderr.String())
	}
	if code := hook(toolJSON(t, "Edit", map[string]any{"file_path": "internal/a.go"}, wt)); code != 0 {
		t.Fatalf("edit in worktree: exit %d %s", code, h.stderr.String())
	}
	denied := []struct{ tool, key, val, rule string }{
		{"Bash", "command", "git push origin HEAD", policy.RuleForbiddenGit},
		{"Bash", "command", "git commit -m x", policy.RuleForbiddenGit},
		{"Bash", "command", "rm -rf /", policy.RuleOutsideWorktree},
		{"Read", "file_path", "../../etc/passwd", policy.RuleOutsideWorktree},
		{"Write", "file_path", ".aco/plan/P01.md", policy.RuleProtectedPath},
		{"Edit", "file_path", "AGENTS.md", policy.RuleProtectedPath},
		{"Write", "file_path", ".github/workflows/x.yml", policy.RuleProtectedPath},
	}
	for _, d := range denied {
		code := hook(toolJSON(t, d.tool, map[string]any{d.key: d.val}, wt))
		if code != hookBlock || !strings.HasPrefix(h.stderr.String(), "ACO guard:") {
			t.Errorf("%s %s: exit %d, stderr %q", d.tool, d.val, code, h.stderr.String())
		}
	}
	b, err := os.ReadFile(denyLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != len(denied) {
		t.Fatalf("deny log has %d lines, want %d", len(lines), len(denied))
	}
	for i, l := range lines {
		var rec map[string]string
		if err := json.Unmarshal([]byte(l), &rec); err != nil || rec["rule"] != denied[i].rule || rec["tool"] != denied[i].tool {
			t.Errorf("deny log line %d: %s", i, l)
		}
		if strings.Contains(l, "git push") {
			t.Error("deny log must not copy command text")
		}
	}
}

// Every failure to decide blocks the call.
func TestHookFailsClosed(t *testing.T) {
	h := newHarness(t)
	p, wt, _ := hookPolicy(t, h)
	cases := map[string][]string{
		"missing policy": {"--policy", filepath.Join(h.home, "none.json")},
		"other run":      {"--run", "r-2", "--policy", p},
	}
	for name, args := range cases {
		h.stdin = toolJSON(t, "Read", map[string]any{"file_path": "a"}, wt)
		if code := h.run(append([]string{"hook", "pre-tool-use"}, args...)...); code != hookBlock {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	for name, stdin := range map[string]string{
		"empty":       "",
		"not json":    "{",
		"no tool":     `{"tool_input":{}}`,
		"other event": `{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{}}`,
		"too large":   `{"tool_name":"Read","x":"` + strings.Repeat("a", maxHookInput) + `"}`,
	} {
		h.stdin = stdin
		if code := h.run("hook", "pre-tool-use", "--run", "r-1", "--policy", p); code != hookBlock {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	h.stdin = ""
	if code := h.run("hook", "post-tool-use", "--policy", p); code != 2 {
		t.Errorf("unknown event name: exit %d", code)
	}
}

// A secret in a denied path is masked in the reason and the deny log.
func TestHookMasksSecrets(t *testing.T) {
	h := newHarness(t)
	h.env["ANTHROPIC_API_KEY"] = securetest.FakeAnthropic
	p, wt, denyLog := hookPolicy(t, h)
	h.stdin = toolJSON(t, "Write", map[string]any{"file_path": ".aco/" + securetest.FakeAnthropic}, wt)
	if code := h.run("hook", "pre-tool-use", "--run", "r-1", "--policy", p); code != hookBlock {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(denyLog)
	if strings.Contains(h.stderr.String(), securetest.FakeAnthropic) || strings.Contains(string(b), securetest.FakeAnthropic) {
		t.Error("secret leaked through the hook")
	}
}
