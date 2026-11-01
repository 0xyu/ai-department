import { useEffect, useMemo, useRef, useState } from 'react';
import { useFrame } from '@react-three/fiber';
import { Html } from '@react-three/drei';
import * as THREE from 'three';
import { useDept } from '../store';
import type { Agent, AgentId } from '../types';
import { HOME, MEETING_SEATS, PANTRY_SPOTS, findPath, visitSpot, type Spot } from './layout';

type Pose = 'sit' | 'type' | 'stand';
interface Goal {
  key: string;
  spot: Spot;
  pose: Pose;
}

const SPEED = 3.2; // m/s
const IDLE_TO_PANTRY_MS = 25_000;
const HANDOFF_PAUSE = 1.2; // seconds standing at the receiver's desk

function mat(color: string) {
  return <meshStandardMaterial color={color} flatShading roughness={0.7} />;
}

function Accessory({ id, color }: { id: AgentId; color: string }) {
  switch (id) {
    case 'max': // tie + slicked hair
      return (
        <group>
          <mesh position={[0, 1.22, 0.265]} rotation={[0.1, 0, 0]}>
            <boxGeometry args={[0.09, 0.32, 0.02]} />
            {mat('#b91c1c')}
          </mesh>
          <mesh position={[0, 1.39, 0.255]}>
            <boxGeometry args={[0.13, 0.06, 0.03]} />
            {mat('#b91c1c')}
          </mesh>
          <mesh position={[0, 1.86, -0.03]} scale={[1, 0.55, 1]}>
            <sphereGeometry args={[0.235, 8, 6]} />
            {mat('#3b2a1a')}
          </mesh>
        </group>
      );
    case 'dev': // headphones
      return (
        <group position={[0, 1.74, 0]}>
          <mesh rotation={[0, 0, 0]}>
            <torusGeometry args={[0.26, 0.03, 4, 12, Math.PI]} />
            {mat('#111827')}
          </mesh>
          {[-1, 1].map((s) => (
            <mesh key={s} position={[s * 0.25, -0.02, 0]} rotation={[0, 0, Math.PI / 2]}>
              <cylinderGeometry args={[0.09, 0.09, 0.07, 8]} />
              {mat('#111827')}
            </mesh>
          ))}
        </group>
      );
    case 'ops': // hard hat
      return (
        <group position={[0, 1.8, 0]}>
          <mesh>
            <sphereGeometry args={[0.26, 8, 5, 0, Math.PI * 2, 0, Math.PI / 2]} />
            {mat('#facc15')}
          </mesh>
          <mesh position={[0, 0.0, 0.05]}>
            <cylinderGeometry args={[0.33, 0.33, 0.03, 10]} />
            {mat('#eab308')}
          </mesh>
        </group>
      );
    case 'sec': // sunglasses + cap
      return (
        <group>
          <mesh position={[0, 1.76, 0.2]}>
            <boxGeometry args={[0.36, 0.08, 0.04]} />
            {mat('#0b0b0b')}
          </mesh>
          <mesh position={[0, 1.88, 0]} scale={[1, 0.5, 1]}>
            <sphereGeometry args={[0.245, 8, 6]} />
            {mat('#1f2937')}
          </mesh>
          <mesh position={[0, 1.86, 0.2]} rotation={[0.2, 0, 0]}>
            <boxGeometry args={[0.3, 0.02, 0.2]} />
            {mat('#1f2937')}
          </mesh>
        </group>
      );
    case 'qa': // round glasses + clipboard
      return (
        <group>
          {[-1, 1].map((s) => (
            <mesh key={s} position={[s * 0.09, 1.76, 0.22]}>
              <torusGeometry args={[0.06, 0.012, 4, 10]} />
              {mat('#111')}
            </mesh>
          ))}
          <mesh position={[0.3, 1.0, 0.18]} rotation={[0.3, 0, 0]}>
            <boxGeometry args={[0.22, 0.3, 0.02]} />
            {mat('#d6b98c')}
          </mesh>
        </group>
      );
    case 'doc': // beret + book
      return (
        <group>
          <mesh position={[0.04, 1.93, 0]} rotation={[0, 0, 0.25]}>
            <cylinderGeometry args={[0.23, 0.26, 0.08, 10]} />
            {mat('#7f1d1d')}
          </mesh>
          <mesh position={[-0.3, 1.0, 0.16]} rotation={[0.2, 0, 0]}>
            <boxGeometry args={[0.2, 0.26, 0.06]} />
            {mat(color === '#ec4899' ? '#1e3a8a' : '#333')}
          </mesh>
        </group>
      );
  }
  return null;
}

function angleLerp(a: number, b: number, t: number) {
  let d = ((b - a + Math.PI) % (Math.PI * 2)) - Math.PI;
  if (d < -Math.PI) d += Math.PI * 2;
  return a + d * t;
}

