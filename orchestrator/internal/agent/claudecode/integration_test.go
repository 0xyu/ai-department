//go:build integration

package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildACO builds the aco binary for use as the guard hook.
func buildACO(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "aco")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/aco")
	cmd.Dir = filepath.Join("..", "..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// The generated settings run the real `aco hook pre-tool-use` with the
// invocation's guard policy: denied calls exit 2 with a reason, allowed calls
// exit 0, and the adapter collects the denials from the deny log.
func TestGuardHookEndToEnd(t *testing.T) {
	aco := buildACO(t)
	h := newHarness(t)
	calls := []map[string]any{
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}},
		{"tool_name": "Edit", "tool_input": map[string]any{"file_path": "internal/x.go"}},
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "git push origin HEAD"}},
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -am wip"}},
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /"}},
		{"tool_name": "Read", "tool_input": map[string]any{"file_path": "../../etc/passwd"}},
		{"tool_name": "Write", "tool_input": map[string]any{"file_path": ".aco/plan/P01.md"}},
		{"tool_name": "Edit", "tool_input": map[string]any{"file_path": "AGENTS.md"}},
		{"tool_name": "Write", "tool_input": map[string]any{"file_path": ".github/workflows/x.yml"}},
		{"tool_name": "mcp__x__y", "tool_input": map[string]any{}},
	}
	wantExit := []int{0, 0, 2, 2, 2, 2, 2, 2, 2, 2}
	cf := h.file("calls.json")
	b, _ := json.Marshal(calls)
	if err := os.WriteFile(cf, b, 0o600); err != nil {
		t.Fatal(err)
	}
	outFile := h.file("hookout.json")
	a := h.adapter(func(o *Options) { o.HookBin = aco }, "fixture="+h.fixture("success_impl", nil), "hookcalls="+cf, "hookout="+outFile)
	res, err := a.Implementer().Start(context.Background(), h.implReq("e2e/a"))
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Exit   int    `json:"exit"`
		Stderr string `json:"stderr"`
	}
	ob, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ob, &got); err != nil || len(got) != len(calls) {
		t.Fatalf("hook results: %v %s", err, ob)
	}
	for i, r := range got {
		if r.Exit != wantExit[i] {
			t.Errorf("call %d %v: exit %d (%s)", i, calls[i], r.Exit, r.Stderr)
		}
		if r.Exit == 2 && !strings.HasPrefix(r.Stderr, "ACO guard:") {
			t.Errorf("call %d: reason %q", i, r.Stderr)
		}
	}
	if len(res.GuardDenials) != 8 {
		t.Errorf("adapter collected %d denials, want 8: %+v", len(res.GuardDenials), res.GuardDenials)
	}
}
