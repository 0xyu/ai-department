import type { AgentId } from '../types';

/** A place a character can stand or sit: position on the floor + facing angle (radians, 0 = facing +z). */
export interface Spot {
  x: number;
  z: number;
  rot: number;
}

export interface Rect {
  x0: number;
  z0: number;
  x1: number;
  z1: number;
}

export const FLOOR = { x0: -14, z0: -9, x1: 14, z1: 9 };

// ---------- furniture ----------

export interface Desk {
  id: string;
  owner: AgentId | null;
  x: number;
  z: number;
}

/** Six open-plan workstations (one spare hot desk) + Max's desk in the manager office. */
export const OPEN_DESKS: Desk[] = [
  { id: 'd1', owner: 'dev', x: -6, z: -0.5 },
  { id: 'd2', owner: 'ops', x: 0, z: -0.5 },
  { id: 'd3', owner: 'sec', x: 6, z: -0.5 },
  { id: 'd4', owner: 'qa', x: -6, z: 4 },
  { id: 'd5', owner: 'doc', x: 0, z: 4 },
  { id: 'd6', owner: null, x: 6, z: 4 },
];
export const MANAGER_DESK: Desk = { id: 'm', owner: 'max', x: -10.5, z: -7 };
export const DESK_W = 2.4;
export const DESK_D = 1.0;

export const MANAGER_OFFICE: Rect = { x0: -14, z0: -9, x1: -7, z1: -3 };
export const MEETING_ROOM: Rect = { x0: 7, z0: -9, x1: 14, z1: -3 };
export const MEETING_TABLE = { x: 10.5, z: -6, w: 3.6, d: 1.6 };
export const PANTRY: Rect = { x0: 9, z0: 4.5, x1: 14, z1: 9 };
export const WHITEBOARD = { x: 0, z: -8.85, w: 7, h: 2.6, y: 2.2 };

/** Door gaps in the office fronts (z = -3). */
export const MANAGER_DOOR = { x0: -9.6, x1: -8 };
export const MEETING_DOOR = { x0: 8, x1: 9.6 };

/** Walls as thin rectangles (front walls leave a door gap). */
export const WALLS: Rect[] = [
  // manager office
  { x0: -7.1, z0: -9, x1: -6.9, z1: -3 },
  { x0: -14, z0: -3.1, x1: MANAGER_DOOR.x0, z1: -2.9 },
  { x0: MANAGER_DOOR.x1, z0: -3.1, x1: -6.9, z1: -2.9 },
  // meeting room
  { x0: 6.9, z0: -9, x1: 7.1, z1: -3 },
  { x0: 6.9, z0: -3.1, x1: MEETING_DOOR.x0, z1: -2.9 },
  { x0: MEETING_DOOR.x1, z0: -3.1, x1: 14, z1: -2.9 },
];

export const PANTRY_COUNTER: Rect = { x0: 10, z0: 8.2, x1: 14, z1: 9 };
export const PANTRY_TABLE = { x: 11.6, z: 6.2, r: 0.6 };

// ---------- spots ----------

const PI = Math.PI;

function seatFor(d: Desk): Spot {
  return { x: d.x, z: d.z + 1.15, rot: PI }; // chair in front of the desk, facing the monitor (-z)
}

export const HOME: Record<AgentId, Spot> = {
  max: seatFor(MANAGER_DESK),
  dev: seatFor(OPEN_DESKS[0]),
  ops: seatFor(OPEN_DESKS[1]),
  sec: seatFor(OPEN_DESKS[2]),
  qa: seatFor(OPEN_DESKS[3]),
  doc: seatFor(OPEN_DESKS[4]),
};

/** Where a visitor stands when bringing a task to someone's desk. */
export function visitSpot(target: AgentId): Spot {
  const h = HOME[target];
  return { x: h.x + 1.5, z: h.z - 0.1, rot: -PI / 2 };
}

const T = MEETING_TABLE;
export const MEETING_SEATS: Spot[] = [
  { x: T.x - 1.2, z: T.z - 1.25, rot: 0 },
  { x: T.x, z: T.z - 1.25, rot: 0 },
  { x: T.x + 1.2, z: T.z - 1.25, rot: 0 },
  { x: T.x - 1.2, z: T.z + 1.25, rot: PI },
  { x: T.x, z: T.z + 1.25, rot: PI },
  { x: T.x + 1.2, z: T.z + 1.25, rot: PI },
];

export const PANTRY_SPOTS: Record<AgentId, Spot> = {
  max: { x: 10.4, z: 7.5, rot: 0 },
  dev: { x: 11.4, z: 7.5, rot: 0 },
  ops: { x: 12.4, z: 7.5, rot: 0 },
  sec: { x: 13.3, z: 7.3, rot: 0 },
  qa: { x: 10.6, z: 5.6, rot: PI / 2 },
  doc: { x: 12.6, z: 5.6, rot: -PI / 2 },
};

// ---------- pathfinding (A* on a grid) ----------

const CELL = 0.25;
const COLS = Math.round((FLOOR.x1 - FLOOR.x0) / CELL);
const ROWS = Math.round((FLOOR.z1 - FLOOR.z0) / CELL);
const blocked = new Uint8Array(COLS * ROWS);

