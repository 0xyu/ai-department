import { useEffect, useMemo } from 'react';
import * as THREE from 'three';
import { RoundedBox } from '@react-three/drei';
import { Label } from './Label';
import { useDept, STATUS_LABEL } from '../store';
import type { Task } from '../types';
import {
  DESK_D, DESK_W, FLOOR, MANAGER_DESK, MANAGER_DOOR, MANAGER_OFFICE, MEETING_DOOR, MEETING_ROOM,
  MEETING_SEATS, MEETING_TABLE, OPEN_DESKS, PANTRY, PANTRY_COUNTER, PANTRY_TABLE, WHITEBOARD,
  type Desk, type Rect,
} from './layout';

// ---------- palette (soft, sunlit low-poly office) ----------
const C = {
  wall: '#eef0f5',
  wallCap: '#5f6475',
  baseboard: '#b9bdca',
  oak: '#e3b56f',
  oakDark: '#c99752',
  metal: '#4a4f5c',
  chair: '#474c58',
  chairDark: '#2f333c',
  screen: '#1c2029',
  glass: '#a8d4f2',
  frame: '#555a69',
  potWhite: '#f5f5f2',
  soil: '#5b4030',
  leaf: '#5cbf3a',
  leafDark: '#3f9e2b',
  sofa: '#6d9fd8',
  rug: '#5b8fd0',
};

const flat = (color: string, extra: Partial<THREE.MeshStandardMaterialParameters> = {}) => (
  <meshStandardMaterial color={color} flatShading roughness={0.8} {...extra} />
);

// ---------- textures ----------
function makeTexture(size: number, draw: (g: CanvasRenderingContext2D, s: number) => void, repeat: [number, number]) {
  const c = document.createElement('canvas');
  c.width = c.height = size;
  draw(c.getContext('2d')!, size);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  t.wrapS = t.wrapT = THREE.RepeatWrapping;
  t.repeat.set(...repeat);
  t.anisotropy = 8;
  return t;
}

/** Light grey square tiles (2x2 per texture with slight shade variation). */
function useTileTexture(w: number, h: number, tile = 1.4) {
  return useMemo(
    () =>
      makeTexture(
        256,
        (g, s) => {
          const shades = ['#e6e7ee', '#e1e2ea', '#e3e4ec', '#e8e9ef'];
          const half = s / 2;
          for (let i = 0; i < 4; i++) {
            g.fillStyle = shades[i];
            g.fillRect((i % 2) * half, Math.floor(i / 2) * half, half, half);
          }
          g.strokeStyle = '#cfd1dc';
          g.lineWidth = 3;
          for (let i = 0; i <= 2; i++) {
            g.beginPath();
            g.moveTo(i * half, 0);
            g.lineTo(i * half, s);
            g.moveTo(0, i * half);
            g.lineTo(s, i * half);
            g.stroke();
          }
        },
        [w / (tile * 2), h / (tile * 2)],
      ),
    [w, h, tile],
  );
}

/** Warm wooden planks. */
function usePlankTexture(w: number, h: number, base: string) {
  return useMemo(
    () =>
      makeTexture(
        256,
        (g, s) => {
          const rows = 8;
          const rh = s / rows;
          const col = new THREE.Color(base);
          for (let r = 0; r < rows; r++) {
            const shade = col.clone().offsetHSL(0, 0, ((r * 37) % 7) / 100 - 0.03);
            g.fillStyle = `#${shade.getHexString()}`;
            g.fillRect(0, r * rh, s, rh);
            g.fillStyle = 'rgba(90,60,30,0.25)';
            g.fillRect(0, r * rh, s, 2);
            const seam = ((r * 97) % 10) / 10;
            g.fillRect(seam * s, r * rh, 2, rh);
          }
        },
        [w / 3, h / 3],
      ),
    [w, h, base],
  );
}

// ---------- props ----------
function OfficeChair({ x, z, rot }: { x: number; z: number; rot: number }) {
  return (
    <group position={[x, 0, z]} rotation={[0, rot, 0]}>
      <RoundedBox args={[0.56, 0.1, 0.52]} radius={0.04} smoothness={2} position={[0, 0.47, 0]} castShadow>
        {flat(C.chair)}
      </RoundedBox>
      <RoundedBox args={[0.52, 0.56, 0.08]} radius={0.04} smoothness={2} position={[0, 0.84, 0.25]} rotation={[-0.1, 0, 0]} castShadow>
        {flat(C.chair)}
      </RoundedBox>
      {[-1, 1].map((s) => (
        <mesh key={s} position={[s * 0.3, 0.62, 0.02]} castShadow>
          <boxGeometry args={[0.05, 0.05, 0.36]} />
          {flat(C.chairDark)}
        </mesh>
      ))}
      <mesh position={[0, 0.26, 0]}>
        <cylinderGeometry args={[0.035, 0.035, 0.4, 6]} />
        {flat(C.chairDark)}
      </mesh>
      {[0, 1, 2, 3, 4].map((i) => {
        const a = (i / 5) * Math.PI * 2;
        return (
          <group key={i} rotation={[0, a, 0]}>
            <mesh position={[0, 0.07, 0.17]}>
              <boxGeometry args={[0.05, 0.04, 0.34]} />
              {flat(C.chairDark)}
            </mesh>
            <mesh position={[0, 0.035, 0.32]}>
              <sphereGeometry args={[0.035, 5, 4]} />
              {flat('#1b1d22')}
            </mesh>
          </group>
        );
      })}
    </group>
  );
}

/** Low-poly potted plant in a white hexagonal pot. */
function Plant({ x, z, y = 0, scale = 1 }: { x: number; z: number; y?: number; scale?: number }) {
  const leaves = useMemo(() => {
    const out: { yaw: number; tilt: number; len: number; dark: boolean; h: number }[] = [];
    const n = 9;
    for (let i = 0; i < n; i++) {
      out.push({ yaw: (i / n) * Math.PI * 2 + (i % 2) * 0.3, tilt: 0.45 + (i % 3) * 0.22, len: 0.34 + (i % 3) * 0.06, dark: i % 2 === 0, h: 0.42 + (i % 3) * 0.08 });
    }
    return out;
  }, []);
  return (
    <group position={[x, y, z]} scale={scale}>
      <mesh position={[0, 0.2, 0]} castShadow receiveShadow>
        <cylinderGeometry args={[0.22, 0.17, 0.4, 6]} />
        {flat(C.potWhite)}
      </mesh>
      <mesh position={[0, 0.395, 0]}>
        <cylinderGeometry args={[0.2, 0.2, 0.02, 6]} />
        {flat(C.soil)}
      </mesh>
      {/* leaves are octahedra squashed into blades */}
      {leaves.map((l, i) => (
        <group key={i} position={[0, l.h, 0]} rotation={[0, l.yaw, 0]}>
          <mesh
            position={[0, l.len * 0.5 * Math.cos(l.tilt), l.len * 0.5 * Math.sin(l.tilt)]}
            rotation={[l.tilt, 0, 0]}
            scale={[0.14, l.len * 0.62, 0.04]}
            castShadow
          >
            <octahedronGeometry args={[1, 0]} />
            <meshStandardMaterial color={l.dark ? C.leafDark : C.leaf} flatShading roughness={0.7} />
          </mesh>
        </group>
      ))}
    </group>
  );
}

function Mug({ x, z, color = '#f4f4f4' }: { x: number; z: number; color?: string }) {
  return (
    <mesh position={[x, 0.85, z]} castShadow>
      <cylinderGeometry args={[0.05, 0.045, 0.12, 8]} />
      {flat(color)}
    </mesh>
  );
}

function BookStack({ x, z, rot = 0 }: { x: number; z: number; rot?: number }) {
  const colors = ['#3b82c4', '#4caf7a', '#e0a63a'];
  return (
    <group position={[x, 0.79, z]} rotation={[0, rot, 0]}>
      {colors.map((c, i) => (
        <mesh key={i} position={[0, 0.03 + i * 0.055, 0]} rotation={[0, i * 0.15, 0]} castShadow>
          <boxGeometry args={[0.26, 0.05, 0.34]} />
          {flat(c)}
        </mesh>
      ))}
    </group>
  );
}

function Monitor({ glow }: { glow?: string }) {
  return (
    <group position={[0, 0.79, -0.25]}>
      <mesh position={[0, 0.015, 0]}>
        <boxGeometry args={[0.32, 0.03, 0.2]} />
        {flat(C.screen)}
      </mesh>
      <mesh position={[0, 0.15, -0.02]}>
        <boxGeometry args={[0.07, 0.26, 0.05]} />
        {flat(C.screen)}
      </mesh>
      <RoundedBox args={[0.98, 0.58, 0.06]} radius={0.02} smoothness={2} position={[0, 0.5, 0]} castShadow>
        {flat(C.screen)}
      </RoundedBox>
      <mesh position={[0, 0.51, 0.032]}>
        <planeGeometry args={[0.9, 0.5]} />
        <meshStandardMaterial color="#232a37" emissive={glow ?? '#000'} emissiveIntensity={glow ? 0.28 : 0} roughness={0.3} />
      </mesh>
    </group>
  );
}

