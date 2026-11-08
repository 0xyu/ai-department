package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

// calls counts every custom conversion the fixtures below perform. Under the
// allowlist none of them may run (errors excepted: Error() runs once).
var calls int

func emitWith(r *secure.Redactor, jsonOut bool, fn func(*slog.Logger)) string {
	var buf bytes.Buffer
	fn(New(&buf, Options{Level: slog.LevelInfo, JSON: jsonOut, Redactor: r}))
	return buf.String()
}

func emit(jsonOut bool, fn func(*slog.Logger)) string { return emitWith(nil, jsonOut, fn) }

func mustJSON(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, line)
	}
	return rec
}

// Adversarial fixtures from every earlier review round. Each hides a fake
// credential behind a different conversion or encoding.
type (
	jsonMarshaler struct{}                  // MarshalJSON emits the credential
	textMarshaler struct{}                  // MarshalText emits it
	stringer      struct{}                  // String emits it
	formatter     struct{}                  // Format emits it
	flipJSON      struct{}                  // clean on first call, secret on second
	panicJSON     struct{}                  // MarshalJSON panics with the credential
	panicText     struct{}                  // MarshalText panics with it
	digit         byte                      // byte type whose JSON is a number
	plainStruct   struct{ Name string }     // credential in a field
	withBytes     struct{ Data []byte }     // credential in nested bytes
	hiddenBytes   struct{ data []byte }     // ...in an unexported field
	withArray     struct{ Key [32]byte }    // ...in a byte array
	withDigits    struct{ Key []digit }     // ...as JSON numbers
	withInts      struct{ Key []int }       // ...as an int list
	cyclic        struct{ Next *cyclic }    // self-referencing
	secretHolder  struct{ K secure.Secret } // a Secret inside a struct
)

func (jsonMarshaler) MarshalJSON() ([]byte, error) {
	calls++
	return json.Marshal(securetest.FakeAnthropic)
}
func (textMarshaler) MarshalText() ([]byte, error) {
	calls++
	return []byte(securetest.FakeOpaque), nil
}
func (stringer) String() string              { calls++; return securetest.FakeUnregistered }
func (formatter) Format(s fmt.State, _ rune) { calls++; _, _ = s.Write([]byte(securetest.FakeOpaque)) }
func (flipJSON) MarshalJSON() ([]byte, error) {
	calls++
	if calls%2 == 1 {
		return []byte(`"clean"`), nil
	}
	return json.Marshal(securetest.FakeAnthropic)
}
func (panicJSON) MarshalJSON() ([]byte, error) { calls++; panic(securetest.FakeUnregistered) }
func (panicText) MarshalText() ([]byte, error) { calls++; panic(securetest.FakeUnregistered) }
func (d digit) MarshalJSON() ([]byte, error)   { calls++; return []byte(fmt.Sprint(int(d))), nil }

func bytesOf(s string) []byte { return []byte(s) }

func adversarial() map[string]any {
	var arr [32]byte
	copy(arr[:], securetest.FakeAnthropic)
	var ds []digit
	var is []int
	for _, b := range []byte(securetest.FakeAnthropic) {
		ds, is = append(ds, digit(b)), append(is, int(b))
	}
	deep := any(bytesOf(securetest.FakeAnthropic))
	for i := 0; i < 40; i++ {
		deep = []any{deep}
	}
	c := &cyclic{}
	c.Next = c
	s := []any{"x", bytesOf(securetest.FakeAnthropic)}
	return map[string]any{
		"json-marshaler": jsonMarshaler{},
		"text-marshaler": textMarshaler{},
		"stringer":       stringer{},
		"formatter":      formatter{},
		"flip-json":      flipJSON{},
		"panic-json":     panicJSON{},
		"panic-text":     panicText{},
		"struct":         plainStruct{securetest.FakeUnregistered},
		"bytes":          bytesOf(securetest.FakeAnthropic),
		"nested-bytes":   withBytes{bytesOf(securetest.FakeAnthropic)},
		"map-bytes":      map[string][]byte{"k": bytesOf(securetest.FakeAnthropic)},
		"hidden-bytes":   hiddenBytes{bytesOf(securetest.FakeAnthropic)},
		"array":          withArray{arr},
		"digits":         withDigits{ds},
		"ints":           withInts{is},
		"any-bytes":      []any{byte('s'), byte('k')},
		"deep":           deep,
		"cycle":          c,
		"overlap":        struct{ Short, Long []any }{s[:1], s},
		"secret-holder":  secretHolder{secure.NewSecret(securetest.FakeUnregistered)},
		"pointer":        &plainStruct{securetest.FakeUnregistered},
	}
}

func TestUnlistedValuesAreNeverConverted(t *testing.T) {
	for label, v := range adversarial() {
		for _, jsonOut := range []bool{false, true} {
			calls = 0
			out := emit(jsonOut, func(l *slog.Logger) {
				l.With("pre", v).Info("m", "v", v, "ok", "still-here")
			})
			mode := label + map[bool]string{false: "/text", true: "/json"}[jsonOut]
			securetest.AssertNoLeak(t, mode, out)
			if calls != 0 {
				t.Errorf("%s: %d custom conversions ran", mode, calls)
			}
			want := Unloggable(reflect.TypeOf(v))
			if !strings.Contains(out, want) || !strings.Contains(out, "still-here") {
				t.Errorf("%s: want %q and the clean attribute: %s", mode, want, out)
			}
			if jsonOut {
				if rec := mustJSON(t, out); rec["v"] != want || rec["pre"] != want {
					t.Errorf("%s: %s", mode, out)
				}
			}
		}
	}
}

