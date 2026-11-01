package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"aidept/internal/events"
	"aidept/internal/store"
)

type toolSpec struct {
	Description string
	Properties  map[string]any
	Required    []string
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// toolSpecs is the full catalogue. Each agent only gets the tools whitelisted in its definition file.
var toolSpecs = map[string]toolSpec{
	"list_files": {
		Description: "List files under a directory of the workspace (recursive, skips .git/node_modules/__pycache__).",
		Properties:  map[string]any{"path": str("Directory relative to the workspace root. Use '.' for the root.")},
	},
	"read_file": {
		Description: "Read a text file from the workspace.",
		Properties:  map[string]any{"path": str("File path relative to the workspace root.")},
		Required:    []string{"path"},
	},
	"search": {
		Description: "Search workspace files with a regular expression. Returns matching lines as path:line: text.",
		Properties:  map[string]any{"pattern": str("Go/RE2 regular expression."), "path": str("Optional sub-directory to search.")},
		Required:    []string{"pattern"},
	},
	"write_file": {
		Description: "Create or overwrite a file in the workspace with the given content. Parent directories are created.",
		Properties:  map[string]any{"path": str("File path relative to the workspace root."), "content": str("Full file content.")},
		Required:    []string{"path", "content"},
	},
	"edit_file": {
		Description: "Replace one exact occurrence of old_text with new_text in a workspace file.",
		Properties: map[string]any{
			"path": str("File path relative to the workspace root."), "old_text": str("Exact text to replace; must occur exactly once."), "new_text": str("Replacement text."),
		},
		Required: []string{"path", "old_text", "new_text"},
	},
	"delete_file": {
		Description: "Delete a file in the workspace. HIGH RISK: requires human approval.",
		Properties:  map[string]any{"path": str("File path relative to the workspace root."), "reason": str("Why it must be deleted.")},
		Required:    []string{"path", "reason"},
	},
	"shell": {
		Description: "Run a bash command inside the Docker sandbox. The workspace is mounted at /workspace (the working directory). " +
			"Available: python3, pip, pytest, node, npm, git, curl, jq. Commands that delete files, deploy, contact remote hosts, " +
			"push or send data externally, or spend money pause for human approval. Default timeout 120s (max 600).",
		Properties: map[string]any{
			"command": str("The bash command."), "workdir": str("Optional working directory relative to the workspace root."),
			"timeout_sec": integer("Optional timeout in seconds (max 600)."), "description": str("Short human-readable description of what the command does (shown to the user)."),
		},
		Required: []string{"command", "description"},
	},
	"ssh": {
		Description: "Run a command on a remote host over SSH from the sandbox. ALWAYS requires human approval.",
		Properties:  map[string]any{"host": str("user@host"), "command": str("Remote command."), "reason": str("Why this is needed.")},
		Required:    []string{"host", "command", "reason"},
	},
	"http_request": {
		Description: "Make an HTTP request from the backend. GET/HEAD run directly; any other method sends data externally and requires human approval.",
		Properties: map[string]any{
			"method": str("GET, HEAD, POST, PUT, PATCH or DELETE."), "url": str("Absolute URL."), "body": str("Optional request body."),
		},
		Required: []string{"method", "url"},
	},
	"create_task": {
		Description: "Create a sub-task on the department board and assign it to one employee. Returns the new task id, which later tasks can list in depends_on.",
		Properties: map[string]any{
			"title":       str("Short task title."),
			"description": str("Precise instructions: what to produce, file names, acceptance criteria."),
			"assignee":    map[string]any{"type": "string", "enum": []string{"dev", "ops", "sec", "qa", "doc"}},
			"depends_on":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Ids of tasks that must finish first."},
		},
		Required: []string{"title", "description", "assignee"},
	},
	"finish_planning": {
		Description: "Call once all sub-tasks have been created.",
		Properties:  map[string]any{"plan_summary": str("One-paragraph summary of the plan for the requester.")},
		Required:    []string{"plan_summary"},
	},
	"complete_task": {
		Description: "Finish your current task. You must call remember first. Summarize exactly what you produced and how it was verified.",
		Properties: map[string]any{
			"summary":   str("What was done and how it was verified (commands run, results)."),
			"artifacts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Workspace paths you produced or changed."},
		},
		Required: []string{"summary"},
	},
	"block_task": {
		Description: "Stop and escalate: you cannot finish the task (missing information, permission denied, out of scope).",
		Properties:  map[string]any{"reason": str("What blocks you and what is needed to continue.")},
		Required:    []string{"reason"},
	},
	"reject_task": {
		Description: "QA only: send a task back to its assignee because it fails testing. Include reproduction steps.",
		Properties: map[string]any{
			"task_id": integer("Id of the upstream task that failed."), "reason": str("Failing tests / reproduction steps / expected vs actual."),
		},
		Required: []string{"task_id", "reason"},
	},
	"deliver": {
		Description: "Manager only: deliver the final summary of the request to the requester. You must call remember first.",
		Properties:  map[string]any{"summary": str("Markdown delivery report: what was produced (paths), test results, open issues, how to run.")},
		Required:    []string{"summary"},
	},
	"remember": {
		Description: "Append a lesson to your long-term memory (reusable know-how for future tasks, not a task log).",
		Properties:  map[string]any{"note": str("One concise, reusable lesson.")},
		Required:    []string{"note"},
	},
}

