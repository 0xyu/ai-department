package gates

import (
	"fmt"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/plan"
)

// DefaultTimeout is the per-command timeout when gates.<type>.timeout is 0.
const DefaultTimeout = 30 * time.Minute

// Problem codes reported by Resolve (preflight).
const (
	ProblemUnresolved      = "gate_unresolved"            // required gate without a command
	ProblemUnjustified     = "not_applicable_unjustified" // not_applicable without the frozen justification
	ProblemCommandNotFound = "command_not_found"          // a command's program is not installed
	ProblemUnknownScanner  = "unknown_scanner"
)

// Problem is a preflight problem. Any problem fails the preflight.
type Problem struct {
	Gate    string `json:"gate"`
	Code    string `json:"code"`
	Class   Class  `json:"class"`
	Program string `json:"program,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Gate + ": " + p.Code + ": " + p.Message }

// ResolveInput selects the gate sources.
type ResolveInput struct {
	Mode Mode
	// Phase is the frozen Phase Specification (ModePhase).
	Phase *plan.Phase
	// Phases are all phases of the plan (ModeFull): their declared commands
	// are added to the whole-project commands.
	Phases []*plan.Phase
	Config config.GatesConfig
	// Dir is the worktree, inspected for auto-detection.
	Dir string
	// LookPath finds programs (preflight and optional tools); nil disables
	// both the preflight program check and optional-tool detection.
	LookPath LookPathFunc
}

// Resolve resolves the six gates in order: Phase Specification → config →
// auto-detection (§11.5). The returned problems are the preflight result.
func Resolve(in ResolveInput) ([]Spec, []Problem) {
	if in.Mode == "" {
		in.Mode = ModePhase
	}
	var det *Detection
	detect := func() *Detection {
		if det == nil {
			det = Detect(in.Dir, in.LookPath)
		}
		return det
	}
	var specs []Spec
	var probs []Problem
	for _, t := range Types {
		s, p := resolveOne(in, t, detect)
		specs = append(specs, s)
		probs = append(probs, p...)
	}
	return specs, probs
}

func gateConfig(c config.GatesConfig, t string) ([]string, time.Duration) {
	switch t {
	case Typecheck:
		return c.Typecheck.Commands, time.Duration(c.Typecheck.Timeout)
	case Lint:
		return c.Lint.Commands, time.Duration(c.Lint.Timeout)
	case Unit:
		return c.Unit.Commands, time.Duration(c.Unit.Timeout)
	case Integration:
		return c.Integration.Commands, time.Duration(c.Integration.Timeout)
	case Build:
		return c.Build.Commands, time.Duration(c.Build.Timeout)
	case Security:
		return c.Security.Commands, time.Duration(c.Security.Timeout)
	}
	return nil, 0
}

func toCommands(runs []string) []Command {
	out := make([]Command, 0, len(runs))
	for _, r := range runs {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, Command{Run: r})
		}
	}
	return out
}

func resolveOne(in ResolveInput, t string, detect func() *Detection) (Spec, []Problem) {
	cfgCmds, timeout := gateConfig(in.Config, t)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	s := Spec{Type: t, Required: true, Timeout: timeout}
	var probs []Problem
	var phaseScanners []string

	switch in.Mode {
	case ModeFull:
		// Whole project: config or auto-detected commands, plus every
		// command a phase declared. Not applicable only if every phase
		// said so and nothing else resolves.
		allNA := len(in.Phases) > 0
		var phaseCmds []Command
		var just []string
		for _, ph := range in.Phases {
			e, ok := ph.Gate(t)
			if !ok || !e.NotApplicable {
				allNA = false
			}
			if ok && e.NotApplicable {
				just = append(just, ph.ID+": "+e.Justification)
			}
			if ok {
				phaseCmds = append(phaseCmds, toCommands(e.Commands)...)
				phaseScanners = append(phaseScanners, e.Scanners...)
			}
		}
		base, src := toCommands(cfgCmds), SourceConfig
		if len(base) == 0 && in.Config.AutoDetect {
			base, src = detect().Commands[t], SourceAuto
		}
		for _, c := range phaseCmds {
			if !containsCommand(base, c) {
				base = append(base, c)
			}
		}
		if len(base) == 0 {
			src = SourceNone
		}
		s.Commands, s.Source = base, src
		if allNA && len(s.Commands) == 0 && t != Security {
			s.Required, s.Justification = false, strings.Join(just, "; ")
			return s, nil
		}
	default:
		var entry plan.GateEntry
		ok := false
		if in.Phase != nil {
			entry, ok = in.Phase.Gate(t)
		}
		if ok && entry.NotApplicable {
			s.Required = false
			s.Justification = strings.TrimSpace(entry.Justification)
			if s.Justification == "" {
				probs = append(probs, Problem{Gate: t, Code: ProblemUnjustified, Class: ClassInternal,
					Message: "not_applicable requires a justification in the frozen Phase Specification"})
			}
			return s, probs
		}
		switch {
		case ok && len(entry.Commands) > 0:
			s.Commands, s.Source = toCommands(entry.Commands), SourcePhase
		case len(cfgCmds) > 0:
			s.Commands, s.Source = toCommands(cfgCmds), SourceConfig
		case in.Config.AutoDetect:
			s.Commands, s.Source = append([]Command(nil), detect().Commands[t]...), SourceAuto
			if len(s.Commands) == 0 {
				s.Source = SourceNone
			}
		}
		if ok {
			phaseScanners = entry.Scanners
		}
	}

	if t == Security {
		s.Scanners = resolveScanners(in, phaseScanners, detect)
		for _, sc := range s.Scanners {
			c, known := ScannerCommand(sc, in.Dir)
			switch {
			case !known:
				probs = append(probs, Problem{Gate: t, Code: ProblemUnknownScanner, Class: ClassInternal,
					Message: "unknown security scanner " + sc})
			case c.Run != "":
				s.Commands = append(s.Commands, c)
			}
		}
		if s.Source == SourceNone {
			s.Source = SourceAuto
		}
	} else if len(s.Commands) == 0 {
		probs = append(probs, Problem{Gate: t, Code: ProblemUnresolved, Class: ClassEnvironment,
			Message: "required gate has no command (set it in the Phase Specification or gates." + t + ".commands)"})
	}

	if in.LookPath != nil {
		seen := map[string]bool{}
		for _, c := range s.Commands {
			for _, prog := range Programs(c.Run) {
				if seen[prog] {
					continue
				}
				seen[prog] = true
				if _, err := in.LookPath(prog); err != nil {
					probs = append(probs, Problem{Gate: t, Code: ProblemCommandNotFound, Class: ClassEnvironment,
						Program: prog, Message: fmt.Sprintf("program %q is not installed or not on PATH", prog)})
				}
			}
		}
	}
	return s, probs
}

func resolveScanners(in ResolveInput, phase []string, detect func() *Detection) []string {
	var src []string
	if len(phase) > 0 {
		src = phase
	} else {
		src = append(src, in.Config.Security.Scanners...)
		if in.Config.AutoDetect {
			src = append(src, detect().Scanners...)
		}
	}
	if len(src) == 0 {
		src = []string{ScannerSecrets}
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range src {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ScannerCommand returns the command for a security scanner. The built-in
// secrets scanner has no command (known, empty Run).
func ScannerCommand(name, dir string) (Command, bool) {
	switch name {
	case ScannerSecrets:
		return Command{Scanner: name}, true
	case ScannerGosec:
		return Command{Run: "gosec -quiet -fmt sarif -out {reports}/gosec.sarif ./...", Scanner: name}, true
	case ScannerGovulncheck:
		return Command{Run: "govulncheck -format json ./...", Scanner: name}, true
	case ScannerSemgrep:
		cfg := "auto"
		if fileExists(dir, ".semgrep.yml") {
			cfg = ".semgrep.yml"
		}
		return Command{Run: "semgrep scan --config " + cfg + " --metrics=off --quiet --sarif --output {reports}/semgrep.sarif", Scanner: name}, true
	case ScannerNpmAudit:
		return Command{Run: "npm audit --json", Scanner: name}, true
	case ScannerPipAudit:
		if fileExists(dir, "requirements.txt") {
			return Command{Run: "pip-audit -r requirements.txt -f json -o {reports}/pip-audit.json", Scanner: name}, true
		}
		return Command{Run: "pip-audit -f json -o {reports}/pip-audit.json", Scanner: name}, true
	case ScannerTrivy:
		return Command{Run: "trivy fs --quiet --scanners vuln --format sarif --output {reports}/trivy.sarif .", Scanner: name}, true
	}
	return Command{}, false
}

// shellBuiltins are not looked up on PATH.
var shellBuiltins = map[string]bool{
	"test": true, "[": true, "[[": true, "cd": true, "echo": true, "printf": true, "export": true, "set": true,
	"unset": true, "true": true, "false": true, "exit": true, "if": true, "then": true, "else": true, "elif": true,
	"fi": true, "for": true, "while": true, "until": true, "do": true, "done": true, "case": true, "esac": true,
	"!": true, "{": true, "}": true, "(": true, ")": true, ":": true, ".": true, "source": true, "eval": true,
	"exec": true, "command": true, "time": true, "env": true, "read": true, "local": true, "return": true,
	"shift": true, "trap": true, "wait": true, "pushd": true, "popd": true, "type": true,
}

// Programs returns the programs a shell command line starts (the first word
// of every simple command), skipping shell builtins, keywords, variable
// assignments, and words built from expansions. It is a best-effort reader
// for the preflight, not a shell parser: words it cannot judge are skipped,
// and a missing program is then caught at run time (exit 127).
func Programs(run string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seg := range splitCommands(run) {
		words := strings.Fields(seg)
		for len(words) > 0 {
			w := words[0]
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.ContainsAny(w[:strings.Index(w, "=")], `"'$/\`) {
				words = words[1:] // VAR=value prefix
				continue
			}
			break
		}
		if len(words) == 0 {
			continue
		}
		w := strings.Trim(words[0], `"'`)
		if w == "" || shellBuiltins[w] || strings.ContainsAny(w, "$`(){}*?<>|&;") {
			continue
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// splitCommands splits a command line at ;, &&, ||, |, newlines, and opening
// $( outside single and double quotes.
func splitCommands(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			if quote == '"' && c == '$' && i+1 < len(s) && s[i+1] == '(' {
				// $( inside double quotes starts a nested command.
				flush()
				i++
				continue
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || c == '\n' || c == '|' || c == '&' || c == '{' || c == '}':
			flush()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			flush()
			i++
		case c == '(' || c == ')':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