function Table({ w, d, h = 0.75, top = C.oak }: { w: number; d: number; h?: number; top?: string }) {
  return (
    <group>
      <mesh position={[0, h - 0.04, 0]} castShadow receiveShadow>
        <boxGeometry args={[w, 0.08, d]} />
        {flat(top)}
      </mesh>
      {[
        [-1, -1],
        [1, -1],
        [-1, 1],
        [1, 1],
      ].map(([sx, sz], i) => (
        <mesh key={i} position={[sx * (w / 2 - 0.08), (h - 0.08) / 2, sz * (d / 2 - 0.08)]} castShadow>
          <boxGeometry args={[0.06, h - 0.08, 0.06]} />
          {flat(C.metal)}
        </mesh>
      ))}
    </group>
  );
}

function Workstation({ desk, color, seed }: { desk: Desk; color?: string; seed: number }) {
  return (
    <group>
      <group position={[desk.x, 0, desk.z]}>
        <Table w={DESK_W} d={DESK_D} />
        <Monitor glow={color} />
        {/* keyboard + mouse */}
        <mesh position={[0, 0.8, 0.14]} castShadow>
          <boxGeometry args={[0.58, 0.025, 0.18]} />
          {flat('#2f333b')}
        </mesh>
        <mesh position={[0.42, 0.8, 0.16]}>
          <boxGeometry args={[0.07, 0.03, 0.11]} />
          {flat('#2f333b')}
        </mesh>
        {seed % 2 === 0 ? <Plant x={-0.85} z={-0.15} y={0.75} scale={0.55} /> : <BookStack x={-0.85} z={-0.1} rot={0.2} />}
        <Mug x={0.72} z={-0.05} color={seed % 3 === 0 ? '#f4f4f4' : '#9aa4b5'} />
        {seed % 3 === 1 && <Plant x={0.9} z={-0.25} y={0.75} scale={0.45} />}
      </group>
      <OfficeChair x={desk.x} z={desk.z + 1.25} rot={0} />
    </group>
  );
}

// ---------- walls ----------
const WALL_H = 3.2;
const WALL_T = 0.3;

/** Solid painted wall with a dark cap, given as an axis-aligned rectangle. */
function SolidWall({ r, h = WALL_H }: { r: Rect; h?: number }) {
  const w = r.x1 - r.x0;
  const d = r.z1 - r.z0;
  return (
    <group position={[(r.x0 + r.x1) / 2, 0, (r.z0 + r.z1) / 2]}>
      <mesh position={[0, h / 2, 0]} castShadow receiveShadow>
        <boxGeometry args={[w, h, d]} />
        {flat(C.wall)}
      </mesh>
      <mesh position={[0, h + 0.05, 0]} castShadow>
        <boxGeometry args={[w + 0.04, 0.1, d + 0.04]} />
        {flat(C.wallCap)}
      </mesh>
    </group>
  );
}

/** Wall along z (at x) with a row of tall windows. `inward` is +1 if the room is on +x. */
function WindowWall({ x, z0, z1, inward }: { x: number; z0: number; z1: number; inward: 1 | -1 }) {
  const SILL = 0.85;
  const HEAD = 2.75;
  const len = z1 - z0;
  const n = Math.max(1, Math.round(len / 2.3));
  const seg = len / n;
  const POST = 0.34;
  const zc = (z0 + z1) / 2;
  const gh = HEAD - SILL;
  return (
    <group>
      <mesh position={[x, SILL / 2, zc]} castShadow receiveShadow>
        <boxGeometry args={[WALL_T, SILL, len]} />
        {flat(C.wall)}
      </mesh>
      <mesh position={[x, (HEAD + WALL_H) / 2, zc]} castShadow>
        <boxGeometry args={[WALL_T, WALL_H - HEAD, len]} />
        {flat(C.wall)}
      </mesh>
      <mesh position={[x, WALL_H + 0.05, zc]} castShadow>
        <boxGeometry args={[WALL_T + 0.04, 0.1, len + 0.04]} />
        {flat(C.wallCap)}
      </mesh>
      {/* inner window ledge */}
      <mesh position={[x + inward * 0.2, SILL + 0.03, zc]} receiveShadow castShadow>
        <boxGeometry args={[0.16, 0.06, len]} />
        {flat('#dfe2ea')}
      </mesh>
      {Array.from({ length: n + 1 }, (_, i) => {
        const zz = Math.min(z1 - POST / 2, Math.max(z0 + POST / 2, z0 + i * seg));
        return (
          <mesh key={'p' + i} position={[x, (SILL + HEAD) / 2, zz]} castShadow>
            <boxGeometry args={[WALL_T, gh, POST]} />
            {flat(C.wall)}
          </mesh>
        );
      })}
      {Array.from({ length: n }, (_, i) => {
        const zz = z0 + (i + 0.5) * seg;
        const pw = seg - POST;
        return (
          <group key={'g' + i} position={[x, (SILL + HEAD) / 2, zz]}>
            <mesh>
              <boxGeometry args={[0.04, gh, pw]} />
              <meshStandardMaterial color={C.glass} transparent opacity={0.35} roughness={0.1} metalness={0.1} />
            </mesh>
            {/* frame: top, bottom, middle mullion */}
            {[gh / 2 - 0.03, -gh / 2 + 0.03].map((y, k) => (
              <mesh key={k} position={[0, y, 0]} castShadow>
                <boxGeometry args={[0.08, 0.06, pw]} />
                {flat(C.frame)}
              </mesh>
            ))}
            <mesh position={[0, 0, 0]} castShadow>
              <boxGeometry args={[0.08, gh, 0.05]} />
              {flat(C.frame)}
            </mesh>
            <mesh position={[0, -gh / 2 + gh * 0.62, 0]} castShadow>
              <boxGeometry args={[0.08, 0.05, pw]} />
              {flat(C.frame)}
            </mesh>
          </group>
        );
      })}
    </group>
  );
}

/** Interior glass partition with dark frame (axis-aligned). */
function GlassWall({ x0, z0, x1, z1, h = 2.5 }: { x0: number; z0: number; x1: number; z1: number; h?: number }) {
  const alongX = Math.abs(x1 - x0) > Math.abs(z1 - z0);
  const len = alongX ? Math.abs(x1 - x0) : Math.abs(z1 - z0);
  const cx = (x0 + x1) / 2;
  const cz = (z0 + z1) / 2;
  const n = Math.max(1, Math.round(len / 1.6));
  const box = (l: number, hh: number, t: number): [number, number, number] => (alongX ? [l, hh, t] : [t, hh, l]);
  return (
    <group position={[cx, 0, cz]}>
      <mesh position={[0, h / 2, 0]}>
        <boxGeometry args={box(len, h, 0.04)} />
        <meshStandardMaterial color={C.glass} transparent opacity={0.3} roughness={0.1} />
      </mesh>
      <mesh position={[0, 0.06, 0]} castShadow receiveShadow>
        <boxGeometry args={box(len, 0.12, 0.16)} />
        {flat(C.frame)}
      </mesh>
      <mesh position={[0, h, 0]} castShadow>
        <boxGeometry args={box(len, 0.1, 0.14)} />
        {flat(C.frame)}
      </mesh>
      {Array.from({ length: n + 1 }, (_, i) => {
        const o = -len / 2 + (i * len) / n;
        return (
          <mesh key={i} position={alongX ? [o, h / 2, 0] : [0, h / 2, o]} castShadow>
            <boxGeometry args={box(0.08, h, 0.1)} />
            {flat(C.frame)}
          </mesh>
        );
      })}
    </group>
  );
}

// ---------- decor ----------
function Bookshelf({ x, z, w = 2.2, h = 1.9, rot = 0 }: { x: number; z: number; w?: number; h?: number; rot?: number }) {
  const shelves = 4;
  const sh = h / shelves;
  const books = useMemo(() => {
    const palette = ['#3b82c4', '#e4574a', '#4caf7a', '#e0a63a', '#7c5cc4', '#2f9fa0', '#f0f0f0', '#d4567e'];
    const out: { x: number; row: number; w: number; h: number; c: string }[] = [];
    for (let row = 0; row < shelves - 1; row++) {
      let cx = -w / 2 + 0.1;
      let k = row * 5;
      while (cx < w / 2 - 0.2) {
        const bw = 0.06 + ((k * 13) % 5) * 0.012;
        if ((k * 7) % 11 !== 0) out.push({ x: cx + bw / 2, row, w: bw, h: sh * (0.62 + ((k * 3) % 4) * 0.07), c: palette[(k * 5) % palette.length] });
        cx += bw + 0.008;
        k++;
      }
    }
    return out;
  }, [w, sh]);
  return (
    <group position={[x, 0, z]} rotation={[0, rot, 0]}>
      <mesh position={[0, h / 2, -0.2]} castShadow receiveShadow>
        <boxGeometry args={[w, h, 0.05]} />
        {flat(C.oakDark)}
      </mesh>
      {[-w / 2, 0, w / 2].map((sx, i) => (
        <mesh key={i} position={[sx, h / 2, 0]} castShadow>
          <boxGeometry args={[0.06, h, 0.44]} />
          {flat(C.oak)}
        </mesh>
      ))}
      {Array.from({ length: shelves + 1 }, (_, i) => (
        <mesh key={i} position={[0, Math.min(h - 0.03, i * sh + 0.03), 0]} castShadow receiveShadow>
          <boxGeometry args={[w, 0.06, 0.44]} />
          {flat(C.oak)}
        </mesh>
      ))}
      {/* bottom cupboard doors */}
      {[-w / 4, w / 4].map((dx, i) => (
        <mesh key={i} position={[dx, sh / 2 + 0.03, 0.21]}>
          <boxGeometry args={[w / 2 - 0.08, sh - 0.08, 0.03]} />
          {flat('#d6a660')}
        </mesh>
      ))}
      {books.map((b, i) => (
        <mesh key={i} position={[b.x, (b.row + 1) * sh + 0.06 + b.h / 2, 0.02]} castShadow>
          <boxGeometry args={[b.w, b.h, 0.3]} />
          {flat(b.c)}
        </mesh>
      ))}
    </group>
  );
}

