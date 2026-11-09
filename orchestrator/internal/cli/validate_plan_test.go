package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/secure/securetest"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	validPlanMD  = "../plan/testdata/valid/plan.md"
	validPlanDir = "../plan/testdata/valid-dir/.aco/plan"
	brokenPlan   = "testdata/validate-plan/broken.md"
)

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", "validate-plan", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cli -run ValidatePlan -update)", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs from golden file:\n%s", name, got)
	}
}

// AC: golden tests for validate-plan --json.
func TestValidatePlanJSONGolden(t *testing.T) {
	cases := []struct {
		golden string
		path   string
		code   int
	}{
		{"valid-md.golden.json", validPlanMD, exitcode.Completed},
		{"valid-dir.golden.json", validPlanDir, exitcode.Completed},
		{"broken.golden.json", brokenPlan, exitcode.Failed},
	}
	for _, tc := range cases {
		h := newHarness(t)
		code := h.run("validate-plan", "--json", abs(t, tc.path))
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\n%s%s", tc.golden, code, tc.code, h.stdout.String(), h.stderr.String())
		}
		var r plan.Report
		if err := json.Unmarshal(h.stdout.Bytes(), &r); err != nil {
			t.Fatalf("%s: output is not JSON: %v\n%s", tc.golden, err, h.stdout.String())
		}
		golden(t, tc.golden, h.stdout.String())
	}
}

func TestValidatePlanTextOutput(t *testing.T) {
	h := newHarness(t)
	if code := h.run("validate-plan", abs(t, brokenPlan)); code != exitcode.Failed {
		t.Errorf("exit %d", code)
	}
	golden(t, "broken.zh.txt", h.stdout.String())

	h.write(filepath.Join(h.repo, ".aco.yaml"), "language: {operator: en}\n")
	h.run("validate-plan", abs(t, brokenPlan))
	golden(t, "broken.en.txt", h.stdout.String())

	h = newHarness(t)
	if code := h.run("validate-plan", abs(t, validPlanDir)); code != exitcode.Completed {
		t.Errorf("valid dir: exit %d\n%s", code, h.stdout.String())
	}
	for _, want := range []string{"计划包目录", "阶段 3 个，需求 4 个，验收标准 9 条", "AGENTS.md：AGENTS.md（sha256:", "P02  HTTP API（验收标准 3 条，依赖：P01）", "结果：通过（0 条警告）"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.stdout.String())
		}
	}
}

// The implementation plan itself passes validate-plan, also with --strict.
func TestValidatePlanOnImplementationPlan(t *testing.T) {
	for _, extra := range [][]string{nil, {"--strict"}} {
		h := newHarness(t)
		args := append([]string{"validate-plan", "--json", abs(t, "../../AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md")}, extra...)
		code := h.run(args...)
		var r plan.Report
		if err := json.Unmarshal(h.stdout.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if code != exitcode.Completed || !r.Valid || r.Summary.Phases != 14 || r.Summary.Requirements != 30 ||
			r.Summary.Errors != 0 || r.Summary.Warnings != 0 {
			t.Errorf("%v: exit %d, summary %+v\n%s", extra, code, r.Summary, h.stdout.String())
		}
	}
}

func TestValidatePlanStrict(t *testing.T) {
	src, err := os.ReadFile(validPlanMD)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	p := filepath.Join(h.repo, "plan.md")
	h.write(p, strings.Replace(string(src), "### Goal\nDocument the API.\n", "", 1)) // missing goal: a warning
	if code := h.run("validate-plan", p); code != exitcode.Completed || !strings.Contains(h.stdout.String(), "missing_goal") {
		t.Errorf("non-strict: exit %d\n%s", code, h.stdout.String())
	}
	if code := h.run("validate-plan", "--strict", "plan.md"); code != exitcode.Failed || !strings.Contains(h.stdout.String(), "严格模式") {
		t.Errorf("strict (relative path): exit %d\n%s", code, h.stdout.String())
	}
}

func TestValidatePlanUsageErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run("validate-plan"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "请指定计划文件") {
		t.Errorf("no path: exit %d %q", code, h.stderr.String())
	}
	if code := h.run("validate-plan", "nope.md"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "找不到计划：nope.md") {
		t.Errorf("missing file: exit %d %q", code, h.stderr.String())
	}
	if code := h.run("validate-plan", "a.md", "b.md"); code != exitcode.Usage {
		t.Errorf("two paths: exit %d", code)
	}
}

