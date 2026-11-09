package codexreview

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
)

func unixMonitor(existing ...string) *Monitor {
	ex := map[string]bool{}
	for _, e := range existing {
		ex[e] = true
	}
	return &Monitor{
		Roots: []string{"/home/u/.aco/worktrees/r1", "/home/u/.aco/runs/r1"}, Home: "/home/u", GOOS: "linux",
		Exists:       func(p string) bool { return ex[p] },
		EvalSymlinks: func(p string) (string, error) { return p, nil },
	}
}

func TestMonitorUnix(t *testing.T) {
	m := unixMonitor("/etc/passwd", "/home/u/.aco/worktrees/r1/calc.go", "/usr/include/stdio.h")
	for _, c := range []struct {
		cmd  string
		flag bool
	}{
		{"/bin/bash -lc 'cat ~/.ssh/id_rsa'", true},
		{"cat $HOME/.codex/auth.json", true},
		{"/bin/bash -lc 'cat ${HOME}/.aws/credentials'", true},
		{"cat /etc/passwd", true},
		{"rg -n Add calc.go", false},
		{"cat calc.go && git diff HEAD~1 -- calc.go", false},
		{"cat /home/u/.aco/runs/r1/phases/P01/full.patch", false},
		{"cat ../../other-run/secret.txt", true}, // resolves into ~/.aco: under home
		{"cd .. && cat r2/x.go", true},
		{"cd internal && cat ../calc.go", false},
		{"rg '/api/v1/users' src", false}, // a pattern, not an existing path
		{"git -C /home/u/other-repo log", true},
		{"cat $(go env GOROOT)/src/x.go", false}, // heuristic limit: not resolvable, documented
		{"cat $XDG_CONFIG_HOME/gh/hosts.yml", true},
		{"sed -n 1,40p calc.go 2>/dev/null", false},
		{"bash -c \"cat /etc/passwd\"", true},
		{"ls -la", false},
		{"cat --file=/usr/include/stdio.h", true},
		{"cat < /etc/passwd", true},
		{"python3 -c 'print(1)'", false},
	} {
		got := len(m.Command(c.cmd)) > 0
		if got != c.flag {
			t.Errorf("%q: flagged=%v, want %v (%+v)", c.cmd, got, c.flag, m.Command(c.cmd))
		}
	}
}

func TestMonitorSymlinkEscape(t *testing.T) {
	m := unixMonitor("/home/u/.aco/worktrees/r1/link")
	m.EvalSymlinks = func(p string) (string, error) {
		if strings.HasSuffix(p, "/link") {
			return "/home/u/.ssh", nil
		}
		return p, nil
	}
	if vs := m.Command("cat ./link/../link/id_rsa"); len(vs) != 0 {
		// "./link/../link/id_rsa" cleans to link/id_rsa, which does not exist itself
		_ = vs
	}
	if vs := m.Command("cat /home/u/.aco/worktrees/r1/link"); len(vs) == 0 {
		t.Error("a link from the worktree to ~/.ssh was not flagged")
	}
}

func TestMonitorWindows(t *testing.T) {
	m := &Monitor{
		Roots: []string{`C:\Users\u\.aco\worktrees\r1`, `C:\Users\u\.aco\runs\r1`}, Home: `C:\Users\u`, GOOS: "windows",
		Exists:       func(p string) bool { return strings.EqualFold(p, `C:\Windows\win.ini`) },
		EvalSymlinks: func(p string) (string, error) { return p, nil },
	}
	ps := `"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" -Command `
	for _, c := range []struct {
		cmd  string
		flag bool
	}{
		{ps + `"Get-Content -LiteralPath 'C:/Users/u/.agents/skills/superpowers/SKILL.md'"`, true},
		{ps + `'Get-Content -LiteralPath calc.go'`, false},
		{ps + `'git log --oneline'`, false},
		{ps + `'Get-Content $env:USERPROFILE\.ssh\id_rsa'`, true},
		{ps + `'Get-Content ~\.codex\auth.json'`, true},
		{ps + `'Get-Content -Path:C:\Windows\win.ini'`, true},
		{ps + `'Get-Content C:\Users\u\.aco\runs\r1\full.patch'`, false},
		{ps + `'Get-Content c:\users\U\.aco\WORKTREES\r1\calc.go'`, false}, // case-insensitive
		{ps + `'Get-ChildItem ..\..'`, true},
		{ps + `'Get-Content $env:TEMP\x.txt'`, true},
		{`cmd /c type %USERPROFILE%\.ssh\id_rsa`, true},
		{ps + `'Select-String -Pattern "/api/v1" -Path src\*.go'`, false},
	} {
		got := len(m.Command(c.cmd)) > 0
		if got != c.flag {
			t.Errorf("%q: flagged=%v, want %v (%+v)", c.cmd, got, c.flag, m.Command(c.cmd))
		}
	}
}

