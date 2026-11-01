import { create } from 'zustand';
import type { Agent, AgentId, Approval, DeptEvent, Presence, Request, Task } from './types';

/** A one-off action the 3D scene plays because of a real backend event. */
export interface SceneAction {
  key: number;
  kind: 'walk_handoff' | 'fly_card';
  from: AgentId;
  to: AgentId;
  taskId: number;
}

interface DeptState {
  connected: boolean;
  agents: Record<string, Agent>;
  tasks: Record<number, Task>;
  requests: Request[];
  approvals: Approval[];
  events: DeptEvent[];
  actions: SceneAction[];
  selectedAgent: AgentId | null;
  boardOpen: boolean;
  /** bumps whenever an event concerns an agent, so the sidebar can refresh */
  agentTick: Record<string, number>;

  setConnected(v: boolean): void;
  loadSnapshot(s: { agents: Agent[]; tasks: Task[]; requests: Request[]; approvals: Approval[]; events: DeptEvent[] }): void;
  apply(e: DeptEvent): void;
  consumeAction(key: number): void;
  select(id: AgentId | null): void;
  setBoardOpen(v: boolean): void;
}

let actionKey = 1;

export const useDept = create<DeptState>((set) => ({
  connected: false,
  agents: {},
  tasks: {},
  requests: [],
  approvals: [],
  events: [],
  actions: [],
  selectedAgent: null,
  boardOpen: false,
  agentTick: {},

  setConnected: (v) => set({ connected: v }),

  loadSnapshot: (s) =>
    set(() => {
      const agents: Record<string, Agent> = {};
      for (const a of s.agents) agents[a.id] = a;
      const tasks: Record<number, Task> = {};
      for (const t of s.tasks ?? []) tasks[t.id] = t;
      return { agents, tasks, requests: s.requests ?? [], approvals: s.approvals ?? [], events: s.events ?? [] };
    }),

  apply: (e) =>
    set((st) => {
      const next: Partial<DeptState> = { events: [...st.events.slice(-299), e] };
      if (e.agent_id) next.agentTick = { ...st.agentTick, [e.agent_id]: (st.agentTick[e.agent_id] ?? 0) + 1 };
      switch (e.type) {
        case 'agent_state': {
          const p = e.payload as Presence;
          const a = st.agents[p.agent_id];
          if (a) next.agents = { ...st.agents, [p.agent_id]: { ...a, presence: p } };
          break;
        }
        case 'task_created':
        case 'task_updated': {
          const t = e.payload as Task;
          next.tasks = { ...st.tasks, [t.id]: t };
          break;
        }
        case 'request_created':
        case 'request_updated': {
          const r = e.payload as Request;
          const others = st.requests.filter((x) => x.id !== r.id);
          next.requests = [r, ...others].sort((a, b) => b.id - a.id);
          break;
        }
        case 'approval_requested':
          next.approvals = [...st.approvals.filter((a) => a.id !== e.payload.id), e.payload as Approval];
          break;
        case 'approval_resolved':
          next.approvals = st.approvals.filter((a) => a.id !== e.payload.id);
          break;
        case 'handoff': {
          const p = e.payload;
          next.actions = [
            ...st.actions,
            { key: actionKey++, kind: p.walk ? 'walk_handoff' : 'fly_card', from: p.from, to: p.to, taskId: p.task_id },
          ];
          break;
        }
      }
      return next;
    }),

  consumeAction: (key) => set((st) => ({ actions: st.actions.filter((a) => a.key !== key) })),
  select: (id) => set({ selectedAgent: id }),
  setBoardOpen: (v) => set({ boardOpen: v }),
}));

export const STATUS_LABEL: Record<string, string> = {
  todo: '待办',
  in_progress: '进行中',
  review: '审核',
  done: '完成',
  blocked: '阻塞',
};
