package cli

import (
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/exitcode"
)

const fakeSecret = "sk-ant-api03-FAKEFAKEFAKEFAKE0000"

func TestSetSecretIsNeverPrinted(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"doctor", "--set", "auth.api_key=" + fakeSecret}, "--set auth.api_key"},
		{[]string{"doctor", "--set=auth.api_key=" + fakeSecret}, "--set auth.api_key"},
		{[]string{"doctor", "--set", fakeSecret}, `"#1"`},
		{[]string{"doctor", "--set", "planner.model=[" + fakeSecret}, "--set planner.model"},
		{[]string{"doctor", "--set", "auth.mode=" + fakeSecret}, "auth.mode"},
		{[]string{"doctor", "--retry=" + fakeSecret}, ""},
		{[]string{"doctor", fakeSecret}, ""},
		{[]string{fakeSecret}, ""},
	}
	for _, tc := range cases {
		h := newHarness(t)
		code := h.run(tc.args...)
		out := h.stdout.String() + h.stderr.String()
		if code != exitcode.Usage {
			t.Errorf("%v: exit %d", tc.args, code)
		}
		if strings.Contains(out, "FAKEFAKE") {
			t.Errorf("%v: secret printed:\n%s", tc.args, out)
		}
		if !strings.Contains(h.stderr.String(), tc.want) {
			t.Errorf("%v: want %q in stderr:\n%s", tc.args, tc.want, h.stderr.String())
		}
	}
}

func TestDoubleDashEndsFlags(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--", "--help"},
		{"version", "--", "first", "--bogus"},
		{"version", "--", "-h"},
		{"doctor", "--", "--repo"},
	} {
		h := newHarness(t)
		code := h.run(args...)
		if code != exitcode.Usage || !strings.Contains(h.stderr.String(), "多余的参数") {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, code, h.stdout.String(), h.stderr.String())
		}
		if strings.Contains(h.stdout.String(), "用法") {
			t.Errorf("%v: printed help", args)
		}
	}
	h := newHarness(t)
	h.run("version", "--", "first", "--bogus")
	if !strings.Contains(h.stderr.String(), "first --bogus") {
		t.Errorf("positional args not kept in order: %q", h.stderr.String())
	}
}

func TestHelpTokenAsFlagValue(t *testing.T) {
	h := newHarness(t)
	// "--help" is the value of --repo, not a help request.
	code := h.run("doctor", "--repo", "--help")
	if strings.Contains(h.stdout.String(), "用法") {
		t.Errorf("help printed for a flag value: %q", h.stdout.String())
	}
	if code != exitcode.Completed || !strings.Contains(h.stdout.String(), "--help") {
		t.Errorf("doctor should run with repo \"--help\": exit %d %q", code, h.stdout.String())
	}
	h = newHarness(t)
	if code := h.run("doctor", "--set", "-h"); code != exitcode.Usage || strings.Contains(h.stdout.String(), "用法") {
		t.Errorf("--set -h: exit %d stdout %q", code, h.stdout.String())
	}
}

func TestFlagsAfterPositionalAndLocalizedHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--help", "--set", "language.operator=en"); code != 0 || !strings.Contains(h.stdout.String(), "Usage") {
		t.Errorf("localized help: exit %d %q", code, h.stdout.String())
	}
	h = newHarness(t)
	if code := h.run("doctor", "extra", "--set", "language.operator=en"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "Unexpected arguments: extra") {
		t.Errorf("flag after positional: exit %d %q", code, h.stderr.String())
	}
	h = newHarness(t)
	if code := h.run("version", "extra", "--bogus"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "bogus") {
		t.Errorf("unknown flag after positional: exit %d %q", code, h.stderr.String())
	}
}
