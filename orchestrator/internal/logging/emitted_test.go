package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// flipMarshaler returns clean JSON on its first call and a credential on
// every later call. If the handler inspected one call and let the inner
// handler make another, the second call would leak.
type flipMarshaler struct{ calls *int }

func (f flipMarshaler) MarshalJSON() ([]byte, error) {
	*f.calls++
	if *f.calls == 1 {
		return []byte(`{"state":"clean"}`), nil
	}
	return []byte(`{"state":"` + fakeAnthropic + `"}`), nil
}

// flipText does the same through MarshalText (used by the text handler).
type flipText struct{ calls *int }

func (f flipText) MarshalText() ([]byte, error) {
	*f.calls++
	if *f.calls == 1 {
		return []byte("clean"), nil
	}
	return []byte(fakeAnthropic), nil
}

// formattedErr prints safe text through fmt but returns a credential from Error().
type formattedErr struct{}

func (formattedErr) Error() string { return "auth failed: " + fakeAnthropic }
func (formattedErr) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "auth failed (details withheld)")
}

func emit(jsonOut bool, fn func(*slog.Logger)) string {
	var buf bytes.Buffer
	fn(New(&buf, Options{Level: slog.LevelInfo, JSON: jsonOut}))
	return buf.String()
}

func mustJSON(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, line)
	}
	return rec
}

func TestStatefulMarshalerConvertedOnce(t *testing.T) {
	calls := 0
	out := emit(true, func(l *slog.Logger) { l.Info("m", "v", flipMarshaler{&calls}) })
	if strings.Contains(out, "FAKEFAKE") {
		t.Fatalf("second MarshalJSON call leaked: %s", out)
	}
	if calls != 1 {
		t.Errorf("MarshalJSON called %d times, want 1", calls)
	}
	if v, ok := mustJSON(t, out)["v"].(map[string]any); !ok || v["state"] != "clean" {
		t.Errorf("structure: %s", out)
	}

	calls = 0
	out = emit(false, func(l *slog.Logger) { l.Info("m", "v", flipText{&calls}) })
	if strings.Contains(out, "FAKEFAKE") || calls != 1 || !strings.Contains(out, "v=clean") {
		t.Errorf("MarshalText calls=%d out=%s", calls, out)
	}

	// The same value logged through WithAttrs is converted once as well.
	calls = 0
	out = emit(true, func(l *slog.Logger) { l.With("v", flipMarshaler{&calls}).Info("m") })
	if strings.Contains(out, "FAKEFAKE") || calls != 1 {
		t.Errorf("WithAttrs: calls=%d out=%s", calls, out)
	}
}

func TestErrorWithSafeFormatterButSecretError(t *testing.T) {
	if s := fmt.Sprintf("%+v", formattedErr{}); strings.Contains(s, "FAKEFAKE") {
		t.Fatal("test type must hide the secret from fmt")
	}
	for _, jsonOut := range []bool{false, true} {
		out := emit(jsonOut, func(l *slog.Logger) { l.Info("m", "err", formattedErr{}) })
		if strings.Contains(out, "FAKEFAKE") {
			t.Fatalf("Error() leaked (json=%v): %s", jsonOut, out)
		}
		if jsonOut {
			if got := mustJSON(t, out)["err"]; got != "auth failed: [REDACTED]" {
				t.Errorf("json err = %#v", got)
			}
		} else if !strings.Contains(out, `err="auth failed (details withheld)"`) {
			t.Errorf("text err: %s", out)
		}
	}
}

func TestSecretsInKeysAndGroupNames(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		out := emit(jsonOut, func(l *slog.Logger) {
			l.WithGroup("grp-"+fakeAnthropic).
				With(fakeAnthropic+".preset", "p").
				Info("m",
					fakeAnthropic, "top",
					slog.Group("outer", slog.Group("in-"+fakeAnthropic, "k-"+fakeAnthropic, 1)),
				)
		})
		if strings.Contains(out, "FAKEFAKE") {
			t.Fatalf("key leaked (json=%v): %s", jsonOut, out)
		}
		if strings.Count(out, "[REDACTED]") < 5 {
			t.Errorf("expected masked keys (json=%v): %s", jsonOut, out)
		}
		if !jsonOut {
			continue
		}
		rec := mustJSON(t, out)
		g, ok := rec["grp-[REDACTED]"].(map[string]any)
		if !ok || g["[REDACTED]"] != "top" || g["[REDACTED].preset"] != "p" {
			t.Fatalf("group structure: %s", out)
		}
		inner, ok := g["outer"].(map[string]any)["in-[REDACTED]"].(map[string]any)
		if !ok || inner["k-[REDACTED]"] != float64(1) {
			t.Errorf("nested group: %s", out)
		}
	}
}

type ordered struct {
	Zeta  string
	Alpha int
	Inner struct{ B, A string }
}

func TestOrdinaryValuesKeepTheirForm(t *testing.T) {
	v := ordered{Zeta: "z <&>", Alpha: 2}
	v.Inner.B, v.Inner.A = "b", fakeAnthropic
	out := emit(true, func(l *slog.Logger) {
		l.Info("m", "v", v, "err", errors.New("plain failure"), "n", []int{1, 2}, "nil", nil)
	})
	// Field order and unescaped HTML characters match slog's own output.
	if !strings.Contains(out, `"v":{"Zeta":"z <&>","Alpha":2,"Inner":{"B":"b","A":"[REDACTED]"}}`) {
		t.Errorf("struct form: %s", out)
	}
	rec := mustJSON(t, out)
	if rec["err"] != "plain failure" || rec["nil"] != nil {
		t.Errorf("err/nil: %s", out)
	}
	if n, ok := rec["n"].([]any); !ok || len(n) != 2 {
		t.Errorf("slice: %s", out)
	}

	out = emit(false, func(l *slog.Logger) {
		l.Info("m", "err", fmt.Errorf("wrap: %w", errors.New("plain failure")), "b", []byte("raw bytes"))
	})
	if !strings.Contains(out, `err="wrap: plain failure"`) || !strings.Contains(out, `b="raw bytes"`) {
		t.Errorf("text form: %s", out)
	}
}
