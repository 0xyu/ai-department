import { useMemo } from 'react';
import * as THREE from 'three';

/** Text rendered to a canvas texture with system fonts (works offline and for Chinese). */
export function Label({
  text,
  position,
  rotation = [0, 0, 0],
  height = 0.3,
  color = '#334155',
}: {
  text: string;
  position: [number, number, number];
  rotation?: [number, number, number];
  height?: number;
  color?: string;
}) {
  const { texture, aspect } = useMemo(() => {
    const px = 64;
    const font = `600 ${px}px "PingFang SC","Microsoft YaHei","Noto Sans CJK SC",sans-serif`;
    const c = document.createElement('canvas');
    const g = c.getContext('2d')!;
    g.font = font;
    const w = Math.ceil(g.measureText(text).width) + 16;
    c.width = w;
    c.height = px + 24;
    g.font = font;
    g.fillStyle = color;
    g.textBaseline = 'middle';
    g.fillText(text, 8, c.height / 2);
    const t = new THREE.CanvasTexture(c);
    t.colorSpace = THREE.SRGBColorSpace;
    t.anisotropy = 4;
    return { texture: t, aspect: w / c.height };
  }, [text, color]);
  return (
    <mesh position={position} rotation={rotation}>
      <planeGeometry args={[height * aspect, height]} />
      <meshBasicMaterial map={texture} transparent toneMapped={false} depthWrite={false} />
    </mesh>
  );
}