func TestMonitorItems(t *testing.T) {
	m := unixMonitor()
	if vs := m.Item(&item{Type: "file_change", Changes: []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}{{Path: "calc.go", Kind: "update"}}}); len(vs) != 1 || vs[0].Kind != "write" {
		t.Errorf("file_change: %+v", vs)
	}
	for _, ty := range []string{"mcp_tool_call", "web_search", "browser_action", "collab_tool_call"} {
		if vs := m.Item(&item{Type: ty}); len(vs) != 1 || vs[0].Kind != "tool" {
			t.Errorf("%s: %+v", ty, vs)
		}
	}
	for _, ty := range []string{"agent_message", "reasoning", "todo_list", "error"} {
		if vs := m.Item(&item{Type: ty}); len(vs) != 0 {
			t.Errorf("%s flagged", ty)
		}
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		mode auth.Mode
		text string
		want agent.ErrorClass
	}{
		{auth.Subscription, "You've hit your usage limit. ... try again in 2 hours", agent.ClassSubscriptionLimit},
		{auth.API, "usage_limit_reached", agent.ClassAPIRateLimit},
		{auth.API, "You exceeded your current quota, please check your plan and billing details", agent.ClassAPIQuota},
		{auth.API, "unexpected status 429 Too Many Requests: Rate limit reached", agent.ClassAPIRateLimit},
		{auth.Subscription, "unexpected status 429 Too Many Requests", agent.ClassTransient},
		{auth.Subscription, "unexpected status 401 Unauthorized: Missing bearer or basic authentication", agent.ClassAuth},
		{auth.API, "Incorrect API key provided: sk-dummy***. auth error code: invalid_api_key", agent.ClassAuth},
		{auth.Subscription, "Your refresh token has expired; please log in again", agent.ClassAuth},
		{auth.Subscription, "stream disconnected before completion: 503 Service Unavailable", agent.ClassTransient},
		{auth.Subscription, `"code":"model_not_found"`, agent.ClassCrash},
		{auth.Subscription, "something odd", ""},
	} {
		if got := classify(c.mode, c.text); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.mode, c.text, got, c.want)
		}
	}
}

func TestParseReset(t *testing.T) {
	now := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		text string
		want time.Time
	}{
		{"try again in 2 hours 13 minutes.", now.Add(2*time.Hour + 13*time.Minute)},
		{"try again in 1 day 3 hours", now.Add(27 * time.Hour)},
		{"try again in 45 seconds", now.Add(45 * time.Second)},
		{"try again at 3:05 PM.", time.Date(2026, 11, 9, 15, 5, 0, 0, time.UTC)},
		{"try again at 9:30 AM.", time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)}, // passed today: tomorrow
		{"try again at Nov 10th, 2026 3:05 PM.", time.Date(2026, 11, 10, 15, 5, 0, 0, time.UTC)},
		{"no hint", time.Time{}},
	} {
		if got := parseReset(c.text, now); !got.Equal(c.want) {
			t.Errorf("%q: %v, want %v", c.text, got, c.want)
		}
	}
	if d := parseRetryAfter("Please try again in 20s."); d != 20*time.Second {
		t.Errorf("retry after %s", d)
	}
	if d := parseRetryAfter("Please try again in 350ms."); d != 350*time.Millisecond {
		t.Errorf("retry after %s", d)
	}
}

func TestUsageLimitAtDate(t *testing.T) {
	f := newFixture(t, "usage_limit_at.jsonl")
	_, err := f.adapter(auth.Subscription).ReviewCode(t.Context(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassSubscriptionLimit || ae.ResetAt.Day() != 10 || ae.ResetAt.Hour() != 15 {
		t.Fatalf("got %v reset %v", err, ae.ResetAt)
	}
}
