package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	validMD   = "testdata/valid/plan.md"
	validYAML = "testdata/valid/plan.yaml"
	validDir  = "testdata/valid-dir/.aco/plan"
)

func mustLoad(t *testing.T, p string) *Package {
	t.Helper()
	pkg, err := Load(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(NormalizeText(b))
}

func codesOf(ds []Diagnostic) map[Code]int {
	m := map[Code]int{}
	for _, d := range ds {
		m[d.Code]++
	}
	return m
}

func dump(ds []Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString("  " + string(d.Severity) + " " + string(d.Code) + " " + d.Location.String() + ": " + d.Message + "\n")
	}
	return b.String()
}

func canonJSON(p *Package) string {
	b, _ := json.MarshalIndent(p.Canonical(), "", " ")
	return string(b)
}

// AC: this document parses as a single-file plan into 14 phases, each with
// at least 3 acceptance criteria, with no errors.
func TestImplementationPlanDocumentParses(t *testing.T) {
	const docPath = "../../AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md"
	pkg := mustLoad(t, docPath)
	if len(pkg.Diagnostics) != 0 {
		t.Fatalf("parse diagnostics:\n%s", dump(pkg.Diagnostics))
	}
	if len(pkg.Phases) != 14 {
		t.Fatalf("want 14 phases, got %d", len(pkg.Phases))
	}
	src, _ := os.ReadFile(docPath)
	for i, ph := range pkg.Phases {
		if ph.Number != i+1 || ph.ID != PhaseID(i+1) {
			t.Errorf("phase %d numbered %d (%s)", i+1, ph.Number, ph.ID)
		}
		if len(ph.AcceptanceCriteria) < 3 {
			t.Errorf("%s has %d AC", ph.ID, len(ph.AcceptanceCriteria))
		}
		for j, ac := range ph.AcceptanceCriteria {
			if ac.ID != ACID(ph.Number, j+1) {
				t.Errorf("%s AC %d has ID %s", ph.ID, j+1, ac.ID)
			}
		}
		if ph.Title == "" || ph.Goal == "" || len(ph.Scope.In) == 0 || len(ph.Deliverables) == 0 {
			t.Errorf("%s: missing title/goal/scope/deliverables", ph.ID)
		}
		if !bytes.Contains(src, []byte(ph.Raw)) || !strings.HasPrefix(ph.Raw, "## Phase ") {
			t.Errorf("%s: Raw is not a verbatim slice of the source", ph.ID)
		}
		if strings.Contains(ph.Raw, "\n---") {
			t.Errorf("%s: Raw includes the trailing thematic break", ph.ID)
		}
	}
	if got := pkg.Phase(8).DependsOn; !reflect.DeepEqual(got, []int{2, 4, 5, 7}) {
		t.Errorf("P08 depends on %v", got)
	}
	if got := pkg.Phase(14).DependsOn; !reflect.DeepEqual(got, []int{12, 13}) {
		t.Errorf("P14 depends on %v", got)
	}
	// The nested scenario list of Phase 9 stays inside its first criterion.
	if ac := pkg.Phase(9).AcceptanceCriteria; len(ac) != 3 || !strings.Contains(ac[0].Text, "\n11. A file changed after review") {
		t.Errorf("P09 AC: %d items, first %q", len(ac), ac[0].Text)
	}
	if pkg.Title != "AI Coding Orchestrator (ACO) — Implementation Plan" {
		t.Errorf("title %q", pkg.Title)
	}
	// The document passes the full validation (checks, front matter, lock),
	// even in strict mode: every REQ is covered by specific phase AC and
	// every phase declares all six quality gates.
	rep := ValidateFull(pkg, DefaultCheckOptions(), true)
	if !rep.Valid || len(rep.Diagnostics) != 0 {
		t.Errorf("ValidateFull:\n%s", dump(rep.Diagnostics))
	}
	if len(pkg.Requirements) != 30 || pkg.TraceDerived != true || len(pkg.Traceability) != 30 {
		t.Errorf("%d requirements, %d traces", len(pkg.Requirements), len(pkg.Traceability))
	}
	for _, ph := range pkg.Phases {
		if len(ph.Gates) != len(GateTypes) || len(ph.Requirements) == 0 {
			t.Errorf("%s: %d gates, %d requirements", ph.ID, len(ph.Gates), len(ph.Requirements))
		}
		for _, r := range ph.Requirements {
			if len(ph.reqAC[r]) == 0 {
				t.Errorf("%s maps %s to no specific AC", ph.ID, r)
			}
		}
	}
}

