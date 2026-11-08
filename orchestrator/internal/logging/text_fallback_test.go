package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// decimalList is how %+v prints the credential's bytes: "115 107 45 ...".
func decimalList() string {
	s := fmt.Sprint([]byte(fakeAnthropic))
	return s[1 : len(s)-1]
}

func noCredential(t *testing.T, label, out string) {
	t.Helper()
	if strings.Contains(out, "FAKEFAKE") {
		t.Errorf("%s: raw credential: %s", label, out)
	}
	// The first 12 bytes ("sk-ant-api03") as decimals are already enough to
	// show the list is gone; check the full list and a long prefix.
	full := decimalList()
	prefix := strings.Join(strings.Fields(full)[:12], " ")
	if strings.Contains(out, full) || strings.Contains(out, prefix) {
		t.Errorf("%s: recoverable byte list: %s", label, out)
	}
}

type bytesHolder struct {
	Name string
	Data []byte
}

type hiddenBytes struct {
	Name string
	data []byte
}

type arrayHolder struct {
	Key [len(fakeAnthropic)]byte
}

// counted implements every conversion and counts how often any runs.
type counted struct{ calls *int }

func (c counted) MarshalJSON() ([]byte, error) { *c.calls++; return []byte(`"counted"`), nil }
func (c counted) String() string               { *c.calls++; return "counted" }

type mixed struct {
	Data []byte
	C    counted
}

func TestNestedByteSlicesMaskedInTextLogs(t *testing.T) {
	var arr arrayHolder
	copy(arr.Key[:], fakeAnthropic)
	cases := map[string]any{
		"map":        map[string][]byte{"key": []byte(fakeAnthropic)},
		"struct":     bytesHolder{Name: "n", Data: []byte(fakeAnthropic)},
		"pointer":    &bytesHolder{Name: "n", Data: []byte(fakeAnthropic)},
		"slice":      [][]byte{[]byte("ok"), []byte(fakeAnthropic)},
		"unexported": hiddenBytes{Name: "n", data: []byte(fakeAnthropic)},
		"array":      arr,
		"named":      map[string]blob{"k": blob(fakeAnthropic)},
	}
	for label, v := range cases {
		out := emit(false, func(l *slog.Logger) { l.Info("m", "v", v, "ok", "still-here") })
		noCredential(t, label, out)
		if !strings.Contains(out, "ok=still-here") {
			t.Errorf("%s: rest of record lost: %s", label, out)
		}
	}
	// Byte arrays in JSON logs are number lists too.
	out := emit(true, func(l *slog.Logger) { l.Info("m", "v", arr) })
	noCredential(t, "json array", out)
	mustJSON(t, out)
}

func TestCleanNestedBytesUnchangedInTextLogs(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{map[string][]byte{"k": {1, 2, 3}}, `v="map[k:[1 2 3]]"`},
		{bytesHolder{Name: "n", Data: []byte{1, 2}}, `v="{Name:n Data:[1 2]}"`},
		{hiddenBytes{Name: "n", data: []byte("ok")}, `v="{Name:n data:[111 107]}"`},
		{[]int{1, 2}, `v="[1 2]"`},
	}
	for _, tc := range cases {
		out := emit(false, func(l *slog.Logger) { l.Info("m", "v", tc.v) })
		if !strings.Contains(out, tc.want) {
			t.Errorf("want %s in %s", tc.want, out)
		}
	}
}

func TestMaskedFormRunsConversionsOnce(t *testing.T) {
	calls := 0
	out := emit(false, func(l *slog.Logger) { l.Info("m", "v", mixed{Data: []byte(fakeAnthropic), C: counted{&calls}}) })
	noCredential(t, "mixed", out)
	if calls != 1 {
		t.Errorf("conversions ran %d times, want 1: %s", calls, out)
	}
	// Clean value: fmt calls String once on the nested field.
	calls = 0
	emit(false, func(l *slog.Logger) { l.Info("m", "v", mixed{Data: []byte("ok"), C: counted{&calls}}) })
	if calls != 1 {
		t.Errorf("clean: conversions ran %d times", calls)
	}
}

type panicFormatter struct{}

func (panicFormatter) Format(fmt.State, rune) { panic(panicPayload) }

type panicStringer struct{}

func (panicStringer) String() string { panic(panicPayload) }

type holdsPanicStringer struct {
	Name string
	S    panicStringer
}

func TestPanicMarkerTextIsNotAPanic(t *testing.T) {
	msg := "parser rejected literal (PANIC= token"
	for _, v := range []any{
		fmt.Errorf("parser rejected literal (PANIC= token"),
		errors.New(msg),
		struct{ Msg string }{msg},
	} {
		out := emit(false, func(l *slog.Logger) { l.Info("m", "v", v) })
		if !strings.Contains(out, msg) || strings.Contains(out, PanicPlaceholder) {
			t.Errorf("ordinary message replaced: %s", out)
		}
	}
}

func TestRealPanicsInTextFallbackContained(t *testing.T) {
	for label, v := range map[string]any{
		"formatter":       panicFormatter{},
		"stringer":        panicStringer{},
		"error":           panicErr{},
		"nested stringer": holdsPanicStringer{Name: "n"},
	} {
		out := emit(false, func(l *slog.Logger) { l.Info("m", "v", v, "ok", "still-here") })
		if strings.Contains(out, "panic-payload") || !strings.Contains(out, PanicPlaceholder) || !strings.Contains(out, "ok=still-here") {
			t.Errorf("%s: %s", label, out)
		}
	}
}
