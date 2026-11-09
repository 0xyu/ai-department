//go:build integration

package gates

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
)

// copyFixture copies a fixture repository into a fresh git repository.
func copyFixture(t *testing.T, name string) *gitRepo {
	t.Helper()
	g := newGitRepo(t)
	for _, f := range []string{"app.go", "app_test.go"} {
		os.Remove(filepath.Join(g.dir, f))
	}
	src := fixture(name)
	err := filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		writeFile(t, g.dir, filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g.commit("fixture " + name)
	return g
}

// lookCore finds real programs but hides optional tools, so the result does
// not depend on what is installed on the machine.
func lookCore(name string) (string, error) {
	switch name {
	case "gosec", "govulncheck", "golangci-lint", "staticcheck", "semgrep", "trivy":
		return "", errors.New("hidden")
	}
	return exec.LookPath(name)
}

// End to end on the Go fixture with the real toolchain: auto-detection,
// the baseline round (TestKnownBroken fails), a passing round with nothing
// new, and a regression listed by name (AC-06.1, AC-06.2).
func TestGoFixtureEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	g := copyFixture(t, "go")
	ctx := context.Background()
	cfg := config.Default()
	specs, probs := Resolve(ResolveInput{Mode: ModePhase, Config: cfg.Gates, Dir: g.dir, LookPath: lookCore})
	if len(probs) > 0 {
		t.Fatalf("preflight: %v", probs)
	}
	head := g.git("rev-parse", "HEAD")
	r, err := New(Options{Dir: g.dir, Repo: g.r, Base: head, Policy: policy.FromConfig(cfg.Policy, nil, nil),
		ExtraEnv: []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=mod"}})
	if err != nil {
		t.Skip(err)
	}
	baseRound, err := r.Run(ctx, specs, RunOptions{Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range baseRound.Results {
		// The integration command runs every test, so it sees the known
		// failure too.
		want := Passed
		if res.Gate == Unit || res.Gate == Integration {
			want = Failed
		}
		if res.Status != want {
			t.Fatalf("baseline %s = %s (%s)\n%s", res.Gate, res.Status, res.Summary, firstLog(res))
		}
		if res.TreeHash != baseRound.TreeHash || res.TreeAfter != "" {
			t.Errorf("%s: tree %q after %q, round %q", res.Gate, res.TreeHash, res.TreeAfter, baseRound.TreeHash)
		}
	}
	tree, _ := g.r.ResolveTree(ctx, "HEAD")
	if baseRound.TreeHash != tree {
		t.Errorf("round tree %s, HEAD tree %s", baseRound.TreeHash, tree)
	}
	base := NewBaseline(baseRound, head, time.Now())

	again, err := r.Run(ctx, specs, RunOptions{Compare: base, FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Passed() {
		f := again.FirstFailure()
		t.Fatalf("unchanged tree must pass against its baseline: %s %s\n%s", f.Gate, f.Summary, firstLog(f))
	}

	// Break Add (and touch its test so the diff policy's tests_missing
	// does not fire): TestAdd passed at baseline and fails now.
	calc := filepath.Join(g.dir, "calc.go")
	b, _ := os.ReadFile(calc)
	os.WriteFile(calc, []byte(strings.Replace(string(b), "return a + b", "return a + b + 1", 1)), 0o644)
	tf := filepath.Join(g.dir, "calc_test.go")
	tb, _ := os.ReadFile(tf)
	os.WriteFile(tf, append(tb, []byte("\n// touched\n")...), 0o644)
	broken, err := r.Run(ctx, specs, RunOptions{Compare: base})
	if err != nil {
		t.Fatal(err)
	}
	u := broken.Gate(Unit)
	if u.Status != Failed || len(u.Comparison.Regressions) != 1 || u.Comparison.Regressions[0] != "example.com/goproj.TestAdd" {
		t.Fatalf("unit: %s %+v", u.Status, u.Comparison)
	}
	if u.TreeHash == baseRound.TreeHash {
		t.Error("result not bound to the changed tree")
	}
	if i := broken.Gate(Integration); i.Status != Failed || len(i.Comparison.Regressions) != 2 { // TestAdd, TestIntegrationAdd
		t.Errorf("integration: %s %+v", i.Status, i.Comparison)
	}
	if s := broken.Gate(Security); s.Status != Passed {
		t.Errorf("security: %s %+v", s.Status, s.Comparison)
	}
	// A compile error is a new failure in typecheck and build.
	os.WriteFile(calc, []byte("package goproj\n\nfunc Add(a, b int) int { return a + }\n"), 0o644)
	cerr, _ := r.Run(ctx, specs, RunOptions{Compare: base})
	for _, gname := range []string{Typecheck, Build, Unit} {
		if res := cerr.Gate(gname); res.Status != Failed || res.Class != ClassQuality {
			t.Errorf("compile error, %s: %s/%s", gname, res.Status, res.Class)
		}
	}
	_ = gitops.EmptyTree
}

func firstLog(r *Result) string {
	if c := r.FailedCommand(); c != nil {
		return c.Command.Run + "\n" + c.Log
	}
	return ""
}