function WaterCooler({ x, z }: { x: number; z: number }) {
  return (
    <group position={[x, 0, z]}>
      <RoundedBox args={[0.45, 1.0, 0.45]} radius={0.04} smoothness={2} position={[0, 0.5, 0]} castShadow>
        {flat('#f2f3f6')}
      </RoundedBox>
      <mesh position={[0, 0.72, 0.23]}>
        <boxGeometry args={[0.2, 0.14, 0.02]} />
        {flat('#8b93a5')}
      </mesh>
      <mesh position={[0, 1.27, 0]} castShadow>
        <cylinderGeometry args={[0.19, 0.19, 0.5, 10]} />
        <meshStandardMaterial color="#5aa2ee" transparent opacity={0.75} roughness={0.15} />
      </mesh>
    </group>
  );
}

function WallClock({ x, y, z }: { x: number; y: number; z: number }) {
  return (
    <group position={[x, y, z]} rotation={[Math.PI / 2, 0, 0]}>
      <mesh>
        <cylinderGeometry args={[0.36, 0.36, 0.06, 8]} />
        {flat('#3a3e49')}
      </mesh>
      <mesh position={[0, 0.035, 0]}>
        <cylinderGeometry args={[0.3, 0.3, 0.02, 8]} />
        {flat('#fafafa')}
      </mesh>
      <mesh position={[0, 0.05, -0.1]}>
        <boxGeometry args={[0.03, 0.01, 0.2]} />
        {flat('#222')}
      </mesh>
      <mesh position={[0.08, 0.05, 0]} rotation={[0, 0.3, 0]}>
        <boxGeometry args={[0.16, 0.01, 0.025]} />
        {flat('#222')}
      </mesh>
    </group>
  );
}

function Painting({ x, y, z }: { x: number; y: number; z: number }) {
  return (
    <group position={[x, y, z]}>
      <mesh>
        <boxGeometry args={[1.0, 0.75, 0.05]} />
        {flat('#2e323c')}
      </mesh>
      <mesh position={[0, 0, 0.03]}>
        <planeGeometry args={[0.86, 0.61]} />
        {flat('#f4f6f8')}
      </mesh>
      <mesh position={[0, -0.07, 0.035]}>
        <planeGeometry args={[0.72, 0.28]} />
        {flat('#6fa8dc')}
      </mesh>
      <mesh position={[0.05, -0.04, 0.04]} rotation={[0, 0, Math.PI / 4]}>
        <planeGeometry args={[0.3, 0.3]} />
        {flat('#3d6fb0')}
      </mesh>
      <mesh position={[0.22, 0.14, 0.04]}>
        <circleGeometry args={[0.07, 8]} />
        {flat('#f2c94c')}
      </mesh>
    </group>
  );
}

function FloorLamp({ x, z }: { x: number; z: number }) {
  return (
    <group position={[x, 0, z]}>
      <mesh position={[0, 0.03, 0]} castShadow>
        <cylinderGeometry args={[0.2, 0.22, 0.06, 8]} />
        {flat('#6b4f35')}
      </mesh>
      <mesh position={[0, 0.85, 0]} castShadow>
        <cylinderGeometry args={[0.025, 0.025, 1.6, 6]} />
        {flat('#6b4f35')}
      </mesh>
      <mesh position={[0, 1.75, 0]} castShadow>
        <cylinderGeometry args={[0.18, 0.3, 0.4, 7, 1, true]} />
        <meshStandardMaterial color="#fbecc8" emissive="#ffd890" emissiveIntensity={0.6} side={THREE.DoubleSide} flatShading />
      </mesh>
      <pointLight position={[0, 1.6, 0]} color="#ffd59a" intensity={3} distance={4} decay={2} />
    </group>
  );
}

