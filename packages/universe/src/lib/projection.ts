import { mulberry32, fnv1a } from "./hash";
import type { UniverseModel } from "./model";

// The meaning view's positions: the runtime's PCA coordinates (RFC FQ P2),
// centred on their mean and scaled so the cloud has about the density of the
// structure view (radius ~ 2·√n + 20, set by its 98th-percentile spread so a
// few outliers do not shrink everything else). The third component can be
// dropped (planar).
//
// A chunk with no coordinates (not projected yet, or embedded by another model)
// sits beside its document's root when the root has some, with an id-seeded
// offset; otherwise it has no place in this view and is hidden. RFC FQ's
// OQ2 suggested its structure position, but the two views use different
// frames, so that would scatter unplaced stars far outside the cloud.

export interface ProjectionFrame {
  /** xyz per chunk, in scene units. */
  pos: Float32Array;
  /** 1 = the chunk has a place in the meaning view. */
  placed: Uint8Array;
  center: [number, number, number];
  scale: number;
  radius: number;
  planar: boolean;
}

export function projectionFrame(m: UniverseModel, planar: boolean): ProjectionFrame {
  const n = m.chunkCount;
  const own = new Uint8Array(n);
  const c: [number, number, number] = [0, 0, 0];
  let k = 0;
  for (let i = 0; i < n; i++) {
    if (Number.isNaN(m.chunk.proj[i * 3])) continue;
    own[i] = 1;
    c[0] += m.chunk.proj[i * 3];
    c[1] += m.chunk.proj[i * 3 + 1];
    c[2] += planar ? 0 : m.chunk.proj[i * 3 + 2];
    k++;
  }
  if (k > 0) {
    c[0] /= k;
    c[1] /= k;
    c[2] /= k;
  }
  const dists: number[] = [];
  for (let i = 0; i < n; i++) {
    if (!own[i]) continue;
    const z = planar ? 0 : m.chunk.proj[i * 3 + 2] - c[2];
    dists.push(Math.hypot(m.chunk.proj[i * 3] - c[0], m.chunk.proj[i * 3 + 1] - c[1], z));
  }
  dists.sort((a, b) => a - b);
  const spread = dists.length ? dists[Math.min(dists.length - 1, Math.floor(dists.length * 0.98))] : 1;
  const radius = 2 * Math.sqrt(Math.max(1, k)) + 20;
  const scale = spread > 1e-9 ? radius / spread : 1;
  const pos = new Float32Array(n * 3);
  const placed = new Uint8Array(n);
  for (let i = 0; i < n; i++) {
    if (!own[i]) continue;
    pos[i * 3] = (m.chunk.proj[i * 3] - c[0]) * scale;
    pos[i * 3 + 1] = (m.chunk.proj[i * 3 + 1] - c[1]) * scale;
    pos[i * 3 + 2] = planar ? 0 : (m.chunk.proj[i * 3 + 2] - c[2]) * scale;
    placed[i] = 1;
  }
  for (let i = 0; i < n; i++) {
    if (own[i]) continue;
    const root = m.doc.root[m.chunk.doc[i]];
    if (root < 0 || !own[root]) continue;
    const rnd = mulberry32(fnv1a(m.chunk.id[i]));
    const r = 0.6 + rnd() * 0.8;
    const t = rnd() * Math.PI * 2;
    const u = planar ? 0 : rnd() * 2 - 1;
    const s = Math.sqrt(1 - u * u);
    pos[i * 3] = pos[root * 3] + r * s * Math.cos(t);
    pos[i * 3 + 1] = pos[root * 3 + 1] + r * s * Math.sin(t);
    pos[i * 3 + 2] = pos[root * 3 + 2] + r * u;
    placed[i] = 1;
  }
  return { pos, placed, center: c, scale, radius, planar };
}

/** toFrame maps a raw projected point (the search query's) into the frame. */
export function toFrame(f: ProjectionFrame, xyz: [number, number, number]): [number, number, number] {
  return [
    (xyz[0] - f.center[0]) * f.scale,
    (xyz[1] - f.center[1]) * f.scale,
    f.planar ? 0 : (xyz[2] - f.center[2]) * f.scale,
  ];
}
