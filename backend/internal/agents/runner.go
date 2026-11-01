package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"aidept/internal/approval"
	"aidept/internal/events"
	"aidept/internal/sandbox"
	"aidept/internal/store"
)

// Env holds everything an agent run needs.
type Env struct {
	Client       anthropic.Client
	Model        string
	Effort       string
	Store        *store.Store
	Bus          *events.Bus
	Sandbox      *sandbox.Sandbox
	Approvals    *approval.Manager
	WorkspaceDir string
	MemoryDir    string
	Defs         map[string]*Def
	// CreateTask is provided by the orchestrator for the manager's planning run.
	CreateTask func(requestID int64, title, desc, assignee string, deps []int64) (int64, error)
}

type run struct {
	env          *Env
	def          *Def
	requestID    int64
	task         *store.Task
	outcome      *Outcome
	remembered   bool
	createdTasks int
}

// Run executes one agent on one task until it calls a finishing tool (one of finishTools).
// task may be a synthetic task with ID 0 (the manager's planning/summary runs).
func (e *Env) Run(ctx context.Context, def *Def, requestID int64, task *store.Task, prompt string, finishTools []string) (*Outcome, error) {
	r := &run{env: e, def: def, requestID: requestID, task: task}

	tools := make([]anthropic.BetaToolUnionParam, 0, len(def.Tools))
	for _, name := range def.Tools {
		spec := toolSpecs[name]
		t := anthropic.BetaToolParam{
			Name:        name,
			Description: anthropic.String(spec.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: spec.Properties, Required: spec.Required},
		}
		tools = append(tools, anthropic.BetaToolUnionParam{OfTool: &t})
	}

	system := e.systemPrompt(def)
	messages := []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))}

	e.Bus.SetState(def.ID, events.StateWorking, "开始："+task.Title, requestID, task.ID, "", nil)

	nudges := 0
	for turn := 0; turn < def.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := anthropic.BetaMessageNewParams{
			Model:        anthropic.Model(e.Model),
			MaxTokens:    16000,
			System:       []anthropic.BetaTextBlockParam{{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
			Messages:     messages,
			Tools:        tools,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(e.Effort)},
			// Server-side refusal fallback: a declined request is re-served by a fallback model in the same call.
			Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
			Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		}
		resp, err := e.Client.Beta.Messages.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("%s: model call failed: %w", def.Name, err)
		}
		messages = append(messages, resp.ToParam())

		if resp.StopReason == anthropic.BetaStopReasonRefusal {
			return &Outcome{Kind: "block", Reason: "模型拒绝了该请求：" + resp.StopDetails.Explanation}, nil
		}

		var results []anthropic.BetaContentBlockParamUnion
		for _, block := range resp.Content {
			switch b := block.AsAny().(type) {
			case anthropic.BetaTextBlock:
				if txt := strings.TrimSpace(b.Text); txt != "" {
					e.Bus.Emit(events.AgentMessage, requestID, task.ID, def.ID, map[string]string{"text": txt})
				}
			case anthropic.BetaToolUseBlock:
				raw := json.RawMessage(b.JSON.Input.Raw())
				desc := describe(b.Name, raw)
				e.Bus.Activity(def.ID, desc, requestID, task.ID)
				e.Bus.Emit(events.ToolCall, requestID, task.ID, def.ID, map[string]any{
					"tool": b.Name, "description": desc, "input": json.RawMessage(redactInput(raw)), "tool_use_id": b.ID,
				})
				res := r.callTool(ctx, b.Name, raw)
				e.Bus.Emit(events.ToolResult, requestID, task.ID, def.ID, map[string]any{
					"tool": b.Name, "tool_use_id": b.ID, "is_error": res.isError, "output": truncate(res.text, 4000),
				})
				results = append(results, anthropic.NewBetaToolResultBlock(b.ID, res.text, res.isError))
			}
		}

		if r.outcome != nil && contains(finishTools, outcomeTool(r.outcome.Kind)) {
			return r.outcome, nil
		}
		if len(results) > 0 {
			messages = append(messages, anthropic.NewBetaUserMessage(results...))
			continue
		}
		// The model stopped without calling a finishing tool: nudge it.
		if resp.StopReason == anthropic.BetaStopReasonMaxTokens {
			messages = append(messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("Your reply was cut off. Continue, using smaller steps.")))
			continue
		}
		nudges++
		if nudges > 2 {
			return &Outcome{Kind: "block", Reason: "agent stopped without calling a finishing tool"}, nil
		}
		messages = append(messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
			"You have not finished. Continue the work, and end by calling one of: "+strings.Join(finishTools, ", ")+".")))
	}
	return &Outcome{Kind: "block", Reason: fmt.Sprintf("reached the %d-turn limit", def.MaxTurns)}, nil
}

func outcomeTool(kind string) string {
	switch kind {
	case "complete":
		return "complete_task"
	case "block":
		return "block_task"
	case "reject":
		return "reject_task"
	case "deliver":
		return "deliver"
	case "planned":
		return "finish_planning"
	}
	return kind
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// redactInput keeps event payloads small: long string fields (file contents) are shortened.
func redactInput(raw json.RawMessage) []byte {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return []byte(`{}`)
	}
	for k, v := range in {
		if s, ok := v.(string); ok && len(s) > 1500 {
			in[k] = s[:1500] + fmt.Sprintf("… (%d bytes)", len(s))
		}
	}
	b, _ := json.Marshal(in)
	return b
}

func (e *Env) systemPrompt(def *Def) string {
	var sb strings.Builder
	sb.WriteString(def.Body)
	sb.WriteString("\n\n# 部门与工作环境\n")
	sb.WriteString("你是「个人 AI IT 部门」的一员。部门成员：\n")
	ids := make([]string, 0, len(e.Defs))
	for id := range e.Defs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return indexOf(Order, ids[i]) < indexOf(Order, ids[j]) })
	for _, id := range ids {
		d := e.Defs[id]
		fmt.Fprintf(&sb, "- %s（id: %s）：%s\n", d.Name, d.ID, d.Title)
	}
	sb.WriteString(`
- 工作目录是共享的 workspace（沙箱内路径 /workspace）。文件工具的路径都相对于 workspace 根目录。
- shell 命令在 Docker 沙箱中执行，沙箱可以访问互联网。高风险命令会暂停等待人类审批，被拒绝后不要重试同一操作。
- 只使用你被授权的工具；超出职责范围的事情用 block_task 上报，不要越权。
- 实事求是：只报告你真正执行并验证过的结果，不要编造输出。
- 对用户可见的说明使用中文；代码、文件名、命令使用英文。
`)
	if mems, err := e.Store.ListMemories(def.ID, 30); err == nil && len(mems) > 0 {
		sb.WriteString("\n# 你的长期记忆（过去任务积累的经验）\n")
		for i := len(mems) - 1; i >= 0; i-- {
			sb.WriteString("- " + mems[i].Note + "\n")
		}
	}
	return sb.String()
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return len(xs)
}
