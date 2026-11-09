package codexreview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
)

// linkDir points link at the directory target: a junction on Windows (no
// privilege needed), a symlink elsewhere.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Relative operands are resolved against the tracked working directory and
// their link targets checked against the roots (real filesystem).
func TestMonitorRelativeLinks(t *testing.T) {
	root := t.TempDir()
	work, art, outside := filepath.Join(root, "work"), filepath.Join(root, "art"), filepath.Join(root, "outside")
	writeFile(t, filepath.Join(work, "calc.go"), "package calc\n")
	writeFile(t, filepath.Join(work, "sub", "x.go"), "package sub\n")
	writeFile(t, filepath.Join(outside, "secret", "id_rsa"), "KEY\n")
	writeFile(t, filepath.Join(outside, "file.txt"), "secret\n")
	_ = os.MkdirAll(art, 0o700)
	linkDir(t, filepath.Join(outside, "secret"), filepath.Join(work, "linkdir"))
	// A link inside the worktree to another worktree directory is fine.
	linkDir(t, filepath.Join(work, "sub"), filepath.Join(work, "inner"))
	m := &Monitor{Roots: []string{work, art}}
	for _, c := range []struct {
		cmd  string
		flag bool
	}{
		{"cat linkdir/id_rsa", true},                 // through a linked directory / junction
		{"cd linkdir && cat id_rsa", true},           // cd into it
		{"cd sub && cat ../linkdir/id_rsa", true},    // relative to the tracked cwd
		{"cat calc.go", false},                       // normal in-worktree read
		{"cd sub && cat x.go ../calc.go", false},     // relative to the tracked cwd, inside
		{"cat inner/x.go", false},                    // link that stays inside
		{"rg -n Add calc.go missing-file.go", false}, // nonexistent operands
	} {
		if got := len(m.Command(c.cmd)) > 0; got != c.flag {
			t.Errorf("%q: flagged=%v, want %v (%+v)", c.cmd, got, c.flag, m.Command(c.cmd))
		}
	}
	// A relative file symlink (needs symlink privilege on Windows).
	if err := os.Symlink(filepath.Join(outside, "file.txt"), filepath.Join(work, "flink")); err != nil {
		t.Logf("file symlink not available here: %v", err)
	} else if vs := m.Command("cat flink"); len(vs) == 0 {
		t.Error("a relative file symlink to outside was not flagged")
	}
	if err := os.Symlink(filepath.Join("..", "outside", "file.txt"), filepath.Join(work, "rlink")); err == nil {
		if vs := m.Command("cat rlink"); len(vs) == 0 {
			t.Error("a symlink with a relative target to outside was not flagged")
		}
	}
}

func TestResolveLinksAndOverlaps(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	_ = os.MkdirAll(filepath.Join(real, "a"), 0o700)
	linkDir(t, real, filepath.Join(root, "alias"))
	got, err := resolveLinks(filepath.Join(root, "alias", "a", "missing"))
	if err != nil || !samePath(got, filepath.Join(real, "a", "missing")) {
		t.Fatalf("resolve: %s %v", got, err)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{filepath.Join(root, "alias"), real, true},
		{filepath.Join(root, "alias", "a"), real, true},
		{real, filepath.Join(root, "alias", "a", "x"), true},
		{filepath.Join(root, "other"), real, false},
	} {
		if got, err := overlaps(c.a, c.b); err != nil || got != c.want {
			t.Errorf("overlaps(%s, %s) = %v %v", c.a, c.b, got, err)
		}
	}
}

// The adapter discards a review that read through a linked directory and
// runs it once more.
func TestAdapterLinkedReadRetriedOnce(t *testing.T) {
	f := newFixture(t, "read_link.jsonl", "code_valid.jsonl")
	writeFile(t, filepath.Join(f.dir, "outside", "id_rsa"), "KEY\n")
	linkDir(t, filepath.Join(f.dir, "outside"), filepath.Join(f.work, "linkdir"))
	res, err := f.adapter(auth.Subscription, func(o *Options) { o.UserHome = "" }).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 2 || len(res.Calls[0].Violations) == 0 || res.Calls[0].Violations[0].Kind != "read" {
		t.Fatalf("calls %+v", res.Calls)
	}

	g := newFixture(t, "read_link.jsonl")
	writeFile(t, filepath.Join(g.dir, "outside", "id_rsa"), "KEY\n")
	linkDir(t, filepath.Join(g.dir, "outside"), filepath.Join(g.work, "linkdir"))
	_, err = g.adapter(auth.Subscription, func(o *Options) { o.UserHome = "" }).ReviewCode(context.Background(), g.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassPolicy || len(ae.Calls) != 2 {
		t.Fatalf("second violation: %v", err)
	}
	if _, err := g.run.Read("phases/P01/a1/reviews/r01/review.json"); err == nil {
		t.Error("a discarded review was stored as accepted")
	}
}
