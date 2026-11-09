package plan

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- finding 1: repository containment ---

// linkDir makes link point at the directory target: a junction on Windows
// (no privilege needed), a symbolic link elsewhere.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
}

// linkManifest replaces plan.lock.json with a link to target: a file
// symlink where allowed, else (Windows without the privilege) a junction to
// target's directory, which is still a linked manifest.
func linkManifest(t *testing.T, target, link string) {
	t.Helper()
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err == nil {
		return
	}
	linkDir(t, filepath.Dir(target), link)
}

func wantContainment(t *testing.T, what string, err error, path string) {
	t.Helper()
	var ce *ContainmentError
	if !errors.As(err, &ce) || ce.Path != path || !errors.Is(err, ErrNotContained) {
		t.Errorf("%s: want ContainmentError for %q, got %v", what, path, err)
	}
}

func hasDiag(ds []Diagnostic, code Code, file string) bool {
	for _, d := range ds {
		if d.Code == code && d.Location.File == file {
			return true
		}
	}
	return false
}

// externalPlan returns a copy of the valid package (outside any repository
// under test) with its own lock, so a linked repository would otherwise
// load and verify cleanly.
func externalPlan(t *testing.T) (root string, lockBytes []byte) {
	t.Helper()
	root = copyTree(t, "testdata/valid-dir")
	l, err := BuildLock(root, LockMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteLock(root, l); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	return root, b
}

func TestLinkedPackageDirIsRefused(t *testing.T) {
	cases := []struct {
		name, offending string
		link            func(t *testing.T, ext, repo string)
	}{
		{"linked .aco parent", ".aco", func(t *testing.T, ext, repo string) {
			linkDir(t, filepath.Join(ext, ".aco"), filepath.Join(repo, ".aco"))
		}},
		{"linked .aco/plan", ".aco/plan", func(t *testing.T, ext, repo string) {
			if err := os.Mkdir(filepath.Join(repo, ".aco"), 0o755); err != nil {
				t.Fatal(err)
			}
			linkDir(t, filepath.Join(ext, ".aco", "plan"), filepath.Join(repo, ".aco", "plan"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, extLock := externalPlan(t)
			repo := t.TempDir()
			mustWrite(t, filepath.Join(repo, "AGENTS.md"), "rules\n")
			tc.link(t, ext, repo)
			dir := filepath.Join(repo, ".aco", "plan")

			// Loading refuses before reading anything through the link.
			pkg := mustLoad(t, dir)
			if len(pkg.Files) != 0 || len(pkg.Phases) != 0 || !hasDiag(pkg.Diagnostics, CodePathNotContained, tc.offending) {
				t.Errorf("load: files %d, phases %d, diags:\n%s", len(pkg.Files), len(pkg.Phases), dump(pkg.Diagnostics))
			}
			if rep := ValidateFull(pkg, DefaultCheckOptions(), false); rep.Valid {
				t.Error("validation passed for a linked package")
			}

			// Every freeze operation refuses too.
			_, err := BuildLock(repo, LockMeta{})
			wantContainment(t, "BuildLock", err, tc.offending)
			_, err = ReadLock(repo)
			wantContainment(t, "ReadLock", err, tc.offending)
			l, _ := ParseLock(extLock)
			wantContainment(t, "VerifyLock", VerifyLock(repo, l), tc.offending)
			wantContainment(t, "WriteLock", WriteLock(repo, &Lock{Format: LockFormat}), tc.offending)
			if b, _ := os.ReadFile(LockPath(ext)); string(b) != string(extLock) {
				t.Error("WriteLock changed the external manifest")
			}

			// Naming the external directory directly with --repo still cannot
			// verify it through the linked repository.
			out := mustLoadOpts(t, filepath.Join(ext, ".aco", "plan"), Options{RepoRoot: repo})
			rep := ValidateFull(out, DefaultCheckOptions(), false)
			if rep.Valid || !hasDiag(rep.Diagnostics, CodePathNotContained, tc.offending) {
				t.Errorf("--repo through a link:\n%s", dump(rep.Diagnostics))
			}
		})
	}
}

func TestLinkedManifestIsRefused(t *testing.T) {
	ext, extLock := externalPlan(t)
	repo, _ := frozenRepo(t)
	linkManifest(t, LockPath(ext), LockPath(repo))
	const rel = ".aco/plan/plan.lock.json"

	_, err := ReadLock(repo)
	wantContainment(t, "ReadLock", err, rel)
	l, _ := ParseLock(extLock)
	wantContainment(t, "VerifyLock", VerifyLock(repo, l), rel)
	_, err = BuildLock(repo, LockMeta{})
	wantContainment(t, "BuildLock", err, rel)
	wantContainment(t, "WriteLock", WriteLock(repo, l), rel)
	if b, _ := os.ReadFile(LockPath(ext)); string(b) != string(extLock) {
		t.Error("WriteLock wrote through the linked manifest")
	}

	rep := ValidateFull(mustLoad(t, filepath.Join(repo, ".aco", "plan")), DefaultCheckOptions(), false)
	if rep.Valid || !hasDiag(rep.Diagnostics, CodePathNotContained, rel) || (rep.Lock != nil && rep.Lock.Verified) {
		t.Errorf("validate with linked manifest: lock %+v\n%s", rep.Lock, dump(rep.Diagnostics))
	}
}

func TestLinkedPhasesDirIsNotFollowed(t *testing.T) {
	ext, _ := externalPlan(t)
	repo := copyTree(t, "testdata/valid-dir")
	phases := filepath.Join(repo, ".aco", "plan", "phases")
	if err := os.RemoveAll(phases); err != nil {
		t.Fatal(err)
	}
	linkDir(t, filepath.Join(ext, ".aco", "plan", "phases"), phases)
	pkg := mustLoad(t, filepath.Join(repo, ".aco", "plan"))
	if len(pkg.Phases) != 0 || !hasDiag(pkg.Diagnostics, CodePathNotContained, "phases") {
		t.Errorf("phases read through a link:\n%s", dump(pkg.Diagnostics))
	}
	_, err := BuildLock(repo, LockMeta{})
	if !errors.Is(err, ErrNotRegular) {
		t.Errorf("BuildLock with linked phases dir: %v", err)
	}
}

func mustLoadOpts(t *testing.T, p string, o Options) *Package {
	t.Helper()
	pkg, err := Load(p, o)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

// --- finding 2: consistent traceability ---

func TestTraceACOutsideItsPhasesFails(t *testing.T) {
	// Explicit traceability (package directory).
	root := copyTree(t, "testdata/valid-dir")
	tp := filepath.Join(root, ".aco", "plan", TraceabilityName)
	mustWrite(t, tp, strings.Replace(readText(t, tp),
		`"phases": ["P01"], "acceptance_criteria": ["AC-01.1", "AC-01.2", "AC-01.3"]`,
		`"phases": ["P01"], "acceptance_criteria": ["AC-02.1"]`, 1))
	ds := Validate(mustLoad(t, filepath.Join(root, ".aco", "plan")), DefaultCheckOptions())
	got := codesOf(ds)
	if got[CodeTraceACOutsidePhases] == 0 || got[CodeRequirementUncovered] == 0 || Passes(ds, false) {
		t.Errorf("explicit trace REQ-004 -> P01 / AC-02.1 must fail in default mode:\n%s", dump(ds))
	}

	// Structured form.
	y := strings.Replace(readText(t, validYAML), "{requirement: REQ-004, phases: [P01], acceptance_criteria: [AC-01.1, AC-01.2, AC-01.3]}",
		"{requirement: REQ-004, phases: [P01], acceptance_criteria: [AC-02.1]}", 1)
	ds = Validate(LoadBytes("plan.yaml", []byte(y)), DefaultCheckOptions())
	if codesOf(ds)[CodeTraceACOutsidePhases] == 0 || codesOf(ds)[CodeRequirementUncovered] == 0 || Passes(ds, false) {
		t.Errorf("yaml:\n%s", dump(ds))
	}

	// Derived from Markdown: Phase 1 claims Phase 2's criterion.
	md := strings.Replace(readText(t, validMD), "- REQ-004\n", "- REQ-004: AC-02.1\n", 1)
	ds = Validate(LoadBytes("plan.md", []byte(md)), DefaultCheckOptions())
	if codesOf(ds)[CodeTraceACOutsidePhases] != 1 || codesOf(ds)[CodeRequirementUncovered] == 0 || Passes(ds, false) {
		t.Errorf("markdown:\n%s", dump(ds))
	}

	// One valid pair still covers the requirement, but the bad reference fails.
	md = strings.Replace(readText(t, validMD), "- REQ-001: AC-01.1\n", "- REQ-001: AC-01.1, AC-03.1\n", 1)
	ds = Validate(LoadBytes("plan.md", []byte(md)), DefaultCheckOptions())
	if got := codesOf(ds); got[CodeTraceACOutsidePhases] != 1 || got[CodeRequirementUncovered] != 0 {
		t.Errorf("mixed pairs:\n%s", dump(ds))
	}
}

// --- finding 4: empty acceptance criteria ---

func TestEmptyAcceptanceCriteriaAreRejected(t *testing.T) {
	base := readText(t, validMD)
	cases := []struct{ name, old, new, id string }{
		{"explicit ID only", "- AC-01.2: `List` returns items in creation order.\n", "- AC-01.2:\n", "AC-01.2"},
		{"explicit ID and spaces", "- AC-03.1: README lists both endpoints with curl examples.\n", "- **AC-03.1**   \n", "AC-03.1"},
		{"empty bullet", "- A load test shows p95 latency under 200 ms.\n", "-\n", "AC-02.3"},
	}
	for _, tc := range cases {
		src := strings.Replace(base, tc.old, tc.new, 1)
		ds := Validate(LoadBytes("plan.md", []byte(src)), DefaultCheckOptions())
		found := false
		for _, d := range ds {
			if d.Code == CodeEmptyAC && d.Location.Ref == tc.id && d.Location.Line > 0 && d.Location.File == "plan.md" &&
				d.Severity == SeverityError && strings.Contains(d.Message, tc.id) {
				found = true
			}
		}
		if !found || Passes(ds, false) {
			t.Errorf("%s: want empty_acceptance_criterion for %s:\n%s", tc.name, tc.id, dump(ds))
		}
	}

	// An empty criterion never covers a requirement.
	src := strings.Replace(base, "- AC-01.2: `List` returns items in creation order.\n", "- AC-01.2:\n", 1)
	src = strings.Replace(src, "- REQ-004\n", "- REQ-004: AC-01.2\n", 1)
	if ds := Validate(LoadBytes("plan.md", []byte(src)), DefaultCheckOptions()); codesOf(ds)[CodeRequirementUncovered] == 0 {
		t.Errorf("empty AC covered REQ-004:\n%s", dump(ds))
	}

	// Structured form: empty and whitespace-only text.
	for _, text := range []string{`""`, `"   "`} {
		y := strings.Replace(readText(t, validYAML), "{id: AC-03.2, text: README states the latency target.}", "{id: AC-03.2, text: "+text+"}", 1)
		ds := Validate(LoadBytes("plan.yaml", []byte(y)), DefaultCheckOptions())
		ok := false
		for _, d := range ds {
			ok = ok || d.Code == CodeEmptyAC && d.Location.Ref == "AC-03.2" && d.Location.Line > 0
		}
		// REQ-003 relies on AC-02.3 and AC-03.2; AC-02.3 still covers it.
		if !ok || Passes(ds, false) {
			t.Errorf("yaml text %s:\n%s", text, dump(ds))
		}
	}
	y := strings.Replace(readText(t, validYAML), "{id: AC-01.1, text: \"`Create` returns the new item with an ID.\"}", "{id: AC-01.1}", 1)
	if ds := Validate(LoadBytes("plan.yaml", []byte(y)), DefaultCheckOptions()); codesOf(ds)[CodeEmptyAC] != 1 {
		t.Errorf("yaml missing text:\n%s", dump(ds))
	}
}

// --- finding 5: whitespace inside gate commands ---

func TestGateCommandWhitespaceIsPreserved(t *testing.T) {
	const quoted = `sh -c 'echo "a   b"'`
	md := strings.Replace(readText(t, validMD), "- unit: `go test ./... -short -json`\n", "- **Unit   Tests** :  `"+quoted+"`  ;  REQUIRED\n", 1)
	md = strings.Replace(md, "- build: required\n", "- build:  go  build   -o out   ./...  \n", 1)
	y := strings.Replace(readText(t, validYAML), "unit: {commands: [go test ./... -short -json]}", `unit: {commands: ["sh -c 'echo \"a   b\"'"]}`, 1)
	y = strings.Replace(y, "      build:\n", "      build: {commands: [\"go  build   -o out   ./...\"]}\n", 1)

	a, b := LoadBytes("plan.md", []byte(md)), LoadBytes("plan.yaml", []byte(y))
	for _, p := range []*Package{a, b} {
		if ds := Validate(p, DefaultCheckOptions()); len(ds) != 0 {
			t.Fatalf("%s:\n%s", p.Source.Name, dump(ds))
		}
	}
	if g, _ := a.Phase(1).Gate("unit"); len(g.Commands) != 1 || g.Commands[0] != quoted {
		t.Errorf("markdown unit commands %q", g.Commands)
	}
	if g, _ := a.Phase(1).Gate("build"); len(g.Commands) != 1 || g.Commands[0] != "go  build   -o out   ./..." {
		t.Errorf("markdown build commands %q", g.Commands)
	}
	if canonJSON(a) != canonJSON(b) {
		t.Errorf("markdown and yaml differ:\n%s\n---\n%s", canonJSON(a), canonJSON(b))
	}
	// Rendering keeps the spacing too.
	back := LoadBytes("p.md", []byte(a.Phase(1).Raw))
	if back.Phases == nil || canonJSON(&Package{Phases: back.Phases}) != canonJSON(&Package{Phases: []Phase{*b.Phase(1)}}) {
		t.Errorf("rendered phase lost spacing")
	}
	out := RenderPhase(*b.Phase(1), b.Traceability)
	if !strings.Contains(out, "`"+quoted+"`") {
		t.Errorf("render:\n%s", out)
	}
}

// Grouped Requirements entries: the listed AC apply to every listed REQ and
// are checked against the declaring phase; no AC keeps the all-AC fallback.
func TestGroupedRequirementEntries(t *testing.T) {
	base := readText(t, validMD)
	// Cross-phase AC on a grouped entry fails in default mode.
	bad := strings.Replace(base, "- REQ-004\n", "- REQ-004, REQ-001: AC-02.1\n", 1)
	ds := Validate(LoadBytes("plan.md", []byte(bad)), DefaultCheckOptions())
	if codesOf(ds)[CodeTraceACOutsidePhases] != 2 || codesOf(ds)[CodeRequirementUncovered] == 0 || Passes(ds, false) {
		t.Errorf("grouped cross-phase entry:\n%s", dump(ds))
	}
	// Own AC: both REQs get exactly that AC from this phase.
	good := strings.Replace(base, "- REQ-004\n", "- REQ-004, REQ-002: AC-01.3\n", 1)
	pkg := LoadBytes("plan.md", []byte(good))
	if ds := Validate(pkg, DefaultCheckOptions()); len(ds) != 0 {
		t.Fatalf("grouped own AC:\n%s", dump(ds))
	}
	for _, tr := range pkg.Traceability {
		if tr.Requirement == "REQ-004" && strings.Join(tr.AcceptanceCriteria, ",") != "AC-01.3" {
			t.Errorf("REQ-004 -> %v", tr.AcceptanceCriteria)
		}
		if tr.Requirement == "REQ-002" && !containsString(tr.AcceptanceCriteria, "AC-01.3") {
			t.Errorf("REQ-002 -> %v", tr.AcceptanceCriteria)
		}
	}
	// Grouped without AC: every listed REQ maps to all of the phase's AC.
	all := strings.Replace(base, "- REQ-004\n", "- REQ-004, REQ-003\n", 1)
	pkg = LoadBytes("plan.md", []byte(all))
	for _, tr := range pkg.Traceability {
		if tr.Requirement == "REQ-004" && strings.Join(tr.AcceptanceCriteria, ",") != "AC-01.1,AC-01.2,AC-01.3" {
			t.Errorf("fallback REQ-004 -> %v", tr.AcceptanceCriteria)
		}
	}
}
