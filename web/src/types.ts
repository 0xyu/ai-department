export type AgentId = 'max' | 'dev' | 'ops' | 'sec' | 'qa' | 'doc';

export interface Presence {
  agent_id: AgentId;
  state: 'idle' | 'working' | 'handoff' | 'meeting' | 'blocked';
  activity: string;
  task_id: number;
  target?: AgentId;
  participants?: AgentId[];
  since: string;
}

export interface Agent {
  id: AgentId;
  name: string;
  title: string;
  color: string;
  tools: string[];
  body: string;
  presence: Presence;
}

export type TaskStatus = 'todo' | 'in_progress' | 'review' | 'done' | 'blocked';

export interface Task {
  id: number;
  request_id: number;
  title: string;
  description: string;
  assignee: AgentId;
  depends_on: number[];
  status: TaskStatus;
  result: string;
  rework_count: number;
  notes: string;
  created_at: string;
  updated_at: string;
}

export interface Request {
  id: number;
  text: string;
  status: 'planning' | 'running' | 'done' | 'failed';
  summary: string;
  created_at: string;
  finished_at?: string;
}

export interface Approval {
  id: number;
  agent_id: AgentId;
  task_id: number;
  tool: string;
  detail: string;
  reason: string;
  status: string;
  created_at: string;
}

export interface DeptEvent {
  id: number;
  ts: string;
  type: string;
  request_id?: number;
  task_id?: number;
  agent_id?: AgentId;
  payload: any;
}

export interface Memory {
  id: number;
  agent_id: AgentId;
  task_id: number;
  note: string;
  created_at: string;
}
