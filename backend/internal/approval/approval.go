// Package approval pauses high-risk tool calls until a human approves or rejects them.
package approval

import (
	"context"
	"fmt"
	"sync"

	"aidept/internal/events"
	"aidept/internal/store"
)

type Manager struct {
	st  *store.Store
	bus *events.Bus

	mu      sync.Mutex
	waiters map[int64]chan bool
}

func New(st *store.Store, bus *events.Bus) *Manager {
	_ = st.ExpireStaleApprovals()
	return &Manager{st: st, bus: bus, waiters: map[int64]chan bool{}}
}

// Request blocks until the approval is resolved or ctx ends.
func (m *Manager) Request(ctx context.Context, agentID string, requestID, taskID int64, tool, detail, reason string) (bool, error) {
	a := &store.Approval{AgentID: agentID, TaskID: taskID, Tool: tool, Detail: detail, Reason: reason}
	if err := m.st.CreateApproval(a); err != nil {
		return false, err
	}
	ch := make(chan bool, 1)
	m.mu.Lock()
	m.waiters[a.ID] = ch
	m.mu.Unlock()
	m.bus.Emit(events.ApprovalRequest, requestID, taskID, agentID, a)

	select {
	case ok := <-ch:
		return ok, nil
	case <-ctx.Done():
		_ = m.Resolve(a.ID, false)
		return false, ctx.Err()
	}
}

// Resolve records the decision and wakes the waiting agent.
func (m *Manager) Resolve(id int64, approved bool) error {
	m.mu.Lock()
	ch, ok := m.waiters[id]
	delete(m.waiters, id)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval %d is not pending", id)
	}
	if err := m.st.ResolveApproval(id, approved); err != nil {
		return err
	}
	m.bus.Emit(events.ApprovalResolved, 0, 0, "", map[string]any{"id": id, "approved": approved})
	ch <- approved
	return nil
}