function Sofa({ x, z, rot = 0 }: { x: number; z: number; rot?: number }) {
  return (
    <group position={[x, 0, z]} rotation={[0, rot, 0]}>
      <RoundedBox args={[2.0, 0.42, 0.85]} radius={0.06} smoothness={2} position={[0, 0.3, 0]} castShadow receiveShadow>
        {flat(C.sofa)}
      </RoundedBox>
      <RoundedBox args={[2.0, 0.55, 0.22]} radius={0.06} smoothness={2} position={[0, 0.72, -0.32]} castShadow>
        {flat(C.sofa)}
      </RoundedBox>
      {[-1, 1].map((s) => (
        <RoundedBox key={s} args={[0.2, 0.62, 0.85]} radius={0.05} smoothness={2} position={[s * 1.0, 0.42, 0]} castShadow>
          {flat('#5f90cb')}
        </RoundedBox>
      ))}
      {[-0.48, 0.48].map((dx, i) => (
        <RoundedBox key={i} args={[0.9, 0.1, 0.62]} radius={0.04} smoothness={2} position={[dx, 0.55, 0.08]} castShadow>
          {flat('#7aaae0')}
        </RoundedBox>
      ))}
    </group>
  );
}

function PrinterCabinet({ x, z }: { x: number; z: number }) {
  return (
    <group position={[x, 0, z]}>
      <mesh position={[0, 0.42, 0]} castShadow receiveShadow>
        <boxGeometry args={[1.3, 0.84, 0.6]} />
        {flat(C.oak)}
      </mesh>
      {[-0.32, 0.32].map((dx, i) => (
        <mesh key={i} position={[dx, 0.42, -0.31]}>
          <boxGeometry args={[0.6, 0.76, 0.02]} />
          {flat('#d6a660')}
        </mesh>
      ))}
      <RoundedBox args={[0.6, 0.32, 0.48]} radius={0.03} smoothness={2} position={[-0.25, 1.0, 0]} castShadow>
        {flat('#eceef2')}
      </RoundedBox>
      <mesh position={[-0.25, 1.17, 0]}>
        <boxGeometry args={[0.5, 0.03, 0.38]} />
        {flat('#5b6070')}
      </mesh>
    </group>
  );
}

function TrashBin({ x, z }: { x: number; z: number }) {
  return (
    <mesh position={[x, 0.2, z]} castShadow>
      <cylinderGeometry args={[0.17, 0.14, 0.4, 8]} />
      {flat('#6b7080')}
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
      <mesh position={[0, 0, -0.03]} castShadow>
        <boxGeometry args={[W.w + 0.2, W.h + 0.2, 0.06]} />
        {flat('#3a3e49')}
      </mesh>
      <mesh>
        <planeGeometry args={[W.w, W.h]} />
        <meshBasicMaterial map={texture} toneMapped={false} />
      </mesh>
      <mesh position={[0, -W.h / 2 - 0.12, 0.08]} castShadow>
        <boxGeometry args={[W.w * 0.6, 0.05, 0.14]} />
        {flat('#3a3e49')}
      </mesh>
      <Label position={[0, W.h / 2 + 0.3, 0]} height={0.31} color="#334155" text={"任务看板（点击查看）"} />
    </group>
  );
}

