// Package orchestrator turns a request into tasks, schedules them across agents and collects the delivery.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"aidept/internal/agents"
	"aidept/internal/events"
	"aidept/internal/store"
)

const maxRework = 2

type Orchestrator struct {
	Env *agents.Env
	// HandoffDelay is how long a handoff walk takes before the receiver starts working.
	HandoffDelay time.Duration
	// MeetingDuration is how long participants stay in the meeting room.
	MeetingDuration time.Duration

	queue chan *store.Request
	mu    sync.Mutex
	busy  bool
}

func New(env *agents.Env) *Orchestrator {
	o := &Orchestrator{Env: env, HandoffDelay: 4 * time.Second, MeetingDuration: 8 * time.Second, queue: make(chan *store.Request, 32)}
	env.CreateTask = o.createTask
	return o
}

func (o *Orchestrator) st() *store.Store  { return o.Env.Store }
func (o *Orchestrator) bus() *events.Bus  { return o.Env.Bus }
func (o *Orchestrator) def(id string) *agents.Def { return o.Env.Defs[id] }

// Start processes queued requests one at a time.
func (o *Orchestrator) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-o.queue:
				if err := o.Process(ctx, req); err != nil {
					log.Printf("request #%d failed: %v", req.ID, err)
				}
			}
		}
	}()
}

// Submit records a request and queues it.
func (o *Orchestrator) Submit(text string) (*store.Request, error) {
	req, err := o.st().CreateRequest(text)
	if err != nil {
		return nil, err
	}
	o.bus().Emit(events.RequestCreated, req.ID, 0, "", req)
	o.queue <- req
	return req, nil
}

func (o *Orchestrator) setRequest(req *store.Request, status, summary string) {
	req.Status = status
	if summary != "" {
		req.Summary = summary
	}
	_ = o.st().UpdateRequest(req.ID, status, summary)
	o.bus().Emit(events.RequestUpdated, req.ID, 0, "", req)
}

func (o *Orchestrator) fail(req *store.Request, msg string) error {
	o.bus().Emit(events.Error, req.ID, 0, "", map[string]string{"message": msg})
	o.setRequest(req, "failed", msg)
	o.bus().SetState("max", events.StateIdle, "", req.ID, 0, "", nil)
	return fmt.Errorf("%s", msg)
}

// Process runs the whole lifecycle of a request: plan → kickoff → execute → deliver.
func (o *Orchestrator) Process(ctx context.Context, req *store.Request) error {
	maxDef := o.def("max")

	// 1. Max decomposes the request.
	planTask := &store.Task{RequestID: req.ID, Title: "拆解需求", Status: store.StatusInProgress}
	out, err := o.Env.Run(ctx, maxDef, req.ID, planTask, planningPrompt(req.Text), []string{"finish_planning"})
	if err != nil {
		return o.fail(req, "规划失败："+err.Error())
	}
	if out.Kind != "planned" {
		return o.fail(req, "Max 未能完成拆解："+out.Reason)
	}
	tasks, _ := o.st().ListTasks(req.ID)
	if len(tasks) == 0 {
		return o.fail(req, "Max 没有创建任何任务")
	}
	o.setRequest(req, "running", "")
	o.bus().Emit(events.AgentMessage, req.ID, 0, "max", map[string]string{"text": "执行计划：" + out.Summary})

	// 2. Kickoff meeting when several people collaborate on this request.
	assignees := uniqueAssignees(tasks)
	if len(assignees) >= 2 {
		o.meeting(ctx, req.ID, 0, append([]string{"max"}, assignees...), "需求启动会", out.Summary)
	} else {
		o.bus().SetState("max", events.StateIdle, "", req.ID, 0, "", nil)
	}

	// 3. Execute all tasks.
	o.schedule(ctx, req)
	if ctx.Err() != nil {
		return o.fail(req, "已取消")
	}

	// 4. Max reviews results and delivers.
	tasks, _ = o.st().ListTasks(req.ID)
	sumTask := &store.Task{RequestID: req.ID, Title: "汇总交付", Status: store.StatusInProgress}
	out, err = o.Env.Run(ctx, maxDef, req.ID, sumTask, summaryPrompt(req.Text, tasks), []string{"deliver"})
	o.bus().SetState("max", events.StateIdle, "", req.ID, 0, "", nil)
	if err != nil {
		return o.fail(req, "汇总失败："+err.Error())
	}
	status := "done"
	for _, t := range tasks {
		if t.Status == store.StatusBlocked {
			status = "failed"
		}
	}
	summary := out.Summary
	if out.Kind != "deliver" {
		summary = "交付未完成：" + out.Reason
		status = "failed"
	}
	o.setRequest(req, status, summary)
	return nil
}

