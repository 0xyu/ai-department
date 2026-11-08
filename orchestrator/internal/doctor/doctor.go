// Package doctor implements `aco doctor` v1: environment and setup checks.
//
// Results are language-neutral (catalog keys + args); the CLI renders them in
// the operator language.
package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/redact"
)

// Status of one check.
type Status string

// Statuses.
const (
	OK   Status = "ok"
	Info Status = "info"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one doctor result.
type Check struct {
	ID     string // stable machine ID, e.g. "tool.git"
	Status Status
	Msg    i18n.Msg
}

// MinGitVersion is the oldest Git ACO supports (worktree and trailer features).
var MinGitVersion = [3]int{2, 30, 0}

// Exec runs external programs.
type Exec interface {
	LookPath(file string) (string, error)
	Output(ctx context.Context, dir, name string, args ...string) (string, error)
}

// Input is everything doctor inspects.
type Input struct {
	Exec       Exec
	Config     *config.Loaded
	Resolution *auth.Resolution
	Env        *auth.Env
	Keys       []auth.KeyStatus
	ACOHome    string // ~/.aco
	Repo       string // only set when --repo was given
	Security   bool   // --security
}

// Failed reports whether any check failed.
func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Run performs all checks in a fixed order.
func Run(ctx context.Context, in Input) []Check {
	var out []Check
	add := func(id string, st Status, key i18n.Key, args ...any) {
		out = append(out, Check{ID: id, Status: st, Msg: i18n.M(key, args...)})
	}

	// Config layers.
	if in.Config != nil {
		var parts []string
		for _, l := range in.Config.Layers {
			if l.Source != "" {
				parts = append(parts, l.Name+" ("+l.Source+")")
			} else {
				parts = append(parts, l.Name)
			}
		}
		add("config.layers", OK, i18n.DoctorConfigLayers, strings.Join(parts, ", "))
		if in.Config.Config.Reviewer.Model == "" {
			add("config.reviewer_model", Warn, i18n.DoctorReviewerModelEmpty)
		}
	}

	// Env file.
	if ef := in.Env.File(); ef != nil {
		switch {
		case !ef.Found:
			add("auth.env_file", Info, i18n.DoctorEnvFileMissing, ef.Path)
		case ef.LoosePerms:
			add("auth.env_file", Warn, i18n.DoctorEnvFileLoose, ef.Path)
		default:
			add("auth.env_file", OK, i18n.DoctorEnvFileLoaded, ef.Path)
		}
	}

	// Auth modes.
	if r := in.Resolution; r != nil {
		for _, a := range auth.Agents {
			am := r.For(a)
			add("auth.mode."+string(a), Info, i18n.DoctorAuthMode, AgentLabel(a), ModeMsg(am.Mode), SourceMsg(am.Source))
		}
		add("auth.fallback", Info, i18n.DoctorAuthFallback, string(r.Fallback), SourceMsg(r.FallbackSource))
	}

	// Required API keys.
	for _, k := range in.Keys {
		id := "auth.key." + string(k.Agent)
		switch {
		case k.Present:
			add(id, OK, i18n.DoctorKeyPresent, AgentLabel(k.Agent), k.Var, SourceMsg(k.Source))
		case k.Reason == auth.ReasonFallback:
			add(id, Fail, i18n.DoctorKeyMissingFallback, AgentLabel(k.Agent), k.Var)
		default:
			add(id, Fail, i18n.DoctorKeyMissingAPI, AgentLabel(k.Agent), k.Var)
		}
	}

	// Tools.
	out = append(out, toolCheck(ctx, in.Exec, "git", []string{"--version"}, &MinGitVersion))
	out = append(out, toolCheck(ctx, in.Exec, "claude", []string{"--version"}, nil))
	out = append(out, toolCheck(ctx, in.Exec, "codex", []string{"--version"}, nil))

	// ~/.aco writable.
	if err := checkWritable(in.ACOHome); err != nil {
		add("aco_home", Fail, i18n.DoctorHomeNotWritable, in.ACOHome, redact.New().String(err.Error()))
	} else {
		add("aco_home", OK, i18n.DoctorHomeWritable, in.ACOHome)
	}

	// Repository (only when --repo is given).
	if in.Repo != "" {
		top, err := in.Exec.Output(ctx, "", "git", "-C", in.Repo, "rev-parse", "--show-toplevel")
		if err != nil {
			add("repo", Fail, i18n.DoctorRepoInvalid, in.Repo)
		} else {
			add("repo", OK, i18n.DoctorRepoOK, strings.TrimSpace(top))
		}
	}

	if in.Security {
		add("security", Info, i18n.DoctorSecurityPending)
	}
	return out
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion extracts the first "X.Y[.Z]" from s.
func ParseVersion(s string) ([3]int, string, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, "", false
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		if m[i+1] != "" {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return v, m[0], true
}

func less(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func toolCheck(ctx context.Context, ex Exec, name string, args []string, min *[3]int) Check {
	id := "tool." + name
	if _, err := ex.LookPath(name); err != nil {
		return Check{ID: id, Status: Fail, Msg: i18n.M(i18n.DoctorToolMissing, name)}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	outText, err := ex.Output(cctx, "", name, args...)
	if err != nil {
		return Check{ID: id, Status: Fail, Msg: i18n.M(i18n.DoctorToolError, name, firstLine(redact.New().String(err.Error())))}
	}
	v, vs, ok := ParseVersion(outText)
	if !ok {
		return Check{ID: id, Status: Warn, Msg: i18n.M(i18n.DoctorToolUnknownVersion, name, firstLine(outText))}
	}
	if min != nil && less(v, *min) {
		minS := strconv.Itoa(min[0]) + "." + strconv.Itoa(min[1]) + "." + strconv.Itoa(min[2])
		return Check{ID: id, Status: Fail, Msg: i18n.M(i18n.DoctorToolTooOld, name, vs, minS)}
	}
	return Check{ID: id, Status: OK, Msg: i18n.M(i18n.DoctorToolOK, name, vs)}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.WriteString("ok")
	cerr := f.Close()
	rerr := os.Remove(name)
	for _, e := range []error{werr, cerr, rerr} {
		if e != nil {
			return e
		}
	}
	return nil
}

// AgentLabel is the display name of an agent (a product name, not translated).
func AgentLabel(a auth.Agent) string {
	if a == auth.Codex {
		return "Codex CLI"
	}
	return "Claude Code"
}

// ModeMsg renders an auth mode.
func ModeMsg(m auth.Mode) i18n.Msg {
	if m == auth.API {
		return i18n.M(i18n.AuthModeAPI)
	}
	return i18n.M(i18n.AuthModeSubscription)
}

// SourceMsg renders where a value came from.
func SourceMsg(s auth.Source) i18n.Msg {
	switch s.Kind {
	case auth.SourceFlag:
		return i18n.M(i18n.AuthSourceFlag, s.Name)
	case auth.SourceEnv:
		return i18n.M(i18n.AuthSourceEnv, s.Name)
	case auth.SourceEnvFile:
		return i18n.M(i18n.AuthSourceEnvFile, s.Name, s.Path)
	case auth.SourceConfig:
		if s.Path != "" {
			return i18n.M(i18n.AuthSourceConfigFile, s.Name, s.Path)
		}
		return i18n.M(i18n.AuthSourceConfig, s.Name)
	case auth.SourceKeychain:
		return i18n.M(i18n.AuthSourceKeychain, s.Name)
	default:
		return i18n.M(i18n.AuthSourceDefault)
	}
}

// OSExec is the real Exec. Child processes never receive secret variables.
type OSExec struct{}

// LookPath implements Exec.
func (OSExec) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Output implements Exec.
func (OSExec) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = scrubbedEnv()
	b, err := cmd.Output()
	return string(b), err
}

func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if redact.IsSecretVar(k) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// DefaultACOHome returns ~/.aco.
func DefaultACOHome(home string) string { return filepath.Join(home, ".aco") }
