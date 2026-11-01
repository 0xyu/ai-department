import type { AgentId } from '../types';

export const agentsOrder: AgentId[] = ['max', 'dev', 'ops', 'sec', 'qa', 'doc'];

export const STATE_LABEL: Record<string, string> = {
  idle: '空闲',
  working: '工作中',
  handoff: '交接中',
  meeting: '开会中',
  blocked: '阻塞 / 等待审批',
};
