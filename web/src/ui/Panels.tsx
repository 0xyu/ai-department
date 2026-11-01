import { useEffect, useMemo, useState } from 'react';
import { useDept, STATUS_LABEL } from '../store';
import { fetchAgent, resolveApproval, submitRequest } from '../api';
import type { DeptEvent, Memory, Task, TaskStatus } from '../types';
import { describeEvent, time } from './format';
import { STATE_LABEL } from './agentsOrder';

function useNames() {
  const agents = useDept((s) => s.agents);
  return useMemo(() => Object.fromEntries(Object.values(agents).map((a) => [a.id, a.name])), [agents]);
}

// ---------------- request input + delivery ----------------

export function RequestPanel() {
  const requests = useDept((s) => s.requests);
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [open, setOpen] = useState(false);
  const latest = requests[0];

  const send = async () => {
    if (!text.trim()) return;
    setBusy(true);
    setErr('');
    try {
      await submitRequest(text.trim());
      setText('');
    } catch (e: any) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="panel request-panel">
      {latest && (
        <div className="latest">
          <div className="row">
            <span className={`pill req-${latest.status}`}>
              {({ planning: '规划中', running: '执行中', done: '已交付', failed: '失败' } as any)[latest.status]}
            </span>
            <span className="req-text">#{latest.id} {latest.text}</span>
            {latest.summary && (
              <button className="link" onClick={() => setOpen(!open)}>
                {open ? '收起报告' : '交付报告'}
              </button>
            )}
          </div>
          {latest.summary && open && <pre className="summary">{latest.summary}</pre>}
        </div>
      )}
      <div className="row">
        <textarea
          value={text}
          placeholder="向 Max 提需求，例如：帮我做一个监控网站是否在线的脚本，写好测试和文档"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) send();
          }}
        />
        <button className="primary" disabled={busy || !text.trim()} onClick={send}>
          提交
        </button>
      </div>
      {err && <div className="error">{err}</div>}
    </div>
  );
}

// ---------------- approvals ----------------

