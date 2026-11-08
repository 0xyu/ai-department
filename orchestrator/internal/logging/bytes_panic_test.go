package logging

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

type blob []byte

// customBlob is a named byte slice with its own MarshalJSON; it must win.
type customBlob []byte

func (customBlob) MarshalJSON() ([]byte, error) { return []byte(`"custom-form"`), nil }

type withBytes struct {
	Name string
	Data []byte
}

func b64Field(t *testing.T, raw any) []byte {
	t.Helper()
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("want Base64 string, got %#v", raw)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("not Base64: %q", s)
	}
	return b
}

func TestByteSlicesMaskedBeforeEncoding(t *testing.T) {
	out := emit(true, func(l *slog.Logger) {
		l.Info("m",
			"raw", []byte("key="+fakeAnthropic),
			"named", blob(fakeAnthropic),
			"nested", withBytes{Name: "n", Data: []byte(fakeAnthropic)},
		)
	})
	rec := mustJSON(t, out)
	for _, f := range []string{"raw", "named"} {
		if dec := b64Field(t, rec[f]); bytes.Contains(dec, []byte("FAKEFAKE")) || !bytes.Contains(dec, []byte("[REDACTED]")) {
			t.Errorf("%s decodes to %q", f, dec)
		}
	}
	nested := rec["nested"].(map[string]any)
	if dec := b64Field(t, nested["Data"]); bytes.Contains(dec, []byte("FAKEFAKE")) || nested["Name"] != "n" {
		t.Errorf("nested decodes to %q", dec)
	}

	// Text logs: secrets masked, named byte slices stay readable text.
	out = emit(false, func(l *slog.Logger) { l.Info("m", "raw", []byte(fakeAnthropic), "named", blob("hello "+fakeAnthropic)) })
	if strings.Contains(out, "FAKEFAKE") || !strings.Contains(out, `named="hello [REDACTED]"`) {
		t.Errorf("text: %s", out)
	}
}

func TestOrdinaryByteSlicesUnchanged(t *testing.T) {
	bin := []byte{0, 1, 2, 0xff, 0xfe}
	out := emit(true, func(l *slog.Logger) {
		l.Info("m", "bin", bin, "named", blob("hello"), "custom", customBlob(fakeAnthropic))
	})
	rec := mustJSON(t, out)
	want, _ := json.Marshal(bin)
	if !strings.Contains(out, `"bin":`+string(want)) {
		t.Errorf("clean binary changed: %s", out)
	}
	if got := b64Field(t, rec["named"]); string(got) != "hello" {
		t.Errorf("named: %q", got)
	}
	if rec["custom"] != "custom-form" {
		t.Errorf("MarshalJSON precedence lost: %s", out)
	}

	out = emit(false, func(l *slog.Logger) { l.Info("m", "named", blob("hello")) })
	if !strings.Contains(out, "named=hello") {
		t.Errorf("text named: %s", out)
	}
}

// panicPayload is a fake credential that no redaction rule recognizes, so it
// can only stay out of the output if the panic payload is never printed.
const panicPayload = "opaque-panic-payload-FAKE"

type panicErr struct{}

func (panicErr) Error() string { panic(panicPayload) }

type panicText struct{ calls *int }

func (p panicText) MarshalText() ([]byte, error) { *p.calls++; panic(panicPayload) }

type panicJSON struct{ calls *int }

func (p panicJSON) MarshalJSON() ([]byte, error) { *p.calls++; panic(panicPayload) }

func noPayload(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "panic-payload") || strings.Contains(out, "FAKE") {
		t.Errorf("panic payload in output: %s", out)
	}
	if !strings.Contains(out, PanicPlaceholder) {
		t.Errorf("placeholder missing: %s", out)
	}
	if !strings.Contains(out, "still-here") {
		t.Errorf("rest of the record lost: %s", out)
	}
}

func TestPanickingConversionsContained(t *testing.T) {
	// Error() panics, JSON logs.
	out := emit(true, func(l *slog.Logger) { l.Info("m", "err", panicErr{}, "ok", "still-here") })
	noPayload(t, out)
	if rec := mustJSON(t, out); rec["err"] != PanicPlaceholder || rec["ok"] != "still-here" {
		t.Errorf("json record: %s", out)
	}

	// Error() panics, text logs (fmt would print the payload itself).
	noPayload(t, emit(false, func(l *slog.Logger) { l.Info("m", "err", panicErr{}, "ok", "still-here") }))

	// MarshalText panics, text logs via With(...): converted exactly once.
	calls := 0
	out = emit(false, func(l *slog.Logger) { l.With("v", panicText{&calls}).Info("m", "ok", "still-here") })
	noPayload(t, out)
	if calls != 1 {
		t.Errorf("MarshalText called %d times", calls)
	}

	// MarshalJSON panics, JSON logs, both directly and via With(...).
	for _, viaWith := range []bool{false, true} {
		calls = 0
		out = emit(true, func(l *slog.Logger) {
			if viaWith {
				l.With("v", panicJSON{&calls}).Info("m", "ok", "still-here")
			} else {
				l.Info("m", "v", panicJSON{&calls}, "ok", "still-here")
			}
		})
		noPayload(t, out)
		if rec := mustJSON(t, out); rec["v"] != PanicPlaceholder {
			t.Errorf("json v: %s", out)
		}
		if calls != 1 {
			t.Errorf("MarshalJSON called %d times (with=%v)", calls, viaWith)
		}
	}
}

func TestNilPointerErrorLikeSlog(t *testing.T) {
	var e *nilErr
	for _, jsonOut := range []bool{false, true} {
		out := emit(jsonOut, func(l *slog.Logger) { l.Info("m", "err", error(e)) })
		if !strings.Contains(out, "<nil>") || strings.Contains(out, PanicPlaceholder) {
			t.Errorf("json=%v: %s", jsonOut, out)
		}
	}
}

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg }
