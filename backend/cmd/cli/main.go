// Command cli runs one request from the terminal and prints every event.
//
//	go run ./cmd/cli "帮我写一个脚本..."            # Max plans and the team executes
//	go run ./cmd/cli -agent dev "写一个脚本..."      # give the task straight to one agent
//	go run ./cmd/cli -auto-approve ...               # approve every high-risk action (testing only)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"aidept/internal/app"
	"aidept/internal/events"
	"aidept/internal/store"
)

func main() {
	agentID := flag.String("agent", "", "run the request directly on this agent (skip Max)")
	autoApprove := flag.Bool("auto-approve", false, "approve all high-risk operations automatically")
	fast := flag.Bool("fast", false, "skip handoff/meeting pacing delays")
	flag.Parse()
	text := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if text == "" {
		fmt.Fprintln(os.Stderr, "usage: cli [-agent dev] [-auto-approve] \"request\"")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	a, err := app.New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup:", err)
		os.Exit(1)
	}
	if *fast {
		a.Orch.HandoffDelay, a.Orch.MeetingDuration = 0, 0
	}

	sub := a.Bus.Subscribe()
	approvals := make(chan store.Approval, 8)
	go func() {
		for e := range sub {
			printEvent(e)
			if e.Type == events.ApprovalRequest {
				var ap store.Approval
				_ = json.Unmarshal(e.Payload, &ap)
				approvals <- ap
			}
		}
	}()
	go func() {
		in := bufio.NewReader(os.Stdin)
		for ap := range approvals {
			if *autoApprove {
				fmt.Printf("    ✔ auto-approved #%d\n", ap.ID)
				_ = a.Approvals.Resolve(ap.ID, true)
				continue
			}
			fmt.Printf("\n⚠️  审批 #%d [%s] %s\n    %s\n    批准? (y/N): ", ap.ID, ap.AgentID, ap.Reason, ap.Detail)
			line, _ := in.ReadString('\n')
			_ = a.Approvals.Resolve(ap.ID, strings.EqualFold(strings.TrimSpace(line), "y"))
		}
	}()

	start := time.Now()
	var reqID int64
	if *agentID != "" {
		req, err := a.Orch.RunDirect(ctx, *agentID, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		reqID = req.ID
	} else {
		req, err := a.Store.CreateRequest(text)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		reqID = req.ID
		a.Bus.Emit(events.RequestCreated, req.ID, 0, "", req)
		if err := a.Orch.Process(ctx, req); err != nil {
			fmt.Fprintln(os.Stderr, "request failed:", err)
		}
	}
	time.Sleep(200 * time.Millisecond)

	fmt.Printf("\n================ 请求 #%d 结束（%s）================\n", reqID, time.Since(start).Round(time.Second))
	tasks, _ := a.Store.ListTasks(reqID)
	for _, t := range tasks {
		fmt.Printf("#%d [%s] %-6s %s\n", t.ID, t.Status, t.Assignee, t.Title)
	}
	reqs, _ := a.Store.ListRequests(50)
	for _, r := range reqs {
		if r.ID == reqID {
			fmt.Printf("\n状态：%s\n\n%s\n", r.Status, r.Summary)
		}
	}
}

func printEvent(e store.Event) {
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	s := func(k string) string { v, _ := p[k].(string); return v }
	ts := time.Now().Format("15:04:05")
	switch e.Type {
	case events.AgentState:
		fmt.Printf("%s  [%s] 状态 → %s %s\n", ts, e.AgentID, s("state"), s("activity"))
	case events.ToolCall:
		fmt.Printf("%s  [%s] 🔧 %s\n", ts, e.AgentID, s("description"))
	case events.ToolResult:
		out := strings.TrimSpace(s("output"))
		if len(out) > 300 {
			out = out[:300] + "…"
		}
		mark := "✓"
		if b, _ := p["is_error"].(bool); b {
			mark = "✗"
		}
		fmt.Printf("          %s %s\n", mark, strings.ReplaceAll(out, "\n", "\n            "))
	case events.AgentMessage:
		fmt.Printf("%s  [%s] 💬 %s\n", ts, e.AgentID, s("text"))
	case events.TaskCreated, events.TaskUpdated:
		fmt.Printf("%s  📋 任务 #%d %s [%s] → %s\n", ts, e.TaskID, s("title"), s("status"), s("assignee"))
	case events.Handoff:
		fmt.Printf("%s  🤝 %s 把任务 #%d 交给 %s\n", ts, s("from"), e.TaskID, s("to"))
	case events.MeetingStart:
		fmt.Printf("%s  👥 会议开始：%s %v\n", ts, s("topic"), p["participants"])
	case events.MeetingEnd:
		fmt.Printf("%s  👥 会议结束：%s\n", ts, s("topic"))
	case events.MemoryAdded:
		fmt.Printf("%s  [%s] 🧠 记忆：%s\n", ts, e.AgentID, s("note"))
	case events.RequestCreated, events.RequestUpdated:
		fmt.Printf("%s  📨 请求 #%d [%s]\n", ts, e.RequestID, s("status"))
	case events.ApprovalResolved:
		fmt.Printf("%s  审批结果 %v\n", ts, p["approved"])
	case events.Error:
		fmt.Printf("%s  ❌ %s\n", ts, s("message"))
	}
}
