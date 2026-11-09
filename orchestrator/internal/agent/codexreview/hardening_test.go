package codexreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/secure"
)

const sentinel = "SENTINEL-sk-proj-9f8e7d6c5b4a39281706f5e4d3c2b1a0"

// streamFixture writes a one-message JSONL fixture into a temp directory
// and points the fake CLI at it.
func (f *fixture) streamFixture(name, message string) {
	f.t.Helper()
	dir := f.path("streams")
	_ = os.MkdirAll(dir, 0o700)
	ev := []any{
		map[string]any{"type": "thread.started", "thread_id": "t-sentinel"},
		map[string]any{"type": "turn.started"},
		map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": message}},
		map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 10, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 5, "reasoning_output_tokens": 0}},
	}
	var b strings.Builder
	for _, e := range ev {
		j, _ := json.Marshal(e)
		b.Write(j)
		b.WriteByte('\n')
	}
	writeFile(f.t, filepath.Join(dir, name), b.String())
	f.extra = append(f.extra, "streamdir="+dir)
}

func sentinelRedactor() *secure.Redactor {
	r := secure.NewRedactor()
	r.AddSecret(secure.NewSecret(sentinel))
	return r
}

// noSentinel fails when any published artifact contains the sentinel.
func (f *fixture) noSentinel() {
	f.t.Helper()
	_ = filepath.WalkDir(f.run.Path(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), sentinel) {
				f.t.Errorf("the sentinel reached %s", p)
			}
		}
		return nil
	})
}

// outPaths returns the -o destinations of every exec call, and checks
// they were outside the run artifacts and are gone.
func (f *fixture) checkOutPaths() {
	f.t.Helper()
	for n := 0; ; n++ {
		b, err := os.ReadFile(f.path("args-" + strconv.Itoa(n)))
		if err != nil {
			if n == 0 {
				f.t.Fatal("no exec call recorded")
			}
			return
		}
		var args []string
		_ = json.Unmarshal(b, &args)
		for i, a := range args {
			if a != "-o" {
				continue
			}
			o := args[i+1]
			if lexWithin(f.run.Path(), o) {
				f.t.Errorf("-o %s is inside the run artifacts", o)
			}
			if _, err := os.Stat(o); err == nil {
				f.t.Errorf("-o %s was not cleaned up", o)
			}
			if _, err := os.Stat(filepath.Dir(o)); err == nil {
				f.t.Errorf("the private -o directory %s was not removed", filepath.Dir(o))
			}
		}
	}
}

func TestRawOutputNeverPublished(t *testing.T) {
	valid := strings.Replace(codeReviewText(t), "The change implements the calculator.", "Summary with "+sentinel, 1)

	// Success: the message is published masked.
	f := newFixture(t, "s.jsonl")
	f.streamFixture("s.jsonl", valid)
	res, err := f.adapter(auth.Subscription, func(o *Options) { o.Redactor = sentinelRedactor() }).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Raw), sentinel) {
		t.Error("the returned review holds the sentinel")
	}
	f.noSentinel()
	f.checkOutPaths()

	// Processing failure: not JSON, retried, then schema_invalid.
	g := newFixture(t, "s.jsonl")
	g.streamFixture("s.jsonl", "not json "+sentinel)
	_, err = g.adapter(auth.Subscription, func(o *Options) { o.Redactor = sentinelRedactor() }).ReviewCode(context.Background(), g.codeReq())
	if classOf(t, err) != agent.ClassSchemaInvalid || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("got %v", err)
	}
	g.noSentinel()
	g.checkOutPaths()

	// Oversized final message: bounded read, schema retry path, nothing published.
	h := newFixture(t, "s.jsonl")
	h.streamFixture("s.jsonl", valid)
	_, err = h.adapter(auth.Subscription, func(o *Options) { o.Redactor = sentinelRedactor(); o.MaxMessageBytes = 100 }).ReviewCode(context.Background(), h.codeReq())
	if classOf(t, err) != agent.ClassSchemaInvalid || !strings.Contains(err.Error(), "larger than 100 bytes") {
		t.Fatalf("got %v", err)
	}
	h.noSentinel()
	h.checkOutPaths()

	// Publication failure: the artifact path is taken by a directory.
	k := newFixture(t, "s.jsonl")
	k.streamFixture("s.jsonl", valid)
	_ = os.MkdirAll(filepath.Join(k.run.Path(), "phases", "P01", "a1", "reviews", "r01", "call-01", "last-message.json"), 0o700)
	_, err = k.adapter(auth.Subscription, func(o *Options) { o.Redactor = sentinelRedactor() }).ReviewCode(context.Background(), k.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "storing the final message failed") || ae.Invocation.OutputRef != "" {
		t.Fatalf("got %v", err)
	}
	k.noSentinel()
	k.checkOutPaths()
}

func codeReviewText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/stream/code_valid.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e struct {
		Item struct{ Text string } `json:"item"`
	}
	_ = json.Unmarshal([]byte(lines[len(lines)-2]), &e)
	return e.Item.Text
}

func hashFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// §16.5: api mode never touches the user's Codex home, also through an
// alias (a link or junction to it, or to a parent of it) or a configured
// subscription CODEX_HOME; the check runs before status, login, or exec.
func TestAPIHomeAliasesRejected(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fixture) (apiHome string, base []string)
	}{
		{"link to ~/.codex", func(f *fixture) (string, []string) {
			linkDir(t, filepath.Join(f.home, ".codex"), filepath.Join(f.dir, "alias"))
			return filepath.Join(f.dir, "alias"), nil
		}},
		{"aliased parent", func(f *fixture) (string, []string) {
			linkDir(t, f.home, filepath.Join(f.dir, "homealias"))
			return filepath.Join(f.dir, "homealias", ".codex"), nil
		}},
		{"inside ~/.codex", func(f *fixture) (string, []string) {
			return filepath.Join(f.home, ".codex", "api"), nil
		}},
		{"configured subscription CODEX_HOME", func(f *fixture) (string, []string) {
			custom := filepath.Join(f.dir, "custom-codex")
			writeFile(t, filepath.Join(custom, "auth.json"), `{"auth_mode":"chatgpt"}`)
			return custom, []string{"CODEX_HOME=" + custom}
		}},
		{"aliased configured CODEX_HOME", func(f *fixture) (string, []string) {
			custom := filepath.Join(f.dir, "custom-codex")
			writeFile(t, filepath.Join(custom, "auth.json"), `{"auth_mode":"chatgpt"}`)
			linkDir(t, custom, filepath.Join(f.dir, "custom-alias"))
			return filepath.Join(f.dir, "custom-alias"), []string{"CODEX_HOME=" + custom}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "code_valid.jsonl")
			userAuth := filepath.Join(f.home, ".codex", "auth.json")
			before := hashFile(t, userAuth)
			apiHome, extraBase := c.setup(f)
			a := f.adapter(auth.API, func(o *Options) {
				o.APIHome = apiHome
				o.BaseEnv = append(o.BaseEnv, extraBase...)
			})
			_, err := a.ReviewCode(context.Background(), f.codeReq())
			if !errors.Is(err, ErrUnsafeAPIHome) || classOf(t, err) != agent.ClassAuth {
				t.Fatalf("got %v", err)
			}
			if _, err := a.LoginStatus(context.Background()); !errors.Is(err, ErrUnsafeAPIHome) {
				t.Errorf("status ran: %v", err)
			}
			if _, err := os.Stat(f.path("logins")); err == nil {
				t.Error("codex login ran")
			}
			if _, err := os.Stat(f.path("args-0")); err == nil {
				t.Error("codex exec ran")
			}
			if hashFile(t, userAuth) != before {
				t.Error("the user's credentials changed")
			}
			if b, err := os.ReadFile(filepath.Join(f.dir, "custom-codex", "auth.json")); err == nil && strings.Contains(string(b), "apikey") {
				t.Error("the configured CODEX_HOME was logged in with the key")
			}
		})
	}
	// The dedicated home itself must not be a link (ambiguous), even to a safe place.
	f := newFixture(t, "code_valid.jsonl")
	_ = os.MkdirAll(filepath.Join(f.dir, "elsewhere"), 0o700)
	linkDir(t, filepath.Join(f.dir, "elsewhere"), filepath.Join(f.dir, "apilink"))
	if err := f.adapter(auth.API, func(o *Options) { o.APIHome = filepath.Join(f.dir, "apilink") }).EnsureLogin(context.Background()); !errors.Is(err, ErrUnsafeAPIHome) {
		t.Errorf("linked api home: %v", err)
	}
}

// A JSONL stream larger than the transcript limit fails clearly; the
// partial transcript is not stored as complete; metadata is kept.
func TestTranscriptOverflow(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	_, err := f.adapter(auth.Subscription, func(o *Options) { o.MaxTranscriptBytes = 600 }).ReviewCode(context.Background(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassCrash || !strings.Contains(ae.Message, "exceeds the transcript limit of 600 bytes") {
		t.Fatalf("got %v", err)
	}
	inv := ae.Invocation
	if inv.TranscriptRef != "" || inv.SessionID != "01a1bef3-0000-7000-8000-000000000001" || inv.PromptRef == "" || inv.Model != "gpt-6.1-sol" {
		t.Errorf("invocation metadata: %+v", inv)
	}
	prefix := "phases/P01/a1/reviews/r01/call-01/"
	if _, err := f.run.Read(prefix + "transcript.jsonl"); err == nil {
		t.Error("a partial transcript was stored as transcript.jsonl")
	}
	if b, err := f.run.Read(prefix + "transcript.incomplete.txt"); err != nil || !strings.Contains(string(b), "exceeded 600 bytes") {
		t.Errorf("incomplete marker: %q %v", b, err)
	}
	if len(ae.Calls) != 1 {
		t.Errorf("calls: %d", len(ae.Calls))
	}
}

// A review that is valid JSON but misses a nested required field enters
// the schema retry path with the schema error.
func TestNestedSchemaErrorRetried(t *testing.T) {
	valid := codeReviewText(t)
	bad := strings.Replace(valid, `"test_assessment": {"adequate": true, "notes": "unit tests cover both criteria"}`, `"test_assessment": {}`, 1)
	if bad == valid {
		t.Fatal("fixture text did not change")
	}
	f := newFixture(t, "bad.jsonl", "good.jsonl")
	f.streamFixture("bad.jsonl", bad)
	f.extra = f.extra[:0]
	f.streamFixture("good.jsonl", valid)
	res, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 2 || !res.Review.TestAssessment.Adequate {
		t.Fatalf("calls %d", len(res.Calls))
	}
	if second := f.read("stdin-1"); !strings.Contains(second, "$.test_assessment.adequate: missing required field") {
		t.Errorf("the retry lacks the schema error:\n%.800s", second)
	}
}
