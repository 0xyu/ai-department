package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// testEnv isolates git from the user's and the system's configuration.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	content := "[user]\n\tname = Test User\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n[commit]\n\tgpgSign = false\n[tag]\n\tgpgSign = false\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1", "HOME=" + home}
}

// gitT runs git for test setup.
func gitT(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

type fixture struct {
	t    *testing.T
	env  []string
	user string // the user's checkout
	repo *Repo  // Repo for the user's checkout
	ctx  context.Context
}

// newFixture creates a user repository with one commit on main.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	env := testEnv(t)
	dir := filepath.Join(t.TempDir(), "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, env, dir, "init", "-q", "-b", "main")
	write(t, dir, "README.md", "# demo\n")
	write(t, dir, "src/app.go", "package src\n\nfunc A() int { return 1 }\n")
	write(t, dir, "src/app_test.go", "package src\n")
	write(t, dir, ".gitignore", "build/\n*.log\n")
	gitT(t, env, dir, "add", "-A")
	gitT(t, env, dir, "commit", "-q", "-m", "initial")
	r, err := Open(dir, Options{Env: env, Identity: Identity{Name: "ACO Test", Email: "aco@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, env: env, user: dir, repo: r, ctx: context.Background()}
}

// worktree creates a run worktree on a fresh branch.
func (f *fixture) worktree(branch string) *Repo {
	f.t.Helper()
	base, err := f.repo.ResolveCommit(f.ctx, "refs/heads/main")
	if err != nil {
		f.t.Fatal(err)
	}
	wt, err := f.repo.AddWorktree(f.ctx, filepath.Join(f.t.TempDir(), "wt"), branch, base)
	if err != nil {
		f.t.Fatal(err)
	}
	return wt
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// must returns v or panics on err (the test fails with the stack).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var testDate = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

// snapshotDir hashes every file under dir (content and relative path). Paths
// under skip (slash-separated prefixes) are left out.
func snapshotDir(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnapshots(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, k)
		}
	}
	sort.Strings(d)
	return d
}

// check panics on err (the test fails with the stack).
func check(err error) {
	if err != nil {
		panic(err)
	}
}
