package cli

import (
	"strconv"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/exitcode"
)

// fakeOpaque is a fake credential with no recognizable pattern and a double
// quote, so Go's flag package would print it escaped ("opaque\"credential").
const fakeOpaque = `opaque"credential-FAKE`

func assertNoCredential(t *testing.T, args []string, out string) {
	t.Helper()
	quoted := strconv.Quote(fakeOpaque) // "opaque\"credential-FAKE"
	for _, form := range []string{fakeOpaque, quoted, quoted[1 : len(quoted)-1], "opaque", "credential-FAKE"} {
		if strings.Contains(out, form) {
			t.Errorf("%v: output contains %q:\n%s", args, form, out)
		}
	}
}

func TestBadFlagValueNeverEchoed(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"resume", "id", "--from-phase", fakeOpaque}, []string{"--from-phase", "整数"}},
		{[]string{"resume", "id", "--from-phase=" + fakeOpaque}, []string{"--from-phase", "整数"}},
		{[]string{"resume", "id", "--retry=" + fakeOpaque}, []string{"--retry", "true 或 false"}},
		{[]string{"report", "id", "--phase", fakeOpaque}, []string{"--phase", "整数"}},
		{[]string{"resume", "id", "---" + fakeOpaque}, []string{"命令行参数格式无效"}},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.env["OPENAI_API_KEY"] = fakeOpaque
		code := h.run(tc.args...)
		out := h.stdout.String() + h.stderr.String()
		if code != exitcode.Usage {
			t.Errorf("%v: exit %d", tc.args, code)
		}
		assertNoCredential(t, tc.args, out)
		for _, w := range tc.want {
			if !strings.Contains(h.stderr.String(), w) {
				t.Errorf("%v: want %q in stderr:\n%s", tc.args, w, h.stderr.String())
			}
		}
	}
}

func TestBadFlagValueInEnglish(t *testing.T) {
	h := newHarness(t)
	h.write(h.repo+"/.aco.yaml", "language: {operator: en}\n")
	h.env["OPENAI_API_KEY"] = fakeOpaque
	code := h.run("resume", "id", "--from-phase", fakeOpaque)
	if code != exitcode.Usage || !strings.Contains(h.stderr.String(), "Invalid value for --from-phase (expected an integer)") {
		t.Errorf("exit %d stderr %q", code, h.stderr.String())
	}
	assertNoCredential(t, nil, h.stderr.String())
}

func TestUnknownAndMissingFlagDiagnosticsKept(t *testing.T) {
	h := newHarness(t)
	if code := h.run("resume", "id", "--bogus"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "未知参数：-bogus") {
		t.Errorf("unknown flag: exit %d %q", code, h.stderr.String())
	}
	h = newHarness(t)
	if code := h.run("resume", "id", "--from-phase"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "参数 -from-phase 需要一个值") {
		t.Errorf("missing value: exit %d %q", code, h.stderr.String())
	}
	// Valid typed values still parse (the stub then reports "not implemented").
	h = newHarness(t)
	if code := h.run("resume", "id", "--from-phase", "0x2", "--retry"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "尚未实现") {
		t.Errorf("valid flags: exit %d %q", code, h.stderr.String())
	}
}