// Errors: Error() runs once and is masked; panics are contained.
type panicErr struct{}

func (panicErr) Error() string { panic(securetest.FakeUnregistered) }

type fmtErr struct{} // safe Format, secret Error

func (fmtErr) Error() string              { calls++; return "auth failed: " + securetest.FakeAnthropic }
func (fmtErr) Format(s fmt.State, _ rune) { calls += 100; _, _ = s.Write([]byte("safe")) }

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg }

func TestErrorsUseErrorOnce(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		calls = 0
		out := emit(jsonOut, func(l *slog.Logger) {
			l.Info("m",
				"secret", fmtErr{},
				"panic", panicErr{},
				"nil", error((*nilErr)(nil)),
				"plain", fmt.Errorf("wrap: %w", errors.New("parser rejected literal (PANIC= token")),
				"ok", "still-here")
		})
		securetest.AssertNoLeak(t, "errors", out)
		if calls != 1 {
			t.Errorf("Error() should run once and Format never: calls=%d", calls)
		}
		for _, want := range []string{"auth failed: [REDACTED]", PanicPlaceholder, "<nil>", "wrap: parser rejected literal (PANIC= token", "still-here"} {
			if !strings.Contains(out, want) {
				t.Errorf("json=%v: missing %q in %s", jsonOut, want, out)
			}
		}
		if jsonOut {
			mustJSON(t, out)
		}
	}
}

type mode string
type phase int

// logValuer is the supported way to log a structured value.
type logValuer struct {
	Name string
	Key  secure.Secret
}

func (v logValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", v.Name), slog.Any("key", v.Key), slog.Any("raw", jsonMarshaler{}))
}

func TestAllowlistedValues(t *testing.T) {
	out := emit(true, func(l *slog.Logger) {
		l.Info("m",
			"s", "text "+securetest.FakeAnthropic,
			"n", 42, "f", 1.5, "b", true, "d", 90*time.Second,
			"mode", mode("api"), "phase", phase(3),
			"list", []string{"a", securetest.FakeOpenAI},
			"secret", secure.NewSecret(securetest.FakeUnregistered),
			"lv", logValuer{Name: "x", Key: secure.NewSecret(securetest.FakeOpaque)},
			slog.Group("g", "inner", "v"),
			"nil", nil,
		)
	})
	securetest.AssertNoLeak(t, "allowlisted", out)
	rec := mustJSON(t, out)
	checks := map[string]any{
		"s": "text [REDACTED]", "n": float64(42), "f": 1.5, "b": true, "d": float64(90 * time.Second),
		"mode": "api", "phase": float64(3), "secret": "[REDACTED]", "nil": nil,
	}
	for k, want := range checks {
		if rec[k] != want {
			t.Errorf("%s = %#v, want %#v", k, rec[k], want)
		}
	}
	if l, ok := rec["list"].([]any); !ok || l[0] != "a" || l[1] != "[REDACTED]" {
		t.Errorf("list: %#v", rec["list"])
	}
	lv, ok := rec["lv"].(map[string]any)
	if !ok || lv["name"] != "x" || lv["key"] != "[REDACTED]" || lv["raw"] != Unloggable(reflect.TypeOf(jsonMarshaler{})) {
		t.Errorf("LogValuer: %#v", rec["lv"])
	}
	if g, ok := rec["g"].(map[string]any); !ok || g["inner"] != "v" {
		t.Errorf("group: %#v", rec["g"])
	}

	text := emit(false, func(l *slog.Logger) { l.Info("m", "mode", mode("api"), "list", []string{"a", "b"}) })
	if !strings.Contains(text, "mode=api") || !strings.Contains(text, `list="[a b]"`) {
		t.Errorf("text: %s", text)
	}
}

func TestKeysAndGroupNamesMasked(t *testing.T) {
	r := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	for _, jsonOut := range []bool{false, true} {
		out := emitWith(r, jsonOut, func(l *slog.Logger) {
			l.WithGroup("grp-"+securetest.FakeAnthropic).
				With(securetest.FakeOpaque, "p").
				Info("msg "+securetest.FakeOpaque,
					securetest.FakeAnthropic, "top",
					slog.Group("outer", slog.Group("in-"+securetest.FakeOpenAI, "k-"+securetest.FakeAnthropic, 1)))
		})
		securetest.AssertNoLeak(t, "keys", out, securetest.FakeAnthropic, securetest.FakeOpenAI, securetest.FakeOpaque)
		if jsonOut {
			rec := mustJSON(t, out)
			g, ok := rec["grp-[REDACTED]"].(map[string]any)
			if !ok || g["[REDACTED]"] == nil {
				t.Errorf("structure: %s", out)
			}
		}
	}
}
