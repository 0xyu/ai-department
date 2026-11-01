// Package store persists requests, tasks, events, memories and approvals in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Task statuses (shown on the whiteboard).
const (
	StatusTodo       = "todo"        // 待办
	StatusInProgress = "in_progress" // 进行中
	StatusReview     = "review"      // 审核
	StatusDone       = "done"        // 完成
	StatusBlocked    = "blocked"     // 阻塞
)

type Request struct {
	ID         int64  `json:"id"`
	Text       string `json:"text"`
	Status     string `json:"status"` // planning | running | done | failed
	Summary    string `json:"summary"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type Task struct {
	ID          int64   `json:"id"`
	RequestID   int64   `json:"request_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Assignee    string  `json:"assignee"`
	DependsOn   []int64 `json:"depends_on"`
	Status      string  `json:"status"`
	Result      string  `json:"result"`
	ReworkCount int     `json:"rework_count"`
	Notes       string  `json:"notes"` // rework / rejection notes appended over time
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type Event struct {
	ID        int64           `json:"id"`
	TS        string          `json:"ts"`
	Type      string          `json:"type"`
	RequestID int64           `json:"request_id,omitempty"`
	TaskID    int64           `json:"task_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type Memory struct {
	ID        int64  `json:"id"`
	AgentID   string `json:"agent_id"`
	TaskID    int64  `json:"task_id"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

type Approval struct {
	ID         int64  `json:"id"`
	AgentID    string `json:"agent_id"`
	TaskID     int64  `json:"task_id"`
	Tool       string `json:"tool"`
	Detail     string `json:"detail"`
	Reason     string `json:"reason"`
	Status     string `json:"status"` // pending | approved | rejected
	CreatedAt  string `json:"created_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serialize writes; sqlite handles one writer at a time
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  text TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id INTEGER NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  assignee TEXT NOT NULL,
  depends_on TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '',
  rework_count INTEGER NOT NULL DEFAULT 0,
  notes TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  type TEXT NOT NULL,
  request_id INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER NOT NULL DEFAULT 0,
  agent_id TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id, id);
CREATE TABLE IF NOT EXISTS memories (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  agent_id TEXT NOT NULL,
  task_id INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approvals (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  agent_id TEXT NOT NULL,
  task_id INTEGER NOT NULL DEFAULT 0,
  tool TEXT NOT NULL,
  detail TEXT NOT NULL,
  reason TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  resolved_at TEXT NOT NULL DEFAULT ''
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ---------- requests ----------

func (s *Store) CreateRequest(text string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := now()
	res, err := s.db.Exec(`INSERT INTO requests(text,status,created_at) VALUES(?,?,?)`, text, "planning", ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Request{ID: id, Text: text, Status: "planning", CreatedAt: ts}, nil
}

func (s *Store) UpdateRequest(id int64, status, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fin := ""
	if status == "done" || status == "failed" {
		fin = now()
	}
	_, err := s.db.Exec(`UPDATE requests SET status=?, summary=CASE WHEN ?='' THEN summary ELSE ? END, finished_at=? WHERE id=?`,
		status, summary, summary, fin, id)
	return err
}

func (s *Store) ListRequests(limit int) ([]Request, error) {
	rows, err := s.db.Query(`SELECT id,text,status,summary,created_at,finished_at FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.ID, &r.Text, &r.Status, &r.Summary, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- tasks ----------

func (s *Store) CreateTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := now()
	if t.DependsOn == nil {
		t.DependsOn = []int64{}
	}
	deps, _ := json.Marshal(t.DependsOn)
	if t.Status == "" {
		t.Status = StatusTodo
	}
	res, err := s.db.Exec(`INSERT INTO tasks(request_id,title,description,assignee,depends_on,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		t.RequestID, t.Title, t.Description, t.Assignee, string(deps), t.Status, ts, ts)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	t.CreatedAt, t.UpdatedAt = ts, ts
	return nil
}

func (s *Store) SaveTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.UpdatedAt = now()
	deps, _ := json.Marshal(t.DependsOn)
	_, err := s.db.Exec(`UPDATE tasks SET title=?,description=?,assignee=?,depends_on=?,status=?,result=?,rework_count=?,notes=?,updated_at=? WHERE id=?`,
		t.Title, t.Description, t.Assignee, string(deps), t.Status, t.Result, t.ReworkCount, t.Notes, t.UpdatedAt, t.ID)
	return err
}

const taskCols = `id,request_id,title,description,assignee,depends_on,status,result,rework_count,notes,created_at,updated_at`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var deps string
	if err := sc.Scan(&t.ID, &t.RequestID, &t.Title, &t.Description, &t.Assignee, &deps, &t.Status, &t.Result, &t.ReworkCount, &t.Notes, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(deps), &t.DependsOn)
	if t.DependsOn == nil {
		t.DependsOn = []int64{}
	}
	return &t, nil
}

func (s *Store) GetTask(id int64) (*Task, error) {
	return scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

func (s *Store) ListTasks(requestID int64) ([]*Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks`
	args := []any{}
	if requestID > 0 {
		q += ` WHERE request_id=?`
		args = append(args, requestID)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- events ----------

func (s *Store) InsertEvent(e *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.TS == "" {
		e.TS = now()
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	}
	res, err := s.db.Exec(`INSERT INTO events(ts,type,request_id,task_id,agent_id,payload) VALUES(?,?,?,?,?,?)`,
		e.TS, e.Type, e.RequestID, e.TaskID, e.AgentID, string(e.Payload))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// ListEvents returns the most recent events (oldest first). agentID filters when non-empty.
func (s *Store) ListEvents(agentID string, limit int) ([]Event, error) {
	q := `SELECT id,ts,type,request_id,task_id,agent_id,payload FROM events`
	args := []any{}
	if agentID != "" {
		q += ` WHERE agent_id=?`
		args = append(args, agentID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var p string
		if err := rows.Scan(&e.ID, &e.TS, &e.Type, &e.RequestID, &e.TaskID, &e.AgentID, &p); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(p)
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ---------- memories ----------

func (s *Store) AddMemory(agentID string, taskID int64, note string) (*Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := now()
	res, err := s.db.Exec(`INSERT INTO memories(agent_id,task_id,note,created_at) VALUES(?,?,?,?)`, agentID, taskID, note, ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Memory{ID: id, AgentID: agentID, TaskID: taskID, Note: note, CreatedAt: ts}, nil
}

func (s *Store) ListMemories(agentID string, limit int) ([]Memory, error) {
	rows, err := s.db.Query(`SELECT id,agent_id,task_id,note,created_at FROM memories WHERE agent_id=? ORDER BY id DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.AgentID, &m.TaskID, &m.Note, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- approvals ----------

func (s *Store) CreateApproval(a *Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.CreatedAt = now()
	a.Status = "pending"
	res, err := s.db.Exec(`INSERT INTO approvals(agent_id,task_id,tool,detail,reason,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		a.AgentID, a.TaskID, a.Tool, a.Detail, a.Reason, a.Status, a.CreatedAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) ResolveApproval(id int64, approved bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := "rejected"
	if approved {
		st = "approved"
	}
	_, err := s.db.Exec(`UPDATE approvals SET status=?, resolved_at=? WHERE id=? AND status='pending'`, st, now(), id)
	return err
}

func (s *Store) PendingApprovals() ([]Approval, error) {
	rows, err := s.db.Query(`SELECT id,agent_id,task_id,tool,detail,reason,status,created_at,resolved_at FROM approvals WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.AgentID, &a.TaskID, &a.Tool, &a.Detail, &a.Reason, &a.Status, &a.CreatedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ExpireStaleApprovals marks approvals left pending by a previous process as rejected.
func (s *Store) ExpireStaleApprovals() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE approvals SET status='rejected', resolved_at=? WHERE status='pending'`, now())
	return err
}
