package codexreview

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/0xyu/aco/internal/agent"
)

// Monitor flags reviewer activity outside its confinement (§16.3). Codex's
// read-only sandbox stops writes and network access but, in host mode, not
// reads outside the worktree. The monitor inspects every command the
// reviewer runs and flags a path that leaves the allowed roots (the
// worktree and the run's artifact directory): one in the home directory
// (always: ~/.ssh, ~/.codex, ~/.aco, …), or any other existing path. It
// also flags file changes and tool calls a review never needs (MCP, web
// search, browser).
//
// It is a heuristic over command text, not a sandbox: container mode is
// the isolation boundary (Phase 12). Unparseable references to variables
// that hold paths are flagged, so the heuristic fails closed.
type Monitor struct {
	Roots []string // allowed roots, absolute
	Home  string   // the user's home directory ("" = unknown)
	// Exists reports whether a path exists; nil = os.Lstat.
	Exists func(string) bool
	// EvalSymlinks resolves the links and junctions of a path; nil =
	// resolveLinks (filepath.EvalSymlinks does not follow junctions).
	EvalSymlinks func(string) (string, error)
	// Identity reports whether p lies inside root by filesystem identity;
	// nil = identityWithin. Tests on foreign-OS rules switch it off.
	Identity func(root, p string) bool
	// GOOS selects path rules; "" = runtime.GOOS.
	GOOS string
}

// shells whose -c / -Command argument is itself a script.
var shells = map[string][]string{
	"bash": {"-c", "-lc", "-ic"}, "sh": {"-c", "-lc"}, "zsh": {"-c", "-lc"}, "dash": {"-c"},
	"powershell": {"-command", "-c"}, "pwsh": {"-command", "-c"}, "cmd": {"/c", "/k"},
}

// cdCommands change the working directory of the rest of the script.
var cdCommands = map[string]bool{"cd": true, "set-location": true, "sl": true, "pushd": true, "chdir": true, "push-location": true}

var (
	reDrive   = regexp.MustCompile(`^[A-Za-z]:([\\/]|$)`)
	reVar     = regexp.MustCompile(`^(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\$env:[A-Za-z_][A-Za-z0-9_()]*|%[A-Za-z_][A-Za-z0-9_()]*%)`)
	homeVars  = map[string]bool{"$home": true, "${home}": true, "$env:userprofile": true, "$env:home": true, "%userprofile%": true, "%home%": true, "$userprofile": true}
	safeItems = map[string]bool{"agent_message": true, "reasoning": true, "command_execution": true, "error": true, "todo_list": true, "plan": true}
)

func (m *Monitor) goos() string {
	if m.GOOS != "" {
		return m.GOOS
	}
	return runtime.GOOS
}

func (m *Monitor) exists(p string) bool {
	if m.Exists != nil {
		return m.Exists(p)
	}
	_, err := os.Lstat(p)
	return err == nil
}

func (m *Monitor) eval(p string) (string, error) {
	if m.EvalSymlinks != nil {
		return m.EvalSymlinks(p)
	}
	return resolveLinks(p)
}

func (m *Monitor) identity(root, p string) bool {
	if m.Identity != nil {
		return m.Identity(root, p)
	}
	if m.goos() != runtime.GOOS {
		return false
	}
	return identityWithin(root, p)
}

// insideResolved reports whether the resolved target of an existing path
// is inside a root: by name against the roots as given and as resolved,
// or by filesystem identity.
func (m *Monitor) insideResolved(real string) bool {
	if m.inside(real) {
		return true
	}
	for _, r := range m.Roots {
		if rr, err := m.eval(r); err == nil && m.within(rr, real) {
			return true
		}
		if m.identity(r, real) {
			return true
		}
	}
	return false
}

// escapes reports whether an existing path inside the roots by name
// resolves (through a symlink or junction in any component) to a target
// outside them. Unresolvable links fail closed.
func (m *Monitor) escapes(p string) bool {
	if !m.exists(p) {
		return false
	}
	real, err := m.eval(p)
	if err != nil {
		return true
	}
	return !m.insideResolved(real)
}

// Item checks one stream item (item.started or item.completed).
func (m *Monitor) Item(it *item) []agent.AccessViolation {
	if it == nil {
		return nil
	}
	switch {
	case it.Type == "command_execution":
		return m.Command(it.Command)
	case it.Type == "file_change":
		var out []agent.AccessViolation
		for _, c := range it.Changes {
			out = append(out, agent.AccessViolation{Kind: "write", Path: c.Path})
		}
		if len(out) == 0 {
			out = append(out, agent.AccessViolation{Kind: "write"})
		}
		return out
	case safeItems[it.Type]:
		return nil
	}
	t := strings.ToLower(it.Type)
	for _, k := range []string{"tool", "web", "browser", "computer", "image", "patch", "mcp", "agent", "collab"} {
		if strings.Contains(t, k) {
			return []agent.AccessViolation{{Kind: "tool", Command: it.Type + " " + it.Server + " " + it.Tool + it.Query}}
		}
	}
	return nil
}