func TestValidFixturesPass(t *testing.T) {
	for _, p := range []string{validMD, validYAML, validDir} {
		pkg := mustLoad(t, p)
		if ds := Validate(pkg, DefaultCheckOptions()); len(ds) != 0 {
			t.Errorf("%s:\n%s", p, dump(ds))
		}
		if len(pkg.Phases) != 3 || len(pkg.Requirements) != 4 || pkg.ACCount() != 9 {
			t.Errorf("%s: %d phases, %d reqs, %d AC", p, len(pkg.Phases), len(pkg.Requirements), pkg.ACCount())
		}
	}
}

func TestValidModelDetails(t *testing.T) {
	pkg := mustLoad(t, validMD)
	if string(pkg.FrontMatter) != "implementer:\n  effort: high\n" || pkg.FrontMatterLine != 2 {
		t.Errorf("front matter %q at %d", pkg.FrontMatter, pkg.FrontMatterLine)
	}
	p1 := pkg.Phase(1)
	if !reflect.DeepEqual(p1.Scope.Paths, []string{"internal/store/**", "go.mod"}) || !reflect.DeepEqual(p1.Scope.Out, []string{"persistence to disk."}) {
		t.Errorf("scope %+v", p1.Scope)
	}
	if got := p1.AcceptanceCriteria[2]; got.ID != "AC-01.3" || got.Explicit || got.Text != "Concurrent `Create` calls from 100 goroutines all succeed:\n1. no item is lost;\n2. IDs are unique." {
		t.Errorf("AC-01.3 = %+v", got)
	}
	g, _ := p1.Gate("integration")
	if !g.NotApplicable || g.Justification != "the store has no external dependencies" {
		t.Errorf("integration gate %+v", g)
	}
	g, _ = pkg.Phase(2).Gate("security")
	if !reflect.DeepEqual(g.Commands, []string{"gosec ./..."}) || !reflect.DeepEqual(g.Scanners, []string{"secrets", "gosec"}) {
		t.Errorf("security gate %+v", g)
	}
	kinds := []RequirementKind{KindFunctional, KindFunctional, KindNonFunctional, KindConstraint}
	for i, r := range pkg.Requirements {
		if r.ID != ReqID(i+1) || r.Kind != kinds[i] {
			t.Errorf("requirement %d = %+v", i, r)
		}
	}
	want := []Trace{
		{Requirement: "REQ-001", Phases: []int{1, 2}, AcceptanceCriteria: []string{"AC-01.1", "AC-02.1", "AC-02.2", "AC-02.3"}},
		{Requirement: "REQ-002", Phases: []int{1, 2}, AcceptanceCriteria: []string{"AC-01.2", "AC-02.1", "AC-02.2", "AC-02.3"}},
		{Requirement: "REQ-003", Phases: []int{2, 3}, AcceptanceCriteria: []string{"AC-02.3", "AC-03.2"}},
		{Requirement: "REQ-004", Phases: []int{1}, AcceptanceCriteria: []string{"AC-01.1", "AC-01.2", "AC-01.3"}},
	}
	if got := pkg.Canonical().Traceability; !reflect.DeepEqual(got, want) {
		t.Errorf("derived traceability:\n%+v", got)
	}
	if !pkg.TraceDerived {
		t.Error("single-file traceability should be derived")
	}
	if got := pkg.Graph().Ancestors(3); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("ancestors of P03: %v", got)
	}
}

// AC: a package directory and an equivalent single-file plan produce the
// same model (except raw text). The YAML and JSON forms do too.
func TestEquivalentFormsProduceSameModel(t *testing.T) {
	md := mustLoad(t, validMD)
	dir := mustLoad(t, validDir)
	yml := mustLoad(t, validYAML)

	// JSON form: the YAML document converted to JSON.
	var v any
	if err := yaml.Unmarshal([]byte(readText(t, validYAML)), &v); err != nil {
		t.Fatal(err)
	}
	jb, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	js := LoadBytes("plan.json", jb)
	if len(js.Diagnostics) > 0 {
		t.Fatalf("json:\n%s", dump(js.Diagnostics))
	}

	want := canonJSON(md)
	for name, p := range map[string]*Package{"dir": dir, "yaml": yml, "json": js} {
		if got := canonJSON(p); got != want {
			t.Errorf("%s model differs from the single-file model:\n--- single file\n%s\n--- %s\n%s", name, want, name, got)
		}
	}
	if string(yml.FrontMatter) == "" || string(dir.FrontMatter) != string(md.FrontMatter) {
		t.Errorf("front matter: md %q dir %q yaml %q", md.FrontMatter, dir.FrontMatter, yml.FrontMatter)
	}
}

