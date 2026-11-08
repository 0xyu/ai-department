package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure"
)

const fakeAnthropic = "sk-ant-api03-FAKEFAKEFAKEFAKEFAKE1234567890"

func TestRedactsFakeAnthropicKey(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		var buf bytes.Buffer
		log := New(&buf, Options{Level: slog.LevelDebug, JSON: jsonOut})
		log.Info("calling claude with key "+fakeAnthropic,
			"key", fakeAnthropic,
			"err", errors.New("auth failed for "+fakeAnthropic),
			slog.Group("env", "ANTHROPIC_API_KEY", fakeAnthropic),
		)
		log.With("preset", fakeAnthropic).Warn("x")
		out := buf.String()
		if strings.Contains(out, "FAKEFAKE") {
			t.Fatalf("secret leaked (json=%v):\n%s", jsonOut, out)
		}
		if strings.Count(out, secure.Mask) < 5 {
			t.Errorf("expected 5 masks (json=%v):\n%s", jsonOut, out)
		}
	}
}

func TestRedactsKnownEnvValues(t *testing.T) {
	r := secure.NewRedactor()
	r.AddFromEnv(map[string]string{
		"OPENAI_API_KEY":   "plain-looking-secret-value",
		"MY_SERVICE_TOKEN": "another-secret-value",
		"HOME":             "/home/user-not-secret",
	})
	var buf bytes.Buffer
	New(&buf, Options{Level: slog.LevelInfo, Redactor: r}).Info("v", "a", "plain-looking-secret-value", "b", "another-secret-value", "home", "/home/user-not-secret")
	out := buf.String()
	if strings.Contains(out, "plain-looking") || strings.Contains(out, "another-secret") {
		t.Errorf("env secret leaked: %s", out)
	}
	if !strings.Contains(out, "/home/user-not-secret") {
		t.Errorf("non-secret value was masked: %s", out)
	}
}

func TestPatterns(t *testing.T) {
	r := secure.NewRedactor()
	for _, s := range []string{
		"sk-ant-abcdefgh12345678",
		"sk-proj-abcdefghijklmnop1234",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"github_pat_11ABCDEFGHIJKLMNOPQRSTUV",
		"AKIAABCDEFGHIJKLMNOP",
	} {
		if got := r.String("x " + s + " y"); got != "x "+secure.Mask+" y" {
			t.Errorf("%s -> %s", s, got)
		}
	}
	if got := r.String("task-list sk-short"); got != "task-list sk-short" {
		t.Errorf("false positive: %s", got)
	}
}

func TestLevel(t *testing.T) {
	if ParseLevel("debug") != slog.LevelDebug || ParseLevel("") != slog.LevelWarn || ParseLevel("ERROR") != slog.LevelError {
		t.Error("ParseLevel")
	}
}
