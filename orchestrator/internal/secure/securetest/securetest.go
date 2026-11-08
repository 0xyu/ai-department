// Package securetest is the shared leak corpus: fake credentials and every
// encoding in which one could escape through an output. Tests of each sink
// (CLI output, logs, and later events, reports, prompts, API responses)
// write the fakes in and check the output with AssertNoLeak.
//
// Only test code may import this package (enforced by a guard test).
package securetest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Fake credentials. None is real.
const (
	// FakeAnthropic matches the sk-ant- credential pattern.
	FakeAnthropic = "sk-ant-api03-FAKEFAKEFAKEFAKE0000"
	// FakeOpenAI matches the generic sk- pattern.
	FakeOpenAI = "sk-proj-FAKEFAKEFAKEFAKE0000"
	// FakeOpaque matches no pattern and has a quote and a backslash, so its
	// quoted and JSON-escaped forms differ from the raw text. It is masked
	// only when registered with the Redactor (it came from a secret variable).
	FakeOpaque = `opaque"cred\FAKE-0000-zz`
	// FakeUnregistered is a plain credential-shaped value that no rule knows
	// about; diagnostics must still not repeat it (secure.Redactor.Echo).
	FakeUnregistered = "abcdEFGHijkl0123mnopQRST4567uvwx"
)

// All lists every fake.
var All = []string{FakeAnthropic, FakeOpenAI, FakeOpaque, FakeUnregistered}

// Encodings returns the forms in which secret could appear in output, by name.
func Encodings(secret string) map[string]string {
	b := []byte(secret)
	q := strconv.Quote(secret)
	j, _ := json.Marshal(secret)
	dec := make([]string, len(b))
	for i, c := range b {
		dec[i] = strconv.Itoa(int(c))
	}
	out := map[string]string{
		"raw":           secret,
		"go-quoted":     q[1 : len(q)-1],
		"json-escaped":  string(j[1 : len(j)-1]),
		"url-query":     url.QueryEscape(secret),
		"url-path":      url.PathEscape(secret),
		"hex":           hex.EncodeToString(b),
		"hex-upper":     strings.ToUpper(hex.EncodeToString(b)),
		"decimal-space": strings.Join(dec, " "),
		"decimal-comma": strings.Join(dec, ","),
	}
	// Base64 depends on where the secret starts within a 3-byte group, so
	// check the stable middle of the encoding at each alignment.
	for k := 0; k < 3; k++ {
		for name, enc := range map[string]*base64.Encoding{"std": base64.RawStdEncoding, "url": base64.RawURLEncoding} {
			s := enc.EncodeToString(append(make([]byte, k), b...))
			start := 0
			if k > 0 {
				start = 4
			}
			end := (k + len(b)) / 3 * 4
			if end-start >= 16 {
				out["base64-"+name+"-"+strconv.Itoa(k)] = s[start:end]
			}
		}
	}
	return out
}

// Leaks returns the names of the encodings of any secret found in out.
func Leaks(out string, secrets ...string) []string {
	var found []string
	for _, s := range secrets {
		for name, enc := range Encodings(s) {
			if enc != "" && strings.Contains(out, enc) {
				found = append(found, name+" of "+strconv.Quote(s))
			}
		}
	}
	return found
}

// AssertNoLeak fails t when out contains any encoding of any secret (all
// fakes when none are given).
func AssertNoLeak(t testing.TB, label, out string, secrets ...string) {
	t.Helper()
	if len(secrets) == 0 {
		secrets = All
	}
	if found := Leaks(out, secrets...); len(found) > 0 {
		t.Errorf("%s: leaked %v in:\n%s", label, found, out)
	}
}
