package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/proc"
	"github.com/0xyu/aco/internal/secure"
)

// Options configure the adapter.
type Options struct {
	Bin     string   // claude binary; default "claude"
	BinArgs []string // arguments before ACO's own (tests run a fake CLI this way)
	// HookBin is the aco binary the guard hook runs; default os.Executable().
	HookBin string
	// Caps skips detection when set.
	Caps *Capabilities

	Mode     auth.Mode     // resolved Claude auth mode
	Env      *auth.Env     // API keys (api mode) and the subscription OAuth token
	Keys     auth.KeyStore // optional keychain
	BaseEnv  []string      // parent environment; nil = process environment
	ExtraEnv []string

	Redactor *secure.Redactor
	Budget   *agent.Budget // charged in api mode; nil = no USD accounting

	PlannerModel, PlannerEffort         string
	ImplementerModel, ImplementerEffort string
	StrictEffort                        bool
	ExtraAllowedBash                    []string

	MaxTurns          int
	InvocationTimeout time.Duration
	IdleTimeout       time.Duration
	// KillGrace is how long to wait for the CLI to exit after its stdout
	// closed before the tree is killed.
	KillGrace time.Duration

	Now func() time.Time
}

// OptionsFromConfig fills the model, effort, limit, and bash settings from cfg.
func OptionsFromConfig(cfg *config.Config, mode auth.Mode, env *auth.Env, red *secure.Redactor) Options {
	return Options{
		Mode: mode, Env: env, Redactor: red,
		PlannerModel: cfg.Planner.Model, PlannerEffort: cfg.Planner.Effort,
		ImplementerModel: cfg.Implementer.Model, ImplementerEffort: cfg.Implementer.Effort,
		StrictEffort: cfg.Implementer.StrictEffort, ExtraAllowedBash: cfg.Implementer.ExtraAllowedBash,
		MaxTurns:          cfg.Limits.ClaudeMaxTurns,
		InvocationTimeout: time.Duration(cfg.Limits.InvocationTimeout),
		IdleTimeout:       time.Duration(cfg.Limits.IdleTimeout),
	}
}

// Adapter runs Claude Code. It implements agent.Planner (Planner) and
// agent.Implementer (Implementer).
type Adapter struct {
	opts Options

	capsOnce sync.Once
	caps     *Capabilities
	capsErr  error
}

// New returns an adapter.
func New(opts Options) *Adapter {
	if opts.Bin == "" {
		opts.Bin = "claude"
	}
	if opts.HookBin == "" {
		if exe, err := os.Executable(); err == nil {
			opts.HookBin = exe
		}
	}
	if opts.Redactor == nil {
		opts.Redactor = secure.NewRedactor()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 10 * time.Second
	}
	if opts.Mode == "" {
		opts.Mode = auth.Subscription
	}
	if opts.PlannerModel == "" {
		opts.PlannerModel = config.DefaultClaudeModel
	}
	if opts.ImplementerModel == "" {
		opts.ImplementerModel = config.DefaultClaudeModel
	}
	if opts.PlannerEffort == "" {
		opts.PlannerEffort = agent.EffortHigh // §10.2
	}
	if opts.ImplementerEffort == "" {
		opts.ImplementerEffort = agent.EffortMedium // §11.4
	}
	return &Adapter{opts: opts}
}

// Capabilities detects (once) and returns the CLI's capabilities.
func (a *Adapter) Capabilities(ctx context.Context) (*Capabilities, error) {
	a.capsOnce.Do(func() {
		if a.opts.Caps != nil {
			a.caps = a.opts.Caps
			return
		}
		env, err := a.childEnv(auth.Subscription) // no credential needed for --help
		if err != nil {
			a.capsErr = err
			return
		}
		a.caps, a.capsErr = Detect(ctx, a.opts.Bin, a.opts.BinArgs, env)
	})
	return a.caps, a.capsErr
}

func (a *Adapter) childEnv(mode auth.Mode) ([]string, error) {
	return auth.ChildEnv(auth.ChildEnvInput{
		Agent: auth.Claude, Mode: mode, Env: a.opts.Env, Keys: a.opts.Keys,
		Base: a.opts.BaseEnv, Extra: a.opts.ExtraEnv,
	})
}

// call is one invocation.
type call struct {
	profile   Profile
	sessionID string
	resume    bool
	model     string
	effort    string
	system    string
	prompt    string
	schema    []byte
	ws        agent.Workspace
	out       agent.Output
	runID     string
	scopes    []agent.Scope
}

// raw is a finished invocation before profile-specific parsing.
type raw struct {
	inv        agent.Invocation
	structured json.RawMessage
}