// Rendering a package as a directory and loading it back is lossless.
func TestRenderRoundTrip(t *testing.T) {
	for _, src := range []string{validMD, validYAML, validDir} {
		pkg := mustLoad(t, src)
		out := filepath.Join(t.TempDir(), ".aco", "plan")
		if err := WriteDir(out, pkg); err != nil {
			t.Fatal(err)
		}
		back := mustLoad(t, out)
		if ds := Validate(back, DefaultCheckOptions()); len(ds) != 0 {
			t.Errorf("%s rendered:\n%s", src, dump(ds))
		}
		if a, b := canonJSON(pkg), canonJSON(back); a != b {
			t.Errorf("%s round trip changed the model:\n%s\n---\n%s", src, a, b)
		}
		if string(back.FrontMatter) != string(pkg.FrontMatter) && src != validYAML {
			t.Errorf("%s front matter %q -> %q", src, pkg.FrontMatter, back.FrontMatter)
		}
	}
	// YAML phases have rendered Raw text that parses to the same phase.
	yml := mustLoad(t, validYAML)
	for _, ph := range yml.Phases {
		back := LoadBytes("p.md", []byte(ph.Raw))
		if len(back.Phases) != 1 || canonJSON(&Package{Phases: back.Phases}) != canonJSON(&Package{Phases: []Phase{ph}}) {
			t.Errorf("%s rendered Raw does not round-trip:\n%s", ph.ID, ph.Raw)
		}
	}
}

// AC: CRLF input gives the same IDs and hashes; Raw stays byte-for-byte.
func TestCRLFInput(t *testing.T) {
	lf := readText(t, validMD)
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	a := LoadBytes("plan.md", []byte(lf))
	b := LoadBytes("plan.md", []byte(crlf))
	c := LoadBytes("plan.md", append([]byte("\uFEFF"), []byte(strings.ReplaceAll(lf, "\n", "\r"))...)) // BOM + old Mac endings
	if canonJSON(a) != canonJSON(b) || canonJSON(a) != canonJSON(c) {
		t.Errorf("line endings change the model")
	}
	if a.Hash() != b.Hash() || a.Hash() != c.Hash() || a.Files[0].Hash != b.Files[0].Hash {
		t.Errorf("hashes differ: %s %s %s", a.Hash(), b.Hash(), c.Hash())
	}
	if len(Validate(b, DefaultCheckOptions())) != 0 {
		t.Errorf("CRLF plan:\n%s", dump(Validate(b, DefaultCheckOptions())))
	}
	for _, ph := range b.Phases {
		if !strings.Contains(ph.Raw, "\r\n") || !strings.Contains(crlf, ph.Raw) {
			t.Errorf("%s Raw is not the verbatim CRLF text", ph.ID)
		}
	}
	if string(b.FrontMatter) != string(a.FrontMatter) {
		t.Errorf("front matter %q", b.FrontMatter)
	}
}