// RunDirect assigns one task straight to one agent (no planning). Used to verify a single agent.
func (o *Orchestrator) RunDirect(ctx context.Context, agentID, text string) (*store.Request, error) {
	if o.def(agentID) == nil {
		return nil, fmt.Errorf("unknown agent %q", agentID)
	}
	req, err := o.st().CreateRequest(text)
	if err != nil {
		return nil, err
	}
	o.bus().Emit(events.RequestCreated, req.ID, 0, "", req)
	if _, err := o.createTask(req.ID, truncateRunes(text, 40), text, agentID, nil); err != nil {
		return nil, err
	}
	o.setRequest(req, "running", "")
	o.schedule(ctx, req)
	tasks, _ := o.st().ListTasks(req.ID)
	status, summary := "done", tasks[0].Result
	if tasks[0].Status == store.StatusBlocked {
		status = "failed"
	}
	o.setRequest(req, status, summary)
	return req, nil
}

func (o *Orchestrator) createTask(requestID int64, title, desc, assignee string, deps []int64) (int64, error) {
	if assignee == "max" || o.def(assignee) == nil {
		return 0, fmt.Errorf("invalid assignee %q (use dev, ops, sec, qa or doc)", assignee)
	}
	for _, d := range deps {
		t, err := o.st().GetTask(d)
		if err != nil || t.RequestID != requestID {
			return 0, fmt.Errorf("depends_on references unknown task #%d", d)
		}
	}
	t := &store.Task{RequestID: requestID, Title: title, Description: desc, Assignee: assignee, DependsOn: deps}
	if err := o.st().CreateTask(t); err != nil {
		return 0, err
	}
	o.bus().Emit(events.TaskCreated, requestID, t.ID, assignee, t)
	return t.ID, nil
}

func (o *Orchestrator) saveTask(t *store.Task) {
	if err := o.st().SaveTask(t); err != nil {
		log.Printf("save task #%d: %v", t.ID, err)
	}
	o.bus().Emit(events.TaskUpdated, t.RequestID, t.ID, t.Assignee, t)
}

// ---------- meetings & handoffs ----------

func (o *Orchestrator) meeting(ctx context.Context, requestID, taskID int64, participants []string, topic, agenda string) {
	for _, p := range participants {
		o.bus().SetState(p, events.StateMeeting, topic, requestID, taskID, "", participants)
	}
	o.bus().Emit(events.MeetingStart, requestID, taskID, "", map[string]any{"participants": participants, "topic": topic, "agenda": agenda})
	sleep(ctx, o.MeetingDuration)
	o.bus().Emit(events.MeetingEnd, requestID, taskID, "", map[string]any{"participants": participants, "topic": topic})
	for _, p := range participants {
		o.bus().SetState(p, events.StateIdle, "", requestID, taskID, "", nil)
	}
}

// handoff: the giver walks to the receiver's desk with the task; the receiver starts afterwards.
func (o *Orchestrator) handoff(ctx context.Context, from string, t *store.Task, note string) {
	if from == "" || from == t.Assignee {
		return
	}
	giverIdle := o.bus().PresenceOf(from).State == events.StateIdle
	if giverIdle {
		o.bus().SetState(from, events.StateHandoff, fmt.Sprintf("把任务 #%d 交给 %s", t.ID, o.def(t.Assignee).Name), t.RequestID, t.ID, t.Assignee, nil)
	}
	o.bus().Emit(events.Handoff, t.RequestID, t.ID, from, map[string]any{
		"from": from, "to": t.Assignee, "task_id": t.ID, "title": t.Title, "note": truncateRunes(note, 300), "walk": giverIdle,
	})
	sleep(ctx, o.HandoffDelay)
	if giverIdle && o.bus().PresenceOf(from).State == events.StateHandoff {
		o.bus().SetState(from, events.StateIdle, "", t.RequestID, 0, "", nil)
	}
}

// ---------- scheduling ----------

type taskDone struct {
	task    *store.Task
	outcome *agents.Outcome
	err     error
}

func (o *Orchestrator) schedule(ctx context.Context, req *store.Request) {
	busy := map[string]bool{}
	doneCh := make(chan taskDone)
	giverOverride := map[int64]string{} // e.g. QA hands a rejected task back to Dev
	running := 0

	for {
		tasks, err := o.st().ListTasks(req.ID)
		if err != nil {
			log.Printf("list tasks: %v", err)
			return
		}
		byID := map[int64]*store.Task{}
		for _, t := range tasks {
			byID[t.ID] = t
		}
		// Propagate blocked dependencies.
		for _, t := range tasks {
			if t.Status != store.StatusTodo {
				continue
			}
			for _, d := range t.DependsOn {
				if dt := byID[d]; dt != nil && dt.Status == store.StatusBlocked {
					t.Status = store.StatusBlocked
					t.Result = fmt.Sprintf("上游任务 #%d 被阻塞", d)
					o.saveTask(t)
					break
				}
			}
		}
		// Dispatch ready tasks.
		if ctx.Err() == nil {
			for _, t := range tasks {
				if t.Status != store.StatusTodo || busy[t.Assignee] || !depsReady(t, byID) {
					continue
				}
				busy[t.Assignee] = true
				running++
				giver := giverOverride[t.ID]
				delete(giverOverride, t.ID)
				if giver == "" {
					giver = handoffGiver(t, byID)
				}
				go func(t *store.Task, giver string) {
					o.handoff(ctx, giver, t, handoffNote(t, byID))
					out, err := o.execTask(ctx, req, t, byID)
					doneCh <- taskDone{task: t, outcome: out, err: err}
				}(t, giver)
			}
		}
		if running == 0 {
			// Nothing running: either finished or stuck.
			for _, t := range tasks {
				if t.Status == store.StatusTodo {
					t.Status = store.StatusBlocked
					t.Result = "无法调度（依赖未满足或已取消）"
					o.saveTask(t)
				}
			}
			return
		}

		d := <-doneCh
		running--
		busy[d.task.Assignee] = false
		o.bus().SetState(d.task.Assignee, events.StateIdle, "", req.ID, 0, "", nil)
		o.handleDone(ctx, req, d, giverOverride)
	}
}

func depsReady(t *store.Task, byID map[int64]*store.Task) bool {
	for _, d := range t.DependsOn {
		dt := byID[d]
		if dt == nil || (dt.Status != store.StatusDone && dt.Status != store.StatusReview) {
			return false
		}
	}
	return true
}

// handoffGiver picks who brings the task to the assignee: the most recent upstream colleague, else the manager.
func handoffGiver(t *store.Task, byID map[int64]*store.Task) string {
	var best *store.Task
	for _, d := range t.DependsOn {
		dt := byID[d]
		if dt == nil || dt.Assignee == t.Assignee {
			continue
		}
		if best == nil || dt.UpdatedAt > best.UpdatedAt {
			best = dt
		}
	}
	if best != nil {
		return best.Assignee
	}
	for _, d := range t.DependsOn {
		if dt := byID[d]; dt != nil && dt.Assignee == t.Assignee {
			return "" // same person continues their own work
		}
	}
	return "max"
}

