package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Integration test: needs Docker running. Uses the real project workspace.
func TestSandboxExec(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	s := &Sandbox{WorkspaceDir: filepath.Join(root, "workspace"), DockerfileDir: filepath.Join(root, "sandbox")}
	ctx := context.Background()
	if err := s.Ensure(ctx); err != nil {
		t.Skip("docker unavailable:", err)
	}
	marker := filepath.Join(s.WorkspaceDir, ".sandbox_probe")
	os.WriteFile(marker, []byte("hello"), 0o644)
	defer os.Remove(marker)

	r := s.Exec(ctx, false, "", "cat .sandbox_probe && python3 --version && git --version", 30*time.Second)
	if r.ExitCode != 0 || !strings.Contains(r.Output, "hello") || !strings.Contains(r.Output, "Python 3") {
		t.Fatalf("rw exec: %+v", r)
	}
	r = s.Exec(ctx, true, "", "touch should_fail", 30*time.Second)
	if r.ExitCode == 0 {
		t.Fatalf("read-only container allowed a write: %+v", r)
	}
	r = s.Exec(ctx, false, "", "sleep 5", 1*time.Second)
	if !r.TimedOut {
		t.Fatalf("expected timeout: %+v", r)
	}
}
