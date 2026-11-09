package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
)

type gitRepo struct {
	t   *testing.T
	dir string
	env []string
	r   *gitops.Repo
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	content := "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n[commit]\n\tgpgSign = false\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gitRepo{t: t, dir: t.TempDir(), env: []string{"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1", "HOME=" + home}}
	g.git("init", "-q", "-b", "main")
	writeFile(t, g.dir, "app.go", "package app\n\nfunc A() int { return 1 }\n")
	writeFile(t, g.dir, "app_test.go", "package app\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")
	g.commit("initial")
	r, err := gitops.Open(g.dir, gitops.Options{Env: g.env})
	if err != nil {
		t.Fatal(err)
	}
	g.r = r
	return g
}

func (g *gitRepo) git(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), g.env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		g.t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func (g *gitRepo) commit(msg string) string {
	g.git("add", "-A")
	g.git("commit", "-q", "-m", msg)
	return g.git("rev-parse", "HEAD")
}

// A fake AWS key ID (the documented example value), built so this file
// itself does not contain it.
var fakeAWSKey = "AKIA" + "IOSFODNN7EXAMPLE"

func securitySpec(scanners ...string) Spec {
	s := spec(Security)
	s.Scanners = scanners
	return s
}

// Phase mode: the security gate runs the diff policy and the secret scanner
// on Base → working state.
func TestSecurityGatePhaseDiff(t *testing.T) {
	g := newGitRepo(t)
	base := g.git("rev-parse", "HEAD")
	pol := policy.FromConfig(config.Default().Policy, nil, nil)
	r := newRunner(t, func(o *Options) { o.Dir, o.Repo, o.Base, o.Policy = g.dir, g.r, base, pol })
	ctx := context.Background()

	// Clean change with a test: passes.
	writeFile(t, g.dir, "app.go", "package app\n\nfunc A() int { return 2 }\n")
	writeFile(t, g.dir, "app_test.go", "package app\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { _ = A() }\n")
	round, err := r.Run(ctx, []Spec{securitySpec(ScannerSecrets)}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := round.Gate(Security)
	if res.Status != Passed || res.TreeHash == "" || res.TreeHash != round.TreeHash {
		t.Fatalf("clean diff: %s %s tree=%q", res.Status, res.Summary, res.TreeHash)
	}

	// A secret in the diff blocks with class policy, and the finding does
	// not contain the secret.
	writeFile(t, g.dir, "config.go", "package app\n\nconst awsKey = \""+fakeAWSKey+"\"\n")
	round, err = r.Run(ctx, []Spec{securitySpec(ScannerSecrets)}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res = round.Gate(Security)
	if res.Status != Failed || res.Class != ClassPolicy {
		t.Fatalf("secret: %s/%s %s", res.Status, res.Class, res.Summary)
	}
	found := false
	for _, i := range res.Comparison.BlockingIssues {
		if i.Tool == ToolSecrets && i.Path == "config.go" && i.Line == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("secret finding missing: %+v", res.Comparison.BlockingIssues)
	}
	if bytes.Contains(mustMarshal(t, res), []byte(fakeAWSKey)) {
		t.Error("result contains the secret")
	}

	// The secret scanner can be switched off; the diff policy cannot.
	os.Remove(filepath.Join(g.dir, "config.go"))
	writeFile(t, g.dir, "app_test.go", "package app\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Skip(\"later\") }\n")
	round, _ = r.Run(ctx, []Spec{securitySpec()}, RunOptions{})
	res = round.Gate(Security)
	if res.Status != Failed || res.Class != ClassPolicy || !hasRule(res.Comparison.BlockingIssues, policy.CodeTestSkipped) {
		t.Errorf("anti-gaming: %s %+v", res.Status, res.Comparison)
	}
}

// Full mode (Stage 4): every file and every commit of the run is scanned.
// A secret added and removed again inside the run still blocks.
func TestSecurityGateFullHistory(t *testing.T) {
	g := newGitRepo(t)
	base := g.git("rev-parse", "HEAD")
	writeFile(t, g.dir, "leak.txt", "key="+fakeAWSKey+"\n")
	leakCommit := g.commit("add key")
	os.Remove(filepath.Join(g.dir, "leak.txt"))
	g.commit("remove key")

	r := newRunner(t, func(o *Options) { o.Dir, o.Repo, o.Base = g.dir, g.r, base })
	round, err := r.Run(context.Background(), []Spec{securitySpec(ScannerSecrets)}, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	res := round.Gate(Security)
	if res.Status != Failed {
		t.Fatalf("history secret: %s %s", res.Status, res.Summary)
	}
	want := "commit " + leakCommit[:12] + ": leak.txt"
	if !hasPath(res.Comparison.BlockingIssues, want) {
		t.Errorf("want %q in %+v", want, res.Comparison.BlockingIssues)
	}

	// A secret already in the files at the run baseline is reported, not
	// blocking; scanning only commits after it finds nothing new.
	g2 := newGitRepo(t)
	writeFile(t, g2.dir, "old.txt", "key="+fakeAWSKey+"\n")
	start := g2.commit("pre-existing secret")
	r2 := newRunner(t, func(o *Options) { o.Dir, o.Repo, o.Base = g2.dir, g2.r, start })
	ctx := context.Background()
	b, err := r2.Run(ctx, []Spec{securitySpec(ScannerSecrets)}, RunOptions{Mode: ModeFull, Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Gate(Security).Status != Failed {
		t.Errorf("baseline round records the secret: %s", b.Gate(Security).Status)
	}
	bl := NewBaseline(b, start, time.Now())
	writeFile(t, g2.dir, "new.go", "package app\n")
	g2.commit("clean change")
	round, err = r2.Run(ctx, []Spec{securitySpec(ScannerSecrets)}, RunOptions{Mode: ModeFull, Compare: bl})
	if err != nil {
		t.Fatal(err)
	}
	res = round.Gate(Security)
	if res.Status != Passed || res.Comparison.PreexistingIssues != 1 {
		t.Errorf("pre-existing secret: %s %+v", res.Status, res.Comparison)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasRule(is []Issue, code string) bool {
	for _, i := range is {
		if strings.HasPrefix(i.Rule, code) {
			return true
		}
	}
	return false
}

func hasPath(is []Issue, p string) bool {
	for _, i := range is {
		if i.Path == p {
			return true
		}
	}
	return false
}