// A frozen package verifies; editing AGENTS.md or a phase file fails with
// the file name.
func TestValidatePlanLockVerification(t *testing.T) {
	h := newHarness(t)
	copyDir(t, "../plan/testdata/valid-dir", h.repo)
	l, err := plan.BuildLock(h.repo, plan.LockMeta{ReviewRounds: 1, Verdict: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteLock(h.repo, l); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.repo, ".aco", "plan")
	if code := h.run("validate-plan", dir); code != exitcode.Completed || !strings.Contains(h.stdout.String(), "plan.lock.json 校验通过（计划版本 v1）") {
		t.Fatalf("frozen: exit %d\n%s", code, h.stdout.String())
	}
	for _, rel := range []string{"AGENTS.md", ".aco/plan/phases/P02.md"} {
		p := filepath.Join(h.repo, filepath.FromSlash(rel))
		b, _ := os.ReadFile(p)
		h.write(p, string(b)+"\n- changed\n")
		code := h.run("validate-plan", "--json", dir)
		var r plan.Report
		_ = json.Unmarshal(h.stdout.Bytes(), &r)
		found := false
		for _, d := range r.Diagnostics {
			found = found || d.Code == plan.CodeLockMismatch && d.Location.File == rel && strings.Contains(d.Message, rel)
		}
		if code != exitcode.Failed || !found || r.Lock == nil || r.Lock.Verified {
			t.Errorf("%s changed: exit %d, lock %+v\n%s", rel, code, r.Lock, h.stdout.String())
		}
		h.write(p, string(b))
	}
	// A package copied elsewhere cannot be verified against its repository.
	other := filepath.Join(t.TempDir(), "plan-copy")
	copyDir(t, dir, other)
	h.run("validate-plan", other)
	if !strings.Contains(h.stdout.String(), "lock_unverified") {
		t.Errorf("copied package:\n%s", h.stdout.String())
	}
}

// Every plan diagnostic code has a catalog message whose English text is
// the plan package's message for the same arguments.
func TestPlanCheckMessagesInSync(t *testing.T) {
	for code, format := range plan.Codes() {
		_, ok := planCheckKeys[code]
		if !ok {
			t.Errorf("%s: no catalog key", code)
			continue
		}
		n := strings.Count(format, "%s")
		args := make([]string, n)
		for i := range args {
			args[i] = fmt.Sprintf("arg%d", i)
		}
		d := plan.NewDiagnostic(code, plan.SeverityError, plan.Location{}, args...)
		msg := diagMsg(d)
		if got := i18n.T(i18n.EN, msg.Key, msg.Args...); got != d.Message {
			t.Errorf("%s: en catalog %q, plan message %q", code, got, d.Message)
		}
		if got := i18n.T(i18n.ZhCN, msg.Key, msg.Args...); strings.Contains(got, "%!") {
			t.Errorf("%s: zh catalog has wrong arguments: %q", code, got)
		}
	}
	if len(planCheckKeys) != len(plan.Codes()) {
		t.Errorf("planCheckKeys has %d entries, plan has %d codes", len(planCheckKeys), len(plan.Codes()))
	}
}

// Plan text is shown back to the operator (titles, dependency text), so the
// output masks registered secrets and credential patterns in every encoding.
// Unregistered opaque text in a plan is plan content and is not a secret.
func TestValidatePlanOutputMasksSecrets(t *testing.T) {
	for _, f := range []string{securetest.FakeAnthropic, securetest.FakeOpenAI, securetest.FakeOpaque} {
		for _, format := range []string{"text", "json"} {
			h := newHarness(t)
			h.env["OPENAI_API_KEY"] = securetest.FakeOpaque
			h.env["ACO_LOG_LEVEL"] = "debug"
			h.env["ACO_LOG_FORMAT"] = format
			p := filepath.Join(h.repo, "plan.md")
			h.write(p, "# Plan "+f+"\n\n## Phase 1: Leak "+f+"\n\n### Depends On\n"+f+"\n\n### Quality Gates\n- "+f+": required\n")
			args := []string{"validate-plan", p}
			if format == "json" {
				args = append(args, "--json")
			}
			h.run(args...)
			securetest.AssertNoLeak(t, format, h.stdout.String()+h.stderr.String(), f)
		}
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
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
}

// A repository whose .aco is a junction (Windows) or symlink to an external
// plan must not pass, even though the external plan is valid.
func TestValidatePlanRefusesLinkedACO(t *testing.T) {
	ext := t.TempDir()
	copyDir(t, "../plan/testdata/valid-dir", ext)
	h := newHarness(t)
	link := filepath.Join(h.repo, ".aco")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, filepath.Join(ext, ".aco")).CombinedOutput(); err != nil {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
	} else if err := os.Symlink(filepath.Join(ext, ".aco"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	code := h.run("validate-plan", "--json", filepath.Join(h.repo, ".aco", "plan"))
	var r plan.Report
	_ = json.Unmarshal(h.stdout.Bytes(), &r)
	found := false
	for _, d := range r.Diagnostics {
		found = found || d.Code == plan.CodePathNotContained && d.Location.File == ".aco"
	}
	if code != exitcode.Failed || r.Valid || !found || r.Summary.Phases != 0 {
		t.Errorf("exit %d\n%s", code, h.stdout.String())
	}
}

// An AC outside its trace's phases fails in default mode (no --strict).
func TestValidatePlanRejectsInconsistentTraceWithoutStrict(t *testing.T) {
	h := newHarness(t)
	copyDir(t, "../plan/testdata/valid-dir", h.repo)
	tp := filepath.Join(h.repo, ".aco", "plan", "traceability.json")
	b, _ := os.ReadFile(tp)
	h.write(tp, strings.Replace(string(b), `"phases": ["P01"], "acceptance_criteria": ["AC-01.1", "AC-01.2", "AC-01.3"]`,
		`"phases": ["P01"], "acceptance_criteria": ["AC-02.1"]`, 1))
	code := h.run("validate-plan", filepath.Join(h.repo, ".aco", "plan"))
	out := h.stdout.String()
	if code != exitcode.Failed || !strings.Contains(out, "trace_ac_outside_phases") || !strings.Contains(out, "REQ-004 映射到 AC-02.1") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// linkDirForTest points link at target: a junction on Windows, a symlink elsewhere.
func linkDirForTest(t *testing.T, target, link string) {
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

func containmentOnly(t *testing.T, label string, code int, out string, wantFile string) {
	t.Helper()
	var r plan.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%s: %v\n%s", label, err, out)
	}
	found := false
	for _, d := range r.Diagnostics {
		switch d.Code {
		case plan.CodePathNotContained:
			found = found || d.Location.File == wantFile
		case plan.CodeLockMismatch, plan.CodeLockUnverified, plan.CodeParseError:
			t.Errorf("%s: diagnostic derived from content behind the link: %s %s", label, d.Code, d.Message)
		}
	}
	if code == exitcode.Completed || r.Valid || !found || r.Lock != nil || r.Summary.Phases != 0 {
		t.Errorf("%s: exit %d, want a %s containment error only\n%s", label, code, wantFile, out)
	}
}

// (a) A malformed external manifest behind a linked .aco is never opened.
func TestValidatePlanLinkedACODoesNotReadExternalManifest(t *testing.T) {
	ext := t.TempDir()
	copyDir(t, "../plan/testdata/valid-dir", ext)
	if err := os.WriteFile(filepath.Join(ext, ".aco", "plan", "plan.lock.json"), []byte("{EXTERNAL_SENTINEL"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	linkDirForTest(t, filepath.Join(ext, ".aco"), filepath.Join(h.repo, ".aco"))
	code := h.run("validate-plan", "--json", filepath.Join(h.repo, ".aco", "plan"))
	out := h.stdout.String() + h.stderr.String()
	if strings.Contains(out, "EXTERNAL_SENTINEL") {
		t.Errorf("external manifest was read:\n%s", out)
	}
	containmentOnly(t, "linked .aco + malformed manifest", code, h.stdout.String(), ".aco")
}

// (b) An explicit --repo elsewhere does not skip the check for a source at
// <root>/.aco/plan.
func TestValidatePlanExplicitRepoDoesNotBypassContainment(t *testing.T) {
	ext := t.TempDir()
	copyDir(t, "../plan/testdata/valid-dir", ext) // valid package, no manifest
	h := newHarness(t)
	linkDirForTest(t, filepath.Join(ext, ".aco"), filepath.Join(h.repo, ".aco"))
	other := t.TempDir()
	code := h.run("validate-plan", "--json", filepath.Join(h.repo, ".aco", "plan"), "--repo", other)
	containmentOnly(t, "linked .aco with --repo elsewhere", code, h.stdout.String(), ".aco")

	// Ordinary standalone plan directories still validate, with or without --repo.
	standalone := filepath.Join(t.TempDir(), "my-plan")
	copyDir(t, filepath.Join(ext, ".aco", "plan"), standalone)
	for _, args := range [][]string{{"validate-plan", standalone}, {"validate-plan", standalone, "--repo", other}} {
		if code := h.run(args...); code != exitcode.Completed {
			t.Errorf("%v: exit %d\n%s", args, code, h.stdout.String())
		}
	}
}

// A grouped Requirements entry gives its AC to every listed REQ; a
// cross-phase AC fails in default mode.
func TestValidatePlanGroupedRequirementEntry(t *testing.T) {
	src, err := os.ReadFile(validPlanMD)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	p := filepath.Join(h.repo, "plan.md")
	h.write(p, strings.Replace(string(src), "- REQ-004\n", "- REQ-004, REQ-001: AC-02.1\n", 1))
	code := h.run("validate-plan", "--json", p)
	var r plan.Report
	if err := json.Unmarshal(h.stdout.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	outside := map[string]bool{}
	uncovered := false
	for _, d := range r.Diagnostics {
		if d.Code == plan.CodeTraceACOutsidePhases && d.Location.Phase == "P01" {
			outside[strings.Fields(d.Message)[0]] = true
		}
		uncovered = uncovered || d.Code == plan.CodeRequirementUncovered && d.Location.Ref == "REQ-004"
	}
	if code != exitcode.Failed || !outside["REQ-004"] || !outside["REQ-001"] || !uncovered {
		t.Errorf("exit %d\n%s", code, h.stdout.String())
	}
}