export function Character({ agent }: { agent: Agent }) {
  const id = agent.id;
  const p = agent.presence;
  const select = useDept((s) => s.select);
  const selected = useDept((s) => s.selectedAgent === id);
  const consumeAction = useDept((s) => s.consumeAction);
  const myAction = useDept((s) => s.actions.find((a) => a.kind === 'walk_handoff' && a.from === id));

  // tick so idle duration is re-evaluated (idle → pantry after a while)
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const goal: Goal = useMemo(() => {
    if (myAction) return { key: `visit-${myAction.key}`, spot: visitSpot(myAction.to), pose: 'stand' };
    switch (p.state) {
      case 'meeting': {
        const idx = Math.max(0, (p.participants ?? []).indexOf(id));
        return { key: `meeting-${idx}`, spot: MEETING_SEATS[idx % MEETING_SEATS.length], pose: 'sit' };
      }
      case 'working':
        return { key: 'home-type', spot: HOME[id], pose: 'type' };
      case 'blocked':
        return { key: 'home-blocked', spot: HOME[id], pose: 'sit' };
      case 'idle': {
        const idleFor = now - Date.parse(p.since);
        if (idleFor > IDLE_TO_PANTRY_MS) return { key: 'pantry', spot: PANTRY_SPOTS[id], pose: 'stand' };
        return { key: 'home-idle', spot: HOME[id], pose: 'sit' };
      }
      default:
        return { key: 'home-idle', spot: HOME[id], pose: 'sit' };
    }
  }, [myAction, p.state, p.participants, p.since, id, now]);

  const goalRef = useRef(goal);
  goalRef.current = goal;
  const actionRef = useRef(myAction);
  actionRef.current = myAction;

  const root = useRef<THREE.Group>(null!);
  const upper = useRef<THREE.Group>(null!);
  const legL = useRef<THREE.Group>(null!);
  const legR = useRef<THREE.Group>(null!);
  const armL = useRef<THREE.Group>(null!);
  const armR = useRef<THREE.Group>(null!);
  const alert = useRef<THREE.Group>(null!);
  // start where the current state says this person is (no replayed walk after a page reload)
  const pos = useRef({ x: goal.spot.x, z: goal.spot.z, rot: goal.spot.rot });
  const path = useRef<[number, number][]>([]);
  const activeKey = useRef(myAction ? '' : goal.key);
  const arrivedAt = useRef<number | null>(0);
  const sitAmount = useRef(1);

  useFrame((state, dtRaw) => {
    const dt = Math.min(dtRaw, 0.1);
    const t = state.clock.elapsedTime;
    const g = goalRef.current;
    if (g.key !== activeKey.current) {
      activeKey.current = g.key;
      path.current = findPath(pos.current.x, pos.current.z, g.spot.x, g.spot.z);
      arrivedAt.current = null;
    }
    // move along the path
    let walking = false;
    if (path.current.length) {
      walking = true;
      const [tx, tz] = path.current[0];
      const dx = tx - pos.current.x;
      const dz = tz - pos.current.z;
      const dist = Math.hypot(dx, dz);
      const step = SPEED * dt;
      if (dist <= step) {
        pos.current.x = tx;
        pos.current.z = tz;
        path.current.shift();
      } else {
        pos.current.x += (dx / dist) * step;
        pos.current.z += (dz / dist) * step;
      }
      pos.current.rot = angleLerp(pos.current.rot, Math.atan2(dx, dz), Math.min(1, dt * 10));
      if (!path.current.length) arrivedAt.current = t;
    } else {
      if (arrivedAt.current === null) arrivedAt.current = t;
      pos.current.rot = angleLerp(pos.current.rot, g.spot.rot, Math.min(1, dt * 6));
    }

    // handoff: stand at the receiver's desk briefly, then the action is done
    const act = actionRef.current;
    if (act && g.key === `visit-${act.key}` && !walking && arrivedAt.current !== null && t - arrivedAt.current > HANDOFF_PAUSE) {
      consumeAction(act.key);
    }

    // pose
    const wantSit = !walking && (g.pose === 'sit' || g.pose === 'type') ? 1 : 0;
    sitAmount.current = THREE.MathUtils.lerp(sitAmount.current, wantSit, Math.min(1, dt * 8));
    const s = sitAmount.current;
    root.current.position.set(pos.current.x, walking ? Math.abs(Math.sin(t * 10)) * 0.05 : 0, pos.current.z);
    root.current.rotation.y = pos.current.rot;
    upper.current.position.y = -0.24 * s;
    const swing = walking ? Math.sin(t * 10) * 0.6 : 0;
    legL.current.position.y = 0.78 - 0.24 * s;
    legR.current.position.y = 0.78 - 0.24 * s;
    legL.current.rotation.x = -Math.PI / 2 * s + swing;
    legR.current.rotation.x = -Math.PI / 2 * s - swing;
    const typing = !walking && g.pose === 'type';
    const carry = !!act && walking;
    armL.current.rotation.x = typing ? -1.15 + Math.sin(t * 14) * 0.12 : walking ? -swing * 0.8 : -0.15 * s;
    armR.current.rotation.x = typing ? -1.15 + Math.sin(t * 14 + Math.PI) * 0.12 : carry ? -1.2 : walking ? swing * 0.8 : -0.15 * s;
    upper.current.rotation.x = typing ? 0.08 + Math.sin(t * 3) * 0.02 : 0;
    if (alert.current) {
      alert.current.position.y = 2.55 + Math.sin(t * 4) * 0.08;
      alert.current.rotation.y = t * 2;
    }
  });

  const blocked = p.state === 'blocked';
  let bubble = '';
  if (myAction) bubble = `交接任务 #${myAction.taskId}`;
  else if (p.state === 'working' || p.state === 'blocked' || p.state === 'meeting') bubble = p.activity;

  return (
    <group
      ref={root}
      onClick={(e) => {
        e.stopPropagation();
        select(id);
      }}
      onPointerOver={() => (document.body.style.cursor = 'pointer')}
      onPointerOut={() => (document.body.style.cursor = '')}
    >
      {/* legs (pivot at the hip) */}
      {[
        [legL, -0.12],
        [legR, 0.12],
      ].map(([ref, x], i) => (
        <group key={i} ref={ref as React.RefObject<THREE.Group>} position={[x as number, 0.78, 0]}>
          <mesh position={[0, -0.38, 0]} castShadow>
            <capsuleGeometry args={[0.1, 0.5, 2, 6]} />
            {mat('#334155')}
          </mesh>
        </group>
      ))}
      <group ref={upper}>
        {/* torso */}
        <mesh position={[0, 1.12, 0]} castShadow>
          <capsuleGeometry args={[0.27, 0.42, 3, 8]} />
          {mat(agent.color)}
        </mesh>
        {/* head */}
        <mesh position={[0, 1.72, 0]} castShadow>
          <sphereGeometry args={[0.24, 10, 8]} />
          {mat('#f1c9a5')}
        </mesh>
        {[-1, 1].map((s) => (
          <mesh key={s} position={[s * 0.08, 1.75, 0.21]}>
            <sphereGeometry args={[0.03, 5, 4]} />
            {mat('#1f2937')}
          </mesh>
        ))}
        {/* arms (pivot at the shoulder) */}
        {[
          [armL, -0.36],
          [armR, 0.36],
        ].map(([ref, x], i) => (
          <group key={i} ref={ref as React.RefObject<THREE.Group>} position={[x as number, 1.36, 0]}>
            <mesh position={[0, -0.26, 0]} castShadow>
              <capsuleGeometry args={[0.08, 0.36, 2, 6]} />
              {mat(agent.color)}
            </mesh>
            {i === 1 && myAction && (
              <mesh position={[0, -0.5, 0.08]}>
                <boxGeometry args={[0.28, 0.04, 0.36]} />
                {mat('#fde68a')}
              </mesh>
            )}
          </group>
        ))}
        <Accessory id={id} color={agent.color} />
        {selected && (
          <mesh position={[0, 0.03, 0]} rotation={[-Math.PI / 2, 0, 0]}>
            <ringGeometry args={[0.45, 0.55, 20]} />
            <meshBasicMaterial color={agent.color} />
          </mesh>
        )}
      </group>

      {blocked && (
        <group ref={alert}>
          <mesh position={[0, 0.2, 0]}>
            <boxGeometry args={[0.16, 0.48, 0.16]} />
            <meshStandardMaterial color="#ef4444" emissive="#ef4444" emissiveIntensity={0.8} />
          </mesh>
          <mesh position={[0, -0.18, 0]}>
            <sphereGeometry args={[0.1, 6, 5]} />
            <meshStandardMaterial color="#ef4444" emissive="#ef4444" emissiveIntensity={0.8} />
          </mesh>
        </group>
      )}

      <Html position={[0, blocked ? 3.45 : 2.35, 0]} center distanceFactor={14} zIndexRange={[20, 0]} style={{ pointerEvents: 'none' }}>
        <div className="char-label">
          {bubble && <div className={`bubble ${blocked ? 'bubble-red' : ''}`}>{bubble}</div>}
          <div className="nametag" style={{ borderColor: agent.color }}>
            {agent.name}
          </div>
        </div>
      </Html>
    </group>
  );
}