func handoffNote(t *store.Task, byID map[int64]*store.Task) string {
	if t.Notes != "" {
		return lastNote(t.Notes)
	}
	var parts []string
	for _, d := range t.DependsOn {
		if dt := byID[d]; dt != nil {
			parts = append(parts, fmt.Sprintf("#%d %s", dt.ID, truncateRunes(dt.Result, 120)))
		}
	}
	if len(parts) == 0 {
		return t.Description
	}
	return strings.Join(parts, "\n")
}

func (o *Orchestrator) execTask(ctx context.Context, req *store.Request, t *store.Task, byID map[int64]*store.Task) (*agents.Outcome, error) {
	t.Status = store.StatusInProgress
	o.saveTask(t)
	finish := []string{"complete_task", "block_task"}
	if t.Assignee == "qa" {
		finish = append(finish, "reject_task")
	}
	return o.Env.Run(ctx, o.def(t.Assignee), req.ID, t, taskPrompt(req, t, byID), finish)
}

func isReviewer(agentID string) bool { return agentID == "qa" || agentID == "sec" }

func (o *Orchestrator) handleDone(ctx context.Context, req *store.Request, d taskDone, giverOverride map[int64]string) {
	t, err := o.st().GetTask(d.task.ID)
	if err != nil {
		return
	}
	if d.err != nil {
		t.Status = store.StatusBlocked
		t.Result = "执行出错：" + d.err.Error()
		o.saveTask(t)
		return
	}
	out := d.outcome
	tasks, _ := o.st().ListTasks(req.ID)

	switch out.Kind {
	case "complete":
		t.Result = out.Summary
		if len(out.Artifacts) > 0 {
			t.Result += "\n\n产出：" + strings.Join(out.Artifacts, ", ")
		}
		t.Status = store.StatusDone
		if hasPendingReviewer(t.ID, tasks) {
			t.Status = store.StatusReview
		}
		o.saveTask(t)
		if isReviewer(t.Assignee) {
			o.releaseReviewed(t, tasks)
		}
	case "block":
		t.Status = store.StatusBlocked
		t.Result = "阻塞：" + out.Reason
		o.saveTask(t)
	case "reject":
		target, err := o.st().GetTask(out.TargetTaskID)
		if err != nil {
			t.Status = store.StatusBlocked
			t.Result = "打回目标不存在"
			o.saveTask(t)
			return
		}
		target.ReworkCount++
		target.Notes += fmt.Sprintf("\n[第 %d 次打回 by %s] %s", target.ReworkCount, o.def(t.Assignee).Name, out.Reason)
		if target.ReworkCount > maxRework {
			target.Status = store.StatusBlocked
			target.Result += "\n\n超过返工上限，仍未通过测试。"
			o.saveTask(target)
			t.Status = store.StatusBlocked
			t.Result = "多次打回仍未通过：" + out.Reason
			o.saveTask(t)
			return
		}
		// Defect review meeting between the tester and the developer, then rework and re-test.
		t.Status = store.StatusTodo
		t.Result = "打回：" + out.Reason
		o.saveTask(t)
		o.meeting(ctx, req.ID, target.ID, []string{t.Assignee, target.Assignee}, fmt.Sprintf("缺陷评审：任务 #%d", target.ID), out.Reason)
		target.Status = store.StatusTodo
		o.saveTask(target)
		giverOverride[target.ID] = t.Assignee
	}
}

func hasPendingReviewer(id int64, tasks []*store.Task) bool {
	for _, x := range tasks {
		if isReviewer(x.Assignee) && x.Status != store.StatusDone && x.Status != store.StatusBlocked && dependsOn(x, id) {
			return true
		}
	}
	return false
}