// Outcome is how a run ended.
type Outcome struct {
	Kind         string   // complete | block | reject | deliver | planned
	Summary      string   // complete/deliver/planned summary
	Reason       string   // block / reject reason
	TargetTaskID int64    // reject target
	Artifacts    []string // produced files
}

// toolResult is what a tool handler returns.
type toolResult struct {
	text    string
	isError bool
}

func ok(s string) toolResult    { return toolResult{text: s} }
func fail(s string) toolResult  { return toolResult{text: s, isError: true} }
func failf(f string, a ...any) toolResult { return fail(fmt.Sprintf(f, a...)) }

// ---------- path helpers ----------

func (r *run) resolve(p string) (string, error) {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "/workspace")
	p = strings.TrimPrefix(p, "workspace/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "."
	}
	abs := filepath.Clean(filepath.Join(r.env.WorkspaceDir, filepath.FromSlash(p)))
	root := filepath.Clean(r.env.WorkspaceDir)
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return abs, nil
}

func (r *run) rel(abs string) string {
	rel, err := filepath.Rel(r.env.WorkspaceDir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".pytest_cache": true, ".venv": true, "venv": true}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n/2] + fmt.Sprintf("\n... [%d bytes truncated] ...\n", len(s)-n) + s[len(s)-n/2:]
}

// ---------- dispatch ----------

func (r *run) callTool(ctx context.Context, name string, raw json.RawMessage) toolResult {
	if !r.def.HasTool(name) {
		return failf("tool %q is not in your whitelist", name)
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid JSON input: %v", err)
	}
	s := func(k string) string { v, _ := in[k].(string); return v }
	switch name {
	case "list_files":
		return r.listFiles(s("path"))
	case "read_file":
		return r.readFile(s("path"))
	case "search":
		return r.search(s("pattern"), s("path"))
	case "write_file":
		return r.writeFile(s("path"), s("content"))
	case "edit_file":
		return r.editFile(s("path"), s("old_text"), s("new_text"))
	case "delete_file":
		return r.deleteFile(ctx, s("path"), s("reason"))
	case "shell":
		t, _ := in["timeout_sec"].(float64)
		return r.shell(ctx, s("command"), s("workdir"), int(t))
	case "ssh":
		return r.ssh(ctx, s("host"), s("command"), s("reason"))
	case "http_request":
		return r.httpRequest(ctx, s("method"), s("url"), s("body"))
	case "create_task":
		var deps []int64
		if arr, ok := in["depends_on"].([]any); ok {
			for _, v := range arr {
				if f, ok := v.(float64); ok {
					deps = append(deps, int64(f))
				}
			}
		}
		return r.createTask(s("title"), s("description"), s("assignee"), deps)
	case "finish_planning":
		if r.createdTasks == 0 {
			return fail("create at least one task with create_task first")
		}
		r.outcome = &Outcome{Kind: "planned", Summary: s("plan_summary")}
		return ok("plan recorded")
	case "complete_task":
		if !r.remembered {
			return fail("call remember with what you learned before completing the task, then call complete_task again")
		}
		var arts []string
		if arr, ok := in["artifacts"].([]any); ok {
			for _, v := range arr {
				if x, ok := v.(string); ok {
					arts = append(arts, x)
				}
			}
		}
		r.outcome = &Outcome{Kind: "complete", Summary: s("summary"), Artifacts: arts}
		return ok("task marked complete")
	case "block_task":
		r.outcome = &Outcome{Kind: "block", Reason: s("reason")}
		return ok("task blocked and escalated to the manager")
	case "reject_task":
		f, _ := in["task_id"].(float64)
		target := int64(f)
		if !r.isDependency(target) {
			return failf("task %d is not one of the tasks you are testing (%v)", target, r.task.DependsOn)
		}
		if !r.remembered {
			return fail("call remember with what you learned before rejecting, then call reject_task again")
		}
		r.outcome = &Outcome{Kind: "reject", Reason: s("reason"), TargetTaskID: target}
		return ok("task sent back for rework")
	case "deliver":
		if !r.remembered {
			return fail("call remember with what you learned before delivering, then call deliver again")
		}
		r.outcome = &Outcome{Kind: "deliver", Summary: s("summary")}
		return ok("delivered")
	case "remember":
		return r.remember(s("note"))
	}
	return failf("tool %q not implemented", name)
}

