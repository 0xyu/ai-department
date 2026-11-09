package engine

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/prompt"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs (run with -update to accept):\n%s", p, got)
	}
}

// AC-09.3: the implement prompt holds the Phase Specification and AGENTS.md
// verbatim, as the engine read them from the frozen worktree.
func TestPhaseImplementPromptGolden(t *testing.T) {
	pe := newPhaseEnv(t, phaseOpts{agentsMD: true})
	pe.impl.Steps = []fake.ImplStep{startStep()}
	pe.reviewer.CodeSteps = reviews(fake.CodeReviewApprove(ac1))
	pe.runPhase(1)
	if len(pe.impl.Starts) != 1 {
		t.Fatal("no start")
	}
	req := pe.impl.Starts[0]
	got, err := prompt.Render(prompt.PhaseImplement, req)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile(filepath.Join(pe.wt.Dir(), filepath.FromSlash(plan.PackageDir), "phases", "P01.md"))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := os.ReadFile(filepath.Join(pe.wt.Dir(), "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	overview, err := os.ReadFile(filepath.Join(pe.wt.Dir(), filepath.FromSlash(plan.PackageDir), plan.PlanFileName))
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n") }
	for name, text := range map[string]string{"phase spec": string(spec), "AGENTS.md": string(agents), "plan overview": string(overview)} {
		if !strings.Contains(got, norm(text)) {
			t.Fatalf("the implement prompt lacks the %s verbatim:\n%s", name, text)
		}
	}
	if req.PhaseSpec != string(spec) || req.AgentsMD != string(agents) {
		t.Fatal("the request does not carry the frozen files byte for byte")
	}
	golden(t, "phase-implement.golden.md", got)
}