// AC: fixtures for each failure yield the expected check code.
func TestCheckCodes(t *testing.T) {
	base := readText(t, validMD)
	type edit struct{ old, new string }
	cases := []struct {
		name  string
		edits []edit
		want  []Code
		not   []Code
	}{
		{"dependency cycle", []edit{{"- `internal/store`\n\n### Requirements", "- `internal/store`\n\n### Depends On\nPhase 3\n\n### Requirements"}},
			[]Code{CodeDependencyCycle, CodeForwardDependency}, nil},
		{"forward dependency", []edit{{"### Depends On\nPhase 1\n", "### Depends On\nPhase 3\n"}, {"- Phase 1\n- Phase 2\n", "- Phase 1\n"}},
			[]Code{CodeForwardDependency}, []Code{CodeDependencyCycle}},
		{"unknown dependency", []edit{{"### Depends On\nPhase 1\n", "### Depends On\nPhase 9\n"}}, []Code{CodeUnknownDependency}, nil},
		{"unreadable dependency", []edit{{"### Depends On\nPhase 1\n", "### Depends On\nthe storage work\n"}}, []Code{CodeDependencyInvalid}, nil},
		{"missing gate entry", []edit{{"- lint: `staticcheck ./...`\n", ""}}, []Code{CodeMissingGateEntry}, nil},
		{"not_applicable without justification", []edit{{"not_applicable — the store has no external dependencies", "not_applicable"}},
			[]Code{CodeGateNAUnjustified}, []Code{CodeMissingGateEntry}},
		{"unknown gate type", []edit{{"- build: required\n- security: scanners: secrets\n", "- deploy: required\n- security: scanners: secrets\n"}},
			[]Code{CodeUnknownGateType, CodeMissingGateEntry}, nil},
		{"duplicate gate entry", []edit{{"- build: required\n- security: scanners: secrets\n", "- build: required\n- build: `make`\n- security: scanners: secrets\n"}},
			[]Code{CodeDuplicateGateEntry}, nil},
		{"unknown scanner", []edit{{"scanners: secrets\n", "scanners: secrets, bandit\n"}}, []Code{CodeGateEntryInvalid}, nil},
		{"requirement without coverage", []edit{{"- REQ-002: Users can list todo items.\n", "- REQ-002: Users can list todo items.\n- REQ-005: Users can delete todo items.\n"}},
			[]Code{CodeRequirementUncovered}, nil},
		{"duplicate AC ID", []edit{{"- AC-03.2: README", "- AC-03.1: README"}}, []Code{CodeDuplicateID}, nil},
		{"duplicate REQ ID", []edit{{"- REQ-002: Users can list", "- REQ-001: Users can list"}}, []Code{CodeDuplicateID}, nil},
		{"duplicate section", []edit{{"### Goal\nDocument the API.\n", "### Goal\nDocument the API.\n\n### Goal\nAgain.\n"}}, []Code{CodeDuplicateSection}, nil},
		{"AC ID of another phase", []edit{{"- AC-03.3: Examples", "- AC-01.3: Examples"}}, []Code{CodeInvalidID, CodeDuplicateID}, nil},
		{"non-contiguous numbering", []edit{{"## Phase 3: Documentation", "## Phase 4: Documentation"}}, []Code{CodePhaseNumbering}, nil},
		{"duplicate phase number", []edit{{"## Phase 3: Documentation", "## Phase 2: Documentation"}}, []Code{CodeDuplicateID}, nil},
		{"unknown requirement reference", []edit{{"- REQ-003: AC-03.2", "- REQ-009: AC-03.2"}}, []Code{CodeUnknownReference}, nil},
		{"unknown AC reference", []edit{{"- REQ-003: AC-03.2", "- REQ-003: AC-03.9"}}, []Code{CodeUnknownReference}, nil},
		{"missing scope", []edit{{"### Scope\n- README with request examples.\n", ""}}, []Code{CodeMissingScope}, nil},
		{"missing acceptance criteria", []edit{{"### Acceptance Criteria\n- AC-03.1: README lists both endpoints with curl examples.\n- AC-03.2: README states the latency target.\n- AC-03.3: Examples in the README are checked by a test.\n", ""}},
			[]Code{CodeMissingAC}, nil},
		{"missing deliverables", []edit{{"### Deliverables\n- `README.md`\n", ""}}, []Code{CodeMissingDeliverables}, nil},
		{"missing goal (warning)", []edit{{"### Goal\nDocument the API.\n", ""}}, []Code{CodeMissingGoal}, nil},
		{"no requirements", []edit{{"- REQ-001: Users can create a todo item with a title.\n- REQ-002: Users can list todo items.\n", ""}, {"- REQ-003: Every endpoint answers within 200 ms at the 95th percentile.\n", ""}, {"| REQ-004 | constraint | Pure Go, no CGO. |\n", ""}},
			[]Code{CodeNoRequirements}, nil},
		{"no phases", []edit{{"## Phase", "## Stage"}, {"## Phase", "## Stage"}, {"## Phase", "## Stage"}}, []Code{CodeNoPhases}, nil},
		{"requirement without kind (warning)", []edit{{"### Constraints\n", "### Other\n"}, {"| REQ-004 | constraint |", "| REQ-004 | |"}}, []Code{CodeRequirementKind}, nil},
	}
	for _, tc := range cases {
		src := base
		for _, e := range tc.edits {
			if !strings.Contains(src, e.old) {
				t.Fatalf("%s: fixture text %q not found", tc.name, e.old)
			}
			src = strings.Replace(src, e.old, e.new, 1)
		}
		for _, form := range []string{"lf", "crlf"} {
			s := src
			if form == "crlf" {
				s = strings.ReplaceAll(src, "\n", "\r\n")
			}
			ds := Validate(LoadBytes("plan.md", []byte(s)), DefaultCheckOptions())
			got := codesOf(ds)
			for _, w := range tc.want {
				if got[w] == 0 {
					t.Errorf("%s (%s): want %s, got:\n%s", tc.name, form, w, dump(ds))
				}
			}
			for _, n := range tc.not {
				if got[n] > 0 {
					t.Errorf("%s (%s): unexpected %s:\n%s", tc.name, form, n, dump(ds))
				}
			}
		}
	}
}

