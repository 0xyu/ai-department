// Package gitops is the orchestrator's Git manager (§15). Only ACO code touches
// Git: it creates the run worktree and branch, snapshots the working state into
// shadow refs, computes tree hashes and diffs, and commits exactly the reviewed
// tree. Agents never run Git themselves.
//
// Every call runs the git CLI with a timeout, with terminal prompts disabled,
// with the repository's hooks, fsmonitor, and every configured clean/smudge/
// process filter turned off, and with secret environment variables removed.
// The user's checkout, index, and base branch are never written: work
// happens in separate ACO-owned worktrees (checked before every commit,
// reset, or removal), and tree hashing uses a temporary index file.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/secure"
)

// DefaultTimeout bounds a single git invocation.
const DefaultTimeout = 2 * time.Minute

// maxOutput bounds what one git invocation may write to stdout.
const maxOutput = 256 << 20

// ErrTimeout reports a git invocation that exceeded its timeout.
var ErrTimeout = errors.New("git: timed out")

// ErrOutputTooLarge reports a git invocation whose output exceeded maxOutput.
var ErrOutputTooLarge = errors.New("git: output too large")

// Error is a failed git invocation. Op is the git subcommand only (never its
// arguments, which may hold paths or messages); Stderr is masked and truncated.
type Error struct {
	Op       string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	msg := "git " + e.Op + ": "
	if e.Err != nil && !errors.Is(e.Err, errExit) {
		msg += e.Err.Error()
	} else {
		msg += fmt.Sprintf("exit status %d", e.ExitCode)
	}
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

var errExit = errors.New("exit")

// Identity is a commit author and committer. Empty fields use the repository's
// own configuration.
type Identity struct {
	Name  string
	Email string
}

// checkpointIdentity signs checkpoint commits, which are internal and never
// reach the run branch, so they work even where no user identity is set.
var checkpointIdentity = Identity{Name: "ACO Checkpoint", Email: "aco@localhost"}

// Options configure a Repo.
type Options struct {
	Bin      string           // git binary; default "git"
	Timeout  time.Duration    // per call; default DefaultTimeout
	Identity Identity         // author/committer for run-branch commits
	Redactor *secure.Redactor // masks stderr in errors and every commit message
	// Env adds variables to every git call (tests use it to isolate config).
	Env []string
}

// Repo runs git in one directory (a repository root or a worktree).
//
// A Repo from Open is the user's side: it may read, create and remove ACO
// worktrees, and write shadow refs, tags, and notes, but it never commits
// or resets. Only a Repo for an ACO-owned worktree (from AddWorktree,
// AddDetachedWorktree, or OpenWorktree) may change files, the index, or a
// branch, and each such call re-checks ownership first (see assertOwned).
type Repo struct {
	dir  string
	opts Options

	role      role
	commonDir string // canonical common git dir, recorded when the worktree was created or opened
	branch    string // run worktree: the full ref of the run branch
}

type role int

const (
	roleUser   role = iota // opened with Open: no commits, no resets
	roleRun                // ACO run worktree on the run branch
	roleVerify             // ACO detached Stage 4 verification worktree
)

// Open returns a Repo for dir. dir must exist; it is not checked to be a
// repository here (see Preflight).
func Open(dir string, opts Options) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("gitops: %s is not a directory", abs)
	}
	if opts.Bin == "" {
		opts.Bin = "git"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Redactor == nil {
		opts.Redactor = secure.NewRedactor()
	}
	return &Repo{dir: abs, opts: opts}, nil
}

// Dir is the directory the Repo runs in.
func (r *Repo) Dir() string { return r.dir }

// Branch is the full ref of the run branch for a run worktree, else "".
func (r *Repo) Branch() string { return r.branch }

// at returns a user-role Repo for another directory with the same options.
func (r *Repo) at(dir string) *Repo { return &Repo{dir: dir, opts: r.opts} }

// call describes one git invocation.
type call struct {
	args   []string
	stdin  io.Reader
	env    []string // extra variables (GIT_INDEX_FILE, identity)
	tokens bool     // keep GitHub tokens in the environment (push only)
}

// baseConfig is passed with -c on every call. Hooks and fsmonitor are turned
// off so that no repository-controlled program runs inside ACO's own Git
// operations; output is made stable and machine-readable.
func (r *Repo) baseConfig() []string {
	return []string{
		"-c", "core.hooksPath=" + noHooksDir,
		"-c", "core.fsmonitor=false",
		"-c", "core.quotepath=false",
		"-c", "color.ui=false",
		"-c", "diff.noprefix=false",
		"-c", "diff.mnemonicPrefix=false",
		"-c", "advice.detachedHead=false",
		"--no-pager",
	}
}

// noHooksDir is a hooks directory that never exists, so Git runs no hooks.
var noHooksDir = filepath.Join(os.TempDir(), "aco-no-hooks-0f6d3c")

// keptTokenVars may stay in the environment of push, where a credential
// helper may need them. Every other secret variable is removed from git calls.
var keptTokenVars = map[string]bool{"GITHUB_TOKEN": true, "GH_TOKEN": true}

