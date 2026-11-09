package codexreview

import (
	"encoding/json"
	"strings"
)

// Codex `exec --json` events (verified with codex-cli 0.162.0), one JSON
// object per line:
//
//	{"type":"thread.started","thread_id":"…"}
//	{"type":"turn.started"}
//	{"type":"item.started"|"item.updated"|"item.completed","item":{"id":"item_1","type":"command_execution",
//	   "command":"…","aggregated_output":"…","exit_code":0,"status":"completed"}}
//	{"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//	{"type":"item.completed","item":{"type":"error","message":"…"}}       (a warning; the turn goes on)
//	{"type":"error","message":"Reconnecting... 2/5 (…)"}                    (retried by the CLI)
//	{"type":"turn.completed","usage":{"input_tokens":…,"cached_input_tokens":…,"output_tokens":…,"reasoning_output_tokens":…}}
//	{"type":"turn.failed","error":{"message":"…"}}
//
// Other item types (reasoning, file_change, mcp_tool_call, web_search,
// todo_list, …) and unknown fields are tolerated.

type event struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *item      `json:"item"`
	Usage    *usage     `json:"usage"`
	Message  string     `json:"message"`
	Error    *errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
}

type item struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Message  string `json:"message"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Changes  []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Query  string `json:"query"`
}

type usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// stream accumulates what the adapter needs from the events.
type stream struct {
	threadID     string
	turns        int
	usage        usage
	completed    bool   // a turn.completed arrived
	failed       string // turn.failed message
	errors       []string
	warnings     []string // item-level error items
	lastMessage  string   // last agent_message text
	commands     int
	seenCommands map[string]bool
}

func newStream() *stream { return &stream{seenCommands: map[string]bool{}} }

// feed parses one line. Lines that are not JSON objects are ignored (the
// CLI may print diagnostics); the transcript keeps them.
func (s *stream) feed(line []byte) *event {
	var e event
	if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
		return nil
	}
	switch e.Type {
	case "thread.started":
		s.threadID = e.ThreadID
	case "turn.completed":
		s.completed = true
		s.turns++
		if e.Usage != nil {
			s.usage.InputTokens += e.Usage.InputTokens
			s.usage.CachedInputTokens += e.Usage.CachedInputTokens
			s.usage.CacheWriteInputTokens += e.Usage.CacheWriteInputTokens
			s.usage.OutputTokens += e.Usage.OutputTokens
			s.usage.ReasoningOutputTokens += e.Usage.ReasoningOutputTokens
		}
	case "turn.failed":
		s.turns++
		if e.Error != nil {
			s.failed = e.Error.Message
		}
		if s.failed == "" {
			s.failed = "turn failed"
		}
	case "error":
		s.errors = append(s.errors, e.Message)
	case "item.started", "item.updated", "item.completed":
		if e.Item == nil {
			return &e
		}
		switch e.Item.Type {
		case "agent_message":
			if e.Type == "item.completed" {
				s.lastMessage = e.Item.Text
			}
		case "error":
			if e.Type == "item.completed" {
				s.warnings = append(s.warnings, e.Item.Message)
			}
		case "command_execution":
			if !s.seenCommands[e.Item.ID] {
				s.seenCommands[e.Item.ID] = true
				s.commands++
			}
		}
	}
	return &e
}

// errorText joins everything the CLI reported as an error, for classification.
func (s *stream) errorText() string {
	parts := append([]string{s.failed}, s.errors...)
	return strings.Join(parts, "\n")
}

// ignoredConfig reports whether Codex said it ignored the config key.
func (s *stream) ignoredConfig(key string) bool {
	for _, w := range s.warnings {
		if strings.Contains(w, "unrecognized configuration") && strings.Contains(w, "`"+key+"`") {
			return true
		}
	}
	return false
}
