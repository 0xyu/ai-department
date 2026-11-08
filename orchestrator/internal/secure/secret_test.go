package secure_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

type holder struct {
	Key    secure.Secret
	hidden secure.Secret
	Ptr    *secure.Secret
}

func TestSecretNeverPrints(t *testing.T) {
	for _, raw := range securetest.All {
		s := secure.NewSecret(raw)
		h := holder{Key: s, hidden: s, Ptr: &s}
		var outs []string
		for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%X", "%d", "%10.3s"} {
			outs = append(outs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, h), fmt.Sprintf(verb, []secure.Secret{s}))
		}
		outs = append(outs, fmt.Sprint(s, h), s.String(), s.GoString())
		for _, v := range []any{s, h, map[string]secure.Secret{"k": s}} {
			j, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			outs = append(outs, string(j))
		}
		txt, _ := s.MarshalText()
		outs = append(outs, string(txt), s.LogValue().String())
		securetest.AssertNoLeak(t, "secret", strings.Join(outs, "\n"), raw)
		if s.Reveal() != raw || s.IsZero() {
			t.Errorf("Reveal lost the value")
		}
	}
	if !secure.NewSecret("").IsZero() || (secure.Secret{}).Reveal() != "" {
		t.Errorf("zero secret")
	}
}

func TestRedactorMasksEncodedForms(t *testing.T) {
	r := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	for name, enc := range securetest.Encodings(securetest.FakeOpaque) {
		switch name {
		case "raw", "go-quoted", "json-escaped", "url-query", "url-path":
			if got := r.String("x " + enc + " y"); got != "x "+secure.Mask+" y" {
				t.Errorf("%s not masked: %s", name, got)
			}
		}
	}
	// Patterns are masked without registration.
	for _, f := range []string{securetest.FakeAnthropic, securetest.FakeOpenAI} {
		if got := secure.NewRedactor().String(f); got != secure.Mask {
			t.Errorf("pattern %s -> %s", f, got)
		}
	}
}

func TestEcho(t *testing.T) {
	for _, ok := range []string{"", "subscriptoin", "extreme", "fr", "-1", "1.5", "NaN", "+Inf", "claude-opus-5-5", "soon"} {
		if secure.NewRedactor().Echo(ok) != ok {
			t.Errorf("%q should be echoed", ok)
		}
	}
	for _, f := range append(securetest.All, "has space", "quote\"x", "aaaaaaaaaaaaaaaaaaaaa") {
		if secure.NewRedactor().Echo(f) != secure.Hidden {
			t.Errorf("%q should be hidden", f)
		}
	}
}

// A registered secret shorter than the masking threshold is never echoed,
// alone or inside a longer argument, and is left alone in free text.
func TestEchoHidesShortRegisteredSecret(t *testing.T) {
	const short = "Fk7q2x" // fake six-character credential
	r := secure.NewRedactor()
	r.AddFromEnv(map[string]string{"OPENAI_API_KEY": short})
	for _, v := range []string{short, "x" + short + "y"} {
		if got := r.Echo(v); got != secure.Hidden {
			t.Errorf("Echo(%q) = %q, want hidden", v, got)
		}
	}
	if got := r.Echo("doctor"); got != "doctor" {
		t.Errorf("Echo(doctor) = %q", got)
	}
	var nilR *secure.Redactor
	if got := nilR.Echo("doctor"); got != "doctor" {
		t.Errorf("nil Echo(doctor) = %q", got)
	}
}

func TestCorpusDetectsEveryEncoding(t *testing.T) {
	for _, f := range securetest.All {
		for name, enc := range securetest.Encodings(f) {
			if len(securetest.Leaks("prefix "+enc+" suffix", f)) == 0 {
				t.Errorf("%s of %q not detected", name, f)
			}
		}
		// Base64 of the secret embedded in other text, at every alignment.
		for k := 0; k < 3; k++ {
			s := string(make([]byte, k)) + "key=" + f + ";"
			for _, enc := range []string{base64Std(s), base64URL(s)} {
				if len(securetest.Leaks(enc, f)) == 0 {
					t.Errorf("embedded base64 (offset %d) of %q not detected", k, f)
				}
			}
		}
	}
	if found := securetest.Leaks("an ordinary log line", securetest.All...); len(found) > 0 {
		t.Errorf("false positive: %v", found)
	}
}
