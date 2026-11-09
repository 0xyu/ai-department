package gates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/proc"
	"github.com/0xyu/aco/internal/secure"
)

// MaxCapture bounds what is kept of one command's stdout and of its combined
// log; output beyond it is dropped and the result is marked truncated.
const MaxCapture = 64 << 20

// MaxReportFile bounds one report file read from the reports directory.
const MaxReportFile = 64 << 20

// ErrNoShell is returned when no POSIX shell is available. Gate commands are
// shell command lines (§11.5); on Windows the sh of Git for Windows is used.
var ErrNoShell = errors.New("gates: no POSIX shell (sh) found; on Windows install Git for Windows")

// Options configure a Runner.
type Options struct {
	// Dir is the worktree the gates run in.
	Dir string
	// Repo is the worktree's git repository: it binds results to the tree
	// hash and feeds the security gate's diff policy and secret scan. Nil
	// runs commands only (TreeHash stays empty, built-in scans are skipped).
	Repo *gitops.Repo
	// Base is the tree-ish the security gate scans from: the previous
	// phase's commit (ModePhase) or the run's base commit (ModeFull, for the
	// history secret scan).
	Base string
	// Policy is the diff policy for the security gate (ModePhase).
	Policy policy.Config
	// FailSeverity is security.fail_severity (default high).
	FailSeverity Severity

	// BaseEnv is the parent environment (nil: process environment); it is
	// filtered by auth.GateEnv. ExtraEnv holds fixed non-secret additions.
	BaseEnv  []string
	ExtraEnv []string
	// Shell is the argv prefix a command line is appended to; default
	// DefaultShell.
	Shell []string

	Redactor    *secure.Redactor
	MaxLogBytes int // per command result; default MaxLogBytes

	// Out, when set, keeps every command's full redacted log as an artifact
	// under Prefix.
	Out    *artifacts.RunDir
	Prefix string

	Now func() time.Time
}

// Runner runs gates.
type Runner struct {
	opts Options
	env  []string
}

