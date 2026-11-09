package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Guard (policy part 2): the decision logic behind `aco hook pre-tool-use`.
// Claude Code calls the hook before every tool use; the hook loads the
// GuardPolicy the orchestrator wrote for that invocation and denies the call
// when it would leave the worktree, touch .git or a protected path, change
// files outside the phase scope (scope_enforcement: block), run a forbidden
// Git command or sudo, reach the network against policy, or pipe a download
// into an interpreter (§16.2). Code decides, not the model; anything the
// guard cannot read reliably is denied.
//
// The guard is a policy check, not a sandbox: a program the agent is allowed
// to run (a test, a build script) can still do anything its OS user can. The
// container profile (Phase 12) is the isolation boundary.

// GuardVersion is the version of the GuardPolicy file format.
const GuardVersion = 1

// Guard profiles.
const (
	ProfilePlanner     = "planner"
	ProfileImplementer = "implementer"
)

// Network modes (config policy.network).
const (
	NetworkDeny      = "deny"
	NetworkAllowlist = "allowlist"
	NetworkAllow     = "allow"
)

// Guard rule names (stable; recorded in the deny log and events).
const (
	RuleOutsideWorktree = "outside_worktree"
	RuleWorktreeRoot    = "worktree_root"
	RuleGitDir          = "git_dir"
	RuleProtectedPath   = "protected_path"
	RuleScope           = "scope_violation"
	RuleReadOnly        = "read_only"
	RuleForbiddenGit    = "forbidden_git"
	RulePrivilege       = "privilege"
	RuleNetwork         = "network"
	RuleDownloadExec    = "download_exec"
	RuleEval            = "eval"
	RuleHomeReference   = "home_reference"
	RuleUnknownTool     = "unknown_tool"
	RuleUnparsable      = "unparsable_command"
	RuleBadInput        = "bad_input"
)

// GuardPolicy is what the hook enforces for one agent invocation. The
// orchestrator writes it as JSON into the run directory, outside the
// worktree, so the agent cannot change it.
type GuardPolicy struct {
	Version          int      `json:"version"`
	RunID            string   `json:"run_id"`
	Profile          string   `json:"profile"` // planner | implementer
	Root             string   `json:"root"`    // absolute worktree (or repository) path
	ProtectedPaths   []string `json:"protected_paths"`
	ScopePaths       []string `json:"scope_paths,omitempty"`
	ScopeEnforcement string   `json:"scope_enforcement"` // flag | block
	Network          string   `json:"network"`           // deny | allowlist | allow
	NetworkAllow     []string `json:"network_allow,omitempty"`
	// DenyLog is a file the hook appends one JSON line to per denied call.
	DenyLog string `json:"deny_log,omitempty"`

	rootReal string
}

// Validate checks the policy and resolves the root.
func (g *GuardPolicy) Validate() error {
	if g.Version != GuardVersion {
		return fmt.Errorf("guard policy: unsupported version %d", g.Version)
	}
	if g.Profile != ProfilePlanner && g.Profile != ProfileImplementer {
		return fmt.Errorf("guard policy: unknown profile %q", g.Profile)
	}
	switch g.Network {
	case NetworkDeny, NetworkAllowlist, NetworkAllow:
	default:
		return fmt.Errorf("guard policy: unknown network mode %q", g.Network)
	}
	if !filepath.IsAbs(g.Root) {
		return errors.New("guard policy: root must be an absolute path")
	}
	real, err := filepath.EvalSymlinks(g.Root)
	if err != nil {
		return fmt.Errorf("guard policy: root: %w", err)
	}
	g.rootReal = filepath.Clean(real)
	return nil
}

// ToolCall is one tool use as Claude Code reports it to the hook.
type ToolCall struct {
	Tool  string
	Input map[string]any
	Cwd   string
}

