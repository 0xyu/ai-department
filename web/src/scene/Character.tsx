import { useEffect, useMemo, useRef, useState } from 'react';
import { useFrame } from '@react-three/fiber';
import { Html, RoundedBox } from '@react-three/drei';
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
  return <meshStandardMaterial color={color} flatShading roughness={0.75} />;
}

// chibi proportions
const HIP = 0.62; // hip height when standing
const SIT = 0.1; // how far the body drops when sitting
const HEAD_Y = 1.5; // head centre
const FACE_Z = 0.28; // front of the face
const SKIN = '#f6d2b3';
const PANTS = '#3d4250';

const HAIR: Record<AgentId, string> = {
  max: '#2b1d14',
  dev: '#5a3720',
  ops: '#7a4b26',
  sec: '#1d1d22',
  qa: '#4a2e1c',
  doc: '#8a5a2b',
};

/** Blocky hair: cap + back + sides + chunky fringe. */
function Hair({ color, short = false }: { color: string; short?: boolean }) {
  return (
    <group>
      <RoundedBox args={[0.68, 0.24, 0.62]} radius={0.09} smoothness={2} position={[0, HEAD_Y + 0.27, -0.02]} castShadow>
        {mat(color)}
      </RoundedBox>
      <RoundedBox args={[0.66, short ? 0.3 : 0.44, 0.16]} radius={0.06} smoothness={2} position={[0, HEAD_Y + (short ? 0.12 : 0.05), -0.22]} castShadow>
        {mat(color)}
      </RoundedBox>
      {[-1, 1].map((s) => (
        <mesh key={s} position={[s * 0.32, HEAD_Y + 0.1, -0.03]}>
          <boxGeometry args={[0.07, 0.3, 0.44]} />
          {mat(color)}
        </mesh>
      ))}
      {[-0.2, -0.04, 0.13, 0.25].map((x, i) => (
        <mesh key={i} position={[x, HEAD_Y + 0.17 - (i % 2) * 0.03, FACE_Z - 0.01]} rotation={[0.2, 0, (i - 1.5) * 0.12]} castShadow>
          <boxGeometry args={[0.17, 0.13 + (i % 2) * 0.04, 0.08]} />
          {mat(color)}
        </mesh>
      ))}
    </group>
  );
}

