package codexreview

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/proc"
	"github.com/0xyu/aco/internal/secure"
)

// Options configure the adapter.
type Options struct {
	Bin     string   // codex binary; default "codex"
	BinArgs []string // arguments before ACO's own (tests run a fake CLI this way)
	Caps    *Capabilities

	Mode     auth.Mode     // resolved Codex auth mode
	Env      *auth.Env     // OPENAI_API_KEY (api mode)
	Keys     auth.KeyStore // optional keychain
	BaseEnv  []string      // parent environment; nil = process environment
	ExtraEnv []string
	// APIHome is the dedicated CODEX_HOME of api mode (~/.aco/codex-api).
	APIHome string
	// UserHome is the user's home directory: reads below it are always
	// violations, and api mode refuses an APIHome equal to ~/.codex.
	UserHome string

	Redactor *secure.Redactor
	Budget   *agent.Budget           // charged in api mode when a price is configured
	Pricing  map[string]config.Price // pricing.codex

	Model, FallbackModel, ReasoningEffort string
	// WindowsSandbox is passed as windows.sandbox on Windows; default
	// "unelevated" (the read-only sandbox needs it to run commands).
	WindowsSandbox string
	// MaxContextBytes caps the context pack on stdin (§16.3); larger diffs
	// are passed as artifact files. Default 512 KiB.
	MaxContextBytes int
	// MaxMessageBytes bounds the final message read from -o (default 4 MiB).
	MaxMessageBytes int
	// MaxTranscriptBytes bounds the stored JSONL stream (default and
	// maximum: the artifact size limit).
	MaxTranscriptBytes int

	InvocationTimeout time.Duration
	IdleTimeout       time.Duration
	KillGrace         time.Duration

	// SchemaRetries is how often a schema-invalid answer is retried with
	// the validation errors per model (default 2, §14.1).
	SchemaRetries int
	// TransientRetries bounds retries of transient failures and 429s per
	// call (default 4); Backoff gives the delay (default exponential with
	// jitter, honouring the CLI's retry hint, at most 5 minutes).
	TransientRetries int
	Backoff          func(retry int, hint time.Duration) time.Duration
	Sleep            func(ctx context.Context, d time.Duration) error

	Now func() time.Time
}

// OptionsFromConfig fills the model, effort, pricing, and limit settings
// from cfg. acoHome is ~/.aco (for the api-mode CODEX_HOME).
func OptionsFromConfig(cfg *config.Config, mode auth.Mode, env *auth.Env, red *secure.Redactor, acoHome, userHome string) Options {
	return Options{
		Mode: mode, Env: env, Redactor: red,
		APIHome: auth.CodexAPIHome(acoHome), UserHome: userHome,
		Model: cfg.Reviewer.Model, FallbackModel: cfg.Reviewer.FallbackModel, ReasoningEffort: cfg.Reviewer.ReasoningEffort,
		WindowsSandbox: cfg.Reviewer.WindowsSandbox, MaxContextBytes: cfg.Reviewer.MaxContextBytes,
		Pricing:           cfg.Pricing.Codex,
		InvocationTimeout: time.Duration(cfg.Limits.InvocationTimeout),
		IdleTimeout:       time.Duration(cfg.Limits.IdleTimeout),
	}
}

// DefaultMaxContextBytes is the default context pack cap.
const DefaultMaxContextBytes = 512 << 10

// Adapter runs Codex CLI as the reviewer. It implements agent.Reviewer.
type Adapter struct {
	opts Options

	capsOnce sync.Once
	caps     *Capabilities
	capsErr  error
	loginMu  sync.Mutex
}

var _ agent.Reviewer = (*Adapter)(nil)

