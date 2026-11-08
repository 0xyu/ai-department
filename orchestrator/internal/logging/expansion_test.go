package logging

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure/securetest"
)

// selfNesting returns a group that contains itself (twice, so an unbounded
// handler would also blow up in width, not only in depth).
type selfNesting struct{}

func (selfNesting) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("k", securetest.FakeAnthropic),
		slog.Any("a", selfNesting{}),
		slog.Any("b", selfNesting{}),
	)
}

const recursionChildEnv = "ACO_TEST_LOGVALUER_RECURSION_CHILD"

// TestRecursiveLogValuerIsBounded runs the recursive case in a child process:
// before the shared budget, it was a fatal stack overflow that no recover()
// can catch and would take the whole test binary down.
func TestRecursiveLogValuerIsBounded(t *testing.T) {
	if os.Getenv(recursionChildEnv) == "1" {
		for _, jsonOut := range []bool{true, false} {
			out := emit(jsonOut, func(l *slog.Logger) {
				l.Info("m", "before", "first", "loop", selfNesting{}, "after", "last", slog.Group("g", "inner", "v"))
			})
			_, _ = os.Stdout.WriteString(out)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecursiveLogValuerIsBounded$", "-test.v")
	cmd.Env = append(os.Environ(), recursionChildEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 2000 {
			tail = tail[:2000]
		}
		t.Fatalf("child process failed: %v\n%s", err, tail)
	}
	out := stdout.String()
	securetest.AssertNoLeak(t, "recursive LogValuer", out)
	lines := strings.Split(out, "\n")
	var jsonLine, textLine string
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "{"):
			jsonLine = ln
		case strings.HasPrefix(ln, "time="):
			textLine = ln
		}
	}
	if jsonLine == "" || textLine == "" {
		t.Fatalf("missing log lines:\n%s", out)
	}
	rec := mustJSON(t, jsonLine)
	if rec["before"] != "first" || rec["after"] != "last" {
		t.Errorf("sibling attributes lost: before=%v after=%v", rec["before"], rec["after"])
	}
	if g, ok := rec["g"].(map[string]any); !ok || g["inner"] != "v" {
		t.Errorf("sibling group lost: %#v", rec["g"])
	}
	if !strings.Contains(jsonLine, TruncatedPlaceholder) {
		t.Errorf("no truncation placeholder in JSON output")
	}
	for _, want := range []string{"before=first", "after=last", "g.inner=v", TruncatedPlaceholder} {
		if !strings.Contains(textLine, want) {
			t.Errorf("text output missing %q", want)
		}
	}
	if n := len(jsonLine); n > 1<<20 {
		t.Errorf("JSON line is %d bytes; expansion not bounded", n)
	}
}

// Deep groups built without a LogValuer are bounded by depth too.
func TestDeepGroupIsTruncated(t *testing.T) {
	v := slog.StringValue("leaf")
	for i := 0; i < maxValueDepth+5; i++ {
		v = slog.GroupValue(slog.Attr{Key: "n", Value: v})
	}
	out := emit(true, func(l *slog.Logger) { l.Info("m", slog.Attr{Key: "deep", Value: v}, "after", "ok") })
	if !strings.Contains(out, TruncatedPlaceholder) || strings.Contains(out, "leaf") {
		t.Errorf("deep group not truncated:\n%s", out)
	}
	if rec := mustJSON(t, out); rec["after"] != "ok" {
		t.Errorf("sibling lost: %v", rec["after"])
	}
}
