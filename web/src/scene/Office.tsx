import { useEffect, useMemo } from 'react';
import * as THREE from 'three';
import { Label } from './Label';
import { useDept, STATUS_LABEL } from '../store';
import type { Task } from '../types';
import {
  DESK_D, DESK_W, FLOOR, MANAGER_DESK, MANAGER_DOOR, MANAGER_OFFICE, MEETING_DOOR, MEETING_ROOM,
  MEETING_SEATS, MEETING_TABLE, OPEN_DESKS, PANTRY, PANTRY_COUNTER, PANTRY_TABLE, WHITEBOARD,
  type Desk,
} from './layout';

const flat = (color: string, extra: Partial<THREE.MeshStandardMaterialParameters> = {}) => (
  <meshStandardMaterial color={color} flatShading roughness={0.85} {...extra} />
);

function Chair({ x, z, rot }: { x: number; z: number; rot: number }) {
  return (
    <group position={[x, 0, z]} rotation={[0, rot, 0]}>
      <mesh position={[0, 0.45, 0]} castShadow>
        <boxGeometry args={[0.55, 0.08, 0.55]} />
        {flat('#3f4654')}
      </mesh>
      <mesh position={[0, 0.8, 0.26]} castShadow>
        <boxGeometry args={[0.55, 0.6, 0.07]} />
        {flat('#3f4654')}
      </mesh>
      <mesh position={[0, 0.22, 0]}>
        <cylinderGeometry args={[0.04, 0.04, 0.44, 5]} />
        {flat('#22262e')}
      </mesh>
    </group>
  );
}

function Workstation({ desk, color }: { desk: Desk; color?: string }) {
  return (
    <group>
      <group position={[desk.x, 0, desk.z]}>
        <mesh position={[0, 0.75, 0]} castShadow receiveShadow>
          <boxGeometry args={[DESK_W, 0.08, DESK_D]} />
          {flat('#c8a27a')}
        </mesh>
        {[-1, 1].map((s) => (
          <mesh key={s} position={[s * (DESK_W / 2 - 0.08), 0.37, 0]} castShadow>
            <boxGeometry args={[0.08, 0.74, DESK_D - 0.1]} />
            {flat('#8a6a4c')}
          </mesh>
        ))}
        {/* monitor */}
        <mesh position={[0, 1.12, -0.28]} castShadow>
          <boxGeometry args={[0.9, 0.55, 0.05]} />
          {flat('#1d2129')}
        </mesh>
        <mesh position={[0, 1.12, -0.25]}>
          <planeGeometry args={[0.82, 0.47]} />
          <meshStandardMaterial color={color ?? '#2a3140'} emissive={color ?? '#000'} emissiveIntensity={color ? 0.25 : 0} />
        </mesh>
        <mesh position={[0, 0.88, -0.28]}>
          <boxGeometry args={[0.08, 0.2, 0.08]} />
          {flat('#1d2129')}
        </mesh>
        {/* keyboard */}
        <mesh position={[0, 0.8, 0.12]}>
          <boxGeometry args={[0.6, 0.03, 0.2]} />
          {flat('#e7e9ee')}
        </mesh>
      </group>
      <Chair x={desk.x} z={desk.z + 1.25} rot={0} />
    </group>
  );
}

function Wall({ x0, z0, x1, z1, h = 2.4, color = '#dfe6ee', glass = false }: { x0: number; z0: number; x1: number; z1: number; h?: number; color?: string; glass?: boolean }) {
  const w = Math.max(Math.abs(x1 - x0), 0.1);
  const d = Math.max(Math.abs(z1 - z0), 0.1);
  return (
    <mesh position={[(x0 + x1) / 2, h / 2, (z0 + z1) / 2]} castShadow={!glass} receiveShadow>
      <boxGeometry args={[w, h, d]} />
      {glass ? <meshStandardMaterial color="#a9d4ef" transparent opacity={0.28} /> : flat(color)}
    </mesh>
  );
}