export function Office() {
  const agents = useDept((s) => s.agents);
  const F = FLOOR;
  const fw = F.x1 - F.x0;
  const fd = F.z1 - F.z0;
  const tiles = useTileTexture(fw, fd);
  const woodM = usePlankTexture(MANAGER_OFFICE.x1 - MANAGER_OFFICE.x0, MANAGER_OFFICE.z1 - MANAGER_OFFICE.z0, '#d8ad78');
  const woodR = usePlankTexture(MEETING_ROOM.x1 - MEETING_ROOM.x0, MEETING_ROOM.z1 - MEETING_ROOM.z0, '#ddb07a');
  const T = WALL_T;
  const DOOR = { x0: -1.3, x1: 1.3 };
  return (
    <group>
      {/* floor slab + tiles */}
      <mesh position={[0, -0.12, 0]} receiveShadow>
        <boxGeometry args={[fw + 2 * T, 0.24, fd + 2 * T]} />
        {flat('#c9cad6')}
      </mesh>
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[0, 0.001, 0]} receiveShadow>
        <planeGeometry args={[fw, fd]} />
        <meshStandardMaterial map={tiles} roughness={0.85} />
      </mesh>
      {/* room floors */}
      {[
        [MANAGER_OFFICE, woodM],
        [MEETING_ROOM, woodR],
      ].map(([r, tex], i) => {
        const rr = r as Rect;
        return (
          <mesh key={i} rotation={[-Math.PI / 2, 0, 0]} position={[(rr.x0 + rr.x1) / 2, 0.006, (rr.z0 + rr.z1) / 2]} receiveShadow>
            <planeGeometry args={[rr.x1 - rr.x0, rr.z1 - rr.z0]} />
            <meshStandardMaterial map={tex as THREE.Texture} roughness={0.8} />
          </mesh>
        );
      })}
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[(PANTRY.x0 + PANTRY.x1) / 2, 0.006, (PANTRY.z0 + PANTRY.z1) / 2]} receiveShadow>
        <planeGeometry args={[PANTRY.x1 - PANTRY.x0, PANTRY.z1 - PANTRY.z0]} />
        {flat('#e9dfcf')}
      </mesh>

      {/* outer walls: back (solid), left/right (windows), front (low, with entrance) */}
      <SolidWall r={{ x0: F.x0 - T, z0: F.z0 - T, x1: F.x1 + T, z1: F.z0 }} />
      <mesh position={[0, 0.06, F.z0 + 0.02]}>
        <boxGeometry args={[fw, 0.12, 0.04]} />
        {flat(C.baseboard)}
      </mesh>
      <WindowWall x={F.x0 - T / 2} z0={F.z0} z1={F.z1 + T} inward={1} />
      <WindowWall x={F.x1 + T / 2} z0={F.z0} z1={F.z1 + T} inward={-1} />
      <SolidWall r={{ x0: F.x0, z0: F.z1, x1: DOOR.x0, z1: F.z1 + T }} h={1.0} />
      <SolidWall r={{ x0: DOOR.x1, z0: F.z1, x1: F.x1, z1: F.z1 + T }} h={1.0} />
      {/* entrance: door frame, open glass door, doormat */}
      {[DOOR.x0, DOOR.x1].map((x, i) => (
        <mesh key={i} position={[x + (i ? 0.06 : -0.06), 1.1, F.z1 + T / 2]} castShadow>
          <boxGeometry args={[0.12, 2.2, T + 0.04]} />
          {flat(C.frame)}
        </mesh>
      ))}
      <group position={[DOOR.x1 - 0.05, 0, F.z1 + T]} rotation={[0, -1.15, 0]}>
        <mesh position={[-1.15, 1.05, 0]} castShadow>
          <boxGeometry args={[2.3, 2.1, 0.06]} />
          <meshStandardMaterial color={C.glass} transparent opacity={0.4} roughness={0.1} />
        </mesh>
        {[0.04, 2.06].map((y, i) => (
          <mesh key={i} position={[-1.15, y, 0]} castShadow>
            <boxGeometry args={[2.3, 0.08, 0.08]} />
            {flat(C.frame)}
          </mesh>
        ))}
        {[-0.02, -2.28].map((x, i) => (
          <mesh key={i} position={[x, 1.05, 0]} castShadow>
            <boxGeometry args={[0.08, 2.1, 0.08]} />
            {flat(C.frame)}
          </mesh>
        ))}
        <mesh position={[-2.1, 1.05, 0.08]}>
          <boxGeometry args={[0.04, 0.4, 0.04]} />
          {flat('#b08a4e')}
        </mesh>
      </group>
      <mesh position={[0, 0.01, F.z1 + T + 0.6]} receiveShadow>
        <boxGeometry args={[2.0, 0.02, 0.9]} />
        {flat('#5b6070')}
      </mesh>
      <Plant x={2.1} z={F.z1 + T + 0.7} scale={1.3} />

      {/* manager office (glass) */}
      <GlassWall x0={-7} z0={-9} x1={-7} z1={-3} />
      <GlassWall x0={-14} z0={-3} x1={MANAGER_DOOR.x0} z1={-3} />
      <GlassWall x0={MANAGER_DOOR.x1} z0={-3} x1={-7} z1={-3} />
      <Label position={[-10.5, 2.85, -2.95]} height={0.35} color="#7c5a12" text={"经理办公室"} />
      <Workstation desk={MANAGER_DESK} color={agents.max?.color} seed={1} />
      <Bookshelf x={-12.6} z={-8.72} w={1.8} h={1.9} />
      <FloorLamp x={-7.6} z={-8.5} />
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[-12.6, 0.012, -5.0]} receiveShadow>
        <planeGeometry args={[1.6, 2.4]} />
        {flat(C.rug)}
      </mesh>
      <Sofa x={-13.45} z={-5.0} rot={Math.PI / 2} />
      <group position={[-12.35, 0, -5.0]}>
        <Table w={0.6} d={1.1} h={0.42} />
        <Plant x={0} z={0.25} y={0.42} scale={0.45} />
      </group>

      {/* meeting room (glass) */}
      <GlassWall x0={7} z0={-9} x1={7} z1={-3} />
      <GlassWall x0={7} z0={-3} x1={MEETING_DOOR.x0} z1={-3} />
      <GlassWall x0={MEETING_DOOR.x1} z0={-3} x1={14} z1={-3} />
      <Label position={[10.5, 2.85, -2.95]} height={0.35} color="#1e3a5f" text={"会议室"} />
      <group position={[MEETING_TABLE.x, 0, MEETING_TABLE.z]}>
        <Table w={MEETING_TABLE.w} d={MEETING_TABLE.d} />
        <Plant x={0} z={0} y={0.75} scale={0.5} />
      </group>
      {MEETING_SEATS.map((s, i) => (
        <OfficeChair key={i} x={s.x} z={s.z + (s.rot === 0 ? -0.1 : 0.1)} rot={s.rot === 0 ? Math.PI : 0} />
      ))}
      {/* wall TV */}
      <mesh position={[10.5, 1.8, -8.95]} castShadow>
        <boxGeometry args={[2.2, 1.2, 0.06]} />
        {flat('#22252d')}
      </mesh>

      {/* open-plan desks */}
      {OPEN_DESKS.map((d, i) => (
        <Workstation key={d.id} desk={d} color={d.owner ? agents[d.owner]?.color : undefined} seed={i} />
      ))}
      {OPEN_DESKS.map((d) =>
        d.owner && agents[d.owner] ? (
          <Label key={d.id + 't'} position={[d.x, 0.02, d.z - 0.95]} rotation={[-Math.PI / 2, 0, 0]} height={0.29} color="#475569" text={`${agents[d.owner].name} · ${agents[d.owner].title}`} />
        ) : (
          <Label key={d.id + 't'} position={[d.x, 0.02, d.z - 0.95]} rotation={[-Math.PI / 2, 0, 0]} height={0.29} color="#94a3b8" text={"空工位"} />
        ),
      )}

      {/* back wall decor */}
      <Bookshelf x={-5.5} z={-8.72} w={2.2} h={1.9} />
      <WallClock x={-5.5} y={2.7} z={-8.96} />
      <WaterCooler x={4.4} z={-8.65} />
      <Painting x={5.6} y={2.3} z={-8.96} />

      {/* pantry */}
      <Label position={[11.5, 0.02, 4.3]} rotation={[-Math.PI / 2, 0, 0]} height={0.35} color="#7c4a1e" text={"茶水间"} />
      <group position={[(PANTRY_COUNTER.x0 + PANTRY_COUNTER.x1) / 2, 0, (PANTRY_COUNTER.z0 + PANTRY_COUNTER.z1) / 2]}>
        <mesh position={[0, 0.45, 0]} castShadow receiveShadow>
          <boxGeometry args={[PANTRY_COUNTER.x1 - PANTRY_COUNTER.x0, 0.9, PANTRY_COUNTER.z1 - PANTRY_COUNTER.z0]} />
          {flat(C.oak)}
        </mesh>
        <mesh position={[0, 0.93, 0]} castShadow receiveShadow>
          <boxGeometry args={[PANTRY_COUNTER.x1 - PANTRY_COUNTER.x0 + 0.04, 0.06, PANTRY_COUNTER.z1 - PANTRY_COUNTER.z0 + 0.04]} />
          {flat('#f1f1ee')}
        </mesh>
      </group>
      {/* coffee machine */}
      <RoundedBox args={[0.45, 0.5, 0.4]} radius={0.04} smoothness={2} position={[10.6, 1.21, 8.6]} castShadow>
        {flat('#2d313a')}
      </RoundedBox>
      <Mug x={10.95} z={8.5} />
      {/* fridge */}
      <RoundedBox args={[0.8, 2, 0.7]} radius={0.05} smoothness={2} position={[13.4, 1.0, 8.55]} castShadow>
        {flat('#e7eaef')}
      </RoundedBox>
      <mesh position={[13.08, 1.25, 8.19]}>
        <boxGeometry args={[0.04, 0.5, 0.04]} />
        {flat('#8b93a5')}
      </mesh>
      <mesh position={[PANTRY_TABLE.x, 0.72, PANTRY_TABLE.z]} castShadow receiveShadow>
        <cylinderGeometry args={[PANTRY_TABLE.r, PANTRY_TABLE.r, 0.06, 10]} />
        {flat(C.oak)}
      </mesh>
      <mesh position={[PANTRY_TABLE.x, 0.36, PANTRY_TABLE.z]}>
        <cylinderGeometry args={[0.05, 0.05, 0.7, 6]} />
        {flat(C.metal)}
      </mesh>
      <mesh position={[PANTRY_TABLE.x, 0.02, PANTRY_TABLE.z]}>
        <cylinderGeometry args={[0.3, 0.3, 0.04, 8]} />
        {flat(C.metal)}
      </mesh>

      {/* printer corner */}
      <PrinterCabinet x={-11.6} z={8.6} />
      <TrashBin x={-10.5} z={8.6} />

      {/* plants */}
      {[
        [-13.5, 8.5, 1.5],
        [6.5, -8.55, 1.2],
        [-13.5, -2.4, 1.3],
        [13.5, -2.4, 1.3],
        [13.5, 3.9, 1.2],
        [-3.9, -8.6, 1.1],
      ].map(([x, z, s], i) => (
        <Plant key={i} x={x} z={z} scale={s} />
      ))}

      <Whiteboard />
    </group>
  );
}
