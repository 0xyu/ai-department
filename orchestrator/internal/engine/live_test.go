//go:build live

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/claudecode"
	"github.com/0xyu/aco/internal/agent/codexreview"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/state"
)

// AC-08.5: with the real Claude Code and Codex CLIs (subscription mode by
// default), a short requirement document produces a frozen plan of at least
// two phases on a toy repository. Run with
//
//	ACO_LIVE_PLAN=1 go test -tags=live ./internal/engine/ -run TestLivePlan -v -timeout 90m
//
// ACO_LIVE_AUTH_MODE=api uses ANTHROPIC_API_KEY and OPENAI_API_KEY instead.
func TestLivePlan(t *testing.T) {
	if os.Getenv("ACO_LIVE_PLAN") != "1" {
		t.Skip("set ACO_LIVE_PLAN=1 to call the real Claude Code and Codex CLIs")
	}
	for _, bin := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	mode := auth.Subscription
	if os.Getenv("ACO_LIVE_AUTH_MODE") == "api" {
		mode = auth.API
	}
	e := newEnv(t, envOpts{})
	e.cfg.Limits.PlanReviewRounds = 3
	red := secure.NewRedactor()
	env := auth.NewEnv(os.LookupEnv, nil)
	budget := agent.NewBudget(e.cfg.Limits.BudgetUSD)

	co := claudecode.OptionsFromConfig(e.cfg, mode, env, red)
	co.HookBin = buildACO(t)
	co.Budget = budget
	co.InvocationTimeout, co.IdleTimeout = 30*time.Minute, 10*time.Minute
	home, _ := os.UserHomeDir()
	xo := codexreview.OptionsFromConfig(e.cfg, mode, env, red, filepath.Join(t.TempDir(), "aco"), home)
	xo.Budget = budget
	xo.InvocationTimeout, xo.IdleTimeout = 30*time.Minute, 10*time.Minute

	d := e.deps()
	d.Planner = claudecode.New(co).Planner()
	d.Reviewer = codexreview.New(xo)
	d.PlannerAuth, d.ReviewerAuth = string(mode), string(mode)
	d.Now = time.Now
	d.LookPath = exec.LookPath
	t.Logf("artifacts: %s", e.runDir.Path())

	reqs := "# Toy calculator\n\nExtend package calc (calc/calc.go) of this Go module:\n\n" +
		"1. Add Sub(a, b int) int that returns a - b.\n" +
		"2. Add Div(a, b int) (int, error) that returns an error when b is 0.\n" +
		"3. Every function has table-driven unit tests.\n\n" +
		"Plan it in at least two phases: first Sub, then Div.\n"
	out, err := RunPlanning(e.ctx, d, PlanningInput{RunID: e.run.ID, BaseCommit: e.base, Requirements: reqs})
	if err != nil {
		t.Fatalf("RunPlanning: %v", err)
	}
	t.Logf("status %s, rounds %d, revisions %d, decision %+v", out.Status, out.Rounds, out.Revisions, out.Decision)
	if out.Package != nil {
		for _, ph := range out.Package.Phases {
			t.Logf("phase %d: %s (%d AC)", ph.Number, ph.Title, len(ph.AcceptanceCriteria))
		}
	}
	for _, f := range out.Ledger.Entries {
		t.Logf("finding %s [%s/%s] %s -> %s", f.Finding.ID, f.Finding.Severity, f.Finding.Dimension, f.Finding.Title, f.Status)
	}
	if out.Status != state.RunCompleted || !out.Frozen() {
		t.Fatalf("not frozen: %+v", out.Reason)
	}
	if n := len(out.Package.Phases); n < 2 {
		t.Fatalf("%d phases, want >= 2", n)
	}
	assertFrozen(t, e, out, out.Rounds, true)
}

func buildACO(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "aco")
	if filepath.Separator == '\\' {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/aco")
	cmd.Dir = filepath.Join("..", "..")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}
