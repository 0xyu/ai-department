package claudecode

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// HookCommand is the PreToolUse hook command line. Claude Code runs hook
// commands through a shell (sh/bash, or Git Bash on Windows), so paths are
// written with forward slashes and double-quoted, which both shells accept.
func HookCommand(acoBin, runID, policyPath string) string {
	q := func(s string) string {
		s = filepath.ToSlash(s)
		s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
		return `"` + s + `"`
	}
	return q(acoBin) + " hook pre-tool-use --run " + q(runID) + " --policy " + q(policyPath)
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type settingsFile struct {
	Hooks       map[string][]hookMatcher `json:"hooks"`
	Permissions struct {
		Deny []string `json:"deny"`
	} `json:"permissions"`
	EffortLevel     string `json:"effortLevel,omitempty"`
	DisableAllHooks bool   `json:"disableAllHooks"`
}

// HookTimeoutSeconds bounds one guard hook call.
const HookTimeoutSeconds = 30

// Settings returns the generated settings file: the guard hook on every tool
// ("*"), the profile's deny rules, and the effort level when the CLI has no
// --effort flag.
func Settings(hookCmd string, deny []string, effortViaSettings string) ([]byte, error) {
	var s settingsFile
	s.Hooks = map[string][]hookMatcher{
		"PreToolUse": {{Matcher: "*", Hooks: []hookEntry{{Type: "command", Command: hookCmd, Timeout: HookTimeoutSeconds}}}},
	}
	s.Permissions.Deny = append([]string{}, deny...)
	s.EffortLevel = effortViaSettings
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