function blockRect(r: Rect, pad: number) {
  const c0 = Math.max(0, Math.floor((r.x0 - pad - FLOOR.x0) / CELL));
  const c1 = Math.min(COLS - 1, Math.floor((r.x1 + pad - FLOOR.x0) / CELL));
  const r0 = Math.max(0, Math.floor((r.z0 - pad - FLOOR.z0) / CELL));
  const r1 = Math.min(ROWS - 1, Math.floor((r.z1 + pad - FLOOR.z0) / CELL));
  for (let i = r0; i <= r1; i++) for (let j = c0; j <= c1; j++) blocked[i * COLS + j] = 1;
}

const PAD = 0.3;
for (const d of [...OPEN_DESKS, MANAGER_DESK]) blockRect({ x0: d.x - DESK_W / 2, z0: d.z - DESK_D / 2, x1: d.x + DESK_W / 2, z1: d.z + DESK_D / 2 }, PAD);
for (const w of WALLS) blockRect(w, PAD);
blockRect({ x0: T.x - T.w / 2, z0: T.z - T.d / 2, x1: T.x + T.w / 2, z1: T.z + T.d / 2 }, PAD);
blockRect(PANTRY_COUNTER, PAD);
blockRect({ x0: PANTRY_TABLE.x - 0.6, z0: PANTRY_TABLE.z - 0.6, x1: PANTRY_TABLE.x + 0.6, z1: PANTRY_TABLE.z + 0.6 }, PAD);
blockRect({ x0: FLOOR.x0, z0: FLOOR.z0, x1: FLOOR.x1, z1: FLOOR.z0 + 0.2 }, PAD); // back wall

const toCell = (x: number, z: number): [number, number] => [
  Math.min(COLS - 1, Math.max(0, Math.floor((x - FLOOR.x0) / CELL))),
  Math.min(ROWS - 1, Math.max(0, Math.floor((z - FLOOR.z0) / CELL))),
];
const center = (c: number, r: number): [number, number] => [FLOOR.x0 + (c + 0.5) * CELL, FLOOR.z0 + (r + 0.5) * CELL];
const free = (c: number, r: number) => c >= 0 && r >= 0 && c < COLS && r < ROWS && !blocked[r * COLS + c];

function nearestFree(c: number, r: number): [number, number] {
  if (free(c, r)) return [c, r];
  for (let rad = 1; rad < 12; rad++)
    for (let dr = -rad; dr <= rad; dr++)
      for (let dc = -rad; dc <= rad; dc++) if (free(c + dc, r + dr)) return [c + dc, r + dr];
  return [c, r];
}

function lineFree(a: [number, number], b: [number, number]): boolean {
  const steps = Math.ceil(Math.hypot(b[0] - a[0], b[1] - a[1]) / (CELL / 2));
  for (let i = 0; i <= steps; i++) {
    const t = steps === 0 ? 0 : i / steps;
    const [c, r] = toCell(a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t);
    if (!free(c, r)) return false;
  }
  return true;
}

/** Returns waypoints from (x0,z0) to (x1,z1) avoiding furniture and walls. */
export function findPath(x0: number, z0: number, x1: number, z1: number): [number, number][] {
  const [sc, sr] = nearestFree(...toCell(x0, z0));
  const [gc, gr] = nearestFree(...toCell(x1, z1));
  const start = sr * COLS + sc;
  const goal = gr * COLS + gc;
  const g = new Float32Array(COLS * ROWS).fill(Infinity);
  const came = new Int32Array(COLS * ROWS).fill(-1);
  const open: number[] = [start];
  const inOpen = new Uint8Array(COLS * ROWS);
  const f = new Float32Array(COLS * ROWS).fill(Infinity);
  g[start] = 0;
  f[start] = Math.hypot(gc - sc, gr - sr);
  inOpen[start] = 1;
  const dirs = [[1, 0], [-1, 0], [0, 1], [0, -1], [1, 1], [1, -1], [-1, 1], [-1, -1]];
  while (open.length) {
    let bi = 0;
    for (let i = 1; i < open.length; i++) if (f[open[i]] < f[open[bi]]) bi = i;
    const cur = open[bi];
    open.splice(bi, 1);
    inOpen[cur] = 0;
    if (cur === goal) break;
    const cc = cur % COLS;
    const cr = (cur - cc) / COLS;
    for (const [dc, dr] of dirs) {
      const nc = cc + dc;
      const nr = cr + dr;
      if (!free(nc, nr)) continue;
      if (dc && dr && (!free(cc + dc, cr) || !free(cc, cr + dr))) continue; // no corner cutting
      const n = nr * COLS + nc;
      const ng = g[cur] + (dc && dr ? Math.SQRT2 : 1);
      if (ng < g[n]) {
        g[n] = ng;
        came[n] = cur;
        f[n] = ng + Math.hypot(gc - nc, gr - nr);
        if (!inOpen[n]) {
          open.push(n);
          inOpen[n] = 1;
        }
      }
    }
  }
  if (came[goal] === -1 && goal !== start) return [[x1, z1]];
  const cells: [number, number][] = [];
  for (let n = goal; n !== -1; n = came[n]) {
    const c = n % COLS;
    cells.push(center(c, (n - c) / COLS));
    if (n === start) break;
  }
  cells.reverse();
  // string-pulling: keep only the waypoints needed for line of sight
  const pts: [number, number][] = [[x0, z0], ...cells, [x1, z1]];
  const out: [number, number][] = [];
  let anchor = pts[0];
  for (let i = 1; i < pts.length - 1; i++) {
    if (!lineFree(anchor, pts[i + 1])) {
      out.push(pts[i]);
      anchor = pts[i];
    }
  }
  out.push([x1, z1]);
  return out;
}
