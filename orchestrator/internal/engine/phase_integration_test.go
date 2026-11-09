//go:build integration

package engine

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/state"
)

// realGates is the real gate runner with a pinned Go environment.
func realGates(o gates.Options) (GateRunner, error) {
	o.ExtraEnv = append(o.ExtraEnv, "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	return gates.New(o)
}

// lookCore finds real programs but hides optional tools.
func lookCore(name string) (string, error) {
	switch name {
	case "gosec", "govulncheck", "golangci-lint", "staticcheck", "semgrep", "trivy":
		return "", errors.New("hidden")
	}
	return exec.LookPath(name)
}

const vetBroken = `package calc

import "fmt"

func Add(a, b int) int { return a + b }

// Sub subtracts.
func Sub(a, b int) int {
	fmt.Printf("%d\n", "x")
	return a - b
}
`

const subFixed = `package calc

func Add(a, b int) int { return a + b }

// Sub subtracts.
func Sub(a, b int) int { return a - b }
`

const subTest = `package calc

import "testing"

func TestSub(t *testing.T) {
	for _, c := range []struct{ a, b, want int }{{3, 1, 2}, {5, 5, 0}} {
		if got := Sub(c.a, c.b); got != c.want {
			t.Fatalf("Sub(%d, %d) = %d", c.a, c.b, got)
		}
	}
}
`

// TestPhasesEndToEndRealGates runs two phases with the real gate runner and
// Go toolchain: a go vet failure in phase 1 is fixed in the same session,
// both phases commit in order, and phase 2 reuses phase 1's gate results
// as its baseline.
func TestPhasesEndToEndRealGates(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	if _, err := gates.DefaultShell(exec.LookPath); err != nil {
		t.Skip(err)
	}
	pe := newPhaseEnv(t, phaseOpts{})
	d := pe.phaseDeps()
	d.Gates, d.LookPath = realGates, lookCore

	pe.impl.Steps = []fake.ImplStep{
		{Edit: func(dir string) error {
			if err := edit("calc/calc.go", vetBroken)(dir); err != nil {
				return err
			}
			return edit("calc/calc_test.go", subTest)(dir)
		}, Result: fake.ImplResult(implReport("subtraction", ac1))},
		{Edit: edit("calc/calc.go", subFixed), Result: fake.ImplResult(implReport("fixed vet", ac1, "G-typecheck-1"))},
		{Edit: func(dir string) error {
			if err := edit("calc/mul.go", "package calc\n\n// Mul multiplies.\nfunc Mul(a, b int) int { return a * b }\n")(dir); err != nil {
				return err
			}
			return edit("calc/mul_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif Mul(2, 3) != 6 {\n\t\tt.Fatal()\n\t}\n}\n")(dir)
		}, Result: fake.ImplResult(fake.Report("multiplication", []string{"AC-02.1", "AC-02.2"}))},
	}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1), fake.CodeReviewApprove([]string{"AC-02.1", "AC-02.2"}))

	out1, err := RunPhase(pe.ctx, d, PhaseInput{RunID: pe.run.ID, Phase: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out1.Status != state.PhaseCommitted {
		b, _ := pe.runDir.Read(out1.BlockedReport)
		t.Fatalf("phase 1: %+v\n%s", out1, b)
	}
	if len(pe.impl.Continues) != 1 || pe.impl.Continues[0].GateFailures[0].Gate != gates.Typecheck ||
		!strings.Contains(pe.impl.Continues[0].GateFailures[0].Log, "Printf") {
		t.Fatalf("gate fix %+v", pe.impl.Continues)
	}
	if gr := pe.reviewer.CodeReviews[0].GateResults; strings.Contains(gr, ": failed") {
		t.Fatalf("reviewed with red gates:\n%s", gr)
	}
	out2, err := RunPhase(pe.ctx, d, PhaseInput{RunID: pe.run.ID, Phase: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != state.PhaseCommitted {
		b, _ := pe.runDir.Read(out2.BlockedReport)
		t.Fatalf("phase 2: %+v\n%s", out2, b)
	}
	log := pe.git("log", "--format=%s", pe.freeze+".."+out2.Commit)
	if log != "feat(phase-02): Operation 2\nfeat(phase-01): Operation 1" {
		t.Fatalf("run branch history:\n%s", log)
	}
	for n := 1; n <= 2; n++ {
		r, err := reports.ReadPhase(pe.wt.Dir(), n)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range r.Gates {
			if g.Status != "passed" && g.Status != "not_applicable" {
				t.Fatalf("phase %d gate %+v", n, g)
			}
		}
	}
	// One baseline round in total: phase 2 compares with phase 1's results.
	steps, _ := pe.store.ListSteps(pe.ctx, pe.run.ID)
	baselines := 0
	for _, s := range steps {
		if s.Kind == "baseline" {
			baselines++
		}
	}
	if baselines != 1 {
		t.Fatalf("baseline rounds %d", baselines)
	}
	if clean := pe.wtGit("status", "--porcelain"); clean != "" {
		t.Fatalf("worktree not clean: %s", clean)
	}
}
