import { useMemo, useRef } from 'react';
import { Canvas, useFrame } from '@react-three/fiber';
import { OrbitControls } from '@react-three/drei';
import * as THREE from 'three';
import { useDept, type SceneAction } from '../store';
import { agentsOrder } from '../ui/agentsOrder';
import { Office } from './Office';
import { Character } from './Character';
import { HOME } from './layout';

/** A task card flying between desks: a handoff while the giver is busy with other work. */
function FlyingCard({ action }: { action: SceneAction }) {
  const ref = useRef<THREE.Mesh>(null!);
  const start = useRef<number | null>(null);
  const consume = useDept((s) => s.consumeAction);
  const a = HOME[action.from];
  const b = HOME[action.to];
  useFrame((st) => {
    if (start.current === null) start.current = st.clock.elapsedTime;
    const k = Math.min(1, (st.clock.elapsedTime - start.current) / 1.6);
    ref.current.position.set(a.x + (b.x - a.x) * k, 1.4 + Math.sin(k * Math.PI) * 2, a.z + (b.z - a.z) * k);
    ref.current.rotation.y = k * Math.PI * 4;
    if (k >= 1) consume(action.key);
  });
  return (
    <mesh ref={ref}>
      <boxGeometry args={[0.4, 0.02, 0.3]} />
      <meshStandardMaterial color="#fde68a" emissive="#f59e0b" emissiveIntensity={0.4} />
    </mesh>
  );
}

export function Scene() {
  const agents = useDept((s) => s.agents);
  const actions = useDept((s) => s.actions);
  const cards = useMemo(() => actions.filter((a) => a.kind === 'fly_card'), [actions]);
  const select = useDept((s) => s.select);
  return (
    <Canvas shadows="soft" camera={{ position: [0, 21, 18], fov: 45 }} onPointerMissed={() => select(null)}>
      <color attach="background" args={['#dcdde8']} />
      <fog attach="fog" args={['#dcdde8', 50, 95]} />
      {/* outside ground */}
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[0, -0.24, 0]} receiveShadow>
        <planeGeometry args={[200, 200]} />
        <meshStandardMaterial color="#d9dae6" roughness={1} />
      </mesh>
      <hemisphereLight args={['#f1f2ff', '#b9b3c9', 1.1]} />
      <ambientLight intensity={0.25} color="#fff4e8" />
      {/* warm afternoon sun streaming through the left windows */}
      <directionalLight
        position={[-20, 15, -2]}
        intensity={2.4}
        color="#ffe9cc"
        castShadow
        shadow-mapSize={[4096, 4096]}
        shadow-bias={-0.0004}
        shadow-normalBias={0.03}
        shadow-camera-left={-22}
        shadow-camera-right={22}
        shadow-camera-top={22}
        shadow-camera-bottom={-22}
        shadow-camera-far={70}
      />
      {/* cool fill from the front-right */}
      <directionalLight position={[12, 14, 16]} intensity={0.45} color="#dfe6ff" />
      <Office />
      {agentsOrder.map((id) => (agents[id] ? <Character key={id} agent={agents[id]} /> : null))}
      {cards.map((c) => (
        <FlyingCard key={c.key} action={c} />
      ))}
      <OrbitControls target={[0, 0, 0]} maxPolarAngle={Math.PI / 2.3} minDistance={8} maxDistance={40} />
    </Canvas>
  );
}
