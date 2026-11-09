package codexreview

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFakeCodex is not a real test. The adapter tests run the test binary
// as a fake `codex`: BinArgs carry "--fake-codex <options> --end-fake",
// followed by the arguments the adapter built. The fake answers the
// detection commands with recorded output, implements `login status` and
// `login --with-api-key` against CODEX_HOME (like the real CLI it stores
// the key in $CODEX_HOME/auth.json), records what `exec` was given, plays a
// recorded JSONL fixture, and writes the last agent message to -o. Like
// the real CLI it writes state into its home ($CODEX_HOME, else
// <home>/.codex), so tests can prove which home was used.
func TestFakeCodex(t *testing.T) {
	start := -1
	for i, a := range os.Args {
		if a == "--fake-codex" {
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
	os.Exit(fakeCodexMain(opts, args))
}

func fakeHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	for _, v := range []string{"USERPROFILE", "HOME"} {
		if h := os.Getenv(v); h != "" {
			return filepath.Join(h, ".codex")
		}
	}
	return ""
}

func fakeCodexMain(opts map[string]string, args []string) int {
	td := opts["testdata"]
	cat := func(name string) int {
		b, _ := os.ReadFile(filepath.Join(td, name))
		os.Stdout.Write(b)
		return 0
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		return cat("codex-version-0.162.0.txt")
	case len(args) == 2 && args[0] == "exec" && args[1] == "--help":
		return cat("codex-exec-help-0.162.0.txt")
	case len(args) == 2 && args[0] == "features":
		return cat("codex-features-0.162.0.txt")
	case len(args) == 2 && args[0] == "login" && args[1] == "status":
		if s := opts["status"]; s != "" {
			os.Stdout.WriteString(s + "\n")
			return 0
		}
		b, err := os.ReadFile(filepath.Join(fakeHome(), "auth.json"))
		if err != nil {
			if os.Getenv("CODEX_HOME") == "" {
				os.Stdout.WriteString("Logged in using ChatGPT\n")
				return 0
			}
			os.Stdout.WriteString("Not logged in\n")
			return 1
		}
		var a struct {
			AuthMode string `json:"auth_mode"`
			Key      string `json:"OPENAI_API_KEY"`
		}
		_ = json.Unmarshal(b, &a)
		if a.AuthMode == "chatgpt" || len(a.Key) < 14 {
			os.Stdout.WriteString("Logged in using ChatGPT\n")
			return 0
		}
		k := a.Key
		os.Stdout.WriteString("Logged in using an API key - " + k[:8] + "***" + k[len(k)-5:] + "\n")
		return 0
	case len(args) == 2 && args[0] == "login" && args[1] == "--with-api-key":
		key, _ := io.ReadAll(os.Stdin)
		home := fakeHome()
		_ = os.MkdirAll(home, 0o700)
		b, _ := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": strings.TrimSpace(string(key))})
		_ = os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600)
		if p := opts["loginlog"]; p != "" {
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			f.WriteString(os.Getenv("CODEX_HOME") + "\n")
			f.Close()
		}
		os.Stdout.WriteString("Reading API key from stdin...\nSuccessfully logged in\n")
		return 0
	}
	if len(args) == 0 || args[0] != "exec" {
		return 3
	}
	// codex exec: which call is this?
	n := 0
	if p := opts["counter"]; p != "" {
		b, _ := os.ReadFile(p)
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		_ = os.WriteFile(p, []byte(strconv.Itoa(n+1)), 0o600)
	}
	suffix := "-" + strconv.Itoa(n)
	if p := opts["dumpargs"]; p != "" {
		b, _ := json.Marshal(args)
		_ = os.WriteFile(p+suffix, b, 0o600)
	}
	if p := opts["dumpenv"]; p != "" {
		_ = os.WriteFile(p+suffix, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	stdin, _ := io.ReadAll(os.Stdin)
	if p := opts["dumpstdin"]; p != "" {
		_ = os.WriteFile(p+suffix, stdin, 0o600)
	}
	// Session state, like the real CLI writes even with --ephemeral.
	if h := fakeHome(); h != "" {
		_ = os.MkdirAll(h, 0o700)
		_ = os.WriteFile(filepath.Join(h, "state_5.sqlite"), []byte(time.Now().String()), 0o600)
	}
	fixtures := strings.Split(opts["fixtures"], ",")
	f := fixtures[min(n, len(fixtures)-1)]
	worktree, outPath := "", ""
	for i, a := range args {
		if a == "-C" && i+1 < len(args) {
			worktree = args[i+1]
		}
		if a == "-o" && i+1 < len(args) {
			outPath = args[i+1]
		}
	}
	last := ""
	if f != "" {
		dir := filepath.Join(td, "stream")
		if d := opts["streamdir"]; d != "" {
			dir = d
		}
		fh, err := os.Open(filepath.Join(dir, f))
		if err != nil {
			return 9
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		w := bufio.NewWriter(os.Stdout)
		for sc.Scan() {
			line := strings.NewReplacer("{{WORKTREE}}", filepath.ToSlash(worktree), "{{ARTIFACTS}}", filepath.ToSlash(opts["artifacts"])).Replace(sc.Text())
			var e struct {
				Item *struct{ Type, Text string } `json:"item"`
				Type string                       `json:"type"`
			}
			if json.Unmarshal([]byte(line), &e) == nil && e.Type == "item.completed" && e.Item != nil && e.Item.Type == "agent_message" {
				last = e.Item.Text
			}
			w.WriteString(line + "\n")
			w.Flush()
		}
		fh.Close()
	}
	if outPath != "" && last != "" {
		_ = os.WriteFile(outPath, []byte(last), 0o600)
	}
	if opts["hang"] != "" {
		time.Sleep(10 * time.Minute)
	}
	if code, err := strconv.Atoi(opts["exit"]); err == nil {
		return code
	}
	return 0
}