func TestDiagnosticsAreStructured(t *testing.T) {
	src := strings.Replace(readText(t, validMD), "- lint: `staticcheck ./...`\n", "", 1)
	ds := Validate(LoadBytes("plan.md", []byte(src)), DefaultCheckOptions())
	if len(ds) != 1 {
		t.Fatalf("want 1 diagnostic:\n%s", dump(ds))
	}
	d := ds[0]
	want := Location{File: "plan.md", Line: 54, Phase: "P01", Section: "quality_gates", Ref: "lint"}
	if d.Code != CodeMissingGateEntry || d.Severity != SeverityError || d.Location != want ||
		d.Message != `P01 has no entry for quality gate "lint"` || !reflect.DeepEqual(d.Args, []string{"P01", "lint"}) {
		t.Errorf("got %+v", d)
	}
	if d.Location.String() != "plan.md:54#quality-gates" {
		t.Errorf("location string %q", d.Location.String())
	}
	b, _ := json.Marshal(d)
	if string(b) != `{"code":"missing_gate_entry","severity":"error","location":{"file":"plan.md","line":54,"phase":"P01","section":"quality_gates","ref":"lint"},"message":"P01 has no entry for quality gate \"lint\""}` {
		t.Errorf("json %s", b)
	}
}

func TestEveryCodeHasAFormat(t *testing.T) {
	verb := regexp.MustCompile(`%[a-z]`)
	for code, f := range Codes() {
		for _, v := range verb.FindAllString(f, -1) {
			if v != "%s" {
				t.Errorf("%s: only %%s is allowed, got %s", code, v)
			}
		}
	}
}

func TestDirSpecificChecks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dir string)
		want   Code
	}{
		{"phase file name does not match heading", func(d string) {
			mustRename(t, filepath.Join(d, "phases", "P03.md"), filepath.Join(d, "phases", "P04.md"))
		}, CodePhaseFileInvalid},
		{"badly named phase file", func(d string) {
			mustWrite(t, filepath.Join(d, "phases", "notes.md"), "# Notes\n")
		}, CodePhaseFileInvalid},
		{"missing traceability.json", func(d string) { mustRemove(t, filepath.Join(d, TraceabilityName)) }, CodeMissingFile},
		{"missing requirements.md", func(d string) { mustRemove(t, filepath.Join(d, RequirementsName)) }, CodeMissingFile},
		{"malformed traceability.json", func(d string) { mustWrite(t, filepath.Join(d, TraceabilityName), "{\"version\": 1, \"entries\": [}") }, CodeParseError},
		{"unknown field in traceability.json", func(d string) {
			mustWrite(t, filepath.Join(d, TraceabilityName), `{"version": 1, "entries": [], "extra": true}`)
		}, CodeParseError},
		{"traceability disagrees with a phase", func(d string) {
			p := filepath.Join(d, TraceabilityName)
			mustWrite(t, p, strings.Replace(readText(t, p), `"phases": ["P02", "P03"]`, `"phases": ["P02"]`, 1))
		}, CodeTraceMismatch},
		{"traceability references an unknown phase", func(d string) {
			p := filepath.Join(d, TraceabilityName)
			mustWrite(t, p, strings.Replace(readText(t, p), `"phases": ["P01"]`, `"phases": ["P01", "P07"]`, 1))
		}, CodeUnknownReference},
		{"traceability leaves a requirement uncovered", func(d string) {
			p := filepath.Join(d, TraceabilityName)
			mustWrite(t, p, strings.Replace(readText(t, p), `"phases": ["P01"], "acceptance_criteria": ["AC-01.1", "AC-01.2", "AC-01.3"]`, `"phases": [], "acceptance_criteria": []`, 1))
		}, CodeRequirementUncovered},
	}
	for _, tc := range cases {
		root := copyTree(t, "testdata/valid-dir")
		dir := filepath.Join(root, ".aco", "plan")
		tc.mutate(dir)
		ds := Validate(mustLoad(t, dir), DefaultCheckOptions())
		if codesOf(ds)[tc.want] == 0 {
			t.Errorf("%s: want %s, got:\n%s", tc.name, tc.want, dump(ds))
		}
	}
}