/** Draws the task board onto a canvas texture. */
function useBoardTexture(tasks: Task[]) {
  const canvas = useMemo(() => {
    const c = document.createElement('canvas');
    c.width = 1400;
    c.height = 520;
    return c;
  }, []);
  const texture = useMemo(() => {
    const t = new THREE.CanvasTexture(canvas);
    t.colorSpace = THREE.SRGBColorSpace;
    t.anisotropy = 4;
    return t;
  }, [canvas]);
  const agents = useDept((s) => s.agents);

  useEffect(() => {
    const g = canvas.getContext('2d')!;
    g.fillStyle = '#fbfbf8';
    g.fillRect(0, 0, canvas.width, canvas.height);
    const cols = ['todo', 'in_progress', 'review', 'done', 'blocked'] as const;
    const colColor: Record<string, string> = { todo: '#64748b', in_progress: '#2563eb', review: '#d97706', done: '#16a34a', blocked: '#dc2626' };
    const cw = canvas.width / cols.length;
    cols.forEach((st, i) => {
      const list = tasks.filter((t) => t.status === st);
      const x = i * cw;
      g.fillStyle = colColor[st];
      g.fillRect(x + 10, 10, cw - 20, 56);
      g.fillStyle = '#fff';
      g.font = 'bold 32px sans-serif';
      g.fillText(`${STATUS_LABEL[st]}  ${list.length}`, x + 24, 50);
      list.slice(0, 5).forEach((t, j) => {
        const y = 80 + j * 86;
        g.fillStyle = '#fff6c2';
        g.fillRect(x + 14, y, cw - 28, 76);
        g.strokeStyle = '#e5d98f';
        g.strokeRect(x + 14, y, cw - 28, 76);
        g.fillStyle = agents[t.assignee]?.color ?? '#999';
        g.beginPath();
        g.arc(x + 34, y + 24, 10, 0, Math.PI * 2);
        g.fill();
        g.fillStyle = '#1f2937';
        g.font = 'bold 22px sans-serif';
        g.fillText(`#${t.id} ${agents[t.assignee]?.name ?? t.assignee}`, x + 52, y + 32);
        g.font = '22px sans-serif';
        const title = t.title.length > 13 ? t.title.slice(0, 12) + '…' : t.title;
        g.fillText(title, x + 26, y + 62);
      });
      if (list.length > 5) {
        g.fillStyle = '#475569';
        g.font = '22px sans-serif';
        g.fillText(`+${list.length - 5} 更多`, x + 26, 80 + 5 * 86 + 4);
      }
    });
    texture.needsUpdate = true;
  }, [tasks, agents, canvas, texture]);
  return texture;
}

function Whiteboard() {
  const tasksMap = useDept((s) => s.tasks);
  const requests = useDept((s) => s.requests);
  const setBoardOpen = useDept((s) => s.setBoardOpen);
  // Show the board of the latest request.
  const latest = requests[0]?.id;
  const tasks = useMemo(() => Object.values(tasksMap).filter((t) => t.request_id === latest).sort((a, b) => a.id - b.id), [tasksMap, latest]);
  const texture = useBoardTexture(tasks);
  const W = WHITEBOARD;
  return (
    <group
      position={[W.x, W.y, W.z]}
      onClick={(e) => {
        e.stopPropagation();
        setBoardOpen(true);
      }}
      onPointerOver={() => (document.body.style.cursor = 'pointer')}
      onPointerOut={() => (document.body.style.cursor = '')}
    >
      <mesh position={[0, 0, -0.03]}>
        <boxGeometry args={[W.w + 0.2, W.h + 0.2, 0.05]} />
        {flat('#9aa3af')}
      </mesh>
      <mesh>
        <planeGeometry args={[W.w, W.h]} />
        <meshBasicMaterial map={texture} toneMapped={false} />
      </mesh>
      <Label position={[0, W.h / 2 + 0.3, 0]} height={0.31} color="#334155" text={"任务看板（点击查看）"} />
    </group>
  );
}

