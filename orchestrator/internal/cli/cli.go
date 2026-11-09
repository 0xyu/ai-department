// Package cli implements the aco command tree.
//
// All operator-facing output goes through i18n.Printer (enforced by a test in
// internal/i18n); logs go to stderr through the redacting slog handler.
package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/buildinfo"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/doctor"
	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/logging"
	"github.com/0xyu/aco/internal/secure"
)

// System is the process environment the CLI runs in (injectable for tests).
type System struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	Environ   func() []string
	HomeDir   func() (string, error)
	Getwd     func() (string, error)
	Exec      doctor.Exec
	Keychain  auth.KeyStore // nil: no keychain support yet
}

// OSSystem returns the real process environment.
func OSSystem() System {
	return System{
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		LookupEnv: os.LookupEnv,
		Environ:   os.Environ,
		HomeDir:   os.UserHomeDir,
		Getwd:     os.Getwd,
		Exec:      doctor.OSExec{},
	}
}

func (s System) envMap() map[string]string {
	m := map[string]string{}
	if s.Environ == nil {
		return m
	}
	for _, kv := range s.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (s System) home() string {
	if s.HomeDir != nil {
		if h, err := s.HomeDir(); err == nil && h != "" {
			return h
		}
	}
	return "."
}

func (s System) cwd() string {
	if s.Getwd != nil {
		if d, err := s.Getwd(); err == nil {
			return d
		}
	}
	return "."
}

// invocation is one parsed command call.
type invocation struct {
	ctx        context.Context
	sys        System
	cmd        *command
	fs         *flag.FlagSet
	strs       map[string]*string
	bools      map[string]*bool
	ints       map[string]*int
	multis     map[string]*multiFlag
	positional []string
	out        *i18n.Printer
	errOut     *i18n.Printer
	log        *slog.Logger
	redactor   *secure.Redactor
	loaded     *config.Loaded
	loadErr    error
	badFlag    badValue // set when a typed flag value failed to parse
}

func (inv *invocation) str(name string) string {
	if p := inv.strs[name]; p != nil {
		return *p
	}
	return ""
}

func (inv *invocation) flagBool(name string) bool {
	if p := inv.bools[name]; p != nil {
		return *p
	}
	return false
}

func (inv *invocation) multi(name string) []string {
	if p := inv.multis[name]; p != nil {
		return *p
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, sys System) int {
	red := secure.NewRedactor()
	red.AddFromEnv(sys.envMap())
	logger := newLogger(sys, red)

	if len(args) == 0 {
		lang := rootLang(sys)
		printRootHelp(newPrinter(sys.Stderr, lang, red))
		return exitcode.Usage
	}

	switch args[0] {
	case "-h", "--help", "-help":
		printRootHelp(newPrinter(sys.Stdout, rootLang(sys), red))
		return exitcode.Completed
	case "--version", "-version":
		args = []string{"version"}
	case "help":
		lang := rootLang(sys)
		if len(args) > 1 {
			if c := findCommand(args[1]); c != nil {
				inv := newInvocation(ctx, sys, c, logger, red)
				printCommandHelp(newPrinter(sys.Stdout, lang, red), c, inv.fs)
				return exitcode.Completed
			}
		}
		printRootHelp(newPrinter(sys.Stdout, lang, red))
		return exitcode.Completed
	}

	c := findCommand(args[0])
	if c == nil {
		p := newPrinter(sys.Stderr, rootLang(sys), red)
		p.Say(i18n.CLIUnknownCommand, red.Echo(args[0]))
		p.Blank()
		printRootHelp(p)
		return exitcode.Usage
	}

	inv := newInvocation(ctx, sys, c, logger, red)
	pos, wantHelp, perr := parseArgs(inv.fs, args[1:])
	if wantHelp && perr == nil {
		perr = flag.ErrHelp
	}
	inv.positional = pos

	// Load config (best effort) to learn the operator language.
	inv.loaded, inv.loadErr = inv.loadConfig()
	lang := i18n.Default
	if inv.loaded != nil {
		if l, ok := i18n.ParseLang(inv.loaded.Config.Language.Operator); ok {
			lang = l
		}
	}
	inv.out = newPrinter(sys.Stdout, lang, red)
	inv.errOut = newPrinter(sys.Stderr, lang, red)

	if errors.Is(perr, flag.ErrHelp) {
		printCommandHelp(inv.out, c, inv.fs)
		return exitcode.Completed
	}
	if perr != nil {
		renderFlagError(inv.errOut, red, perr, inv.badFlag)
		inv.errOut.Say(i18n.CLIErrHelpHint, c.name)
		return exitcode.Usage
	}

	if c.run == nil {
		inv.errOut.Say(i18n.CLINotImplemented, c.name)
		return exitcode.Usage
	}
	return c.run(inv)
}

func newLogger(sys System, red *secure.Redactor) *slog.Logger {
	var level, format string
	if sys.LookupEnv != nil {
		level, _ = sys.LookupEnv("ACO_LOG_LEVEL")
		format, _ = sys.LookupEnv("ACO_LOG_FORMAT")
	}
	return logging.New(sys.Stderr, logging.Options{
		Level:    logging.ParseLevel(level),
		JSON:     strings.EqualFold(format, "json"),
		Redactor: red,
	})
}

func newInvocation(ctx context.Context, sys System, c *command, log *slog.Logger, red *secure.Redactor) *invocation {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	inv := &invocation{
		ctx: ctx, sys: sys, cmd: c, fs: fs, log: log, redactor: red,
		strs: map[string]*string{}, bools: map[string]*bool{}, ints: map[string]*int{}, multis: map[string]*multiFlag{},
	}
	for _, name := range c.flags {
		d := flagDefs[name]
		switch d.kind {
		case kindString:
			inv.strs[name] = fs.String(name, d.def, string(d.usage))
		case kindBool:
			b := new(bool)
			inv.bools[name] = b
			fs.Var(&boolFlag{p: b, name: name, bad: &inv.badFlag}, name, string(d.usage))
		case kindInt:
			n := new(int)
			inv.ints[name] = n
			fs.Var(&intFlag{p: n, name: name, bad: &inv.badFlag}, name, string(d.usage))
		case kindMulti:
			m := &multiFlag{}
			inv.multis[name] = m
			fs.Var(m, name, string(d.usage))
		}
	}
	return inv
}

// newPrinter returns a printer whose output is masked by red, so no CLI
// message (config errors, flag errors, echoed arguments) can print a secret.
func newPrinter(w io.Writer, lang i18n.Lang, red *secure.Redactor) *i18n.Printer {
	return i18n.NewPrinter(w, lang).SetFilter(red.String)
}

func isHelpArg(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// parseArgs splits a command's arguments into flags and positional
// arguments, then parses the flags with fs.
//
//   - Flags may come after positional arguments ("aco resume <id> --retry").
//   - -h/--help anywhere (outside a flag value and before "--") asks for help;
//     it is pulled out so later flags such as --set language.operator=en
//     still apply to the help text.
//   - A non-boolean flag written without "=" takes the next token as its
//     value, even when that token looks like a flag or like --help.
//   - "--" ends flag parsing: every later token is positional.
func parseArgs(fs *flag.FlagSet, args []string) (pos []string, wantHelp bool, err error) {
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if isHelpArg(a) {
			wantHelp = true
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if _, _, hasValue := strings.Cut(name, "="); hasValue {
			continue
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return pos, wantHelp, err
	}
	// flagArgs holds only flags and their values, so nothing is left over.
	return pos, wantHelp, nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// loadConfig builds the layered config for this invocation.
func (inv *invocation) loadConfig() (*config.Loaded, error) {
	repo := inv.str("repo")
	if repo == "" {
		repo = inv.sys.cwd()
	}
	opts := config.LoadOptions{
		UserConfigPath: filepath.Join(doctor.DefaultACOHome(inv.sys.home()), "config.yaml"),
		RepoDir:        repo,
		ConfigFiles:    inv.multi("config"),
		Overrides:      inv.multi("set"),
	}
	return config.Load(opts)
}

// rootLang determines the operator language for root-level help: user
// config and the current directory's .aco.yaml; defaults on any error.
func rootLang(sys System) i18n.Lang {
	l, err := config.Load(config.LoadOptions{
		UserConfigPath: filepath.Join(doctor.DefaultACOHome(sys.home()), "config.yaml"),
		RepoDir:        sys.cwd(),
	})
	if err != nil {
		return i18n.Default
	}
	if lang, ok := i18n.ParseLang(l.Config.Language.Operator); ok {
		return lang
	}
	return i18n.Default
}

// loadEnvAndAuth loads the env file and resolves auth modes.
func (inv *invocation) loadEnvAndAuth(cfg *config.Config) (*auth.Env, *auth.Resolution, bool) {
	path, explicit := inv.str("env-file"), true
	if path == "" {
		path, explicit = cfg.Auth.EnvFile, false
	}
	var ef *auth.EnvFile
	if path != "" {
		path = auth.ExpandHome(path, inv.sys.home())
		var err error
		ef, err = auth.LoadEnvFile(path, explicit)
		if err != nil {
			renderEnvFileError(inv.errOut, path, err)
			return nil, nil, false
		}
		inv.redactor.AddSecret(ef.AllSecrets()...)
	}
	env := auth.NewEnv(inv.sys.LookupEnv, ef)
	res, err := auth.Resolve(auth.Flags{
		AuthMode:       inv.str("auth-mode"),
		ClaudeAuthMode: inv.str("claude-auth-mode"),
		CodexAuthMode:  inv.str("codex-auth-mode"),
	}, env, cfg.Auth, inv.loaded.Origin)
	if err != nil {
		renderAuthError(inv.errOut, inv.redactor, err)
		return nil, nil, false
	}
	return env, res, true
}

func runVersion(inv *invocation) int {
	if len(inv.positional) > 0 {
		inv.errOut.Say(i18n.CLIErrUnexpectedArgs, echoArgs(inv.redactor, inv.positional))
		return exitcode.Usage
	}
	v, c, d := buildinfo.Info()
	inv.out.Say(i18n.CLIVersion, v, c, d)
	return exitcode.Completed
}

func runDoctor(inv *invocation) int {
	if len(inv.positional) > 0 {
		inv.errOut.Say(i18n.CLIErrUnexpectedArgs, echoArgs(inv.redactor, inv.positional))
		return exitcode.Usage
	}
	if inv.loadErr != nil {
		renderConfigError(inv.errOut, inv.redactor, inv.loadErr)
		return exitcode.Usage
	}
	cfg := inv.loaded.Config
	env, res, ok := inv.loadEnvAndAuth(cfg)
	if !ok {
		return exitcode.Usage
	}
	inv.log.Debug("doctor starting", "claude_auth", res.Claude.Mode, "codex_auth", res.Codex.Mode)

	checks := doctor.Run(inv.ctx, doctor.Input{
		Exec:       inv.sys.Exec,
		Config:     inv.loaded,
		Resolution: res,
		Env:        env,
		Keys:       auth.CheckKeys(res, env, inv.sys.Keychain),
		ACOHome:    doctor.DefaultACOHome(inv.sys.home()),
		Repo:       inv.str("repo"),
		Security:   inv.flagBool("security"),
		Claude:     inv.claudeDoctor(cfg, res, env),
	})

	p := inv.out
	p.Say(i18n.DoctorTitle)
	fails, warns := 0, 0
	for _, c := range checks {
		var label i18n.Key
		switch c.Status {
		case doctor.OK:
			label = i18n.DoctorStatusOK
		case doctor.Warn:
			label, warns = i18n.DoctorStatusWarn, warns+1
		case doctor.Fail:
			label, fails = i18n.DoctorStatusFail, fails+1
		default:
			label = i18n.DoctorStatusInfo
		}
		p.Say(i18n.DoctorLine, i18n.M(label), inv.redactor.String(p.T(c.Msg.Key, c.Msg.Args...)))
	}
	if fails > 0 {
		p.Say(i18n.DoctorSummaryFail, fails, warns)
		return exitcode.PreflightFailed
	}
	p.Say(i18n.DoctorSummaryOK, warns)
	return exitcode.Completed
}

// echoArgs lists rejected positional arguments for a diagnostic, hiding any
// that might be credentials.
func echoArgs(red *secure.Redactor, args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = red.Echo(a)
	}
	return strings.Join(out, " ")
}
