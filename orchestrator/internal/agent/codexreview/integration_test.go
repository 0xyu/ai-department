//go:build integration

package codexreview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/secure"
)

// The installed Codex CLI, without any model call: capability detection,
// the command line it accepts, and the api-mode login of a dedicated
// CODEX_HOME with a dummy key (codex login does not validate the key).
func realCodex(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed")
	}
	return p
}

func TestIntegrationDetect(t *testing.T) {
	bin := realCodex(t)
	a := New(Options{Bin: bin, Model: "gpt-6.1-sol"})
	caps, err := a.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m := caps.Missing(); len(m) > 0 {
		t.Fatalf("codex %s lacks %v", caps.Version, m)
	}
	if caps.Features == nil || !caps.HasFeature("hooks") {
		t.Errorf("features not detected: %v", caps.Features)
	}
	args, err := BuildArgs(caps, CommandSpec{Model: "gpt-6.1-sol", ReasoningEffort: "high", Dir: t.TempDir(),
		SchemaPath: filepath.Join(t.TempDir(), "s.json"), LastMessagePath: filepath.Join(t.TempDir(), "o.json")})
	if err != nil {
		t.Fatal(err)
	}
	// Every disabled feature must be known to the installed CLI (unknown
	// names make codex fail before it starts).
	for i, x := range args {
		if x == "--disable" && !caps.HasFeature(args[i+1]) {
			t.Errorf("--disable %s is not a feature of this CLI", args[i+1])
		}
	}
}

func TestIntegrationAPIHomeLogin(t *testing.T) {
	bin := realCodex(t)
	dir := t.TempDir()
	key := "sk-dummy-not-a-real-key-1234567890abcd"
	env := auth.NewEnv(func(k string) (string, bool) { return key, k == auth.EnvOpenAIKey }, nil)
	red := secure.NewRedactor()
	a := New(Options{Bin: bin, Mode: auth.API, Env: env, APIHome: filepath.Join(dir, "codex-api"), UserHome: filepath.Join(dir, "home"),
		Model: "gpt-6.1-sol", Redactor: red})
	if err := a.EnsureLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := a.LoginStatus(context.Background())
	if err != nil || st.Login != auth.CodexLoginAPIKey || st.KeyHint != auth.CodexKeyHint(secure.NewSecret(key)) {
		t.Fatalf("status %+v %v", st, err)
	}
	// The subscription check against the same home fails (mode mismatch).
	if err := auth.CheckCodexLogin(auth.Subscription, st, ""); err == nil {
		t.Error("api-key login accepted for subscription mode")
	}
	if strings.Contains(red.String(key), key) {
		t.Error("the key is not registered with the redactor")
	}
	if _, err := os.Stat(filepath.Join(dir, "home", ".codex")); err == nil {
		t.Error("api mode created ~/.codex")
	}
}