// releaseReviewed moves upstream tasks from review to done once all their reviewers passed.
func (o *Orchestrator) releaseReviewed(reviewer *store.Task, tasks []*store.Task) {
	byID := map[int64]*store.Task{}
	for _, x := range tasks {
		byID[x.ID] = x
	}
	byID[reviewer.ID] = reviewer
	for _, d := range reviewer.DependsOn {
		up := byID[d]
		if up == nil || up.Status != store.StatusReview {
			continue
		}
		pending := false
		for _, x := range byID {
			if isReviewer(x.Assignee) && dependsOn(x, up.ID) && x.Status != store.StatusDone {
				pending = true
			}
		}
		if !pending {
			up.Status = store.StatusDone
			o.saveTask(up)
		}
	}
}

func dependsOn(t *store.Task, id int64) bool {
	for _, d := range t.DependsOn {
		if d == id {
			return true
		}
	}
	return false
}

func uniqueAssignees(tasks []*store.Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tasks {
		if !seen[t.Assignee] {
			seen[t.Assignee] = true
			out = append(out, t.Assignee)
		}
	}
	order := map[string]int{}
	for i, id := range agents.Order {
		order[id] = i
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// ---------- prompts ----------

func planningPrompt(text string) string {
	return fmt.Sprintf(`老板提出了一个新需求：

<request>
%s
</request>

请先查看 workspace 现状，然后用 create_task 把需求拆成分派给同事的子任务（写清楚目录、文件名、验收标准，并用 depends_on 设置顺序），最后调用 finish_planning。`, text)
}

func taskPrompt(req *store.Request, t *store.Task, byID map[int64]*store.Task) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "老板的原始需求：\n<request>\n%s\n</request>\n\n", req.Text)
	fmt.Fprintf(&sb, "你的任务 #%d：%s\n<task>\n%s\n</task>\n", t.ID, t.Title, t.Description)
	if len(t.DependsOn) > 0 {
		sb.WriteString("\n上游任务（已完成，可在 workspace 中查看其产出）：\n")
		for _, d := range t.DependsOn {
			if dt := byID[d]; dt != nil {
				fmt.Fprintf(&sb, "- 任务 #%d（%s，负责人 %s）：%s\n  结果：%s\n", dt.ID, dt.Title, dt.Assignee, dt.Description, dt.Result)
			}
		}
	}
	if t.Notes != "" {
		fmt.Fprintf(&sb, "\n⚠️ 这是返工。此前的反馈：%s\n请先复现并修复这些问题，再重新运行全部测试。\n", t.Notes)
	}
	if t.Assignee == "qa" && t.Result != "" {
		fmt.Fprintf(&sb, "\n你上一轮的测试结论：%s\n开发已返工，请重新完整测试。\n", t.Result)
	}
	sb.WriteString("\n完成后先调用 remember，再调用 complete_task")
	if t.Assignee == "qa" {
		sb.WriteString("（发现缺陷则调用 reject_task 打回给对应上游任务）")
	}
	sb.WriteString("；无法完成时调用 block_task。")
	return sb.String()
}

func summaryPrompt(text string, tasks []*store.Task) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "需求：\n<request>\n%s\n</request>\n\n所有任务已结束，结果如下：\n", text)
	for _, t := range tasks {
		fmt.Fprintf(&sb, "\n## 任务 #%d %s（%s，状态 %s，返工 %d 次）\n%s\n", t.ID, t.Title, t.Assignee, t.Status, t.ReworkCount, t.Result)
	}
	sb.WriteString("\n请用 read_file / list_files 核对关键产出确实存在，然后调用 remember 记录一条经验，最后调用 deliver 给老板写交付报告。")
	return sb.String()
}

// ---------- helpers ----------

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func lastNote(notes string) string {
	parts := strings.Split(strings.TrimSpace(notes), "\n[")
	return truncateRunes(parts[len(parts)-1], 300)
}
