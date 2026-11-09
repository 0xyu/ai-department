package claudecode

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Stream-json messages. Only the fields ACO uses are decoded; unknown
// message types and fields are ignored (the format grows between releases).
type message struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`

	// system/init
	APIKeySource string   `json:"apiKeySource"`
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`
	Version      string   `json:"claude_code_version"`
	Permission   string   `json:"permissionMode"`

	// assistant
	Message *struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Error string `json:"error"` // assistant: "rate_limit", "authentication_failed", ...

	// result
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	NumTurns         int             `json:"num_turns"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int64           `json:"duration_ms"`
	Usage            *usageMsg       `json:"usage"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	APIErrorStatus   json.RawMessage `json:"api_error_status"`
	Errors           []string        `json:"errors"`

	// system/hook_started, system/hook_response (--include-hook-events)
	HookEvent string `json:"hook_event"`
	HookName  string `json:"hook_name"`
	ExitCode  *int   `json:"exit_code"`
	Outcome   string `json:"outcome"`

	// rate_limit_event
	RateLimit *struct {
		Status        string `json:"status"` // allowed | allowed_warning | rejected
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

type usageMsg struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// apiStatus returns api_error_status as a number (it may be null, a number,
// or a numeric string).
func (m *message) apiStatus() int {
	s := strings.Trim(string(bytes.TrimSpace(m.APIErrorStatus)), `"`)
	n, _ := strconv.Atoi(s)
	return n
}

// stream accumulates what one invocation printed.
type stream struct {
	init          *message
	result        *message
	rejected      *message // last rate_limit_event with status "rejected"
	assistantIDs  map[string]bool
	assistantErr  string // last assistant "error" field
	lastText      string // last assistant text block
	lines, broken int    // lines seen, lines that were not JSON
	sessionID     string
	toolUses      int // tool_use blocks in assistant messages
	guardHooks    int // PreToolUse hook responses
}

func newStream() *stream { return &stream{assistantIDs: map[string]bool{}} }

// feed parses one stdout line. Lines that are not JSON objects are counted
// and otherwise ignored.
func (s *stream) feed(line []byte) *message {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	s.lines++
	var m message
	if line[0] != '{' || json.Unmarshal(line, &m) != nil || m.Type == "" {
		s.broken++
		return nil
	}
	if m.SessionID != "" {
		s.sessionID = m.SessionID
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" && s.init == nil {
			mm := m
			s.init = &mm
		}
		if m.Subtype == "hook_response" && m.HookEvent == "PreToolUse" {
			s.guardHooks++
		}
	case "assistant":
		if m.Message != nil {
			id := m.Message.ID
			if id == "" {
				id = "#" + strconv.Itoa(len(s.assistantIDs))
			}
			s.assistantIDs[id] = true
			for _, c := range m.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					s.lastText = c.Text
				}
				if c.Type == "tool_use" {
					s.toolUses++
				}
			}
		}
		if m.Error != "" {
			s.assistantErr = m.Error
		}
	case "result":
		mm := m
		s.result = &mm
	case "rate_limit_event":
		if m.RateLimit != nil && m.RateLimit.Status == "rejected" {
			mm := m
			s.rejected = &mm
		}
	}
	return &m
}

// hookFailed reports a PreToolUse hook response that neither allowed (exit
// 0) nor blocked (exit 2) the call: the guard did not decide, and Claude
// Code treats that as a non-blocking error and runs the tool anyway.
func (m *message) hookFailed() bool {
	if m.Type != "system" || m.Subtype != "hook_response" || m.HookEvent != "PreToolUse" {
		return false
	}
	return m.ExitCode == nil || (*m.ExitCode != 0 && *m.ExitCode != 2)
}

// turns is the number of distinct assistant messages seen.
func (s *stream) turns() int { return len(s.assistantIDs) }

// extractJSON returns the structured output: the result's
// structured_output, or else a JSON object in the result text (fenced or
// bare), or nil.
func extractJSON(r *message) json.RawMessage {
	if r == nil {
		return nil
	}
	if so := bytes.TrimSpace(r.StructuredOutput); len(so) > 0 && !bytes.Equal(so, []byte("null")) {
		return so
	}
	text := strings.TrimSpace(r.Result)
	if i := strings.Index(text, "```json"); i >= 0 {
		rest := text[i+len("```json"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			text = strings.TrimSpace(rest[:j])
		}
	}
	i, j := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if i < 0 || j <= i {
		return nil
	}
	cand := []byte(text[i : j+1])
	if !json.Valid(cand) {
		return nil
	}
	return cand
}