// describe produces the short human-readable activity text shown in the 3D speech bubble.
func describe(name string, raw json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	s := func(k string) string { v, _ := in[k].(string); return v }
	one := func(x string, n int) string {
		x = strings.TrimSpace(strings.SplitN(x, "\n", 2)[0])
		if r := []rune(x); len(r) > n {
			return string(r[:n]) + "…"
		}
		return x
	}
	switch name {
	case "list_files":
		return "浏览目录 " + one(s("path"), 40)
	case "read_file":
		return "阅读 " + one(s("path"), 50)
	case "search":
		return "搜索 " + one(s("pattern"), 40)
	case "write_file":
		return "编写 " + one(s("path"), 50)
	case "edit_file":
		return "修改 " + one(s("path"), 50)
	case "delete_file":
		return "删除 " + one(s("path"), 50)
	case "shell":
		if d := s("description"); d != "" {
			return one(d, 40) + "：$ " + one(s("command"), 50)
		}
		return "$ " + one(s("command"), 60)
	case "ssh":
		return "SSH " + s("host") + ": " + one(s("command"), 40)
	case "http_request":
		return s("method") + " " + one(s("url"), 50)
	case "create_task":
		return "拆解任务：" + one(s("title"), 30) + " → " + s("assignee")
	case "finish_planning":
		return "确定执行计划"
	case "complete_task":
		return "提交成果"
	case "block_task":
		return "上报阻塞：" + one(s("reason"), 40)
	case "reject_task":
		return "打回任务：" + one(s("reason"), 40)
	case "deliver":
		return "汇总交付"
	case "remember":
		return "记录经验：" + one(s("note"), 40)
	}
	return name
}

// ---------- file tools ----------

func (r *run) listFiles(p string) toolResult {
	root, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	var lines []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		if len(lines) >= 400 {
			return fs.SkipAll
		}
		if d.IsDir() {
			lines = append(lines, r.rel(path)+"/")
		} else if info, err := d.Info(); err == nil {
			lines = append(lines, fmt.Sprintf("%s (%d bytes)", r.rel(path), info.Size()))
		}
		return nil
	})
	if len(lines) == 0 {
		return ok("(empty)")
	}
	sort.Strings(lines)
	return ok(strings.Join(lines, "\n"))
}

func (r *run) readFile(p string) toolResult {
	abs, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	return ok(truncate(string(b), 40000))
}

func (r *run) search(pattern, p string) toolResult {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return failf("bad regex: %v", err)
	}
	root, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	var hits []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= 200 {
			return fs.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for n := 1; sc.Scan(); n++ {
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", r.rel(path), n, truncate(sc.Text(), 200)))
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return ok("no matches")
	}
	return ok(strings.Join(hits, "\n"))
}

func (r *run) checkWritable(abs string) error {
	if r.def.ID == "doc" && !strings.EqualFold(filepath.Ext(abs), ".md") {
		return fmt.Errorf("Doc may only write Markdown (.md) files")
	}
	return nil
}

func (r *run) writeFile(p, content string) toolResult {
	abs, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := r.checkWritable(abs); err != nil {
		return fail(err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail(err.Error())
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fail(err.Error())
	}
	return ok(fmt.Sprintf("wrote %s (%d bytes)", r.rel(abs), len(content)))
}

func (r *run) editFile(p, oldText, newText string) toolResult {
	abs, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := r.checkWritable(abs); err != nil {
		return fail(err.Error())
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return fail(err.Error())
	}
	c := strings.Count(string(b), oldText)
	if c != 1 || oldText == "" {
		return failf("old_text must occur exactly once, found %d occurrences", c)
	}
	if err := os.WriteFile(abs, []byte(strings.Replace(string(b), oldText, newText, 1)), 0o644); err != nil {
		return fail(err.Error())
	}
	return ok("edited " + r.rel(abs))
}

func (r *run) deleteFile(ctx context.Context, p, reason string) toolResult {
	abs, err := r.resolve(p)
	if err != nil {
		return fail(err.Error())
	}
	if abs == filepath.Clean(r.env.WorkspaceDir) {
		return fail("refusing to delete the workspace root")
	}
	if approved, msg := r.approve(ctx, "delete_file", "删除 "+r.rel(abs), "删除："+reason); !approved {
		return fail(msg)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fail(err.Error())
	}
	return ok("deleted " + r.rel(abs))
}

// ---------- execution tools ----------

func (r *run) shell(ctx context.Context, command, workdir string, timeoutSec int) toolResult {
	if strings.TrimSpace(command) == "" {
		return fail("empty command")
	}
	readOnly := r.def.ID == "sec"
	if cat := ShellRisk(command); cat != "" {
		if approved, msg := r.approve(ctx, "shell", "$ "+command, "高风险操作（"+cat+"）"); !approved {
			return fail(msg)
		}
	}
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}
	res := r.env.Sandbox.Exec(ctx, readOnly, workdir, command, time.Duration(timeoutSec)*time.Second)
	out := truncate(res.Output, 12000)
	if res.TimedOut {
		return failf("timed out after %ds\n%s", timeoutSec, out)
	}
	text := fmt.Sprintf("exit code %d\n%s", res.ExitCode, out)
	if res.ExitCode != 0 {
		return toolResult{text: text, isError: true}
	}
	return ok(text)
}

