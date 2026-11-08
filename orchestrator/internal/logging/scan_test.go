package logging

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// noRecoverableBytes checks for the raw credential and its decimal byte list
// in both fmt form ("115 107 ...") and JSON form ("115,107,...").
func noRecoverableBytes(t *testing.T, label, out string) {
	t.Helper()
	noCredential(t, label, out)
	var nums []string
	for _, b := range []byte(fakeAnthropic)[:12] {
		nums = append(nums, strconv.Itoa(int(b)))
	}
	if strings.Contains(out, strings.Join(nums, ",")) {
		t.Errorf("%s: JSON byte list: %s", label, out)
	}
}

// both logs v in text and JSON mode next to a clean attribute, and checks
// the credential is unrecoverable, the clean attribute survives, and JSON
// stays valid. It returns the two outputs.
func both(t *testing.T, label string, v any) (text, js string) {
	t.Helper()
	text = emit(false, func(l *slog.Logger) { l.Info("m", "v", v, "ok", "still-here") })
	js = emit(true, func(l *slog.Logger) { l.Info("m", "v", v, "ok", "still-here") })
	for mode, out := range map[string]string{"text": text, "json": js} {
		noRecoverableBytes(t, label+"/"+mode, out)
		if !strings.Contains(out, "still-here") {
			t.Errorf("%s/%s: clean attribute lost: %s", label, mode, out)
		}
	}
	if rec := mustJSON(t, js); rec["ok"] != "still-here" {
		t.Errorf("%s: json: %s", label, js)
	}
	return text, js
}

func wrap(v any, n int) any {
	for i := 0; i < n; i++ {
		v = []any{v}
	}
	return v
}

func TestScanFailsClosedOnDepth(t *testing.T) {
	var arr [len(fakeAnthropic)]byte
	copy(arr[:], fakeAnthropic)
	for label, v := range map[string]any{
		"slice": wrap([]byte(fakeAnthropic), 40),
		"array": wrap(arr, 40),
	} {
		text, js := both(t, label, v)
		if !strings.Contains(text, UninspectablePlaceholder) || mustJSON(t, js)["v"] != UninspectablePlaceholder {
			t.Errorf("%s: want placeholder\n%s\n%s", label, text, js)
		}
	}
	// Shallow nesting is still inspected and masked normally.
	text, _ := both(t, "shallow", wrap([]byte(fakeAnthropic), 3))
	if strings.Contains(text, UninspectablePlaceholder) {
		t.Errorf("shallow value treated as uninspectable: %s", text)
	}
}

func TestScanFailsClosedOnNodeBudget(t *testing.T) {
	s := make([]any, maxScanNodes+10)
	for i := range s {
		s[i] = []any{i}
	}
	s[len(s)-1] = []byte(fakeAnthropic)
	text, js := both(t, "budget", s)
	if !strings.Contains(text, UninspectablePlaceholder) || mustJSON(t, js)["v"] != UninspectablePlaceholder {
		t.Errorf("want placeholder:\n%.300s\n%.300s", text, js)
	}

	// Large values that cannot hold byte data are not budget-limited.
	big := make([]string, maxScanNodes*2)
	ints := make([]int, maxScanNodes*2)
	for i := range big {
		big[i], ints[i] = "s", 1000+i
	}
	for _, v := range []any{big, ints} {
		out := emit(true, func(l *slog.Logger) { l.Info("m", "v", v) })
		if strings.Contains(out, "REDACTED") {
			t.Errorf("clean large value replaced: %.200s", out)
		}
	}
}

func TestOverlappingSliceViewsBothScanned(t *testing.T) {
	s := []any{"x", []byte(fakeAnthropic)}
	both(t, "short-first", struct{ Short, Long []any }{s[:1], s})
	both(t, "long-first", struct{ Long, Short []any }{s, s[:1]})

	// Pointer to a struct and to its first field share an address.
	type inner struct{ Data []byte }
	type outer struct{ In inner }
	o := &outer{In: inner{Data: []byte(fakeAnthropic)}}
	both(t, "struct-and-field", struct {
		A *outer
		B *inner
	}{o, &o.In})
}

type cyclic struct {
	Next *cyclic
	Data []byte
}

func TestScanCycleProtection(t *testing.T) {
	n := &cyclic{Data: []byte(fakeAnthropic)}
	n.Next = n
	both(t, "cycle", n)

	clean := &cyclic{Data: []byte("ok")}
	clean.Next = clean
	out := emit(false, func(l *slog.Logger) { l.Info("m", "v", clean) })
	if strings.Contains(out, "REDACTED") || !strings.Contains(out, "Data:[111 107]") {
		t.Errorf("clean cycle: %s", out)
	}
}

// digit is a byte type whose MarshalJSON writes its decimal value, so
// encoding/json prints []digit as a number list instead of Base64.
type digit byte

var digitCalls int

func (d digit) MarshalJSON() ([]byte, error) {
	digitCalls++
	return []byte(strconv.Itoa(int(d))), nil
}

func digits(s string) []digit {
	out := make([]digit, len(s))
	for i := range s {
		out[i] = digit(s[i])
	}
	return out
}

func TestByteLikeSlicesJSONCannotMask(t *testing.T) {
	digitCalls = 0
	for label, v := range map[string]any{
		"marshaling byte type": struct{ Key []digit }{digits(fakeAnthropic)},
		"int slice": struct{ Key []int }{func() []int {
			r := []int{}
			for _, b := range []byte(fakeAnthropic) {
				r = append(r, int(b))
			}
			return r
		}()},
		"any of bytes": struct{ Key []any }{func() []any {
			r := []any{}
			for _, b := range []byte(fakeAnthropic) {
				r = append(r, b)
			}
			return r
		}()},
	} {
		text, js := both(t, label, v)
		if !strings.Contains(text, BinarySecretPlaceholder) || mustJSON(t, js)["v"] != BinarySecretPlaceholder {
			t.Errorf("%s: want placeholder\n%s\n%s", label, text, js)
		}
	}
	if digitCalls != 0 {
		t.Errorf("MarshalJSON ran %d times on a value that was replaced", digitCalls)
	}

	// Clean values of the same shapes are unchanged, and MarshalJSON runs
	// once per element (one conversion of the value).
	digitCalls = 0
	js := emit(true, func(l *slog.Logger) { l.Info("m", "v", struct{ Key []digit }{digits("ok")}) })
	if !strings.Contains(js, `"v":{"Key":[111,107]}`) || digitCalls != 2 {
		t.Errorf("clean digits: calls=%d %s", digitCalls, js)
	}
	text := emit(false, func(l *slog.Logger) { l.Info("m", "v", struct{ Key []int }{[]int{1, 2}}) })
	if !strings.Contains(text, `v="{Key:[1 2]}"`) {
		t.Errorf("clean ints: %s", text)
	}
}