function Accessory({ id, color }: { id: AgentId; color: string }) {
  switch (id) {
    case 'max': // red tie
      return (
        <group>
          <mesh position={[0, 0.93, 0.165]}>
            <boxGeometry args={[0.07, 0.28, 0.02]} />
            {mat('#c0392b')}
          </mesh>
          <mesh position={[0, 1.1, 0.165]}>
            <boxGeometry args={[0.1, 0.06, 0.03]} />
            {mat('#a93226')}
          </mesh>
        </group>
      );
    case 'dev': // headphones
      return (
        <group position={[0, HEAD_Y + 0.02, 0]}>
          <mesh>
            <torusGeometry args={[0.38, 0.035, 4, 12, Math.PI]} />
            {mat('#1f2430')}
          </mesh>
          {[-1, 1].map((s) => (
            <mesh key={s} position={[s * 0.36, 0, 0]} rotation={[0, 0, Math.PI / 2]}>
              <cylinderGeometry args={[0.12, 0.12, 0.08, 8]} />
              {mat('#1f2430')}
            </mesh>
          ))}
        </group>
      );
    case 'ops': // hard hat
      return (
        <group position={[0, HEAD_Y + 0.24, 0]}>
          <mesh scale={[1, 0.8, 1]} castShadow>
            <sphereGeometry args={[0.38, 8, 5, 0, Math.PI * 2, 0, Math.PI / 2]} />
            {mat('#f5c518')}
          </mesh>
          <mesh position={[0, 0.0, 0.05]}>
            <cylinderGeometry args={[0.44, 0.44, 0.04, 10]} />
            {mat('#e0ad0c')}
          </mesh>
        </group>
      );
    case 'sec': // sunglasses + cap
      return (
        <group>
          <mesh position={[0, HEAD_Y, FACE_Z + 0.02]}>
            <boxGeometry args={[0.48, 0.11, 0.03]} />
            {mat('#0b0b0b')}
          </mesh>
          <RoundedBox args={[0.7, 0.16, 0.64]} radius={0.06} smoothness={2} position={[0, HEAD_Y + 0.35, -0.01]} castShadow>
            {mat('#1f2937')}
          </RoundedBox>
          <mesh position={[0, HEAD_Y + 0.28, 0.4]}>
            <boxGeometry args={[0.52, 0.03, 0.26]} />
            {mat('#1f2937')}
          </mesh>
        </group>
      );
    case 'qa': // round glasses + clipboard
      return (
        <group>
          {[-1, 1].map((s) => (
            <mesh key={s} position={[s * 0.13, HEAD_Y, FACE_Z + 0.015]}>
              <torusGeometry args={[0.08, 0.013, 4, 10]} />
              {mat('#111')}
            </mesh>
          ))}
          <mesh position={[0, HEAD_Y + 0.01, FACE_Z + 0.015]}>
            <boxGeometry args={[0.08, 0.015, 0.015]} />
            {mat('#111')}
          </mesh>
          <mesh position={[0.36, 0.8, 0.14]} rotation={[0.3, 0, 0]}>
            <boxGeometry args={[0.2, 0.28, 0.02]} />
            {mat('#d6b98c')}
          </mesh>
        </group>
      );
    case 'doc': // beret + book
      return (
        <group>
          <mesh position={[0.05, HEAD_Y + 0.42, 0]} rotation={[0, 0, 0.22]} castShadow>
            <cylinderGeometry args={[0.3, 0.35, 0.1, 10]} />
            {mat('#7f1d1d')}
          </mesh>
          <mesh position={[-0.36, 0.8, 0.13]} rotation={[0.2, 0, 0]}>
            <boxGeometry args={[0.18, 0.24, 0.06]} />
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
    upper.current.position.y = -SIT * s;
    const swing = walking ? Math.sin(t * 10) * 0.6 : 0;
    legL.current.position.y = HIP - SIT * s;
    legR.current.position.y = HIP - SIT * s;
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
  const hair = HAIR[id] ?? '#4a2e1c';
  // pastel shirt tinted with the agent's colour (keeps identity, matches the soft look)
  const shirt = useMemo(() => `#${new THREE.Color(agent.color).lerp(new THREE.Color('#ffffff'), 0.45).getHexString()}`, [agent.color]);
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
        <group key={i} ref={ref as React.RefObject<THREE.Group>} position={[x as number, HIP, 0]}>
          <RoundedBox args={[0.18, 0.52, 0.2]} radius={0.05} smoothness={2} position={[0, -0.26, 0]} castShadow>
            {mat(PANTS)}
          </RoundedBox>
          <RoundedBox args={[0.2, 0.1, 0.28]} radius={0.035} smoothness={2} position={[0, -0.56, 0.04]} castShadow>
            {mat('#2a2d35')}
          </RoundedBox>
        </group>
      ))}
      <group ref={upper}>
        {/* hips */}
        <RoundedBox args={[0.46, 0.16, 0.28]} radius={0.05} smoothness={2} position={[0, HIP + 0.04, 0]} castShadow>
          {mat(PANTS)}
        </RoundedBox>
        {/* torso (shirt) + collar + lanyard badge */}
        <RoundedBox args={[0.52, 0.5, 0.3]} radius={0.08} smoothness={2} position={[0, 0.96, 0]} castShadow>
          {mat(shirt)}
        </RoundedBox>
        {[-1, 1].map((s) => (
          <mesh key={s} position={[s * 0.08, 1.19, 0.12]} rotation={[0.3, 0, s * -0.5]}>
            <boxGeometry args={[0.13, 0.05, 0.1]} />
            {mat('#ffffff')}
          </mesh>
        ))}
        {[-1, 1].map((s) => (
          <mesh key={'l' + s} position={[s * 0.06, 1.05, 0.155]} rotation={[0, 0, s * 0.28]}>
            <boxGeometry args={[0.025, 0.24, 0.01]} />
            {mat('#2f4fa8')}
          </mesh>
        ))}
        <mesh position={[0, 0.88, 0.16]}>
          <boxGeometry args={[0.1, 0.13, 0.015]} />
          {mat('#f8fafc')}
        </mesh>
        <mesh position={[0, 0.91, 0.168]}>
          <boxGeometry args={[0.07, 0.03, 0.005]} />
          {mat(agent.color)}
        </mesh>
        {/* big blocky head */}
        <RoundedBox args={[0.6, 0.56, 0.54]} radius={0.14} smoothness={3} position={[0, HEAD_Y, 0]} castShadow>
          {mat(SKIN)}
        </RoundedBox>
        {/* face: eyes, brows, blush, mouth */}
        {[-1, 1].map((s) => (
          <group key={s}>
            <mesh position={[s * 0.13, HEAD_Y - 0.02, FACE_Z]}>
              <boxGeometry args={[0.065, 0.1, 0.02]} />
              {mat('#2a211d')}
            </mesh>
            <mesh position={[s * 0.115, HEAD_Y - 0.0, FACE_Z + 0.005]}>
              <boxGeometry args={[0.02, 0.025, 0.02]} />
              {mat('#ffffff')}
            </mesh>
            <mesh position={[s * 0.13, HEAD_Y + 0.09, FACE_Z - 0.005]} rotation={[0, 0, s * -0.12]}>
              <boxGeometry args={[0.1, 0.022, 0.02]} />
              {mat(hair)}
            </mesh>
            <mesh position={[s * 0.2, HEAD_Y - 0.11, FACE_Z - 0.01]}>
              <boxGeometry args={[0.08, 0.035, 0.02]} />
              {mat('#f3a59c')}
            </mesh>
          </group>
        ))}
        <mesh position={[0, HEAD_Y - 0.14, FACE_Z]}>
          <boxGeometry args={[0.06, 0.018, 0.02]} />
          {mat('#9a5a4a')}
        </mesh>
        {id !== 'ops' && <Hair color={hair} short={id === 'sec' || id === 'max'} />}
        {/* arms (pivot at the shoulder) */}
        {[
          [armL, -0.33],
          [armR, 0.33],
        ].map(([ref, x], i) => (
          <group key={i} ref={ref as React.RefObject<THREE.Group>} position={[x as number, 1.15, 0]}>
            <RoundedBox args={[0.15, 0.36, 0.17]} radius={0.05} smoothness={2} position={[0, -0.16, 0]} castShadow>
              {mat(shirt)}
            </RoundedBox>
            <RoundedBox args={[0.13, 0.11, 0.13]} radius={0.04} smoothness={2} position={[0, -0.39, 0]} castShadow>
              {mat(SKIN)}
            </RoundedBox>
            {i === 1 && myAction && (
              <mesh position={[0, -0.44, 0.1]}>
                <boxGeometry args={[0.28, 0.04, 0.36]} />
                {mat('#fde68a')}
              </mesh>
            )}
          </group>
        ))}
        <Accessory id={id} color={agent.color} />
      </group>
      {selected && (
        <mesh position={[0, 0.02, 0]} rotation={[-Math.PI / 2, 0, 0]}>
          <ringGeometry args={[0.45, 0.55, 24]} />
          <meshBasicMaterial color={agent.color} />
        </mesh>
      )}

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