export function Office() {
  const agents = useDept((s) => s.agents);
  const F = FLOOR;
  return (
    <group>
      {/* floor */}
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[0, 0, 0]} receiveShadow>
        <planeGeometry args={[F.x1 - F.x0, F.z1 - F.z0]} />
        {flat('#e9e4da')}
      </mesh>
      {/* office floors */}
      {[MANAGER_OFFICE, MEETING_ROOM, PANTRY].map((r, i) => (
        <mesh key={i} rotation={[-Math.PI / 2, 0, 0]} position={[(r.x0 + r.x1) / 2, 0.005, (r.z0 + r.z1) / 2]} receiveShadow>
          <planeGeometry args={[r.x1 - r.x0, r.z1 - r.z0]} />
          {flat(['#d9cdb8', '#cfd8e3', '#e3d6c6'][i])}
        </mesh>
      ))}
      {/* outer walls */}
      <Wall x0={F.x0} z0={F.z0 - 0.1} x1={F.x1} z1={F.z0} h={3.6} />
      <Wall x0={F.x0 - 0.1} z0={F.z0} x1={F.x0} z1={F.z1} h={3.6} />

      {/* manager office (glass) */}
      <Wall x0={-7.05} z0={-9} x1={-6.95} z1={-3} glass />
      <Wall x0={-14} z0={-3.05} x1={MANAGER_DOOR.x0} z1={-2.95} glass />
      <Wall x0={MANAGER_DOOR.x1} z0={-3.05} x1={-6.95} z1={-2.95} glass />
      <Label position={[-10.5, 2.7, -2.95]} height={0.35} color="#7c5a12" text={"经理办公室"} />
      <Workstation desk={MANAGER_DESK} color={agents.max?.color} />
      <mesh position={[-13.4, 0.6, -8.4]} castShadow>
        <boxGeometry args={[0.6, 1.2, 0.6]} />
        {flat('#6b4f35')}
      </mesh>

      {/* meeting room (glass) */}
      <Wall x0={6.95} z0={-9} x1={7.05} z1={-3} glass />
      <Wall x0={6.95} z0={-3.05} x1={MEETING_DOOR.x0} z1={-2.95} glass />
      <Wall x0={MEETING_DOOR.x1} z0={-3.05} x1={14} z1={-2.95} glass />
      <Label position={[10.5, 2.7, -2.95]} height={0.35} color="#1e3a5f" text={"会议室"} />
      <mesh position={[MEETING_TABLE.x, 0.74, MEETING_TABLE.z]} castShadow receiveShadow>
        <boxGeometry args={[MEETING_TABLE.w, 0.08, MEETING_TABLE.d]} />
        {flat('#f2f2f2')}
      </mesh>
      <mesh position={[MEETING_TABLE.x, 0.37, MEETING_TABLE.z]}>
        <cylinderGeometry args={[0.15, 0.25, 0.74, 6]} />
        {flat('#9ca3af')}
      </mesh>
      {MEETING_SEATS.map((s, i) => (
        <Chair key={i} x={s.x} z={s.z + (s.rot === 0 ? -0.1 : 0.1)} rot={s.rot === 0 ? Math.PI : 0} />
      ))}

      {/* open-plan desks */}
      {OPEN_DESKS.map((d) => (
        <Workstation key={d.id} desk={d} color={d.owner ? agents[d.owner]?.color : undefined} />
      ))}
      {OPEN_DESKS.map((d) =>
        d.owner && agents[d.owner] ? (
          <Label key={d.id + 't'} position={[d.x, 0.02, d.z - 0.95]} rotation={[-Math.PI / 2, 0, 0]} height={0.29} color="#475569" text={`${agents[d.owner].name} · ${agents[d.owner].title}`} />
        ) : (
          <Label key={d.id + 't'} position={[d.x, 0.02, d.z - 0.95]} rotation={[-Math.PI / 2, 0, 0]} height={0.29} color="#94a3b8" text={"空工位"} />
        ),
      )}

      {/* pantry */}
      <Label position={[11.5, 0.02, 4.3]} rotation={[-Math.PI / 2, 0, 0]} height={0.35} color="#7c4a1e" text={"茶水间"} />
      <mesh position={[(PANTRY_COUNTER.x0 + PANTRY_COUNTER.x1) / 2, 0.5, (PANTRY_COUNTER.z0 + PANTRY_COUNTER.z1) / 2]} castShadow>
        <boxGeometry args={[PANTRY_COUNTER.x1 - PANTRY_COUNTER.x0, 1, PANTRY_COUNTER.z1 - PANTRY_COUNTER.z0]} />
        {flat('#8d6e63')}
      </mesh>
      <mesh position={[10.6, 1.25, 8.6]} castShadow>
        <boxGeometry args={[0.45, 0.5, 0.4]} />
        {flat('#263238')}
      </mesh>
      <mesh position={[13.4, 1.0, 8.55]} castShadow>
        <boxGeometry args={[0.8, 2, 0.7]} />
        {flat('#cfd8dc')}
      </mesh>
      <mesh position={[PANTRY_TABLE.x, 0.55, PANTRY_TABLE.z]} castShadow>
        <cylinderGeometry args={[PANTRY_TABLE.r, PANTRY_TABLE.r, 0.06, 8]} />
        {flat('#fafafa')}
      </mesh>
      <mesh position={[PANTRY_TABLE.x, 0.27, PANTRY_TABLE.z]}>
        <cylinderGeometry args={[0.06, 0.06, 0.54, 5]} />
        {flat('#555')}
      </mesh>

      {/* plants */}
      {[[-13.3, 8.3], [5.6, -8.3], [-4.6, -8.3], [-13.3, -2.2]].map(([x, z], i) => (
        <group key={i} position={[x, 0, z]}>
          <mesh position={[0, 0.25, 0]} castShadow>
            <cylinderGeometry args={[0.25, 0.2, 0.5, 6]} />
            {flat('#a0522d')}
          </mesh>
          <mesh position={[0, 0.85, 0]} castShadow>
            <icosahedronGeometry args={[0.45, 0]} />
            {flat('#4caf50')}
          </mesh>
        </group>
      ))}

      <Whiteboard />
    </group>
  );
}
