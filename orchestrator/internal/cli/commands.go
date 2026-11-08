package cli

import (
	"github.com/0xyu/aco/internal/i18n"
)

type flagKind int

const (
	kindString flagKind = iota
	kindBool
	kindInt
	kindMulti
)

// flagDef describes one flag. Flags are shared across commands by name.
type flagDef struct {
	kind        flagKind
	usage       i18n.Key
	placeholder i18n.Key // "" for bool flags
	def         string   // default for string flags
}

// flagDefs is the registry of all CLI flags (§17.1).
var flagDefs = map[string]flagDef{
	"repo":             {kind: kindString, usage: i18n.FlagRepo, placeholder: i18n.PhPath},
	"config":           {kind: kindMulti, usage: i18n.FlagConfig, placeholder: i18n.PhFile},
	"set":              {kind: kindMulti, usage: i18n.FlagSet, placeholder: i18n.PhKeyValue},
	"env-file":         {kind: kindString, usage: i18n.FlagEnvFile, placeholder: i18n.PhFile},
	"auth-mode":        {kind: kindString, usage: i18n.FlagAuthMode, placeholder: i18n.PhMode},
	"claude-auth-mode": {kind: kindString, usage: i18n.FlagClaudeAuthMode, placeholder: i18n.PhMode},
	"codex-auth-mode":  {kind: kindString, usage: i18n.FlagCodexAuthMode, placeholder: i18n.PhMode},
	"requirements":     {kind: kindString, usage: i18n.FlagRequirements, placeholder: i18n.PhFile},
	"plan":             {kind: kindString, usage: i18n.FlagPlan, placeholder: i18n.PhFileOrDir},
	"base":             {kind: kindString, usage: i18n.FlagBase, placeholder: i18n.PhBranch},
	"detach":           {kind: kindBool, usage: i18n.FlagDetach},
	"retry":            {kind: kindBool, usage: i18n.FlagRetry},
	"from-phase":       {kind: kindInt, usage: i18n.FlagFromPhase, placeholder: i18n.PhN},
	"revise-plan":      {kind: kindBool, usage: i18n.FlagRevisePlan},
	"follow":           {kind: kindBool, usage: i18n.FlagFollow},
	"json":             {kind: kindBool, usage: i18n.FlagJSON},
	"phase":            {kind: kindInt, usage: i18n.FlagPhase, placeholder: i18n.PhN},
	"final":            {kind: kindBool, usage: i18n.FlagFinal},
	"format":           {kind: kindString, usage: i18n.FlagFormat, placeholder: i18n.PhFormat, def: "md"},
	"strict":           {kind: kindBool, usage: i18n.FlagStrict},
	"security":         {kind: kindBool, usage: i18n.FlagSecurity},
	"addr":             {kind: kindString, usage: i18n.FlagAddr, placeholder: i18n.PhAddr, def: "127.0.0.1:7420"},
	"transport":        {kind: kindString, usage: i18n.FlagTransport, placeholder: i18n.PhTransport, def: "stdio"},
	"run":              {kind: kindString, usage: i18n.FlagRunID, placeholder: i18n.PhRunID},
}

var (
	configFlags = []string{"repo", "config", "set", "env-file"}
	authFlags   = []string{"auth-mode", "claude-auth-mode", "codex-auth-mode"}
)

func flagsOf(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// command is one CLI command.
type command struct {
	name     string
	summary  i18n.Key
	args     i18n.Key // positional synopsis; "" = flags only
	flags    []string
	internal bool
	run      func(inv *invocation) int // nil = stub ("not implemented")
}

// commands returns the command tree in display order.
func commands() []*command {
	return []*command{
		{name: "run", summary: i18n.CmdRunSummary,
			flags: flagsOf([]string{"repo", "requirements", "plan", "base", "detach"}, configFlags[1:], authFlags)},
		{name: "plan", summary: i18n.CmdPlanSummary,
			flags: flagsOf([]string{"repo", "requirements", "base"}, configFlags[1:], authFlags)},
		{name: "resume", summary: i18n.CmdResumeSummary, args: i18n.CmdArgsRunID,
			flags: flagsOf([]string{"retry", "from-phase", "revise-plan"}, configFlags, authFlags)},
		{name: "status", summary: i18n.CmdStatusSummary, args: i18n.CmdArgsOptionalRunID,
			flags: []string{"follow", "json"}},
		{name: "pause", summary: i18n.CmdPauseSummary, args: i18n.CmdArgsRunID},
		{name: "cancel", summary: i18n.CmdCancelSummary, args: i18n.CmdArgsRunID},
		{name: "report", summary: i18n.CmdReportSummary, args: i18n.CmdArgsRunID,
			flags: []string{"phase", "final", "format"}},
		{name: "validate-plan", summary: i18n.CmdValidatePlanSummary, args: i18n.CmdArgsPlanPath,
			flags: []string{"strict", "json"}},
		{name: "doctor", summary: i18n.CmdDoctorSummary,
			flags: flagsOf(configFlags, authFlags, []string{"security"}), run: runDoctor},
		{name: "serve", summary: i18n.CmdServeSummary,
			flags: flagsOf([]string{"addr"}, configFlags)},
		{name: "mcp", summary: i18n.CmdMCPSummary,
			flags: []string{"transport"}},
		{name: "hook", summary: i18n.CmdHookSummary, args: i18n.CmdArgsHookEvent,
			flags: []string{"run"}, internal: true},
		{name: "version", summary: i18n.CmdVersionSummary, run: runVersion},
	}
}

func findCommand(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return c
		}
	}
	return nil
}
