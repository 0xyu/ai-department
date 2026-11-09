package policy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gitops"
)

func gitT(t *testing.T, env []string, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
}

func writeFile(t *testing.T, dir, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCheckDiffFromGit runs the policy on a real working-state diff.
func TestCheckDiffFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	cfgFile := filepath.Join(home, "gitconfig")
	writeFile(t, home, "gitconfig", []byte("[user]\n\tname = T\n\temail = t@example.com\n[core]\n\tautocrlf = false\n"))
	env := []string{"GIT_CONFIG_GLOBAL=" + cfgFile, "GIT_CONFIG_NOSYSTEM=1"}
	dir := t.TempDir()
	gitT(t, env, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "src/a.go", []byte("package src\n"))
	writeFile(t, dir, "src/a_test.go", []byte("package src\n"))
	writeFile(t, dir, "AGENTS.md", []byte("rules\n"))
	gitT(t, env, dir, "add", "-A")
	gitT(t, env, dir, "commit", "-q", "-m", "init")

	ctx := context.Background()
	repo, err := gitops.Open(dir, gitops.Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "src/a.go", []byte("package src\n\nconst key = \"AKIAIOSFODNN7EXAMPLE\"\n"))
	writeFile(t, dir, "AGENTS.md", []byte("rules changed\n"))
	writeFile(t, dir, "big.bin", bytes.Repeat([]byte{0, 1, 2, 3}, (10<<20)/4))
	writeFile(t, dir, "other/x.go", []byte("package other\n"))
	if err := os.Remove(filepath.Join(dir, "src", "a_test.go")); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.WorkingTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pc := config.Default().Policy
	pc.ScopeEnforcement = "block"
	res, err := CheckDiff(ctx, repo, head, tree, FromConfig(pc, []string{"src/**"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"file_too_large:big.bin": true, "binary_file:big.bin": true, "scope_violation:big.bin": true,
		"protected_path:AGENTS.md": true, "scope_violation:AGENTS.md": true,
		"scope_violation:other/x.go": true,
		"secret_detected:src/a.go":   true, "tests_missing:other/x.go": true,
		"test_deleted:src/a_test.go": true,
	}
	got := map[string]bool{}
	for _, f := range res.Findings {
		got[f.Code+":"+f.Path] = true
		if f.Code == CodeSecret && (f.Line != 3 || f.Rule != "aws_access_key") {
			t.Fatalf("secret finding %+v", f)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %s; got %v", k, strings.Join(codes(res), ", "))
		}
	}
	if !res.Rejected() {
		t.Fatal("diff not rejected")
	}
}

// policyRepo creates a repository with isolated Git config, commits files,
// and returns it with its HEAD.
func policyRepo(t *testing.T, files map[string]string) (string, []string, *gitops.Repo, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	writeFile(t, home, "gitconfig", []byte("[user]\n\tname = T\n\temail = t@example.com\n[core]\n\tautocrlf = false\n"))
	env := []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1"}
	dir := t.TempDir()
	gitT(t, env, dir, "init", "-q", "-b", "main")
	for p, c := range files {
		writeFile(t, dir, p, []byte(c))
	}
	gitT(t, env, dir, "add", "-A")
	gitT(t, env, dir, "commit", "-q", "-m", "init")
	repo, err := gitops.Open(dir, gitops.Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return dir, env, repo, head
}

// TestGitattributesCannotHideAddedLines: a changed .gitattributes marks the
// files "-diff", so git shows them as binary with no lines. With
// binary_files: warn the binary finding does not block, but the fake AWS key
// and the skipped test are still found from the blob contents.
func TestGitattributesCannotHideAddedLines(t *testing.T) {
	dir, _, repo, head := policyRepo(t, map[string]string{
		"src/a.go":      "package src\n",
		"src/a_test.go": "package src\n",
	})
	ctx := context.Background()
	writeFile(t, dir, ".gitattributes", []byte("*.go -diff\n"))
	writeFile(t, dir, "src/a.go", []byte("package src\n\nconst key = \"AKIAIOSFODNN7EXAMPLE\"\n"))
	writeFile(t, dir, "src/a_test.go", []byte("package src\n\nfunc TestA(t *testing.T) { t.Skip() }\n"))
	tree, err := repo.WorkingTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Control: git itself reports the files binary and hides their lines.
	patch, err := repo.Patch(ctx, head, tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patch), "AKIA") || !strings.Contains(string(patch), "Binary files") {
		t.Fatalf("control failed: git did not hide the lines:\n%s", patch)
	}
	pc := config.Default().Policy
	if pc.BinaryFiles != "warn" {
		t.Fatal("default binary_files is not warn")
	}
	res, err := CheckDiff(ctx, repo, head, tree, FromConfig(pc, []string{"src/**", ".gitattributes"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	secret := requireFinding(t, res, CodeSecret, "src/a.go", true)
	if secret.Line != 3 || secret.Rule != "aws_access_key" {
		t.Fatalf("secret finding %+v", secret)
	}
	skip := requireFinding(t, res, CodeTestSkipped, "src/a_test.go", true)
	if skip.Line != 3 {
		t.Fatalf("skip finding %+v", skip)
	}
	requireFinding(t, res, CodeBinaryFile, "src/a.go", false)
	if !res.Rejected() {
		t.Fatal("diff not rejected")
	}
}

// TestLongLineBeforeLaterChange: a line far longer than bufio.Scanner's
// 64 KiB default comes before a later security-relevant change; the later
// change is still found, with the right line number.
func TestLongLineBeforeLaterChange(t *testing.T) {
	dir, _, repo, head := policyRepo(t, map[string]string{
		"src/gen.go":      "package src\n",
		"src/gen_test.go": "package src\n",
	})
	ctx := context.Background()
	long := strings.Repeat("y", 3<<20)
	writeFile(t, dir, "src/gen.go", []byte("package src\n\nvar blob = \""+long+"\"\n\nconst key = \"AKIAIOSFODNN7EXAMPLE\"\n"))
	writeFile(t, dir, "src/gen_test.go", []byte("package src\n// #nosec\n"))
	tree, err := repo.WorkingTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pc := config.Default().Policy
	res, err := CheckDiff(ctx, repo, head, tree, FromConfig(pc, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	f := requireFinding(t, res, CodeSecret, "src/gen.go", true)
	if f.Line != 5 {
		t.Fatalf("secret finding %+v", f)
	}
	requireFinding(t, res, CodeSuppression, "src/gen_test.go", true)
}
