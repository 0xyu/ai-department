import { useDept } from './store';

export async function fetchState() {
  const r = await fetch('/api/state');
  if (!r.ok) throw new Error(`state: ${r.status}`);
  useDept.getState().loadSnapshot(await r.json());
}

export async function submitRequest(text: string) {
  const r = await fetch('/api/requests', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text }),
  });
  if (!r.ok) throw new Error((await r.json()).error ?? r.statusText);
  return r.json();
}

export async function resolveApproval(id: number, approved: boolean) {
  const r = await fetch(`/api/approvals/${id}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ approved }),
  });
  if (!r.ok) throw new Error((await r.json()).error ?? r.statusText);
}

export async function fetchAgent(id: string) {
  const r = await fetch(`/api/agents/${id}`);
  if (!r.ok) throw new Error(`agent: ${r.status}`);
  return r.json();
}

/** Connects to /ws, re-syncing the snapshot on every (re)connect. */
export function connect() {
  let ws: WebSocket | null = null;
  let stopped = false;
  let retry = 500;

  const open = () => {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws`);
    ws.onopen = async () => {
      retry = 500;
      try {
        await fetchState();
      } catch (e) {
        console.error(e);
      }
      useDept.getState().setConnected(true);
    };
    ws.onmessage = (m) => useDept.getState().apply(JSON.parse(m.data));
    ws.onclose = () => {
      useDept.getState().setConnected(false);
      if (!stopped) setTimeout(open, (retry = Math.min(retry * 2, 8000)));
    };
  };
  open();
  return () => {
    stopped = true;
    ws?.close();
  };
}
