package codexreview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent"
)

type memStore struct{ files map[string]string }

func (m *memStore) store(rel string, data []byte) (string, error) {
	if m.files == nil {
		m.files = map[string]string{}
	}
	m.files[rel] = string(data)
	return "/runs/r1/" + rel, nil
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
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

func planRequest() agent.PlanReviewRequest {
	return agent.PlanReviewRequest{
		RunID: "r1", Round: 1, Output: agent.Output{Prefix: "plan-review/round-01"},
		Requirements: "# Calculator\nREQ-001: add two numbers.\nREQ-002: subtract.\n",
		PackageFiles: []agent.PackFile{
			{Path: "phases/P01.md", Content: "## Phase 1: Add\n### Acceptance Criteria\n- AC-01.1 Add(2,3) is 5\n"},
			{Path: "IMPLEMENTATION_PLAN.md", Content: "# Plan\nOne phase.\n"},
			{Path: "requirements.md", Content: "- REQ-001 (functional): add\n- REQ-002 (functional): subtract\n"},
			{Path: "traceability.json", Content: `{"REQ-001":{"phases":[1],"acceptance_criteria":["AC-01.1"]}}`},
		},
		ReqIDs:       []string{"REQ-001", "REQ-002"},
		RepoOverview: "go.mod (module calc)\ncalc.go\ncalc_test.go\nlanguages: Go\n",
		AgentsMD:     "Use gofmt. Never commit.",
		CheckResults: "error traceability.json: REQ-002 is not traced to any phase\n",
	}
}

// AC-07.1: the plan review pack contains the requirements, the full
// package, the repository overview, and the deterministic check results.
func TestPlanPackGolden(t *testing.T) {
	var ms memStore
	p, err := BuildPlanPack(planRequest(), "/runs/r1", "plan-review/round-01", DefaultMaxContextBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"REQ-001: add two numbers", `<data name="IMPLEMENTATION_PLAN.md">`, `<data name="phases/P01.md">`, `<data name="requirements.md">`,
		`<data name="traceability.json">`, "calc_test.go", "REQ-002 is not traced", "Use gofmt", "REQ-001, REQ-002",
		"the artifact directory /runs/r1",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("plan pack lacks %q", want)
		}
	}
	if len(ms.files) != 0 {
		t.Errorf("a small pack needs no attachments: %v", ms.files)
	}
	golden(t, "pack-plan.golden.md", p.Text)
}

func TestPlanReReviewPack(t *testing.T) {
	req := planRequest()
	req.Round = 2
	req.Previous = []agent.PlanFindingRecord{
		{Finding: agent.PlanFinding{ID: "PF-001", Severity: "major", Dimension: "coverage", Title: "REQ-002 missing"},
			Response: &agent.FindingResponse{ID: "PF-001", Response: "fixed", Detail: "added P02"}},
		{Finding: agent.PlanFinding{ID: "PF-002", Severity: "minor", Title: "naming"}},
	}
	var ms memStore
	p, err := BuildPlanPack(req, "/runs/r1", "plan-review/round-02", DefaultMaxContextBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Task: re-review the revised implementation plan (round 2)", "author_response: fixed", "author_detail: added P02", "author_response: none"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("re-review pack lacks %q", want)
		}
	}
}

func TestPlanPackCap(t *testing.T) {
	req := planRequest()
	req.PackageFiles = append(req.PackageFiles, agent.PackFile{Path: "phases/P02.md", Content: strings.Repeat("long text\n", 30000)})
	var ms memStore
	p, err := BuildPlanPack(req, "/runs/r1", "plan-review/round-01", 64<<10, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Text) > 64<<10 || ms.files["plan-review/round-01/package/phases/P02.md"] == "" {
		t.Fatalf("cap: %d bytes, files %v", len(p.Text), len(ms.files))
	}
	if !strings.Contains(p.Text, "plan file phases/P02.md: not inlined (size cap); read it at /runs/r1/plan-review/round-01/package/phases/P02.md") {
		t.Error("the pack does not point to the moved file")
	}
}

func codeRequest() agent.CodeReviewRequest {
	return agent.CodeReviewRequest{
		RunID: "r1", Kind: agent.ReviewPhase, Phase: 2, Attempt: 1, Round: 1,
		Output:       agent.Output{Prefix: "phases/P02/a1/reviews/r01"},
		PlanOverview: "# Plan\nUsers store.\n",
		Phases: []agent.PhaseStatus{{Number: 1, Title: "Calculator", Status: "done"}, {Number: 2, Title: "Users", Status: "current"},
			{Number: 3, Title: "API", Status: "future"}},
		PhaseSpec: "## Phase 2: Users\n- AC-02.1 FindUser returns the user\n- AC-02.2 errors are wrapped\n",
		AgentsMD:  "Use parameterized SQL.", ACIDs: []string{"AC-02.1", "AC-02.2"},
		Diff: `diff --git a/store/users.go b/store/users.go
--- a/store/users.go
+++ b/store/users.go
@@ -10,3 +10,5 @@ package store
-func FindUser(db *sql.DB, id int) (*User, error) {
+func FindUser(db *sql.DB, name string) (*User, error) {
+	q := fmt.Sprintf("SELECT id FROM users WHERE name = '%s'", name)
+	return scan(db.Query(q))
 }
`,
		DiffStat:    " store/users.go | 4 +++-\n 1 file changed\n",
		GateResults: "typecheck passed\nlint passed\nunit passed (12 tests)\nintegration not_applicable\nbuild passed\nsecurity passed\n",
		Baseline:    "no new failures", TestChanges: "added store/users_test.go: TestFindUser", Regressions: "(none)",
		Security: "gosec: no findings\ndiff policy: ok\nanti-gaming: ok", Importers: "api/handlers.go imports store",
		ImplReport: `{"summary":"Implemented FindUser by name"}`,
	}
}

