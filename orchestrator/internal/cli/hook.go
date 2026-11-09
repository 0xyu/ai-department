package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/0xyu/aco/internal/agent/claudecode"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/doctor"
	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/policy"
)

// hookBlock is Claude Code's "block this tool call" exit code: stderr is
// shown to the model as the reason.
const hookBlock = 2

// maxHookInput bounds the hook's stdin (one tool call as JSON).
const maxHookInput = 4 << 20

type hookInput struct {
	SessionID     string         `json:"session_id"`
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	Cwd           string         `json:"cwd"`
}

// runHook implements `aco hook pre-tool-use --run <id> --policy <file>`,
// the PreToolUse guard of every Claude Code invocation (§16.2). Claude Code
// pipes the tool call as JSON on stdin; the hook allows it (exit 0, no
// output) or blocks it (exit 2, reason on stderr). Its output is protocol
// text for the model, not operator text, so it is written directly in
// English (masked). Every failure blocks: a hook that cannot decide denies.
func runHook(inv *invocation) int {
	if len(inv.positional) != 1 || inv.positional[0] != "pre-tool-use" {
		inv.errOut.Say(i18n.CLIErrUnexpectedArgs, echoArgs(inv.redactor, inv.positional))
		return exitcode.Usage
	}
	block := func(reason string) int {
		_, _ = inv.sys.Stderr.Write([]byte(inv.redactor.String(reason) + "\n"))
		return hookBlock
	}
	g, err := policy.ReadGuardPolicy(inv.str("policy"))
	if err != nil {
		return block("ACO guard: the guard policy cannot be read, so every tool call is blocked")
	}
	if run := inv.str("run"); run != "" && run != g.RunID {
		return block("ACO guard: the guard policy belongs to another run")
	}
	var in hookInput
	var stdin io.Reader = bytes.NewReader(nil)
	if inv.sys.Stdin != nil {
		stdin = inv.sys.Stdin
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil || len(raw) > maxHookInput || json.Unmarshal(raw, &in) != nil || in.ToolName == "" {
		return block("ACO guard: the tool call could not be read")
	}
	if in.HookEventName != "" && in.HookEventName != "PreToolUse" {
		return block("ACO guard: unexpected hook event")
	}
	d := g.Evaluate(policy.ToolCall{Tool: in.ToolName, Input: in.ToolInput, Cwd: in.Cwd})
	if d.Allow {
		return exitcode.Completed
	}
	if g.DenyLog != "" {
		rec, _ := json.Marshal(map[string]string{
			"time": time.Now().UTC().Format(time.RFC3339), "tool": d.Tool, "rule": d.Rule,
			"path": inv.redactor.String(d.Path),
		})
		if f, err := os.OpenFile(g.DenyLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(rec, '\n'))
			_ = f.Close()
		}
	}
	return block(d.Reason)
}

// claudeDoctor configures doctor's Claude Code checks; --probe adds a real call.
func (inv *invocation) claudeDoctor(cfg *config.Config, res *auth.Resolution, env *auth.Env) *doctor.ClaudeInput {
	ci := &doctor.ClaudeInput{Mode: res.Claude.Mode, StrictEffort: cfg.Implementer.StrictEffort}
	if inv.flagBool("probe") {
		opts := claudecode.OptionsFromConfig(cfg, res.Claude.Mode, env, inv.redactor)
		opts.Keys = inv.sys.Keychain
		if inv.sys.Environ != nil {
			opts.BaseEnv = inv.sys.Environ()
		}
		a := claudecode.New(opts)
		ci.Probe = a.Probe
	}
	return ci
}
