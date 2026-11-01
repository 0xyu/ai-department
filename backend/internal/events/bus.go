// Package events writes every event to SQLite and fans it out to live subscribers.
// It also keeps the current presence state of each agent (what the 3D office renders).
package events

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"aidept/internal/store"
)

// Event types.
const (
	RequestCreated   = "request_created"
	RequestUpdated   = "request_updated"
	TaskCreated      = "task_created"
	TaskUpdated      = "task_updated"
	AgentState       = "agent_state"
	ToolCall         = "tool_call"
	ToolResult       = "tool_result"
	AgentMessage     = "agent_message"
	Handoff          = "handoff"
	MeetingStart     = "meeting_start"
	MeetingEnd       = "meeting_end"
	ApprovalRequest  = "approval_requested"
	ApprovalResolved = "approval_resolved"
	MemoryAdded      = "memory_added"
	Error            = "error"
)

// Presence states.
const (
	StateIdle     = "idle"
	StateWorking  = "working"
	StateHandoff  = "handoff"
	StateMeeting  = "meeting"
	StateBlocked  = "blocked"
)

type Presence struct {
	AgentID      string   `json:"agent_id"`
	State        string   `json:"state"`
	Activity     string   `json:"activity"`
	TaskID       int64    `json:"task_id"`
	Target       string   `json:"target,omitempty"`       // handoff receiver
	Participants []string `json:"participants,omitempty"` // meeting members
	Since        string   `json:"since"`
}

type Bus struct {
	st *store.Store

	mu       sync.RWMutex
	subs     map[chan store.Event]struct{}
	presence map[string]*Presence
}

func NewBus(st *store.Store, agentIDs []string) *Bus {
	b := &Bus{st: st, subs: map[chan store.Event]struct{}{}, presence: map[string]*Presence{}}
	for _, id := range agentIDs {
		b.presence[id] = &Presence{AgentID: id, State: StateIdle, Since: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	return b
}

// Emit persists and broadcasts an event.
func (b *Bus) Emit(typ string, requestID, taskID int64, agentID string, payload any) store.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{}`)
	}
	e := store.Event{Type: typ, RequestID: requestID, TaskID: taskID, AgentID: agentID, Payload: raw}
	if err := b.st.InsertEvent(&e); err != nil {
		log.Printf("event insert failed: %v", err)
	}
	b.mu.RLock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than stall agents
		}
	}
	b.mu.RUnlock()
	return e
}

func (b *Bus) Subscribe() chan store.Event {
	ch := make(chan store.Event, 512)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Bus) Unsubscribe(ch chan store.Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// SetState updates an agent's presence and emits an agent_state event.
func (b *Bus) SetState(agentID, state, activity string, requestID, taskID int64, target string, participants []string) {
	p := &Presence{
		AgentID: agentID, State: state, Activity: activity, TaskID: taskID,
		Target: target, Participants: participants, Since: time.Now().UTC().Format(time.RFC3339Nano),
	}
	b.mu.Lock()
	b.presence[agentID] = p
	b.mu.Unlock()
	b.Emit(AgentState, requestID, taskID, agentID, p)
}

// Activity changes only the activity text of a working agent (bubble text), keeping its state.
func (b *Bus) Activity(agentID, activity string, requestID, taskID int64) {
	b.mu.Lock()
	p, ok := b.presence[agentID]
	if !ok {
		b.mu.Unlock()
		return
	}
	cp := *p
	cp.Activity = activity
	cp.TaskID = taskID
	b.presence[agentID] = &cp
	b.mu.Unlock()
	b.Emit(AgentState, requestID, taskID, agentID, &cp)
}

func (b *Bus) Presence() []Presence {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Presence, 0, len(b.presence))
	for _, p := range b.presence {
		out = append(out, *p)
	}
	return out
}

func (b *Bus) PresenceOf(agentID string) Presence {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if p, ok := b.presence[agentID]; ok {
		return *p
	}
	return Presence{AgentID: agentID, State: StateIdle}
}