// Decision is the guard's verdict.
type Decision struct {
	Allow  bool   `json:"allow"`
	Tool   string `json:"tool"`
	Rule   string `json:"rule,omitempty"`
	Path   string `json:"path,omitempty"` // worktree-relative where known
	Reason string `json:"reason,omitempty"`
}

func allow(tool string) Decision { return Decision{Allow: true, Tool: tool} }

func deny(tool, rule, p, reason string) Decision {
	return Decision{Tool: tool, Rule: rule, Path: p, Reason: reason}
}

var (
	readTools  = map[string]string{"Read": "file_path", "NotebookRead": "notebook_path", "LS": "path", "Glob": "path", "Grep": "path"}
	writeTools = map[string]string{"Write": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}
	// Tools with no file or process access of their own.
	inertTools = map[string]bool{
		"TodoWrite": true, "StructuredOutput": true,
		"BashOutput": true, "KillShell": true, "KillBash": true, "TaskOutput": true, "TaskStop": true,
	}
)

// Evaluate decides one tool call. Unknown tools (MCP tools, sub-agents,
// PowerShell, ...) are denied.
func (g *GuardPolicy) Evaluate(call ToolCall) Decision {
	if g.rootReal == "" {
		if err := g.Validate(); err != nil {
			return deny(call.Tool, RuleBadInput, "", "ACO guard: invalid policy")
		}
	}
	cwd := call.Cwd
	if cwd == "" {
		cwd = g.rootReal
	}
	if _, inside := g.resolve(cwd, g.rootReal); !inside {
		return deny(call.Tool, RuleOutsideWorktree, "", "ACO guard: the working directory is outside the worktree")
	}
	t := call.Tool
	switch {
	case inertTools[t]:
		return allow(t)
	case readTools[t] != "":
		return g.readTool(call, cwd)
	case writeTools[t] != "":
		if g.Profile == ProfilePlanner {
			return deny(t, RuleReadOnly, "", "ACO guard: the planner is read-only; return the plan package as structured output")
		}
		p, _ := call.Input[writeTools[t]].(string)
		if p == "" {
			return deny(t, RuleBadInput, "", "ACO guard: missing file path")
		}
		return g.checkWrite(t, p, cwd)
	case t == "Bash":
		if g.Profile == ProfilePlanner {
			return deny(t, RuleReadOnly, "", "ACO guard: the planner may not run commands")
		}
		cmd, _ := call.Input["command"].(string)
		if strings.TrimSpace(cmd) == "" {
			return deny(t, RuleBadInput, "", "ACO guard: empty command")
		}
		return g.checkShell(cmd, cwd, 0)
	case t == "WebSearch":
		if g.Network == NetworkAllow {
			return allow(t)
		}
		return deny(t, RuleNetwork, "", "ACO guard: web search is disabled by policy.network")
	case t == "WebFetch":
		raw, _ := call.Input["url"].(string)
		if g.Network == NetworkAllow || (g.Network == NetworkAllowlist && g.hostAllowed(raw)) {
			return allow(t)
		}
		return deny(t, RuleNetwork, "", "ACO guard: network access to this host is disabled by policy.network")
	}
	return deny(t, RuleUnknownTool, "", "ACO guard: tool "+t+" is not allowed")
}

func (g *GuardPolicy) hostAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, a := range g.NetworkAllow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (h == a || strings.HasSuffix(h, "."+a)) {
			return true
		}
	}
	return false
}

func (g *GuardPolicy) readTool(call ToolCall, cwd string) Decision {
	t := call.Tool
	key := readTools[t]
	p, _ := call.Input[key].(string)
	if p == "" {
		if key != "path" { // Glob/Grep/LS default to cwd
			return deny(t, RuleBadInput, "", "ACO guard: missing file path")
		}
		p = cwd
	}
	if d := g.checkRead(t, p, cwd); !d.Allow {
		return d
	}
	// Glob and Grep patterns may carry their own absolute or parent paths.
	for _, k := range []string{"pattern", "glob"} {
		if t == "Grep" && k == "pattern" {
			continue // a regular expression, not a path
		}
		if pat, _ := call.Input[k].(string); pat != "" && pathLike(pat) {
			if d := g.checkRead(t, staticPrefix(pat), cwd); !d.Allow {
				return d
			}
		}
	}
	return allow(t)
}

