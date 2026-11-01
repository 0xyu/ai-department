import { useEffect } from 'react';
import { connect } from './api';
import { useDept } from './store';
import { Scene } from './scene/Scene';
import { ApprovalModal, BoardModal, EventFeed, RequestPanel, Sidebar } from './ui/Panels';
import { STATE_LABEL, agentsOrder } from './ui/agentsOrder';

function TopBar() {
  const connected = useDept((s) => s.connected);
  const agents = useDept((s) => s.agents);
  const select = useDept((s) => s.select);
  const setBoardOpen = useDept((s) => s.setBoardOpen);
  return (
    <div className="topbar">
      <b>🏢 个人 AI IT 部门</b>
      <span className={`conn ${connected ? 'ok' : ''}`}>{connected ? '● 已连接' : '○ 连接中…'}</span>
      <div className="roster">
        {agentsOrder.map((id) =>
          agents[id] ? (
            <button key={id} className="chip" onClick={() => select(id)} title={agents[id].presence.activity}>
              <span className="dot" style={{ background: agents[id].color }} />
              {agents[id].name}
              <span className={`mini state-${agents[id].presence.state}`}>{STATE_LABEL[agents[id].presence.state]}</span>
            </button>
          ) : null,
        )}
      </div>
      <button className="chip" onClick={() => setBoardOpen(true)}>
        📋 看板
      </button>
    </div>
  );
}

export default function App() {
  const selected = useDept((s) => s.selectedAgent);
  useEffect(() => connect(), []);
  return (
    <div className="app">
      <Scene />
      <TopBar />
      <RequestPanel />
      {selected ? <Sidebar /> : <EventFeed />}
      <BoardModal />
      <ApprovalModal />
    </div>
  );
}