// AC-07.1: the code review pack contains every section of §11.6.
func TestCodePackGolden(t *testing.T) {
	var ms memStore
	p, err := BuildCodePack(codeRequest(), "/runs/r1", "phases/P02/a1/reviews/r01", DefaultMaxContextBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Instructions",                              // instructions
		"## Implementation plan (frozen)",             // plan
		"- P01 Calculator: done", "- P03 API: future", // phase list
		"## Phase specification (P02)", "AC-02.1 FindUser", // phase spec
		"## AGENTS.md", "Use parameterized SQL.", // AGENTS.md
		"## Complete Git diff", "SELECT id FROM users", // diff
		"## Automated test results", "unit passed (12 tests)", "Baseline comparison", "TestFindUser", "Regressions", // tests
		"## Security data", "anti-gaming: ok", // security
		"## Regression data", "changed store/users.go FindUser (go)", "api/handlers.go imports store", // regression
		"UNVERIFIED CLAIMS", "Implemented FindUser by name", // implementation report
		"Confinement", "prompt_injection", // repository access rules
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("code pack lacks %q", want)
		}
	}
	golden(t, "pack-code.golden.md", p.Text)
}

func TestCodeReReviewAndFinalPacks(t *testing.T) {
	req := codeRequest()
	req.Round = 2
	req.IncrementalDiff = "diff --git a/store/users.go b/store/users.go\n"
	req.Ledger = []agent.LedgerEntry{{ID: "F-02-001", State: "open", Severity: "blocker", Category: "security", Title: "SQL injection", Response: "fixed", Detail: "parameterized"}}
	var ms memStore
	p, err := BuildCodePack(req, "/runs/r1", "x", DefaultMaxContextBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Task: re-review phase P02", "## Finding ledger", "id: F-02-001", "author_detail: parameterized", "## Changes since the last review"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("re-review pack lacks %q", want)
		}
	}
	fin := codeRequest()
	fin.Kind, fin.Phase, fin.PhaseSpec, fin.Requirements = agent.ReviewFinal, 0, "", "REQ-001 users"
	p, err = BuildCodePack(fin, "/runs/r1", "final/review-01", DefaultMaxContextBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "# Task: final review of the whole run") || !strings.Contains(p.Text, "## Requirements") || strings.Contains(p.Text, "## Phase specification") {
		t.Errorf("final pack:\n%.1500s", p.Text)
	}
}

// AC-07.1: a large diff respects the size cap: the full patch goes to the
// artifact directory, the pack keeps the stat and the highest-risk hunks.
func TestCodePackSizeCap(t *testing.T) {
	req := codeRequest()
	var b strings.Builder
	b.WriteString(req.Diff)
	b.WriteString("diff --git a/go.sum b/go.sum\n--- a/go.sum\n+++ b/go.sum\n@@ -1,1 +1,4000 @@\n")
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "+example.com/mod%d v1.0.0 h1:abcdefghijklmnopqrstuvwxyz0123456789=\n", i)
	}
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "diff --git a/docs/page%d.md b/docs/page%d.md\n--- a/docs/page%d.md\n+++ b/docs/page%d.md\n@@ -1,1 +1,200 @@\n", i, i, i, i)
		for j := 0; j < 200; j++ {
			b.WriteString("+Some documentation text that is long enough to matter for the size cap.\n")
		}
	}
	req.Diff = b.String()
	req.GateResults = strings.Repeat("gate log line\n", 20000)
	const capBytes = 96 << 10
	var ms memStore
	p, err := BuildCodePack(req, "/runs/r1", "phases/P02/a1/reviews/r01", capBytes, ms.store)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Text) > capBytes {
		t.Fatalf("pack %d bytes > cap %d", len(p.Text), capBytes)
	}
	if ms.files["phases/P02/a1/reviews/r01/full.patch"] != req.Diff {
		t.Error("the full patch was not written to the artifact directory")
	}
	for _, want := range []string{
		"read the complete patch at /runs/r1/phases/P02/a1/reviews/r01/full.patch",
		"SELECT id FROM users", // the high-risk hunk is inline
		"lower-risk hunks omitted",
		"gate results: shortened to fit the pack; the complete text is at /runs/r1/phases/P02/a1/reviews/r01/context/gate-results.txt",
		" store/users.go | 4 +++-", // the stat stays
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("capped pack lacks %q", want)
		}
	}
	if strings.Contains(p.Text, "example.com/mod3999") {
		t.Error("the low-value go.sum hunk was inlined before the risky one could be")
	}
}