// Artifact file names inside an invocation's prefix.
const (
	filePrompt      = "prompt.md"
	fileSystem      = "system-prompt.md"
	fileSettings    = "settings.json"
	fileGuardPolicy = "guard-policy.json"
	fileDenials     = "guard-denials.jsonl"
	fileTranscript  = "transcript.jsonl"
	fileStderr      = "stderr.log"
)

// maxLine bounds one stream-json line; maxStderr what is kept of stderr.
const (
	maxLine   = 32 << 20
	maxStderr = 256 << 10
)

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (a *Adapter) fail(inv agent.Invocation, class agent.ErrorClass, msg string, err error) *agent.Error {
	return &agent.Error{Agent: "claude", Class: class, Message: a.opts.Redactor.String(msg), Invocation: inv, Err: err}
}

// run executes one invocation end to end.
func (a *Adapter) run(ctx context.Context, c call) (*raw, error) {
	inv := agent.Invocation{SessionID: c.sessionID, AuthMode: string(a.opts.Mode)}
	if c.out.Run == nil || c.out.Prefix == "" {
		return nil, errors.New("claudecode: an artifact output is required")
	}
	if !filepath.IsAbs(c.ws.Dir) {
		return nil, errors.New("claudecode: the workspace directory must be absolute")
	}
	caps, err := a.Capabilities(ctx)
	if err != nil {
		return nil, a.fail(inv, agent.ClassCrash, "claude CLI not usable: "+err.Error(), err)
	}
	inv.CLIVersion = caps.Version
	if c.profile == ProfileImplementer && a.opts.StrictEffort && caps.EffortVia() != EffortFlag {
		return nil, a.fail(inv, agent.ClassCrash, ErrEffortUnavailable.Error(), ErrEffortUnavailable)
	}

	// Budget (api mode): refuse to start when nothing is left, and cap the call.
	maxBudget := 0.0
	if a.opts.Mode == auth.API && a.opts.Budget != nil {
		rem := a.opts.Budget.Remaining(c.scopes...)
		if rem <= 0 {
			return nil, a.fail(inv, agent.ClassBudget, "USD budget exhausted", nil)
		}
		if !math.IsInf(rem, 1) {
			maxBudget = rem
		}
	}

	// Artifacts: prompt, system prompt, guard policy, settings.
	ref := func(name string) string { return c.out.Prefix + "/" + name }
	red := a.opts.Redactor
	prompt, system := red.String(c.prompt), red.String(c.system)
	inv.PromptRef = ref(filePrompt)
	if _, err := c.out.Run.Write(inv.PromptRef, []byte(prompt)); err != nil {
		return nil, err
	}
	if _, err := c.out.Run.Write(ref(fileSystem), []byte(system)); err != nil {
		return nil, err
	}
	denialsAbs, err := c.out.Run.Abs(ref(fileDenials))
	if err != nil {
		return nil, err
	}
	gp := &policy.GuardPolicy{
		Version: policy.GuardVersion, RunID: c.runID, Profile: string(c.profile), Root: c.ws.Dir,
		ProtectedPaths: c.ws.ProtectedPaths, ScopePaths: c.ws.ScopePaths, ScopeEnforcement: c.ws.ScopeEnforcement,
		Network: c.ws.Network, NetworkAllow: c.ws.NetworkAllow, DenyLog: denialsAbs,
	}
	if gp.Network == "" {
		gp.Network = policy.NetworkDeny
	}
	if gp.ScopeEnforcement == "" {
		gp.ScopeEnforcement = "flag"
	}
	if err := gp.Validate(); err != nil {
		return nil, err
	}
	gpJSON, _ := json.MarshalIndent(gp, "", "  ")
	inv.GuardPolicyRef = ref(fileGuardPolicy)
	if _, err := c.out.Run.Write(inv.GuardPolicyRef, gpJSON); err != nil {
		return nil, err
	}
	gpAbs, _ := c.out.Run.Abs(inv.GuardPolicyRef)
	// A hook command that cannot start is a non-blocking error for Claude
	// Code, so the tool call would proceed: check the binary first.
	if fi, err := os.Stat(a.opts.HookBin); a.opts.HookBin == "" || err != nil || !fi.Mode().IsRegular() {
		return nil, a.fail(inv, agent.ClassCrash, "the guard hook binary is missing; refusing to run without the guard", err)
	}
	effortSetting := ""
	if caps.EffortVia() == EffortSettings {
		effortSetting = c.effort
	}
	settings, err := Settings(HookCommand(a.opts.HookBin, c.runID, gpAbs), DisallowedTools(c.profile, gp.Network), effortSetting)
	if err != nil {
		return nil, err
	}
	inv.SettingsRef = ref(fileSettings)
	if _, err := c.out.Run.Write(inv.SettingsRef, settings); err != nil {
		return nil, err
	}
	settingsAbs, _ := c.out.Run.Abs(inv.SettingsRef)

	args, err := BuildArgs(caps, CommandSpec{
		Profile: c.profile, SessionID: c.sessionID, Resume: c.resume, Model: c.model, Effort: c.effort,
		MaxTurns: a.opts.MaxTurns, Schema: c.schema, SystemPrompt: system, SettingsPath: settingsAbs,
		AllowedBash: a.opts.ExtraAllowedBash, Network: gp.Network, MaxBudgetUSD: maxBudget,
	})
	if err != nil {
		return nil, a.fail(inv, agent.ClassCrash, err.Error(), err)
	}
	env, err := a.childEnv(a.opts.Mode)
	if err != nil {
		return nil, a.fail(inv, agent.ClassAuth, err.Error(), err)
	}

	res, err := a.execute(ctx, c, args, env, prompt, &inv)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// execute starts the CLI and follows its stream.
func (a *Adapter) execute(ctx context.Context, c call, args, env []string, prompt string, inv *agent.Invocation) (*raw, error) {
	ref := func(name string) string { return c.out.Prefix + "/" + name }
	transcript, err := c.out.Run.Create(ref(fileTranscript))
	if err != nil {
		return nil, err
	}
	defer transcript.Abort()
	inv.TranscriptRef, inv.StderrRef = ref(fileTranscript), ref(fileStderr)

	cmd := exec.Command(a.opts.Bin, append(append([]string{}, a.opts.BinArgs...), args...)...)
	cmd.Dir = c.ws.Dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrBuf := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderrBuf

	start := a.opts.Now()
	tree, err := proc.Start(cmd)
	if err != nil {
		return nil, a.fail(*inv, agent.ClassCrash, "cannot start claude: "+err.Error(), err)
	}
	defer tree.Close()

	lines := make(chan []byte, 64)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(stdout, 64<<10)
		for {
			line, err := readLine(br, maxLine)
			if len(line) > 0 {
				lines <- line
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr <- err
				}
				return
			}
		}
	}()

	var total <-chan time.Time
	if a.opts.InvocationTimeout > 0 {
		t := time.NewTimer(a.opts.InvocationTimeout)
		defer t.Stop()
		total = t.C
	}
	idleDur := a.opts.IdleTimeout
	if idleDur <= 0 {
		idleDur = 24 * time.Hour
	}
	idle := time.NewTimer(idleDur)
	defer idle.Stop()

	st := newStream()
	initChecked := false
	hookEvents := false
	if caps, _ := a.Capabilities(ctx); caps.Has("include-hook-events") {
		hookEvents = true
	}
	turnLimit := 0
	if a.opts.MaxTurns > 0 {
		turnLimit = a.opts.MaxTurns
		if caps, _ := a.Capabilities(ctx); caps.Has("max-turns") {
			turnLimit = 2*a.opts.MaxTurns + 10 // the CLI enforces the limit; this is a backstop
		}
	}

	var stop *agent.Error
	kill := func(e *agent.Error) {
		if stop == nil {
			stop = e
		}
		_ = tree.Kill()
	}

loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			_, _ = transcript.Write(line)
			_, _ = transcript.Write([]byte("\n"))
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleDur)
			if stop != nil {
				continue // draining after a kill
			}
			m := st.feed(line)
			if m == nil {
				continue
			}
			switch {
			case st.init != nil && !initChecked:
				initChecked = true
				inv.AuthSource, inv.Model = st.init.APIKeySource, st.init.Model
				if st.init.Version != "" {
					inv.CLIVersion = st.init.Version
				}
				if err := auth.CheckClaudeSource(a.opts.Mode, st.init.APIKeySource); err != nil {
					kill(a.fail(*inv, agent.ClassAuth, err.Error(), err))
					continue
				}
				if c.profile == ProfilePlanner {
					for _, t := range st.init.Tools {
						if slices.Contains(writeTools, t) {
							kill(a.fail(*inv, agent.ClassCrash, "planner session has a write tool ("+t+"); refusing to continue", nil))
							break
						}
					}
				}
			case m.hookFailed():
				code := -1
				if m.ExitCode != nil {
					code = *m.ExitCode
				}
				kill(a.fail(*inv, agent.ClassCrash, fmt.Sprintf("the guard hook failed (exit %d); refusing to continue unguarded", code), nil))
			case m.Type == "user" && hookEvents && st.toolUses > 0 && st.guardHooks == 0:
				// A tool result arrived but the guard never answered: the
				// generated settings were not applied (-p ignores invalid ones).
				kill(a.fail(*inv, agent.ClassCrash, "a tool ran but the guard hook never did; the generated settings were not applied", nil))
			case m.Type == "assistant" && turnLimit > 0 && st.turns() > turnLimit:
				kill(a.fail(*inv, agent.ClassMaxTurns, "turn limit reached (enforced by ACO)", nil))
			}
		case <-idle.C:
			kill(a.fail(*inv, agent.ClassTimeout, fmt.Sprintf("no output for %s (idle timeout)", idleDur), nil))
		case <-total:
			kill(a.fail(*inv, agent.ClassTimeout, fmt.Sprintf("invocation timeout %s reached", a.opts.InvocationTimeout), nil))
			total = nil
		case <-ctx.Done():
			kill(a.fail(*inv, agent.ClassCancelled, "cancelled", ctx.Err()))
			ctx = context.Background() // keep draining
		}
	}

	// stdout closed: wait for the exit, kill the tree if it lingers, and
	// always end leftover descendants (background shells).
	waitDone := make(chan error, 1)
	go func() { waitDone <- tree.Wait() }()
	var exitErr error
	select {
	case exitErr = <-waitDone:
	case <-time.After(a.opts.KillGrace):
		_ = tree.Kill()
		exitErr = <-waitDone
	}
	_ = tree.Kill()
	select {
	case err := <-readErr:
		if stop == nil {
			stop = a.fail(*inv, agent.ClassCrash, "reading the CLI output failed: "+err.Error(), err)
		}
	default:
	}

	inv.Usage.Duration = a.opts.Now().Sub(start)
	if st.sessionID != "" {
		inv.SessionID = st.sessionID
	}
	if r := st.result; r != nil {
		inv.Usage.Turns = r.NumTurns
		inv.Usage.CostUSD = r.TotalCostUSD
		if r.Usage != nil {
			inv.Usage.InputTokens, inv.Usage.OutputTokens = r.Usage.InputTokens, r.Usage.OutputTokens
			inv.Usage.CacheReadTokens, inv.Usage.CacheCreationTokens = r.Usage.CacheReadTokens, r.Usage.CacheCreationTokens
		}
		inv.ResultText = a.opts.Redactor.String(r.Result)
	} else {
		inv.Usage.Turns = st.turns()
	}
	inv.GuardDenials = readDenials(c.out, ref(fileDenials))

	// Persist the transcript and stderr (masked) whatever happened.
	if _, err := transcript.Commit(); err != nil {
		return nil, err
	}
	stderrText := stderrBuf.String()
	if _, err := c.out.Run.Write(inv.StderrRef, []byte(stderrText)); err != nil {
		return nil, err
	}

	// Charge api-mode cost even for failed calls: it was spent.
	if a.opts.Mode == auth.API && a.opts.Budget != nil && inv.Usage.CostUSD > 0 {
		inv.Budget = a.opts.Budget.Charge(inv.Usage.CostUSD, c.scopes...)
		inv.Charged = true
	}

	if stop != nil {
		stop.Invocation = *inv
		return nil, stop
	}
	if hookEvents && st.toolUses > 0 && st.guardHooks == 0 {
		// Settings that fail validation are ignored silently in -p mode;
		// tools then ran without the guard.
		return nil, a.fail(*inv, agent.ClassCrash, "tools ran but the guard hook never did; the generated settings were not applied", nil)
	}
	if st.init == nil && st.result != nil {
		// No init message: the auth source cannot be verified (§16.5).
		return nil, a.fail(*inv, agent.ClassAuth, "the CLI sent no init message; the auth source cannot be verified", nil)
	}
	if e := outcome(a.opts.Mode, st, a.opts.Redactor.String(stderrText), exitErr, a.opts.Now()); e != nil {
		e.Message = a.opts.Redactor.String(e.Message)
		e.Invocation = *inv
		return nil, e
	}
	// With inv.Budget.Exceeded set the work is still returned; the engine
	// stops before the next call, and run refuses to start one.
	return &raw{inv: *inv, structured: extractJSON(st.result)}, nil
}

// readLine reads one line of at most limit bytes (longer lines are an error).
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("stream-json line longer than %d bytes", limit)
		}
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// readDenials parses the guard hook's deny log.
func readDenials(out agent.Output, rel string) []agent.GuardDenial {
	b, err := out.Run.Read(rel)
	if err != nil {
		return nil
	}
	var ds []agent.GuardDenial
	for _, l := range bytes.Split(b, []byte("\n")) {
		var d agent.GuardDenial
		if len(bytes.TrimSpace(l)) > 0 && json.Unmarshal(l, &d) == nil {
			ds = append(ds, d)
		}
	}
	return ds
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