// staticPrefix is the part of a glob before its first metacharacter.
func staticPrefix(pat string) string {
	i := strings.IndexAny(pat, "*?[{")
	if i < 0 {
		return pat
	}
	return pat[:i]
}

func (g *GuardPolicy) checkRead(tool, p, cwd string) Decision {
	if isHomeRef(p) {
		return deny(tool, RuleHomeReference, "", "ACO guard: paths in the home directory are outside the worktree")
	}
	if isDevice(p) {
		return allow(tool)
	}
	rel, inside := g.resolve(p, cwd)
	if !inside {
		return deny(tool, RuleOutsideWorktree, "", "ACO guard: "+p+" is outside the worktree")
	}
	if inGitDir(rel) {
		return deny(tool, RuleGitDir, rel, "ACO guard: .git is managed by the orchestrator")
	}
	return allow(tool)
}

func (g *GuardPolicy) checkWrite(tool, p, cwd string) Decision {
	if isHomeRef(p) {
		return deny(tool, RuleHomeReference, "", "ACO guard: paths in the home directory are outside the worktree")
	}
	if isDevice(p) {
		return allow(tool)
	}
	rel, inside := g.resolve(p, cwd)
	switch {
	case !inside:
		return deny(tool, RuleOutsideWorktree, "", "ACO guard: "+p+" is outside the worktree")
	case rel == ".":
		return deny(tool, RuleWorktreeRoot, rel, "ACO guard: the worktree root itself may not be removed or replaced")
	case inGitDir(rel):
		return deny(tool, RuleGitDir, rel, "ACO guard: .git is managed by the orchestrator")
	case MatchAny(g.ProtectedPaths, rel, true):
		return deny(tool, RuleProtectedPath, rel, "ACO guard: "+rel+" is a protected path; the implementer may read it but not change it")
	case len(g.ScopePaths) > 0 && g.ScopeEnforcement == "block" && !MatchAny(g.ScopePaths, rel, false):
		return deny(tool, RuleScope, rel, "ACO guard: "+rel+" is outside this phase's scope.paths")
	}
	return allow(tool)
}

var foldPaths = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

var msysDrive = regexp.MustCompile(`^/([A-Za-z])(/|$)`)

