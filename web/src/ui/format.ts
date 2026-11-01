import type { DeptEvent } from '../types';
import { STATUS_LABEL } from '../store';

export function time(ts: string) {
  const d = new Date(ts);
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

/** One-line human description of an event, or null for events not worth listing. */
export function describeEvent(e: DeptEvent, names: Record<string, string>): string | null {
  const p = e.payload ?? {};
  const n = (id?: string) => (id ? names[id] ?? id : '');
  switch (e.type) {
    case 'request_created':
      return `📨 新需求 #${e.request_id}：${p.text}`;
    case 'request_updated':
      return `📨 需求 #${e.request_id} → ${({ planning: '规划中', running: '执行中', done: '已交付', failed: '失败' } as any)[p.status] ?? p.status}`;
    case 'task_created':
      return `📋 新任务 #${p.id} ${p.title} → ${n(p.assignee)}`;
    case 'task_updated':
      return `📋 任务 #${p.id} ${STATUS_LABEL[p.status] ?? p.status}`;
    case 'tool_call':
      return `🔧 ${n(e.agent_id)}：${p.description}`;
    case 'tool_result':
      return p.is_error ? `✗ ${n(e.agent_id)} ${p.tool} 失败：${String(p.output).slice(0, 80)}` : null;
    case 'agent_message':
      return `💬 ${n(e.agent_id)}：${String(p.text).slice(0, 140)}`;
    case 'handoff':
      return `🤝 ${n(p.from)} 把任务 #${p.task_id} 交给 ${n(p.to)}`;
    case 'meeting_start':
      return `👥 ${p.topic}（${(p.participants ?? []).map(n).join('、')}）`;
    case 'meeting_end':
      return `👥 会议结束：${p.topic}`;
    case 'approval_requested':
      return `⚠️ ${n(p.agent_id)} 请求审批：${p.reason}`;
    case 'approval_resolved':
      return p.approved ? '✅ 已批准' : '⛔ 已拒绝';
    case 'memory_added':
      return `🧠 ${n(e.agent_id)} 记住：${p.note}`;
    case 'error':
      return `❌ ${p.message}`;
  }
  return null;
}