// New returns a Runner.
func New(opts Options) (*Runner, error) {
	if opts.Dir == "" && opts.Repo != nil {
		opts.Dir = opts.Repo.Dir()
	}
	if opts.Dir == "" {
		return nil, errors.New("gates: Dir is required")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	opts.Dir = abs
	if opts.Redactor == nil {
		opts.Redactor = secure.NewRedactor()
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = MaxLogBytes
	}
	if opts.FailSeverity == "" {
		opts.FailSeverity = SevHigh
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Policy.Redactor == nil {
		opts.Policy.Redactor = opts.Redactor
	}
	if len(opts.Shell) == 0 {
		sh, err := DefaultShell(exec.LookPath)
		if err != nil {
			return nil, err
		}
		opts.Shell = sh
	}
	env, err := auth.GateEnv(opts.BaseEnv, opts.ExtraEnv)
	if err != nil {
		return nil, err
	}
	return &Runner{opts: opts, env: env}, nil
}

// DefaultShell finds sh: on PATH, else (Windows) next to Git for Windows.
func DefaultShell(look LookPathFunc) ([]string, error) {
	if p, err := look("sh"); err == nil {
		return []string{p, "-c"}, nil
	}
	if runtime.GOOS == "windows" {
		if g, err := look("git"); err == nil {
			d := filepath.Dir(g)
			for _, rel := range []string{`..\usr\bin\sh.exe`, `..\..\usr\bin\sh.exe`, `..\bin\sh.exe`} {
				p := filepath.Clean(filepath.Join(d, rel))
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
					return []string{p, "-c"}, nil
				}
			}
		}
	}
	return nil, ErrNoShell
}

// RunOptions control one round.
type RunOptions struct {
	Mode Mode
	// FailFast stops at the first gate that does not pass; later gates are
	// Skipped. Baseline rounds always run every gate.
	FailFast bool
	// Baseline marks a baseline round: results are recorded as they are,
	// without comparison.
	Baseline bool
	// Compare is the baseline to compare with (nil: every failure is new).
	Compare *Baseline
}

// Run runs the gates in order and returns the round. An error is returned
// only when the context is cancelled or the working state cannot be read;
// gate failures are in the round.
func (r *Runner) Run(ctx context.Context, specs []Spec, ro RunOptions) (*Round, error) {
	if ro.Mode == "" {
		ro.Mode = ModePhase
	}
	tree, err := r.treeHash(ctx)
	if err != nil {
		return nil, err
	}
	round := &Round{TreeHash: tree, Mode: ro.Mode, Baseline: ro.Baseline}
	stop := false
	for _, s := range specs {
		if stop && s.Required {
			round.Results = append(round.Results, &Result{Gate: s.Type, Status: Skipped, Required: s.Required,
				Source: s.Source, TreeHash: tree, Summary: "skipped after an earlier gate failed"})
			continue
		}
		res, err := r.RunGate(ctx, s, ro.Mode)
		if err != nil {
			return round, err
		}
		if !ro.Baseline {
			var base *Result
			if ro.Compare != nil {
				base = ro.Compare.Results[s.Type]
			}
			Evaluate(res, base, r.opts.FailSeverity)
		} else {
			Evaluate(res, nil, r.opts.FailSeverity)
			res.Comparison = nil
		}
		round.Results = append(round.Results, res)
		if ro.FailFast && !ro.Baseline && !res.Status.OK() {
			stop = true
		}
	}
	return round, nil
}

func (r *Runner) treeHash(ctx context.Context) (string, error) {
	if r.opts.Repo == nil {
		return "", nil
	}
	return r.opts.Repo.WorkingTree(ctx)
}

// RunGate runs one gate's commands and built-in scans without judging it;
// Evaluate (called by Run) sets its status against a baseline.
func (r *Runner) RunGate(ctx context.Context, s Spec, mode Mode) (*Result, error) {
	start := r.opts.Now()
	tree, err := r.treeHash(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{Gate: s.Type, Required: s.Required, Justification: s.Justification, Source: s.Source, TreeHash: tree}
	if !s.Required {
		res.Status = NotApplicable
		res.Summary = "not applicable: " + r.opts.Redactor.String(s.Justification)
		return res, nil
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// observe records the first tree seen that is not the gate's tree, at
	// every command boundary: a command that changes the tree and a later
	// one that restores it still leave TreeAfter set, so the gate's
	// evidence is never taken as evidence for one tree.
	observe := func() error {
		if res.TreeAfter != "" || r.opts.Repo == nil {
			return nil
		}
		after, err := r.treeHash(ctx)
		if err != nil {
			return err
		}
		if after != tree {
			res.TreeAfter = after
		}
		return nil
	}
	for i, c := range s.Commands {
		cr, parsed, err := r.runCommand(ctx, s.Type, i, c, timeout)
		if err != nil {
			return nil, err
		}
		if err := observe(); err != nil {
			return nil, err
		}
		res.Commands = append(res.Commands, cr)
		res.Tests = append(res.Tests, parsed.Tests...)
		res.Issues = append(res.Issues, parsed.Issues...)
		res.Vulns = append(res.Vulns, parsed.Vulns...)
	}
	if s.Type == Security {
		issues, cr, err := r.builtinScans(ctx, s, mode, tree)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		res.Issues = append(res.Issues, issues...)
		if cr != nil {
			res.Commands = append(res.Commands, *cr)
		}
	}
	if err := observe(); err != nil {
		return nil, err
	}
	res.Duration = r.opts.Now().Sub(start)
	return res, nil
}

// lockedWriter appends to a shared capture from stdout and stderr.
type capture struct {
	mu   sync.Mutex
	f    *os.File
	n    int64
	full bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += int64(len(p))
	if c.full {
		return len(p), nil
	}
	room := MaxCapture - (c.n - int64(len(p)))
	w := p
	if int64(len(w)) > room {
		w = w[:room]
		c.full = true
	}
	if _, err := c.f.Write(w); err != nil {
		c.full = true
	}
	return len(p), nil
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(c.f, MaxCapture))
	return b
}

func newCapture(dir, name string) (*capture, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) // #nosec G304 -- private temp dir
	if err != nil {
		return nil, err
	}
	return &capture{f: f}, nil
}

// quoteShell single-quotes s for sh. Windows paths use forward slashes,
// which Git for Windows' sh and native programs both accept.
func quoteShell(s string) string {
	if runtime.GOOS == "windows" {
		s = filepath.ToSlash(s)
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r *Runner) runCommand(ctx context.Context, gate string, idx int, c Command, timeout time.Duration) (CommandResult, Parsed, error) {
	shown := Command{Run: r.opts.Redactor.String(c.Run), Scanner: c.Scanner}
	cr := CommandResult{Command: shown}
	tmp, err := os.MkdirTemp("", "aco-gate-")
	if err != nil {
		return cr, Parsed{}, fmt.Errorf("gates: %w", err)
	}
	defer os.RemoveAll(tmp)
	reports := filepath.Join(tmp, "reports")
	if err := os.Mkdir(reports, 0o700); err != nil {
		return cr, Parsed{}, fmt.Errorf("gates: %w", err)
	}
	stdout, err := newCapture(tmp, "stdout")
	if err != nil {
		return cr, Parsed{}, fmt.Errorf("gates: %w", err)
	}
	defer stdout.f.Close()
	combined, err := newCapture(tmp, "combined")
	if err != nil {
		return cr, Parsed{}, fmt.Errorf("gates: %w", err)
	}
	defer combined.f.Close()

	// Forward slashes: accepted by Git for Windows' sh and native programs.
	line := strings.ReplaceAll(c.Run, "{reports}", quoteShell(filepath.ToSlash(reports)))
	args := append(append([]string(nil), r.opts.Shell[1:]...), line)
	cmd := exec.Command(r.opts.Shell[0], args...) // #nosec G204 -- gate commands come from the frozen plan, config, or detection
	cmd.Dir = r.opts.Dir
	cmd.Env = append(append([]string(nil), r.env...), "ACO_GATE_REPORTS="+reports)
	cmd.Stdin = nil
	cmd.Stdout = io.MultiWriter(stdout, combined)
	cmd.Stderr = combined
	cmd.WaitDelay = 5 * time.Second

	start := r.opts.Now()
	tree, err := proc.Start(cmd)
	if err != nil {
		cr.Status, cr.Class, cr.ExitCode = Failed, ClassEnvironment, -1
		cr.Reason = "shell could not be started"
		return cr, Parsed{}, nil
	}
	defer tree.Close()
	waitErr := make(chan error, 1)
	go func() { waitErr <- tree.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var werr error
	timedOut, cancelled := false, false
	select {
	case werr = <-waitErr:
		_ = tree.Kill() // descendants left behind by the shell
	case <-timer.C:
		timedOut = true
		_ = tree.Kill()
		werr = <-waitErr
	case <-ctx.Done():
		cancelled = true
		_ = tree.Kill()
		werr = <-waitErr
	}
	cr.Duration = r.opts.Now().Sub(start)
	if cancelled {
		return cr, Parsed{}, ctx.Err()
	}
	cr.ExitCode = 0
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			cr.ExitCode = ee.ExitCode()
		} else {
			cr.ExitCode = -1
		}
	}

	out := stdout.bytes()
	rep := readReports(reports)
	var parsed Parsed
	if c.Scanner != "" {
		parsed = parseScanner(c.Scanner, out, rep, r.opts.Dir)
	} else {
		parsed = parseOutput(c.Run, out, rep, r.opts.Dir)
	}
	parsed = r.redactParsed(parsed)
	cr.Parsed = parsed.OK

	full := combined.bytes()
	cr.LogBytes = combined.n
	if combined.full || stdout.full {
		cr.Truncated = true
	}
	logText := full
	if parsed.OK && len(parsed.Text) > 0 {
		// go test -json: the human-readable test output plus non-JSON lines.
		logText = parsed.Text
	}
	masked := []byte(r.opts.Redactor.String(string(logText)))
	if r.opts.Out != nil {
		rel := fmt.Sprintf("%s/%s-%d.log", strings.Trim(r.opts.Prefix, "/"), gate, idx+1)
		if r.opts.Prefix == "" {
			rel = fmt.Sprintf("gates/%s-%d.log", gate, idx+1)
		}
		if ref, err := r.opts.Out.Write(rel, masked); err == nil {
			cr.LogArtifact = ref.Path
		}
	}
	trimmed, cut := Trim(masked, r.opts.MaxLogBytes)
	cr.Log = string(trimmed)
	cr.Truncated = cr.Truncated || cut

	testsFailed := false
	for _, t := range parsed.Tests {
		if t.Status == TestFail {
			testsFailed = true
			break
		}
	}
	switch {
	case timedOut:
		cr.Status, cr.Class = Timeout, ClassTimeout
		cr.Reason = fmt.Sprintf("killed after %s", timeout)
	case cr.ExitCode == 0:
		cr.Status = Passed
	case c.Scanner != "" && parsed.OK:
		// Scanners exit non-zero when they find something; the findings
		// are judged by severity and baseline.
		cr.Status = Passed
	default:
		if reason := environmentReason(cr.ExitCode, masked, testsFailed); reason != "" {
			cr.Status, cr.Class, cr.Reason = Failed, ClassEnvironment, reason
		} else if c.Scanner != "" {
			cr.Status, cr.Class, cr.Reason = Errored, ClassInternal, "scanner output could not be read"
		} else {
			cr.Status, cr.Class = Failed, ClassQuality
		}
		if !testsFailed && cr.Class != ClassInternal {
			cr.Signatures = Signatures(masked)
		}
	}
	return cr, parsed, nil
}

// readReports reads the regular files a command left in its reports dir.
func readReports(dir string) map[string][]byte {
	out := map[string][]byte{}
	_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !e.Type().IsRegular() {
			return nil
		}
		fi, err := e.Info()
		if err != nil || fi.Size() > MaxReportFile {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 -- private temp dir
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	return out
}

func (r *Runner) redactParsed(p Parsed) Parsed {
	red := r.opts.Redactor.String
	for i := range p.Issues {
		p.Issues[i].Message = red(p.Issues[i].Message)
		p.Issues[i].Path = red(p.Issues[i].Path)
	}
	for i := range p.Tests {
		p.Tests[i].Name = red(p.Tests[i].Name)
		p.Tests[i].Package = red(p.Tests[i].Package)
	}
	for i := range p.Vulns {
		p.Vulns[i].Summary = red(p.Vulns[i].Summary)
	}
	return p
}