// Command checks one command line the reviewer ran (cwd = the worktree,
// Roots[0]).
func (m *Monitor) Command(command string) []agent.AccessViolation {
	cwd := ""
	if len(m.Roots) > 0 {
		cwd = m.Roots[0]
	}
	var out []agent.AccessViolation
	seen := map[string]bool{}
	m.script(command, cwd, 0, func(path string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, agent.AccessViolation{Kind: "read", Path: path, Command: shorten(command)})
		}
	})
	return out
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// script analyses a command line: segments split by shell operators, the
// first word of a segment is the program, the rest are arguments.
func (m *Monitor) script(text, cwd string, depth int, flag func(string)) {
	toks := tokenize(text)
	var seg []string
	flush := func() {
		if len(seg) > 0 {
			cwd = m.segment(seg, cwd, depth, flag)
		}
		seg = seg[:0]
	}
	for _, t := range toks {
		if t.op {
			flush()
			continue
		}
		seg = append(seg, t.s)
	}
	flush()
}

func progName(s string) string {
	b := strings.ToLower(filepath.Base(strings.ReplaceAll(s, `\`, "/")))
	return strings.TrimSuffix(b, ".exe")
}

// segment checks one simple command and returns the working directory for
// the next one.
func (m *Monitor) segment(words []string, cwd string, depth int, flag func(string)) string {
	i := 0
	for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") && !strings.ContainsAny(words[i], `/\`) {
		i++ // VAR=value prefixes
	}
	if i >= len(words) {
		return cwd
	}
	prog := progName(words[i])
	args := words[i+1:]
	// A shell's script argument is analysed as a script.
	if opts, ok := shells[prog]; ok && depth < 3 {
		for j, a := range args {
			for _, o := range opts {
				if strings.EqualFold(a, o) && j+1 < len(args) {
					script := args[j+1]
					if prog == "cmd" {
						script = strings.Join(args[j+1:], " ") // cmd /c takes the rest of the line
					}
					m.script(script, cwd, depth+1, flag)
					return cwd
				}
			}
		}
	}
	// The program itself may be a path (./script.sh, ../x, /usr/bin/cat):
	// only relative escapes matter, absolute tool paths are not reads.
	if strings.Contains(words[i], "..") {
		m.check(words[i], cwd, flag)
	}
	next := cwd
	for _, a := range args {
		for _, cand := range candidates(a) {
			p, ok := m.check(cand, cwd, flag)
			if cdCommands[prog] && ok && p != "" {
				next = p
			}
		}
	}
	return next
}

// candidates returns the path-like parts of an argument: the argument, the
// value of --flag=value or -Flag:value, and a redirection target.
func candidates(a string) []string {
	a = strings.TrimLeft(a, "<>&12")
	if a == "" {
		return nil
	}
	if strings.HasPrefix(a, "-") {
		if _, v, ok := strings.Cut(a, "="); ok {
			return []string{v}
		}
		if i := strings.Index(a, ":"); i > 1 && !reDrive.MatchString(a[1:]) {
			return []string{a[i+1:]}
		}
		return nil
	}
	return []string{a}
}

// check resolves a candidate and flags it when it leaves the roots. It
// returns the resolved absolute path when the candidate is path-like.
func (m *Monitor) check(c, cwd string, flag func(string)) (string, bool) {
	c = strings.Trim(c, `'"`)
	if c == "" || strings.Contains(c, "://") {
		return "", false
	}
	low := strings.ToLower(c)
	expanded := c
	sensitive := false
	switch {
	case c == "~" || strings.HasPrefix(c, "~/") || strings.HasPrefix(c, `~\`):
		expanded, sensitive = m.Home+c[1:], true
	case reVar.MatchString(c):
		v := reVar.FindString(c)
		rest := c[len(v):]
		if homeVars[strings.ToLower(v)] {
			expanded, sensitive = m.Home+rest, true
		} else if strings.ContainsAny(rest, `/\`) || strings.HasPrefix(low, "$(") {
			flag(c) // a path built from a variable cannot be checked
			return "", false
		} else {
			return "", false
		}
	case strings.HasPrefix(c, "$(") || strings.HasPrefix(c, "`"):
		if strings.ContainsAny(c, `/\`) {
			flag(c)
		}
		return "", false
	}
	if !isPathLike(expanded) {
		// A plain relative operand: resolved against the tracked working
		// directory, it may still reach out through a symlink or junction.
		if cwd == "" || strings.ContainsAny(expanded, "$%`") {
			return "", false
		}
		p := m.resolve(expanded, cwd)
		if p == "" {
			return "", false
		}
		if m.inside(p) && m.escapes(p) {
			flag(c)
		}
		return p, true
	}
	p := m.resolve(expanded, cwd)
	if p == "" {
		return "", false
	}
	if m.inside(p) {
		// A link inside the roots may still point out of them.
		if m.escapes(p) {
			flag(c)
		}
		return p, true
	}
	if sensitive || m.underHome(p) || m.exists(p) {
		flag(c)
	}
	return p, true
}

// isPathLike: absolute, drive-letter, home-relative, or containing a ".."
// segment. Plain relative names stay below the working directory.
func isPathLike(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || reDrive.MatchString(s) {
		return true
	}
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func (m *Monitor) resolve(s, cwd string) string {
	win := m.goos() == "windows"
	s = strings.TrimRight(s, "*?")
	if win {
		s = strings.ReplaceAll(s, "/", `\`)
		if strings.HasPrefix(s, `\`) && !strings.HasPrefix(s, `\\`) {
			s = filepath.VolumeName(cwd) + s // \etc\passwd on the current drive
		}
	}
	if !m.isAbs(s) {
		if cwd == "" {
			return ""
		}
		s = cwd + string(m.sep()) + s
	}
	return m.clean(s)
}

func (m *Monitor) sep() byte {
	if m.goos() == "windows" {
		return '\\'
	}
	return '/'
}

// isAbs: a drive-letter path counts as absolute on every OS (fail closed:
// it is never meant relative to the worktree).
func (m *Monitor) isAbs(s string) bool {
	if m.goos() == "windows" {
		return reDrive.MatchString(s) || strings.HasPrefix(s, `\\`)
	}
	return strings.HasPrefix(s, "/") || reDrive.MatchString(s)
}

// clean is filepath.Clean for the monitor's OS (tests run the rules of
// either OS on any host).
func (m *Monitor) clean(s string) string {
	if m.goos() == runtime.GOOS {
		return filepath.Clean(s)
	}
	sep := string(m.sep())
	prefix := ""
	if m.goos() == "windows" && reDrive.MatchString(s) {
		prefix, s = s[:2], s[2:]
	}
	var out []string
	for _, seg := range strings.Split(s, sep) {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return prefix + sep + strings.Join(out, sep)
}

func (m *Monitor) within(root, p string) bool {
	if root == "" {
		return false
	}
	root, p = m.clean(root), m.clean(p)
	if m.goos() == "windows" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, string(m.sep()))+string(m.sep()))
}

func (m *Monitor) inside(p string) bool {
	for _, r := range m.Roots {
		if m.within(r, p) {
			return true
		}
	}
	return false
}

func (m *Monitor) underHome(p string) bool { return m.Home != "" && m.within(m.Home, p) }

type token struct {
	s  string
	op bool
}

// tokenize splits a command line into words and operators (; | & && ||
// newline, parentheses, braces). Quotes group; a backslash is literal
// (Windows paths), except \" inside double quotes. Quote characters are
// kept off the words.
func tokenize(s string) []token {
	var out []token
	var cur strings.Builder
	has := false
	emit := func() {
		if has {
			out = append(out, token{s: cur.String()})
		}
		cur.Reset()
		has = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
			} else {
				cur.WriteString(s[i+1 : i+1+j])
				i += j + 1
			}
			has = true
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && s[i+1] == '"' {
					i++
				}
				cur.WriteByte(s[i])
				i++
			}
			has = true
		case c == ' ' || c == '\t' || c == '\r':
			emit()
		case c == ';' || c == '|' || c == '&' || c == '\n' || c == '(' || c == ')' || c == '{' || c == '}':
			if (c == '(' || c == '{') && cur.Len() > 0 && strings.HasSuffix(cur.String(), "$") {
				cur.WriteByte(c) // $( and ${ stay in the word
				has = true
				continue
			}
			if c == '}' && strings.Contains(cur.String(), "${") && strings.Count(cur.String(), "{") > strings.Count(cur.String(), "}") {
				cur.WriteByte(c)
				has = true
				continue
			}
			if c == '&' && cur.Len() > 0 && (strings.HasSuffix(cur.String(), ">") || strings.HasSuffix(cur.String(), "<")) {
				cur.WriteByte(c) // 2>&1
				has = true
				continue
			}
			emit()
			out = append(out, token{op: true})
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	emit()
	return out
}