// New returns an adapter.
func New(opts Options) *Adapter {
	if opts.Bin == "" {
		opts.Bin = "codex"
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
	if opts.WindowsSandbox == "" {
		opts.WindowsSandbox = "unelevated"
	}
	if opts.MaxContextBytes <= 0 {
		opts.MaxContextBytes = DefaultMaxContextBytes
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = 4 << 20
	}
	if opts.MaxTranscriptBytes <= 0 || opts.MaxTranscriptBytes > artifacts.MaxSize {
		opts.MaxTranscriptBytes = artifacts.MaxSize
	}
	if opts.SchemaRetries <= 0 {
		opts.SchemaRetries = 2
	}
	if opts.TransientRetries <= 0 {
		opts.TransientRetries = 4
	}
	if opts.Backoff == nil {
		opts.Backoff = defaultBackoff
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.UserHome == "" {
		opts.UserHome, _ = os.UserHomeDir()
	}
	if opts.Mode == auth.API && opts.Env != nil {
		if key, ok := auth.LookupCodexKey(opts.Env, opts.Keys); ok {
			opts.Redactor.AddSecret(key)
		}
	}
	return &Adapter{opts: opts}
}

func defaultBackoff(retry int, hint time.Duration) time.Duration {
	d := time.Duration(1<<min(retry, 8)) * 2 * time.Second
	d += time.Duration(time.Now().UnixNano() % int64(time.Second)) // jitter
	if hint > d {
		d = hint
	}
	return min(d, 5*time.Minute)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Capabilities detects (once) and returns the CLI's capabilities.
func (a *Adapter) Capabilities(ctx context.Context) (*Capabilities, error) {
	a.capsOnce.Do(func() {
		if a.opts.Caps != nil {
			a.caps = a.opts.Caps
			return
		}
		env, err := a.childEnv(auth.Subscription) // no credential needed
		if err != nil {
			a.capsErr = err
			return
		}
		a.caps, a.capsErr = Detect(ctx, a.opts.Bin, a.opts.BinArgs, env)
	})
	return a.caps, a.capsErr
}

func (a *Adapter) childEnv(mode auth.Mode) ([]string, error) {
	home := ""
	if mode == auth.API {
		// Every api-mode command (status, login, exec) gets its environment
		// here: the dedicated home is checked before any of them runs.
		if err := a.checkAPIHome(); err != nil {
			return nil, err
		}
		home = a.opts.APIHome
	}
	return auth.ChildEnv(auth.ChildEnvInput{
		Agent: auth.Codex, Mode: mode, Env: a.opts.Env, Keys: a.opts.Keys,
		Base: a.opts.BaseEnv, Extra: a.opts.ExtraEnv, CodexHome: home,
	})
}

func (a *Adapter) fail(inv agent.Invocation, class agent.ErrorClass, msg string, err error) *agent.Error {
	return &agent.Error{Agent: "codex", Class: class, Message: a.opts.Redactor.String(msg), Invocation: inv, Err: err}
}

// runCLI runs a short codex command (login, status) and returns its
// combined output and exit error.
func (a *Adapter) runCLI(ctx context.Context, env []string, stdin io.Reader, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.Command(a.opts.Bin, append(append([]string{}, a.opts.BinArgs...), args...)...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = stdin
	}
	out := &limitedBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	t, err := proc.Start(cmd)
	if err != nil {
		return "", err
	}
	defer t.Close()
	done := make(chan error, 1)
	go func() { done <- t.Wait() }()
	select {
	case err = <-done:
	case <-cctx.Done():
		_ = t.Kill()
		<-done
		err = cctx.Err()
	}
	return a.opts.Redactor.String(out.String()), err
}

// LoginStatus runs `codex login status` for the resolved mode's CODEX_HOME.
func (a *Adapter) LoginStatus(ctx context.Context) (auth.CodexStatus, error) {
	env, err := a.childEnv(a.opts.Mode)
	if err != nil {
		return auth.CodexStatus{}, err
	}
	out, err := a.runCLI(ctx, env, nil, "login", "status")
	st := auth.ParseCodexLoginStatus(out)
	if st.Login == auth.CodexLoginUnknown && err != nil {
		return st, err
	}
	return st, nil
}

// EnsureLogin verifies that Codex is logged in for the resolved mode
// (§16.5). In api mode it first logs the dedicated CODEX_HOME in with the
// current OPENAI_API_KEY when it is not, or with another key; the user's
// ~/.codex is never used. Called before every review.
func (a *Adapter) EnsureLogin(ctx context.Context) error {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	home := ""
	if a.opts.Mode == auth.API {
		if err := a.checkAPIHome(); err != nil {
			return err
		}
		home = a.opts.APIHome
		key, ok := auth.LookupCodexKey(a.opts.Env, a.opts.Keys)
		if !ok {
			return &auth.KeyMissingError{Agent: auth.Codex, Var: auth.EnvOpenAIKey}
		}
		st, _ := a.LoginStatus(ctx)
		if st.Login != auth.CodexLoginAPIKey || (auth.CodexKeyHint(key) != "" && st.KeyHint != auth.CodexKeyHint(key)) {
			if err := os.MkdirAll(a.opts.APIHome, 0o700); err != nil {
				return err
			}
			env, err := a.childEnv(auth.API)
			if err != nil {
				return err
			}
			if out, err := a.runCLI(ctx, env, auth.CodexLoginStdin(key), "login", "--with-api-key"); err != nil {
				return fmt.Errorf("codex login --with-api-key failed: %s", firstLine(out))
			}
		}
	}
	st, err := a.LoginStatus(ctx)
	if err != nil {
		return fmt.Errorf("codex login status: %w", err)
	}
	return auth.CheckCodexLogin(a.opts.Mode, st, home)
}

// ErrUnsafeAPIHome reports a dedicated api-mode CODEX_HOME that is, or may
// be, one of the user's own Codex homes.
var ErrUnsafeAPIHome = errors.New("codexreview: unsafe api-mode CODEX_HOME")

// userCodexHomes are the Codex homes of the user's subscription login:
// ~/.codex and a CODEX_HOME set in the parent environment.
func (a *Adapter) userCodexHomes() []string {
	var out []string
	if a.opts.UserHome != "" {
		out = append(out, filepath.Join(a.opts.UserHome, ".codex"))
	}
	base := a.opts.BaseEnv
	if base == nil {
		base = auth.ProcessEnviron()
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, auth.EnvCodexHome) && strings.TrimSpace(v) != "" {
			v = strings.TrimSpace(v)
			if strings.HasPrefix(v, "~") && a.opts.UserHome != "" {
				v = filepath.Join(a.opts.UserHome, v[1:])
			}
			out = append(out, v)
		}
	}
	return out
}

// checkAPIHome refuses a dedicated home that is, contains, or lies inside
// one of the user's Codex homes: by name, after resolving symlinks and
// junctions (also in parent directories), or by filesystem identity. A
// home that is itself a link, is not a directory, or cannot be resolved is
// ambiguous and refused too. It runs before any api-mode command, so
// credentials are never changed in the wrong place.
func (a *Adapter) checkAPIHome() error {
	h := a.opts.APIHome
	if h == "" || !filepath.IsAbs(h) {
		return fmt.Errorf("%w: api mode needs an absolute dedicated CODEX_HOME", ErrUnsafeAPIHome)
	}
	h = filepath.Clean(h)
	if fi, err := os.Lstat(h); err == nil {
		if isLink(fi) {
			return fmt.Errorf("%w: %s is a link or junction", ErrUnsafeAPIHome, h)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUnsafeAPIHome, h)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s: %v", ErrUnsafeAPIHome, h, err)
	}
	for _, u := range a.userCodexHomes() {
		if !filepath.IsAbs(u) {
			return fmt.Errorf("%w: the user's CODEX_HOME %q is not absolute; cannot prove the api home is separate", ErrUnsafeAPIHome, u)
		}
		same, err := overlaps(h, u)
		if err != nil {
			return fmt.Errorf("%w: cannot resolve %s or %s: %v", ErrUnsafeAPIHome, h, u, err)
		}
		if same {
			return fmt.Errorf("%w: %s is or overlaps the user's Codex home %s", ErrUnsafeAPIHome, h, u)
		}
	}
	return nil
}

// call is one invocation.
type call struct {
	model     string
	prompt    string
	schemaRel string // run-relative schema file
	ws        agent.Workspace
	out       agent.Output // Prefix: this invocation's directory
	scopes    []agent.Scope
	noGitRepo bool // probe: the working directory is not a Git repository
}

// raw is a finished invocation.
type raw struct {
	inv      agent.Invocation
	message  string // the final message (masked)
	commands int    // commands the reviewer ran
}

// Artifact file names inside an invocation's directory.
const (
	fileContext     = "context.md"
	fileTranscript  = "transcript.jsonl"
	fileLastMessage = "last-message.json"
	fileStderr      = "stderr.log"
	// fileTranscriptIncomplete replaces the transcript when the stream
	// exceeded the size limit.
	fileTranscriptIncomplete = "transcript.incomplete.txt"
)

const (
	maxLine   = 32 << 20
	maxStderr = 256 << 10
)

// price returns the USD cost of the usage (false when no price is set).
func (a *Adapter) price(model string, u agent.Usage) (float64, bool) {
	p, ok := a.opts.Pricing[model]
	if !ok || (p.InputPerMTok == 0 && p.OutputPerMTok == 0) {
		return 0, false
	}
	return float64(u.InputTokens)*p.InputPerMTok/1e6 + float64(u.OutputTokens)*p.OutputPerMTok/1e6, true
}

// invoke runs one codex exec call end to end.
func (a *Adapter) invoke(ctx context.Context, c call) (*raw, error) {
	inv := agent.Invocation{AuthMode: string(a.opts.Mode), Model: c.model}
	if c.out.Run == nil || c.out.Prefix == "" {
		return nil, errors.New("codexreview: an artifact output is required")
	}
	if !filepath.IsAbs(c.ws.Dir) {
		return nil, errors.New("codexreview: the workspace directory must be absolute")
	}
	caps, err := a.Capabilities(ctx)
	if err != nil {
		return nil, a.fail(inv, agent.ClassCrash, "codex CLI not usable: "+err.Error(), err)
	}
	inv.CLIVersion = caps.Version
	if a.opts.Mode == auth.API && a.opts.Budget != nil {
		if _, priced := a.price(c.model, agent.Usage{InputTokens: 1}); priced && a.opts.Budget.Remaining(c.scopes...) <= 0 {
			return nil, a.fail(inv, agent.ClassBudget, "USD budget exhausted", nil)
		}
	}

	ref := func(name string) string { return c.out.Prefix + "/" + name }
	inv.PromptRef = ref(fileContext)
	if _, err := c.out.Run.Write(inv.PromptRef, []byte(c.prompt)); err != nil {
		return nil, err
	}
	prompt, err := c.out.Run.Read(inv.PromptRef) // the masked pack is what Codex gets
	if err != nil {
		return nil, err
	}
	schemaAbs, err := c.out.Run.Abs(c.schemaRel)
	if err != nil {
		return nil, err
	}
	inv.OutputRef = ref(fileLastMessage)
	// Codex writes -o itself, unmasked: into a private per-invocation
	// directory outside the run artifacts, removed on every exit path. The
	// message is published only through the masking artifact writer.
	outDir, err := os.MkdirTemp("", "aco-codex-out-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outDir)
	lastAbs := filepath.Join(outDir, fileLastMessage)
	winSandbox := ""
	if runtime.GOOS == "windows" {
		winSandbox = a.opts.WindowsSandbox
	}
	args, err := BuildArgs(caps, CommandSpec{
		Model: c.model, ReasoningEffort: a.opts.ReasoningEffort, Dir: c.ws.Dir,
		SchemaPath: schemaAbs, LastMessagePath: lastAbs, WindowsSandbox: winSandbox, SkipGitRepoCheck: c.noGitRepo,
	})
	if err != nil {
		return nil, a.fail(inv, agent.ClassCrash, err.Error(), err)
	}
	env, err := a.childEnv(a.opts.Mode)
	if err != nil {
		return nil, a.fail(inv, agent.ClassAuth, err.Error(), err)
	}
	mon := &Monitor{Roots: []string{c.ws.Dir, c.out.Run.Path()}, Home: a.opts.UserHome}
	return a.execute(ctx, c, args, env, prompt, mon, lastAbs, &inv)
}

// readBounded reads at most limit bytes of p; fits is false when the file
// is larger. A missing file is empty.
func readBounded(p string, limit int) (b []byte, fits bool, err error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err = io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > limit {
		return nil, false, nil
	}
	return b, true, nil
}

// execute starts the CLI and follows its event stream.
func (a *Adapter) execute(ctx context.Context, c call, args, env []string, prompt []byte, mon *Monitor, lastAbs string, inv *agent.Invocation) (*raw, error) {
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
	cmd.Stdin = bytes.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrBuf := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderrBuf

	start := a.opts.Now()
	tree, err := proc.Start(cmd)
	if err != nil {
		return nil, a.fail(*inv, agent.ClassCrash, "cannot start codex: "+err.Error(), err)
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
	checked := map[string]bool{}
	written, overflow := 0, false
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
			if !overflow {
				masked := []byte(a.opts.Redactor.String(string(line)) + "\n")
				var werr error
				if written+len(masked) > a.opts.MaxTranscriptBytes {
					werr = artifacts.ErrTooLarge
				} else {
					_, werr = transcript.Write(masked)
				}
				written += len(masked)
				if werr != nil {
					overflow = true
					transcript.Abort()
					stop = nil // this failure wins: the record of the call is incomplete
					kill(a.fail(*inv, agent.ClassCrash, fmt.Sprintf("the JSONL stream exceeds the transcript limit of %d bytes (%v); the call was stopped and its transcript is incomplete", a.opts.MaxTranscriptBytes, werr), werr))
				}
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleDur)
			if stop != nil {
				continue
			}
			e := st.feed(line)
			if e == nil || e.Item == nil || !strings.HasPrefix(e.Type, "item.") {
				continue
			}
			key := e.Item.ID + "|" + e.Item.Type + "|" + e.Item.Command
			if checked[key] {
				continue
			}
			checked[key] = true
			if vs := mon.Item(e.Item); len(vs) > 0 {
				for i := range vs {
					vs[i].Path = a.opts.Redactor.String(vs[i].Path)
					vs[i].Command = a.opts.Redactor.String(vs[i].Command)
				}
				inv.Violations = append(inv.Violations, vs...)
				kill(a.fail(*inv, agent.ClassPolicy, describeViolation(vs[0]), nil))
			}
		case <-idle.C:
			kill(a.fail(*inv, agent.ClassTimeout, fmt.Sprintf("no output for %s (idle timeout)", idleDur), nil))
		case <-total:
			kill(a.fail(*inv, agent.ClassTimeout, fmt.Sprintf("invocation timeout %s reached", a.opts.InvocationTimeout), nil))
			total = nil
		case <-ctx.Done():
			kill(a.fail(*inv, agent.ClassCancelled, "cancelled", ctx.Err()))
			ctx = context.Background()
		}
	}

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

	inv.SessionID = st.threadID
	inv.Usage = agent.Usage{
		InputTokens: st.usage.InputTokens, OutputTokens: st.usage.OutputTokens,
		CacheReadTokens: st.usage.CachedInputTokens, CacheCreationTokens: st.usage.CacheWriteInputTokens,
		ReasoningTokens: st.usage.ReasoningOutputTokens, Turns: st.turns, Duration: a.opts.Now().Sub(start),
	}
	if cost, ok := a.price(c.model, inv.Usage); ok {
		inv.Usage.CostUSD, inv.Priced = cost, true
		if a.opts.Mode == auth.API && a.opts.Budget != nil {
			inv.Budget = a.opts.Budget.Charge(cost, c.scopes...)
			inv.Charged = true
		}
	}

	if overflow {
		// Never present the partial stream as the transcript: drop it and
		// leave a marker that says why.
		inv.TranscriptRef = ""
		_, _ = c.out.Run.Write(ref(fileTranscriptIncomplete), []byte(fmt.Sprintf(
			"The JSONL stream of this call exceeded %d bytes; it was stopped and the transcript was not stored.\n", a.opts.MaxTranscriptBytes)))
	} else if _, err := transcript.Commit(); err != nil {
		inv.TranscriptRef = ""
		return nil, a.fail(*inv, agent.ClassCrash, "storing the transcript failed: "+err.Error(), err)
	}
	stderrText := a.opts.Redactor.String(stderrBuf.String())
	if _, err := c.out.Run.Write(inv.StderrRef, []byte(stderrText)); err != nil {
		return nil, a.fail(*inv, agent.ClassCrash, "storing stderr failed: "+err.Error(), err)
	}
	// The final message: from the private -o file (bounded), else the last
	// agent message of the stream; masked, then published.
	message := st.lastMessage
	b, fits, rerr := readBounded(lastAbs, a.opts.MaxMessageBytes)
	switch {
	case rerr != nil:
		inv.OutputRef = ""
		return nil, a.fail(*inv, agent.ClassCrash, "reading the final message failed: "+rerr.Error(), rerr)
	case !fits:
		inv.OutputRef = ""
		if stop == nil {
			return nil, a.fail(*inv, agent.ClassSchemaInvalid, fmt.Sprintf("the final message is larger than %d bytes", a.opts.MaxMessageBytes), nil)
		}
		message = ""
	case len(bytes.TrimSpace(b)) > 0:
		message = string(b)
	}
	if len(message) > a.opts.MaxMessageBytes {
		message = message[:a.opts.MaxMessageBytes]
	}
	message = a.opts.Redactor.String(message)
	if inv.OutputRef != "" {
		if _, err := c.out.Run.Write(inv.OutputRef, []byte(message)); err != nil {
			inv.OutputRef = ""
			return nil, a.fail(*inv, agent.ClassCrash, "storing the final message failed: "+err.Error(), err)
		}
	}
	inv.ResultText = shortText(message)

	if stop != nil {
		stop.Invocation = *inv
		return nil, stop
	}
	if a.opts.ReasoningEffort != "" && st.ignoredConfig("model_reasoning_effort") {
		return nil, a.fail(*inv, agent.ClassCrash, "codex ignored model_reasoning_effort; the installed CLI does not accept it", nil)
	}
	if st.failed != "" || !st.completed {
		text := st.errorText() + "\n" + stderrText
		e := &agent.Error{Agent: "codex", Invocation: *inv, Class: classify(a.opts.Mode, text)}
		switch e.Class {
		case agent.ClassSubscriptionLimit:
			e.ResetAt = parseReset(text, a.opts.Now())
			e.Message = "subscription usage limit reached"
		case agent.ClassAPIRateLimit, agent.ClassTransient:
			e.RetryAfter = parseRetryAfter(text)
			e.Message = firstLine(st.failed + "\n" + text)
		case "":
			e.Class = agent.ClassCrash
			msg := firstLine(st.failed + "\n" + text)
			if msg == "" {
				msg = "the CLI exited without completing the turn"
				if exitErr != nil {
					msg += " (" + exitErr.Error() + ")"
				}
			}
			e.Message = msg
		default:
			e.Message = firstLine(st.failed + "\n" + text)
		}
		e.Message = a.opts.Redactor.String(e.Message)
		return nil, e
	}
	if strings.TrimSpace(message) == "" {
		return nil, a.fail(*inv, agent.ClassSchemaInvalid, "the reviewer returned no final message", nil)
	}
	return &raw{inv: *inv, message: message, commands: st.commands}, nil
}

func describeViolation(v agent.AccessViolation) string {
	switch v.Kind {
	case "read":
		return "the reviewer accessed a path outside the worktree and the artifact directory: " + v.Path
	case "write":
		return "the reviewer tried to change files: " + v.Path
	}
	return "the reviewer used a tool it may not use: " + v.Command
}

func shortText(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

// cliSchema prepares a schema file for --output-schema: without the root
// "$schema" and "$id" (the structured-output validator accepts only the
// schema keywords themselves).
func cliSchema(schema []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	delete(root, "$schema")
	delete(root, "$id")
	return json.MarshalIndent(root, "", "  ")
}

// readLine reads one line of at most limit bytes (longer lines are an error).
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("JSONL line longer than %d bytes", limit)
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
