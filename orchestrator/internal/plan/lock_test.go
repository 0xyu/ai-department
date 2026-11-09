package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frozenRepo(t *testing.T) (string, *Lock) {
	t.Helper()
	root := copyTree(t, "testdata/valid-dir")
	l, err := BuildLock(root, LockMeta{RunID: "run-1", ReviewerModel: "gpt-sol-6.1", ReviewRounds: 2, Verdict: "pass",
		FrozenAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.FixedZone("x", 3600))})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteLock(root, l); err != nil {
		t.Fatal(err)
	}
	return root, l
}

func TestLockBuildWriteReadVerify(t *testing.T) {
	root, l := frozenRepo(t)
	wantFiles := []string{
		".aco/plan/IMPLEMENTATION_PLAN.md", ".aco/plan/phases/P01.md", ".aco/plan/phases/P02.md", ".aco/plan/phases/P03.md",
		".aco/plan/requirements.md", ".aco/plan/traceability.json", "AGENTS.md",
	}
	var got []string
	for _, f := range l.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("locked files %v", got)
	}
	if l.PlanVersion != "v1" || l.FrozenAt != "2026-11-08T09:00:00Z" || l.Format != LockFormat {
		t.Errorf("lock meta %+v", l)
	}
	back, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Marshal()) != string(l.Marshal()) {
		t.Errorf("read lock differs")
	}
	if err := VerifyLock(root, back); err != nil {
		t.Errorf("fresh lock does not verify: %v", err)
	}
	// The lock's plan hash differs from the package hash only by AGENTS.md.
	pkg := mustLoad(t, filepath.Join(root, ".aco", "plan"))
	if pkg.Hash() == l.PlanHash {
		t.Error("lock hash should include AGENTS.md")
	}
	if ds := Validate(pkg, DefaultCheckOptions()); len(ds) != 0 {
		t.Errorf("plan.lock.json must not be read as a plan file:\n%s", dump(ds))
	}
}

// AC: modifying any frozen file or AGENTS.md makes verification fail with
// the file name.
func TestLockVerifyNamesChangedFiles(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(root string)
		path   string
		prob   string
	}{
		{"phase spec edited", func(r string) { appendTo(t, filepath.Join(r, ".aco/plan/phases/P02.md"), "- one more criterion\n") }, ".aco/plan/phases/P02.md", ProblemModified},
		{"plan edited", func(r string) { appendTo(t, filepath.Join(r, ".aco/plan/IMPLEMENTATION_PLAN.md"), "x\n") }, ".aco/plan/IMPLEMENTATION_PLAN.md", ProblemModified},
		{"traceability edited", func(r string) { appendTo(t, filepath.Join(r, ".aco/plan/traceability.json"), " ") }, ".aco/plan/traceability.json", ProblemModified},
		{"AGENTS.md edited", func(r string) { appendTo(t, filepath.Join(r, "AGENTS.md"), "- Push to origin.\n") }, "AGENTS.md", ProblemModified},
		{"AGENTS.md deleted", func(r string) { mustRemove(t, filepath.Join(r, "AGENTS.md")) }, "AGENTS.md", ProblemMissing},
		{"phase deleted", func(r string) { mustRemove(t, filepath.Join(r, ".aco/plan/phases/P03.md")) }, ".aco/plan/phases/P03.md", ProblemMissing},
		{"phase added", func(r string) { mustWrite(t, filepath.Join(r, ".aco/plan/phases/P04.md"), "## Phase 4: Extra\n") }, ".aco/plan/phases/P04.md", ProblemUnexpected},
	}
	for _, tc := range cases {
		root, l := frozenRepo(t)
		tc.mutate(root)
		err := VerifyLock(root, l)
		var me MismatchError
		if !errors.As(err, &me) || len(me) != 1 || me[0].Path != tc.path || me[0].Problem != tc.prob {
			t.Errorf("%s: got %v", tc.name, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: message %q lacks the file name", tc.name, err)
		}
	}
}

func TestLockToleratesLineEndingChanges(t *testing.T) {
	root, l := frozenRepo(t)
	p := filepath.Join(root, ".aco/plan/phases/P01.md")
	mustWrite(t, p, strings.ReplaceAll(readText(t, p), "\n", "\r\n"))
	if err := VerifyLock(root, l); err != nil {
		t.Errorf("CRLF checkout should verify: %v", err)
	}
}

func TestLockAddedAgentsMD(t *testing.T) {
	root := copyTree(t, "testdata/valid-dir")
	mustRemove(t, filepath.Join(root, "AGENTS.md"))
	l, err := BuildLock(root, LockMeta{})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "new rules\n")
	var me MismatchError
	if err := VerifyLock(root, l); !errors.As(err, &me) || me[0].Path != "AGENTS.md" || me[0].Problem != ProblemUnexpected {
		t.Errorf("AGENTS.md added after freeze: %v", err)
	}
}

func TestParseLockRejectsTampering(t *testing.T) {
	_, l := frozenRepo(t)
	good := string(l.Marshal())
	cases := map[string]string{
		"path traversal":  strings.Replace(good, `".aco/plan/requirements.md"`, `"../../etc/passwd"`, 1),
		"absolute path":   strings.Replace(good, `".aco/plan/requirements.md"`, `"/etc/passwd"`, 1),
		"outside plan":    strings.Replace(good, `".aco/plan/requirements.md"`, `"go.mod"`, 1),
		"windows path":    strings.Replace(good, `".aco/plan/requirements.md"`, `".aco\\\\plan\\\\requirements.md"`, 1),
		"drive letter":    strings.Replace(good, `".aco/plan/requirements.md"`, `"C:/x.md"`, 1),
		"file hash edit":  strings.Replace(good, `"sha256:`, `"sha256:0`, 2),
		"plan hash edit":  strings.Replace(good, l.PlanHash, "sha256:"+strings.Repeat("0", 64), 1),
		"unknown field":   strings.Replace(good, `"format"`, `"extra": 1, "format"`, 1),
		"unknown format":  strings.Replace(good, LockFormat, "aco.plan.lock/v9", 1),
		"duplicate entry": strings.Replace(good, `".aco/plan/requirements.md"`, `".aco/plan/phases/P01.md"`, 1),
	}
	for name, s := range cases {
		if s == good {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := ParseLock([]byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseLock([]byte(good)); err != nil {
		t.Errorf("good lock rejected: %v", err)
	}
}

func appendTo(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
