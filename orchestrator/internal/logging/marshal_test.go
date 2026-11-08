package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// sneaky hides its credential from fmt but emits it from MarshalJSON.
type sneaky struct{ ID int }

func (sneaky) String() string { return "sneaky{redacted-by-design}" }
func (s sneaky) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"id": s.ID, "auth": "Bearer " + fakeAnthropic, "nested": []any{fakeAnthropic, 7}})
}

// textOnly emits its credential only through MarshalText (used by the text handler).
type textOnly struct{}

func (textOnly) MarshalText() ([]byte, error) { return []byte("token=" + fakeAnthropic), nil }

type plain struct {
	Name  string
	Count int
}

func logLines(t *testing.T, jsonOut bool, args ...any) string {
	t.Helper()
	var buf bytes.Buffer
	New(&buf, Options{Level: slog.LevelInfo, JSON: jsonOut}).Info("m", args...)
	return buf.String()
}

func TestRedactsJSONMarshalerOutput(t *testing.T) {
	if s := fmt.Sprint(sneaky{}); strings.Contains(s, "FAKEFAKE") {
		t.Fatal("test type must hide the secret from fmt")
	}
	for _, jsonOut := range []bool{false, true} {
		out := logLines(t, jsonOut, "v", sneaky{ID: 42}, "t", textOnly{})
		if strings.Contains(out, "FAKEFAKE") {
			t.Fatalf("secret leaked (json=%v): %s", jsonOut, out)
		}
		if !jsonOut {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		v, ok := rec["v"].(map[string]any)
		if !ok {
			t.Fatalf("structure lost: %#v", rec["v"])
		}
		if v["id"] != float64(42) || v["auth"] != "Bearer [REDACTED]" {
			t.Errorf("fields: %#v", v)
		}
		if n, ok := v["nested"].([]any); !ok || n[0] != "[REDACTED]" || n[1] != float64(7) {
			t.Errorf("nested: %#v", v["nested"])
		}
		if rec["t"] != "token=[REDACTED]" {
			t.Errorf("text marshaler: %#v", rec["t"])
		}
	}
}

func TestCleanStructuredValuesUnchanged(t *testing.T) {
	out := logLines(t, true, "p", plain{Name: "phase-1", Count: 3}, "list", []int{1, 2}, "err", errors.New("plain failure"))
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	p, ok := rec["p"].(map[string]any)
	if !ok || p["Name"] != "phase-1" || p["Count"] != float64(3) {
		t.Errorf("struct: %#v", rec["p"])
	}
	if rec["err"] != "plain failure" {
		t.Errorf("err: %#v", rec["err"])
	}
	// A struct with a secret field stays a JSON object.
	out = logLines(t, true, "p", plain{Name: fakeAnthropic, Count: 1})
	if strings.Contains(out, "FAKEFAKE") {
		t.Fatalf("leak: %s", out)
	}
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if p, ok := rec["p"].(map[string]any); !ok || p["Name"] != "[REDACTED]" || p["Count"] != float64(1) {
		t.Errorf("struct with secret: %#v", rec["p"])
	}
	// Errors with secrets keep the handler's string form.
	out = logLines(t, true, "err", fmt.Errorf("wrap: %w", errors.New("bad key "+fakeAnthropic)))
	if strings.Contains(out, "FAKEFAKE") || !strings.Contains(out, `"err":"wrap: bad key [REDACTED]"`) {
		t.Errorf("error: %s", out)
	}
}