// gitEnv builds the child environment: the process environment without secret
// variables and without variables that redirect Git, plus fixed settings.
func (r *Repo) gitEnv(c call) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		switch {
		case name == "":
			continue
		case secure.IsSecretVar(name) && !(c.tokens && keptTokenVars[upper]):
			continue
		case upper == "GIT_DIR", upper == "GIT_WORK_TREE", upper == "GIT_INDEX_FILE",
			upper == "GIT_OBJECT_DIRECTORY", upper == "GIT_COMMON_DIR", upper == "GIT_NAMESPACE",
			upper == "GIT_CEILING_DIRECTORIES", upper == "GIT_EXTERNAL_DIFF", upper == "GIT_PAGER":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"GIT_EDITOR=:",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
		"LANGUAGE=C",
	)
	env = append(env, r.opts.Env...)
	return append(env, c.env...)
}

// ErrFilterUnavailable reports that a path needs a Git filter marked
// required (for example Git LFS). ACO never runs external filters, so it
// cannot stage or check out that path safely.
var ErrFilterUnavailable = errors.New("gitops: a required Git filter (filter.<driver>.required) applies to a path; ACO does not run external clean/smudge/process filters")

// ErrUnsafeFilterName reports a configured filter driver whose name cannot be
// overridden on the command line.
var ErrUnsafeFilterName = errors.New("gitops: a configured Git filter driver has a name ACO cannot disable safely")

// filterFailRE matches Git's message when a required filter has no command.
var filterFailRE = regexp.MustCompile(`(?:clean|smudge) filter '?[^\s']*'? failed|filter-process`)

// filterOverrides returns -c arguments that empty the clean, smudge, and
// process commands of every configured filter driver, so Git keeps blobs and
// files byte-for-byte instead of running repository-selected programs.
// Drivers marked required then make Git fail, which run reports as
// ErrFilterUnavailable.
func (r *Repo) filterOverrides(ctx context.Context) ([]string, error) {
	b, err := r.run(ctx, call{args: []string{"config", "-z", "--get-regexp", `^filter\.`}})
	if err != nil {
		if exitCode(err) == 1 { // no filter configured
			return nil, nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range splitNUL(b) {
		key, _, _ := strings.Cut(rec, "\n")
		rest, ok := strings.CutPrefix(key, "filter.")
		i := strings.LastIndexByte(rest, '.')
		if !ok || i <= 0 {
			continue
		}
		name := rest[:i]
		if seen[name] {
			continue
		}
		seen[name] = true
		if strings.ContainsAny(name, "=\n\r\x00") {
			return nil, ErrUnsafeFilterName
		}
		for _, v := range []string{"clean", "smudge", "process"} {
			out = append(out, "-c", "filter."+name+"."+v+"=")
		}
	}
	return out, nil
}

// filterScope carries the filter overrides of one directory through the git
// calls of one high-level operation.
type filterScope struct {
	dir string
	ov  []string
}

type filterKey struct{}

// scope reads the filter configuration once and returns a context whose git
// calls in r's directory reuse it. Public operations that run several git
// commands call it first; a lone call outside a scope reads the
// configuration itself. Either way the configuration is read at the start
// of every operation, so a driver added later is still disabled.
func (r *Repo) scope(ctx context.Context) (context.Context, error) {
	if s, ok := ctx.Value(filterKey{}).(filterScope); ok && s.dir == r.dir {
		return ctx, nil
	}
	ov, err := r.filterOverrides(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, filterKey{}, filterScope{dir: r.dir, ov: ov}), nil
}

// run executes git and returns stdout. Every call except the config read
// itself disables all configured filter drivers (see filterOverrides).
func (r *Repo) run(ctx context.Context, c call) ([]byte, error) {
	args := r.baseConfig()
	op := opName(c.args)
	if op != "config" {
		s, ok := ctx.Value(filterKey{}).(filterScope)
		if !ok || s.dir != r.dir {
			ov, err := r.filterOverrides(ctx)
			if err != nil {
				return nil, err
			}
			s = filterScope{dir: r.dir, ov: ov}
		}
		args = append(args, s.ov...)
	}
	args = append(args, c.args...)
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.opts.Bin, args...) // #nosec G204 -- fixed binary; arguments built by this package
	cmd.Dir = r.dir
	cmd.Env = r.gitEnv(c)
	cmd.Stdin = c.stdin
	var stdout limitedBuffer
	stdout.max = maxOutput
	var stderr limitedBuffer
	stderr.max = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &Error{Op: op, ExitCode: -1, Err: ErrTimeout}
	}
	if stdout.over {
		return nil, &Error{Op: op, ExitCode: -1, Err: ErrOutputTooLarge}
	}
	if err != nil {
		e := &Error{Op: op, ExitCode: -1, Stderr: r.cleanStderr(stderr.Bytes())}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			e.ExitCode = ee.ExitCode()
			e.Err = errExit
			if filterFailRE.MatchString(stderr.String()) {
				e.Err = ErrFilterUnavailable
			}
		} else {
			e.Err = err
		}
		return nil, e
	}
	return stdout.Bytes(), nil
}

// out runs git and returns stdout with surrounding whitespace removed.
func (r *Repo) out(ctx context.Context, args ...string) (string, error) {
	b, err := r.run(ctx, call{args: args})
	return strings.TrimSpace(string(b)), err
}

func (r *Repo) cleanStderr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		s = s[:2000] + "…"
	}
	return r.opts.Redactor.String(s)
}

// opName is the git subcommand of args (the first argument that is not a
// global option).
func opName(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" || a == "-C" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "?"
}

// exitCode returns the exit code of a git error, or -1.
func exitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.ExitCode
	}
	return -1
}

type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		room := b.max - b.Len()
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