// resolve makes p absolute (relative to cwd), resolves symbolic links in its
// longest existing prefix, and returns its slash-separated path relative to
// the root and whether it lies inside the root.
func (g *GuardPolicy) resolve(p, cwd string) (string, bool) {
	if runtime.GOOS == "windows" {
		if m := msysDrive.FindStringSubmatch(p); m != nil { // Git Bash form /c/...
			p = strings.ToUpper(m[1]) + ":/" + p[len(m[0]):]
		} else if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) && !strings.HasPrefix(p, `\\`) {
			return "", false // rooted but driveless: outside the worktree
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	abs := realPath(filepath.Clean(p))
	root := g.rootReal
	a, r := abs, root
	if foldPaths {
		a, r = strings.ToLower(a), strings.ToLower(r)
	}
	if a == r {
		return ".", true
	}
	if !strings.HasPrefix(a, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator)) {
		return "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// realPath resolves symbolic links in the longest existing prefix of p.
func realPath(p string) string {
	rest := ""
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return filepath.Clean(real)
			}
			return filepath.Clean(filepath.Join(real, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		base := filepath.Base(cur)
		if rest == "" {
			rest = base
		} else {
			rest = filepath.Join(base, rest)
		}
		cur = parent
	}
}

func inGitDir(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

var homeRefRE = regexp.MustCompile(`(?i)^(~|\$home\b|\$\{home\}|\$userprofile\b|\$\{userprofile\}|%userprofile%|%appdata%|%localappdata%|\$env:)`)

func isHomeRef(p string) bool { return homeRefRE.MatchString(p) }

func isDevice(p string) bool {
	switch strings.ToLower(p) {
	case "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/stdout", "/dev/stderr", "/dev/stdin", "/dev/tty", "nul", "nul:", "con":
		return true
	}
	return false
}

var winAbsRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// pathLike reports whether a command argument should be checked as a path:
// it is absolute, refers to the home directory, or climbs with "..".
func pathLike(s string) bool {
	if s == "" || strings.Contains(s, "://") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || winAbsRE.MatchString(s) || isHomeRef(s) {
		return true
	}
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// cmdName strips a directory and a Windows executable suffix from a command name.
func cmdName(w string) string {
	w = strings.ReplaceAll(w, `\`, "/")
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		w = w[i+1:]
	}
	w = strings.ToLower(w)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		w = strings.TrimSuffix(w, ext)
	}
	return w
}

var (
	privilegeCmds = map[string]bool{"sudo": true, "su": true, "doas": true, "runas": true, "pkexec": true, "gsudo": true}
	networkCmds   = map[string]bool{
		"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "telnet": true, "ssh": true, "scp": true,
		"sftp": true, "rsync": true, "ftp": true, "tftp": true, "socat": true, "aria2c": true, "lynx": true,
		"invoke-webrequest": true, "iwr": true, "invoke-restmethod": true, "irm": true, "certutil": true, "bitsadmin": true,
	}
	interpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "python": true, "python3": true,
		"node": true, "perl": true, "ruby": true, "php": true, "pwsh": true, "powershell": true, "iex": true, "invoke-expression": true,
	}
	shells      = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}
	opaqueShell = map[string]bool{"cmd": true, "pwsh": true, "powershell": true, "fish": true}
	// Commands whose path arguments are written (all of them, or see below).
	mutating = map[string]bool{
		"rm": true, "rmdir": true, "mv": true, "ln": true, "touch": true, "chmod": true, "chown": true, "chgrp": true,
		"truncate": true, "tee": true, "mkdir": true, "install": true, "shred": true, "unlink": true, "del": true,
		"rd": true, "erase": true, "move": true, "ren": true, "rename": true,
	}
	// Prefix commands that run the rest of the line as a command.
	wrappers = map[string]bool{
		"command": true, "builtin": true, "exec": true, "nohup": true, "time": true, "nice": true,
		"timeout": true, "stdbuf": true, "ionice": true, "setsid": true, "unbuffer": true, "xargs": true, "env": true,
		"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "{": true, "}": true,
	}
	// Git subcommands the implementer may run: read-only ones.
	gitAllowed = map[string]bool{
		"status": true, "diff": true, "log": true, "show": true, "blame": true, "grep": true, "ls-files": true,
		"ls-tree": true, "rev-parse": true, "describe": true, "shortlog": true, "cat-file": true, "help": true,
		"version": true, "merge-base": true, "name-rev": true, "check-ignore": true, "check-attr": true,
		"rev-list": true, "show-ref": true, "whatchanged": true, "diff-tree": true, "diff-files": true, "diff-index": true,
		"count-objects": true, "for-each-ref": true,
	}
	// Options that make an allowed git subcommand write files or run programs.
	gitBadOpts = []string{"--output", "--ext-diff", "--textconv", "-O", "--open-files-in-pager", "--exec"}
	gitDirVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_EXEC_PATH"}
)

var assignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// checkShell decides a Bash command line.
func (g *GuardPolicy) checkShell(src, cwd string, depth int) Decision {
	const tool = "Bash"
	if depth > maxShellDepth {
		return deny(tool, RuleUnparsable, "", "ACO guard: command nesting is too deep")
	}
	sc, err := parseShell(src)
	if err != nil {
		return deny(tool, RuleUnparsable, "", "ACO guard: the command cannot be parsed reliably; use a simpler command")
	}
	for _, sub := range sc.subs {
		if d := g.checkShell(sub, cwd, depth+1); !d.Allow {
			return d
		}
	}
	// Download piped into an interpreter.
	netIn := map[int]bool{}
	for _, c := range sc.cmds {
		words := stripPrefix(c.words)
		if len(words) == 0 {
			continue
		}
		name := cmdName(words[0])
		if networkCmds[name] {
			netIn[c.pipeline] = true
		} else if c.pipePos > 0 && netIn[c.pipeline] && interpreters[name] {
			return deny(tool, RuleDownloadExec, "", "ACO guard: piping a download into an interpreter is not allowed")
		}
	}
	for _, c := range sc.cmds {
		if d := g.checkSimple(c, cwd, depth); !d.Allow {
			return d
		}
	}
	return allow(tool)
}

// stripPrefix drops leading shell keywords, wrappers and their options,
// and environment assignments, leaving the command that actually runs.
func stripPrefix(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case assignRE.MatchString(w):
			words = words[1:]
		case wrappers[cmdName(w)]:
			name := cmdName(w)
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				opt := words[0]
				words = words[1:]
				// Options of nice/timeout/xargs/stdbuf that take a value.
				if (name == "nice" && opt == "-n") || (name == "xargs" && (opt == "-I" || opt == "-n" || opt == "-P" || opt == "-L" || opt == "-d")) ||
					(name == "timeout" && (opt == "-s" || opt == "-k")) {
					if len(words) > 0 {
						words = words[1:]
					}
				}
			}
			if name == "timeout" && len(words) > 0 { // duration
				words = words[1:]
			}
		default:
			return words
		}
	}
	return words
}

func (g *GuardPolicy) checkSimple(c simpleCmd, cwd string, depth int) Decision {
	const tool = "Bash"
	// Assignments: no Git redirection; path values must stay inside.
	for _, w := range c.words {
		if !assignRE.MatchString(w) {
			break
		}
		name, val, _ := strings.Cut(w, "=")
		for _, v := range gitDirVars {
			if strings.EqualFold(name, v) {
				return deny(tool, RuleGitDir, "", "ACO guard: "+name+" may not be set")
			}
		}
		if d := g.checkArgPath(val, cwd); !d.Allow {
			return d
		}
	}
	for _, r := range c.redirects {
		var d Decision
		if r.op == "<" {
			d = g.checkRead(tool, r.target, cwd)
		} else {
			d = g.checkWrite(tool, r.target, cwd)
		}
		if !d.Allow {
			return d
		}
	}
	words := stripPrefix(c.words)
	if len(words) == 0 {
		return allow(tool)
	}
	name := cmdName(words[0])
	args := words[1:]
	switch {
	case privilegeCmds[name]:
		return deny(tool, RulePrivilege, "", "ACO guard: "+name+" is not allowed")
	case name == "eval":
		return deny(tool, RuleEval, "", "ACO guard: eval is not allowed")
	case name == "git":
		return g.checkGit(args, cwd)
	case networkCmds[name] && g.Network != NetworkAllow:
		return deny(tool, RuleNetwork, "", "ACO guard: "+name+" is disabled by policy.network")
	case shells[name]:
		for i, a := range args {
			if a == "-c" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c")) {
				if i+1 >= len(args) {
					return deny(tool, RuleUnparsable, "", "ACO guard: missing script for "+name+" -c")
				}
				return g.checkShell(args[i+1], cwd, depth+1)
			}
		}
	case opaqueShell[name]:
		for _, a := range args {
			la := strings.ToLower(a)
			if la == "/c" || la == "/k" || la == "-c" || la == "-command" || la == "-encodedcommand" || la == "-e" || la == "-ec" {
				return deny(tool, RuleUnparsable, "", "ACO guard: "+name+" scripts cannot be checked; run the command directly")
			}
		}
	case name == "find":
		return g.checkFind(args, cwd, depth)
	case name == "sed" || name == "perl":
		inPlace := false
		for _, a := range args {
			if a == "--in-place" || strings.HasPrefix(a, "--in-place=") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i")) {
				inPlace = true
			}
		}
		if inPlace {
			return g.checkWrites(editedFiles(args), cwd)
		}
	case name == "cp" || name == "copy" || name == "xcopy" || name == "robocopy":
		nf := nonFlags(args)
		if len(nf) > 0 {
			for _, src := range nf[:len(nf)-1] {
				if d := g.checkArgPath(src, cwd); !d.Allow {
					return d
				}
			}
			return g.checkWrites(nf[len(nf)-1:], cwd)
		}
	case name == "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				if d := g.checkWrite(tool, v, cwd); !d.Allow {
					return d
				}
			} else if v, ok := strings.CutPrefix(a, "if="); ok {
				if d := g.checkRead(tool, v, cwd); !d.Allow {
					return d
				}
			}
		}
	case mutating[name]:
		return g.checkWrites(nonFlags(args), cwd)
	}
	for _, a := range args {
		if d := g.checkArgPath(a, cwd); !d.Allow {
			return d
		}
	}
	return allow(tool)
}

// nonFlags returns the arguments that are not options ("--" ends options).
func nonFlags(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--" {
			return append(out, args[i+1:]...)
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// checkArgPath checks one argument (or the value of --opt=value) that looks
// like a path: it must stay inside the worktree and out of .git.
func (g *GuardPolicy) checkArgPath(a, cwd string) Decision {
	if strings.HasPrefix(a, "-") {
		if _, v, ok := strings.Cut(a, "="); ok {
			a = v
		} else {
			return allow("Bash")
		}
	}
	if !pathLike(a) && !strings.HasPrefix(strings.ToLower(a), ".git") && !strings.Contains(strings.ToLower(a), "/.git") {
		return allow("Bash")
	}
	return g.checkRead("Bash", a, cwd)
}

// checkWrites checks write targets, expanding glob patterns in them.
func (g *GuardPolicy) checkWrites(targets []string, cwd string) Decision {
	for _, t := range targets {
		if strings.ContainsAny(t, "*?[") {
			if d := g.checkWrite("Bash", staticPrefixDir(t), cwd); !d.Allow && d.Rule != RuleWorktreeRoot {
				return d
			}
			pat := t
			if !filepath.IsAbs(pat) {
				pat = filepath.Join(cwd, pat)
			}
			matches, _ := filepath.Glob(pat)
			for _, m := range matches {
				if d := g.checkWrite("Bash", m, cwd); !d.Allow {
					return d
				}
			}
			continue
		}
		if d := g.checkWrite("Bash", t, cwd); !d.Allow {
			return d
		}
	}
	return allow("Bash")
}

// staticPrefixDir is the directory part of a glob before its first metacharacter.
func staticPrefixDir(pat string) string {
	p := staticPrefix(pat)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i+1]
	}
	return "."
}

func (g *GuardPolicy) checkGit(args []string, cwd string) Decision {
	const tool = "Bash"
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		switch {
		case a == "-C":
			if i+1 >= len(args) {
				return deny(tool, RuleBadInput, "", "ACO guard: git -C needs a path")
			}
			rel, inside := g.resolve(args[i+1], cwd)
			if !inside || inGitDir(rel) {
				return deny(tool, RuleOutsideWorktree, "", "ACO guard: git -C must stay inside the worktree")
			}
			i += 2
			continue
		case a == "-c" || strings.HasPrefix(a, "--config-env"):
			return deny(tool, RuleForbiddenGit, "", "ACO guard: git -c / --config-env are not allowed")
		case strings.HasPrefix(a, "--git-dir"), strings.HasPrefix(a, "--work-tree"), strings.HasPrefix(a, "--namespace"),
			strings.HasPrefix(a, "--exec-path="), a == "--bare":
			return deny(tool, RuleGitDir, "", "ACO guard: git repository options are not allowed")
		}
		i++
	}
	if i >= len(args) {
		return allow(tool) // "git" or "git --version"
	}
	sub := strings.ToLower(args[i])
	if !gitAllowed[sub] {
		return deny(tool, RuleForbiddenGit, "", "ACO guard: git "+sub+" is reserved for the orchestrator; the implementer may only read history (status, diff, log, show, ...)")
	}
	for _, a := range args[i+1:] {
		for _, bad := range gitBadOpts {
			if a == bad || strings.HasPrefix(a, bad+"=") {
				return deny(tool, RuleForbiddenGit, "", "ACO guard: git option "+bad+" is not allowed")
			}
		}
		if d := g.checkArgPath(a, cwd); !d.Allow {
			return d
		}
	}
	return allow(tool)
}

// checkFind checks find's search paths, -delete, and -exec commands.
func (g *GuardPolicy) checkFind(args []string, cwd string, depth int) Decision {
	var roots []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" {
		roots = append(roots, args[i])
		i++
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	for _, r := range roots {
		if d := g.checkRead("Bash", r, cwd); !d.Allow {
			return d
		}
	}
	for ; i < len(args); i++ {
		switch args[i] {
		case "-delete":
			for _, r := range roots {
				if d := g.checkWrite("Bash", r, cwd); !d.Allow && d.Rule != RuleWorktreeRoot {
					return d
				}
			}
			// Deleting from the worktree root could reach protected paths.
			if len(g.ProtectedPaths) > 0 {
				return deny("Bash", RuleProtectedPath, "", "ACO guard: find -delete is not allowed; delete files by name")
			}
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j] != ";" && args[j] != "+" {
				j++
			}
			var words []string
			placeholder := false
			for _, w := range args[i+1 : j] {
				if w == "{}" {
					placeholder = true
					continue
				}
				words = append(words, w)
			}
			if cmd := stripPrefix(words); placeholder && len(cmd) > 0 && (mutating[cmdName(cmd[0])] || cmdName(cmd[0]) == "sed" || cmdName(cmd[0]) == "perl") && len(g.ProtectedPaths) > 0 {
				return deny("Bash", RuleProtectedPath, "", "ACO guard: changing files found by find is not allowed; change files by name")
			}
			sub := simpleCmd{words: words}
			if d := g.checkSimple(sub, cwd, depth+1); !d.Allow {
				return d
			}
			i = j
		case "-fprint", "-fprintf", "-fls", "-fprint0":
			if i+1 < len(args) {
				if d := g.checkWrite("Bash", args[i+1], cwd); !d.Allow {
					return d
				}
			}
		default:
			if d := g.checkArgPath(args[i], cwd); !d.Allow {
				return d
			}
		}
	}
	return allow("Bash")
}

// ReadGuardPolicy loads and validates a policy file written by WriteGuardPolicy.
func ReadGuardPolicy(path string) (*GuardPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g GuardPolicy
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("guard policy: %w", err)
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return &g, nil
}

// WriteGuardPolicy writes the policy as JSON (0600) for the hook to read.
func WriteGuardPolicy(path string, g *GuardPolicy) error {
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// editedFiles returns the file arguments of "sed -i" / "perl -i": the
// non-option arguments, minus the script when it is not given by -e/-f.
func editedFiles(args []string) []string {
	scriptByOpt := false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			scriptByOpt = true
			i++ // the option's value
		case strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file="):
			scriptByOpt = true
		case strings.HasPrefix(a, "-") && a != "-":
			// Combined short options such as -pi -e or -ne: a trailing e/f takes the next word.
			if !strings.HasPrefix(a, "--") && (strings.HasSuffix(a, "e") || strings.HasSuffix(a, "f")) && len(a) > 2 {
				scriptByOpt = true
				i++
			}
		default:
			files = append(files, a)
		}
	}
	if !scriptByOpt && len(files) > 0 {
		files = files[1:]
	}
	return files
}