func (r *run) ssh(ctx context.Context, host, command, reason string) toolResult {
	if approved, msg := r.approve(ctx, "ssh", "ssh "+host+" "+command, "远程操作："+reason); !approved {
		return fail(msg)
	}
	cmd := fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new %s %s",
		shq(host), shq(command))
	res := r.env.Sandbox.Exec(ctx, false, "", cmd, 120*time.Second)
	return toolResult{text: fmt.Sprintf("exit code %d\n%s", res.ExitCode, truncate(res.Output, 12000)), isError: res.ExitCode != 0}
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (r *run) httpRequest(ctx context.Context, method, url, body string) toolResult {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "HEAD" {
		if approved, msg := r.approve(ctx, "http_request", method+" "+url+"\n"+truncate(body, 2000), "对外发送数据"); !approved {
			return fail(msg)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, url, strings.NewReader(body))
	if err != nil {
		return fail(err.Error())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 20000))
	return ok(fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(b)))
}

// approve asks the human. While waiting the agent shows a red "!" and its task is blocked.
func (r *run) approve(ctx context.Context, tool, detail, reason string) (bool, string) {
	r.env.Bus.SetState(r.def.ID, events.StateBlocked, "等待审批："+reason, r.requestID, r.task.ID, "", nil)
	prev := r.task.Status
	r.setTaskStatus(store.StatusBlocked)
	approved, err := r.env.Approvals.Request(ctx, r.def.ID, r.requestID, r.task.ID, tool, detail, reason)
	r.setTaskStatus(prev)
	r.env.Bus.SetState(r.def.ID, events.StateWorking, "继续工作", r.requestID, r.task.ID, "", nil)
	if err != nil {
		return false, "approval aborted: " + err.Error()
	}
	if !approved {
		return false, "the human REJECTED this operation. Do not retry it; find a safer alternative or block the task."
	}
	return true, ""
}

func (r *run) setTaskStatus(status string) {
	if r.task.ID == 0 || r.task.Status == status {
		return
	}
	r.task.Status = status
	if err := r.env.Store.SaveTask(r.task); err == nil {
		r.env.Bus.Emit(events.TaskUpdated, r.requestID, r.task.ID, r.def.ID, r.task)
	}
}

// ---------- coordination tools ----------

func (r *run) createTask(title, desc, assignee string, deps []int64) toolResult {
	if r.env.CreateTask == nil {
		return fail("create_task is not available in this context")
	}
	if title == "" || desc == "" {
		return fail("title and description are required")
	}
	id, err := r.env.CreateTask(r.requestID, title, desc, assignee, deps)
	if err != nil {
		return fail(err.Error())
	}
	r.createdTasks++
	return ok(fmt.Sprintf("created task #%d (assignee %s)", id, assignee))
}

func (r *run) isDependency(id int64) bool {
	for _, d := range r.task.DependsOn {
		if d == id {
			return true
		}
	}
	return false
}

func (r *run) remember(note string) toolResult {
	note = strings.TrimSpace(note)
	if note == "" {
		return fail("note is empty")
	}
	m, err := r.env.Store.AddMemory(r.def.ID, r.task.ID, note)
	if err != nil {
		return fail(err.Error())
	}
	if err := appendMemoryFile(r.env.MemoryDir, r.def, r.task, note); err != nil {
		return fail(err.Error())
	}
	r.remembered = true
	r.env.Bus.Emit(events.MemoryAdded, r.requestID, r.task.ID, r.def.ID, m)
	return ok("saved to long-term memory")
}

func appendMemoryFile(dir string, def *Def, task *store.Task, note string) error {
	path := filepath.Join(dir, def.ID+".md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		header := fmt.Sprintf("# %s（%s）的长期记忆\n\n", def.Name, def.Title)
		if err := os.WriteFile(path, []byte(header), 0o644); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- [%s] (任务 #%d %s) %s\n", time.Now().Format("2006-01-02 15:04"), task.ID, task.Title, strings.ReplaceAll(note, "\n", " "))
	return err
}