func TestStructuredErrors(t *testing.T) {
	base := readText(t, validYAML)
	cases := []struct {
		name, old, new string
		want           Code
	}{
		{"unknown field", "title: Todo", "colour: red\ntitle: Todo", CodeParseError},
		{"bad version", "version: 1", "version: 2", CodeUnsupportedPlanFormat},
		{"unknown gate type", "      build:\n", "      deploy:\n", CodeUnknownGateType},
		{"not_applicable with commands", "{not_applicable: true, justification: documentation only}", "{not_applicable: true, justification: docs, commands: [make]}", CodeGateEntryConflict},
		{"bad phase reference", "depends_on: [P01]", "depends_on: [first]", CodeParseError},
		{"bad requirement kind", "kind: constraint", "kind: wish", CodeInvalidID},
		{"two documents", "version: 1", "version: 1\n---\nversion: 1", CodeParseError},
	}
	for _, tc := range cases {
		if !strings.Contains(base, tc.old) {
			t.Fatalf("%s: %q not found", tc.name, tc.old)
		}
		ds := Validate(LoadBytes("plan.yaml", []byte(strings.Replace(base, tc.old, tc.new, 1))), DefaultCheckOptions())
		if codesOf(ds)[tc.want] == 0 {
			t.Errorf("%s: want %s, got:\n%s", tc.name, tc.want, dump(ds))
		}
	}
	// Errors carry the YAML line.
	ds := Validate(LoadBytes("plan.yaml", []byte(strings.Replace(base, "      build:\n", "      deploy:\n", 1))), DefaultCheckOptions())
	for _, d := range ds {
		if d.Code == CodeUnknownGateType && d.Location.Line != 14 {
			t.Errorf("unknown gate type at line %d, want 14 (the phase)", d.Location.Line)
		}
	}
}

func TestPackageHashIsStable(t *testing.T) {
	a, b := mustLoad(t, validDir), mustLoad(t, validDir)
	if a.Hash() != b.Hash() || !strings.HasPrefix(a.Hash(), "sha256:") {
		t.Errorf("hash %s vs %s", a.Hash(), b.Hash())
	}
	// Converting every file to CRLF keeps the hash.
	root := copyTree(t, "testdata/valid-dir")
	dir := filepath.Join(root, ".aco", "plan")
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			mustWrite(t, p, strings.ReplaceAll(readText(t, p), "\n", "\r\n"))
		}
		return nil
	})
	c := mustLoad(t, dir)
	if c.Hash() != a.Hash() || canonJSON(c) != canonJSON(a) {
		t.Errorf("CRLF directory: hash %s vs %s", c.Hash(), a.Hash())
	}
}

func TestAgentsMDDiscovery(t *testing.T) {
	pkg := mustLoad(t, validDir) // repo root inferred from <repo>/.aco/plan
	if pkg.AgentsMD == nil || pkg.AgentsMD.Path != AgentsMDName || pkg.AgentsMD.Hash != HashText([]byte(readText(t, "testdata/valid-dir/AGENTS.md"))) {
		t.Fatalf("AGENTS.md not discovered: %+v", pkg.AgentsMD)
	}
	if pkg.RepoRoot == "" {
		t.Error("repo root not inferred")
	}
	if p := mustLoad(t, validMD); p.AgentsMD != nil {
		t.Error("single file without RepoRoot should not look for AGENTS.md")
	}

	root := t.TempDir()
	if f, err := DiscoverAgentsMD(root); f != nil || err != nil {
		t.Errorf("empty repo: %v %v", f, err)
	}
	mustWrite(t, filepath.Join(root, "agents.md"), "rules\n")
	f, err := DiscoverAgentsMD(root)
	if err != nil || f == nil || f.Path != "agents.md" {
		t.Errorf("case-insensitive discovery: %+v %v", f, err)
	}

	link := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "agents.md"), filepath.Join(link, AgentsMDName)); err != nil {
		t.Skip("symlinks not available:", err)
	}
	if _, err := DiscoverAgentsMD(link); !errors.Is(err, ErrNotRegular) {
		t.Errorf("symlinked AGENTS.md: %v", err)
	}
}

// --- helpers ---

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func mustRename(t *testing.T, a, b string) {
	t.Helper()
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies src into a new temp dir and returns it.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
