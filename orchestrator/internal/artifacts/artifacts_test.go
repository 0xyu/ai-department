package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

func newRunDir(t *testing.T, red *secure.Redactor) (*RunDir, string) {
	t.Helper()
	root := t.TempDir()
	d, err := New(root, red).Run("20261109-abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	return d, root
}

func TestWriteReadAtomicReplace(t *testing.T) {
	d, root := newRunDir(t, nil)
	if d.Path() != filepath.Join(root, "runs", "20261109-abcd1234") {
		t.Errorf("path %s", d.Path())
	}
	rel := GateLog(3, 1, 2, "unit")
	if rel != "phases/P03/a1/gates/r02/unit.log" {
		t.Errorf("layout %s", rel)
	}
	ref, err := d.Write(rel, []byte("FAIL TestX\n"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("FAIL TestX\n"))
	if ref.Size != 11 || ref.SHA256 != hex.EncodeToString(sum[:]) || ref.Path != rel {
		t.Errorf("ref %+v", ref)
	}
	if _, err := d.Write(rel, []byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	got, err := d.Read(rel)
	if err != nil || string(got) != "ok\n" {
		t.Errorf("read %q %v", got, err)
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(filepath.Join(d.Path(), filepath.FromSlash(rel))))
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(d.Path(), filepath.FromSlash(rel)))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", fi.Mode())
		}
	}
	abs, err := d.Abs(rel)
	if err != nil || !strings.HasPrefix(abs, d.Path()) {
		t.Errorf("abs %s %v", abs, err)
	}
}

func TestCheckPath(t *testing.T) {
	good := []string{"a.txt", "phases/P01/a1/steps/s0001-implement/prompt.md", "schemas/code-review.schema.json",
		Planning(1, "x.md"), PlanReview(12, "review.json"), Baseline(1, 2, "unit.log"), StepFile(1, 1, 7, "implement", "t.jsonl"),
		ReviewFile(1, 1, 1, "context.md"), DiffFile(1, 1, 1, "full.patch"), Final("build.log"), ReportFile("blocked.md"),
		SchemaFile("plan-review.schema.json")}
	for _, p := range good {
		if err := CheckPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	bad := []string{"", "/abs", "../x", "a/../b", "a//b", "a/", "./a", ".hidden", "a/.git/config", `a\b`, "C:/x",
		"c:x", "a b", "con", "NUL.txt", "lpt1", "trail.", "-flag", "a/" + strings.Repeat("x", 129),
		strings.Repeat("a/", 17) + "f", "名字.md", "a\x00b"}
	for _, p := range bad {
		if err := CheckPath(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("%q accepted (%v)", p, err)
		}
	}
	if _, err := New(t.TempDir(), nil).Run("../evil"); !errors.Is(err, ErrBadPath) {
		t.Errorf("bad run ID: %v", err)
	}
}

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

// A linked directory inside the run directory (or a linked run directory)
// is never followed for writes or reads.
func TestLinksAreNotFollowed(t *testing.T) {
	d, root := newRunDir(t, nil)
	outside := t.TempDir()
	linkDir(t, outside, filepath.Join(d.Path(), "phases"))
	if _, err := d.Write("phases/P01/a1/x.log", []byte("x")); !errors.Is(err, ErrNotRegular) {
		t.Errorf("write through link: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Read("phases/secret.txt"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("read through link: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("wrote outside the run directory: %v", entries)
	}

	// A linked run directory is refused when it is opened.
	other := filepath.Join(root, "runs", "20261109-linked00")
	linkDir(t, outside, other)
	if _, err := New(root, nil).Run("20261109-linked00"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("linked run dir: %v", err)
	}
}

func TestWriterBuffersAndMasksAcrossChunks(t *testing.T) {
	red := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	d, _ := newRunDir(t, red)
	w, err := d.Create(StepFile(1, 1, 1, "implement", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	half := len(securetest.FakeOpaque) / 2
	w.Write([]byte("line " + securetest.FakeOpaque[:half]))
	w.Write([]byte(securetest.FakeOpaque[half:] + " end\n"))
	if _, err := os.Stat(filepath.Join(d.Path(), "phases")); !errors.Is(err, os.ErrNotExist) {
		t.Error("writer stored content before Commit")
	}
	ref, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.Read(ref.Path)
	if string(got) != "line "+secure.Mask+" end\n" {
		t.Errorf("got %q", got)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrWriterState) {
		t.Errorf("write after commit: %v", err)
	}
	if _, err := w.Commit(); !errors.Is(err, ErrWriterState) {
		t.Errorf("commit twice: %v", err)
	}
	a, _ := d.Create("aborted.txt")
	a.Write([]byte("x"))
	a.Abort()
	if _, err := d.Read("aborted.txt"); err == nil {
		t.Error("aborted writer stored content")
	}
}

func TestTooLarge(t *testing.T) {
	d, _ := newRunDir(t, nil)
	if _, err := d.Write("big.log", make([]byte, MaxSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v", err)
	}
}

// Artifacts written with fake secrets are masked: registered secrets in
// every encoding the redactor covers, and credential patterns even when not
// registered.
func TestArtifactsNeverStoreSecrets(t *testing.T) {
	var secrets []secure.Secret
	for _, f := range securetest.All {
		secrets = append(secrets, secure.NewSecret(f))
	}
	d, _ := newRunDir(t, secure.NewRedactor(secrets...))
	var in strings.Builder
	for _, f := range securetest.All {
		enc := securetest.Encodings(f)
		for _, name := range []string{"raw", "go-quoted", "json-escaped", "url-query", "url-path"} {
			in.WriteString(name + ": " + enc[name] + "\n")
		}
	}
	in.WriteString(`{"prompt":"key=` + securetest.FakeAnthropic + `"}` + "\n")
	ref, err := d.Write(ReviewFile(2, 1, 1, "context.md"), []byte(in.String()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.Read(ref.Path)
	if !strings.Contains(string(got), secure.Mask) {
		t.Fatal("nothing masked")
	}
	securetest.AssertNoLeak(t, "artifact", string(got))

	// Patterns are masked without any registered secret.
	plain, _ := newRunDir(t, nil)
	ref, _ = plain.Write("p.txt", []byte(securetest.FakeAnthropic+" "+securetest.FakeOpenAI))
	got, _ = plain.Read(ref.Path)
	securetest.AssertNoLeak(t, "pattern artifact", string(got), securetest.FakeAnthropic, securetest.FakeOpenAI)
}