export function ApprovalModal() {
  const approvals = useDept((s) => s.approvals);
  const agents = useDept((s) => s.agents);
  const tasks = useDept((s) => s.tasks);
  const [busy, setBusy] = useState(false);
  const a = approvals[0];
  if (!a) return null;
  const decide = async (ok: boolean) => {
    setBusy(true);
    try {
      await resolveApproval(a.id, ok);
    } catch (e) {
      alert(String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="approval-dock">
      <div className="modal approval">
        <h2>⚠️ 高风险操作需要你的审批</h2>
        <p>
          <b style={{ color: agents[a.agent_id]?.color }}>{agents[a.agent_id]?.name}</b> 想要执行：<b>{a.reason}</b>
        </p>
        {a.task_id > 0 && tasks[a.task_id] && (
          <p className="muted">
            任务 #{a.task_id}：{tasks[a.task_id].title}
          </p>
        )}
        <pre className="detail">{a.detail}</pre>
        <div className="row end">
          {approvals.length > 1 && <span className="muted">还有 {approvals.length - 1} 个待审批</span>}
          <button className="danger" disabled={busy} onClick={() => decide(false)}>
            拒绝
          </button>
          <button className="primary" disabled={busy} onClick={() => decide(true)}>
            批准
          </button>
        </div>
      </div>
    </div>
  );
}

// ---------------- task board ----------------

const COLUMNS: TaskStatus[] = ['todo', 'in_progress', 'review', 'done', 'blocked'];

function TaskCard({ t }: { t: Task }) {
  const agents = useDept((s) => s.agents);
  const [open, setOpen] = useState(false);
  const a = agents[t.assignee];
  return (
    <div className="card" onClick={() => setOpen(!open)}>
      <div className="card-head">
        <span className="dot" style={{ background: a?.color }} />#{t.id} {a?.name}
        {t.rework_count > 0 && <span className="pill small warn">返工 {t.rework_count}</span>}
      </div>
      <div className="card-title">{t.title}</div>
      {t.depends_on.length > 0 && <div className="muted small">依赖：{t.depends_on.map((d) => '#' + d).join(' ')}</div>}
      {open && (
        <div className="card-body">
          <h4>说明</h4>
          <pre>{t.description}</pre>
          {t.notes && (
            <>
              <h4>打回记录</h4>
              <pre>{t.notes.trim()}</pre>
            </>
          )}
          {t.result && (
            <>
              <h4>结果</h4>
              <pre>{t.result}</pre>
            </>
          )}
        </div>
      )}
    </div>
  );
}

export function BoardModal() {
  const open = useDept((s) => s.boardOpen);
  const setOpen = useDept((s) => s.setBoardOpen);
  const tasks = useDept((s) => s.tasks);
  const requests = useDept((s) => s.requests);
  const [reqId, setReqId] = useState<number | null>(null);
  const current = reqId ?? requests[0]?.id;
  const list = Object.values(tasks).filter((t) => t.request_id === current);
  if (!open) return null;
  return (
    <div className="modal-backdrop" onClick={() => setOpen(false)}>
      <div className="modal board" onClick={(e) => e.stopPropagation()}>
        <div className="row">
          <h2>任务看板</h2>
          <select value={current ?? ''} onChange={(e) => setReqId(Number(e.target.value))}>
            {requests.map((r) => (
              <option key={r.id} value={r.id}>
                #{r.id} {r.text.slice(0, 40)}
              </option>
            ))}
          </select>
          <button className="link" onClick={() => setOpen(false)}>
            关闭
          </button>
        </div>
        <div className="columns">
          {COLUMNS.map((c) => (
            <div key={c} className={`column col-${c}`}>
              <h3>
                {STATUS_LABEL[c]} <span>{list.filter((t) => t.status === c).length}</span>
              </h3>
              {list
                .filter((t) => t.status === c)
                .sort((a, b) => a.id - b.id)
                .map((t) => (
                  <TaskCard key={t.id} t={t} />
                ))}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

// ---------------- agent sidebar ----------------

interface AgentDetail {
  current_task: Task | null;
  events: DeptEvent[];
  memories: Memory[];
}

export function Sidebar() {
  const id = useDept((s) => s.selectedAgent);
  const agent = useDept((s) => (id ? s.agents[id] : null));
  const tick = useDept((s) => (id ? s.agentTick[id] ?? 0 : 0));
  const select = useDept((s) => s.select);
  const names = useNames();
  const [detail, setDetail] = useState<AgentDetail | null>(null);
  const [tab, setTab] = useState<'log' | 'memory' | 'role'>('log');

  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    const t = setTimeout(() => {
      fetchAgent(id)
        .then((d) => !cancelled && setDetail(d))
        .catch(() => {});
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [id, tick]);

  if (!id || !agent) return null;
  const p = agent.presence;
  const log = (detail?.events ?? [])
    .map((e) => ({ e, text: describeEvent(e, names) }))
    .filter((x) => x.text)
    .reverse();

  return (
    <div className="panel sidebar">
      <div className="row">
        <span className="avatar" style={{ background: agent.color }}>
          {agent.name[0]}
        </span>
        <div>
          <h2>{agent.name}</h2>
          <div className="muted">{agent.title}</div>
        </div>
        <button className="link right" onClick={() => select(null)}>
          ✕
        </button>
      </div>
      <div className={`state state-${p.state}`}>
        {STATE_LABEL[p.state]}
        {p.activity && <div className="activity">{p.activity}</div>}
      </div>
      <section>
        <h3>当前任务</h3>
        {detail?.current_task ? (
          <div className="card static">
            <div className="card-head">
              #{detail.current_task.id} · {STATUS_LABEL[detail.current_task.status]}
            </div>
            <div className="card-title">{detail.current_task.title}</div>
            <pre className="small">{detail.current_task.description}</pre>
          </div>
        ) : (
          <div className="muted">无</div>
        )}
      </section>
      <div className="tabs">
        <button className={tab === 'log' ? 'on' : ''} onClick={() => setTab('log')}>
          最近日志
        </button>
        <button className={tab === 'memory' ? 'on' : ''} onClick={() => setTab('memory')}>
          记忆 {detail?.memories?.length ?? 0}
        </button>
        <button className={tab === 'role' ? 'on' : ''} onClick={() => setTab('role')}>
          角色定义
        </button>
      </div>
      <div className="tab-body">
        {tab === 'log' &&
          (log.length ? (
            log.map(({ e, text }) => (
              <div key={e.id} className="log-line">
                <span className="ts">{time(e.ts)}</span> {text}
              </div>
            ))
          ) : (
            <div className="muted">暂无</div>
          ))}
        {tab === 'memory' &&
          ((detail?.memories ?? []).length ? (
            detail!.memories.map((m) => (
              <div key={m.id} className="memory">
                <div className="muted small">
                  {time(m.created_at)} · 任务 #{m.task_id}
                </div>
                {m.note}
              </div>
            ))
          ) : (
            <div className="muted">还没有记忆</div>
          ))}
        {tab === 'role' && (
          <>
            <div className="muted small">工具白名单：{agent.tools.join(', ')}</div>
            <pre className="role">{agent.body}</pre>
          </>
        )}
      </div>
    </div>
  );
}

// ---------------- live feed ----------------

export function EventFeed() {
  const events = useDept((s) => s.events);
  const names = useNames();
  const lines = useMemo(
    () =>
      events
        .map((e) => ({ e, text: describeEvent(e, names) }))
        .filter((x) => x.text && x.e.type !== 'task_updated')
        .slice(-14)
        .reverse(),
    [events, names],
  );
  return (
    <div className="panel feed">
      <h3>实时事件</h3>
      {lines.map(({ e, text }) => (
        <div key={e.id} className="log-line">
          <span className="ts">{time(e.ts)}</span> {text}
        </div>
      ))}
    </div>
  );
}
