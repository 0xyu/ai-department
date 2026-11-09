package claudecode

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFakeClaude is not a real test. The adapter tests run the test binary
// as a fake `claude`: BinArgs carry "--fake-claude <options> --end-fake",
// followed by the arguments the adapter built. The fake records what it was
// given (arguments, environment, stdin), plays a recorded stream-json
// fixture, and can hang or spawn a grandchild for the timeout and
// cancellation tests. It never needs environment variables, because the
// adapter passes only an allowlisted environment.
func TestFakeClaude(t *testing.T) {
	start := -1
	for i, a := range os.Args {
		if a == "--fake-claude" {
			start = i
			break
		}
	}
	if start < 0 {
		return
	}
	opts := map[string]string{}
	var args []string
	for i := start + 1; i < len(os.Args); i++ {
		if os.Args[i] == "--end-fake" {
			args = os.Args[i+1:]
			break
		}
		k, v, _ := strings.Cut(os.Args[i], "=")
		opts[k] = v
	}
	os.Exit(fakeMain(opts, args))
}

func fakeMain(opts map[string]string, args []string) int {
	if p := opts["sleeper"]; p != "" { // grandchild mode
		time.Sleep(10 * time.Minute)
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		b, _ := os.ReadFile(opts["version"])
		os.Stdout.Write(b)
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		b, _ := os.ReadFile(opts["help"])
		os.Stdout.Write(b)
		return 0
	}
	if p := opts["dumpargs"]; p != "" {
		b, _ := json.Marshal(args)
		_ = os.WriteFile(p, b, 0o600)
	}
	if p := opts["dumpenv"]; p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	stdin, _ := io.ReadAll(os.Stdin)
	if p := opts["dumpstdin"]; p != "" {
		_ = os.WriteFile(p, stdin, 0o600)
	}
	if p := opts["spawn"]; p != "" {
		c := exec.Command(os.Args[0], "-test.run=^TestFakeClaude$", "--", "--fake-claude", "sleeper=1", "--end-fake")
		if c.Start() == nil {
			_ = os.WriteFile(p, []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		}
	}
	if opts["deny"] != "" {
		writeDenial(args, opts["deny"])
	}
	if opts["hookcalls"] != "" {
		runHookCalls(args, opts["hookcalls"], opts["hookout"])
	}
	session := argAfter(args, "--session-id")
	if session == "" {
		session = argAfter(args, "--resume")
	}
	if f := opts["fixture"]; f != "" {
		fh, err := os.Open(f)
		if err != nil {
			return 9
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		w := bufio.NewWriter(os.Stdout)
		for sc.Scan() {
			w.WriteString(strings.ReplaceAll(sc.Text(), "{{SESSION}}", session) + "\n")
			w.Flush()
		}
		fh.Close()
	}
	if opts["hang"] != "" {
		time.Sleep(10 * time.Minute)
	}
	if n, err := strconv.Atoi(opts["exit"]); err == nil {
		return n
	}
	return 0
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

var policyArgRE = regexp.MustCompile(`--policy "([^"]+)"`)

// writeDenial follows the generated settings to the guard policy and
// appends a denial to its deny log, as the real hook would.
func writeDenial(args []string, rule string) {
	b, err := os.ReadFile(argAfter(args, "--settings"))
	if err != nil {
		return
	}
	var s settingsFile
	if json.Unmarshal(b, &s) != nil || len(s.Hooks["PreToolUse"]) == 0 {
		return
	}
	m := policyArgRE.FindStringSubmatch(s.Hooks["PreToolUse"][0].Hooks[0].Command)
	if m == nil {
		return
	}
	pb, err := os.ReadFile(m[1])
	if err != nil {
		return
	}
	var gp struct {
		DenyLog string `json:"deny_log"`
	}
	if json.Unmarshal(pb, &gp) != nil || gp.DenyLog == "" {
		return
	}
	f, err := os.OpenFile(gp.DenyLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]string{"tool": "Bash", "rule": rule, "path": ""})
	f.Write(append(line, '\n'))
}

// runHookCalls runs the PreToolUse hook command from the generated settings
// for each tool call in callsFile (a JSON array of {tool_name, tool_input}),
// the way Claude Code would, and records exit codes and stderr in outFile.
func runHookCalls(args []string, callsFile, outFile string) {
	b, err := os.ReadFile(argAfter(args, "--settings"))
	if err != nil {
		return
	}
	var s settingsFile
	if json.Unmarshal(b, &s) != nil || len(s.Hooks["PreToolUse"]) == 0 {
		return
	}
	argv := splitCommand(s.Hooks["PreToolUse"][0].Hooks[0].Command)
	cb, _ := os.ReadFile(callsFile)
	var calls []map[string]any
	_ = json.Unmarshal(cb, &calls)
	cwd, _ := os.Getwd()
	type result struct {
		Exit   int    `json:"exit"`
		Stderr string `json:"stderr"`
	}
	var results []result
	for _, c := range calls {
		c["hook_event_name"], c["cwd"], c["session_id"] = "PreToolUse", cwd, "s"
		in, _ := json.Marshal(c)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(string(in))
		var se strings.Builder
		cmd.Stderr = &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		results = append(results, result{Exit: code, Stderr: se.String()})
	}
	out, _ := json.Marshal(results)
	_ = os.WriteFile(outFile, out, 0o600)
}

// splitCommand splits a hook command written by HookCommand: words, and
// double-quoted strings with backslash escapes.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQ && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '"':
			inQ, have = !inQ, true
		case c == ' ' && !inQ:
			if have || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if have || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
