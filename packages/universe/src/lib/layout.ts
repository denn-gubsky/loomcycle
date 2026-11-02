import { fnv1a, mulberry32 } from "./hash";
import { homeDir, type UniverseModel } from "./model";

// The structure layout: deterministic, computed from ids and names only, so the
// same scope draws the same sky in every session and on every client, and a
// reload that keeps chunk ids keeps every constellation where it was.
//
//   1. Constellations. A document's chunk tree is laid out around its root:
//      depth-1 chunks on a Fibonacci sphere, deeper chunks fanned in a cone
//      along their parent's outward direction, with id-seeded jitter.
//   2. Formations. The directory tree becomes nested spheres: a directory's
//      children (sub-directories and documents) sit on a Fibonacci sphere
//      around its centre, at a radius that keeps siblings from touching, the
//      sphere rotated by a rotation seeded from the directory's path.
//
// Units are arbitrary; a leaf-to-parent step is ~1.

export const UNFILED = "(unfiled)";

export interface DirNode {
  path: string;
  name: string;
  parent: number;
  children: number[];
  docs: number[];
  /** Documents in this directory and every directory under it. */
  docTotal: number;
  center: [number, number, number];
  radius: number;
  /** Radius of the sphere the children sit on. */
  rho: number;
  depth: number;
}

export interface Layout {
  /** xyz per chunk. */
  chunkPos: Float32Array;
  /** xyz per document (its root chunk's position). */
  docCenter: Float32Array;
  docRadius: Float32Array;
  dirs: DirNode[];
  /** The directory each document is filed in. */
  docDir: Int32Array;
  /** Bounding radius of the whole sky around the origin. */
  radius: number;
}

type V3 = [number, number, number];
type M3 = [number, number, number, number, number, number, number, number, number];

const GOLDEN = Math.PI * (3 - Math.sqrt(5));
// Breathing room between sibling spheres.
const GAP = 1.06;
const PAIR_CHECK_MAX = 1500;

// fib returns the i-th of n points evenly spread on the unit sphere.
function fib(i: number, n: number): V3 {
  if (n === 1) return [0, 0, 0];
  const y = 1 - (2 * (i + 0.5)) / n;
  const r = Math.sqrt(Math.max(0, 1 - y * y));
  const t = GOLDEN * i;
  return [Math.cos(t) * r, y, Math.sin(t) * r];
}

// rotation returns a uniformly random rotation matrix seeded by s (Shoemake's
// quaternion method), so formations do not all face the same way.
function rotation(seed: number): M3 {
  const rnd = mulberry32(seed);
  const u1 = rnd();
  const u2 = rnd() * 2 * Math.PI;
  const u3 = rnd() * 2 * Math.PI;
  const a = Math.sqrt(1 - u1);
  const b = Math.sqrt(u1);
  const x = a * Math.sin(u2);
  const y = a * Math.cos(u2);
  const z = b * Math.sin(u3);
  const w = b * Math.cos(u3);
  return [
    1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w),
    2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w),
    2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y),
  ];
}

function apply(m: M3, v: V3): V3 {
  return [
    m[0] * v[0] + m[1] * v[1] + m[2] * v[2],
    m[3] * v[0] + m[4] * v[1] + m[5] * v[2],
    m[6] * v[0] + m[7] * v[1] + m[8] * v[2],
  ];
}

function norm(v: V3): V3 {
  const l = Math.hypot(v[0], v[1], v[2]) || 1;
  return [v[0] / l, v[1] / l, v[2] / l];
}

function cross(a: V3, b: V3): V3 {
  return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
}

// basis returns two unit vectors perpendicular to d and to each other.
function basis(d: V3): [V3, V3] {
  const helper: V3 = Math.abs(d[1]) < 0.9 ? [0, 1, 0] : [1, 0, 0];
  const u = norm(cross(d, helper));
  const v = cross(d, u);
  return [u, v];
}

// layoutConstellation writes document di's chunk positions RELATIVE to its
// root into out (xyz per chunk index) and returns the constellation's radius.
function layoutConstellation(m: UniverseModel, di: number, out: Float32Array, kids: number[][]): number {
  const start = m.doc.chunkStart[di];
  const count = m.doc.chunkCount[di];
  const root = m.doc.root[di];
  if (count === 0 || root < 0) return 0.6;
  out[root * 3] = out[root * 3 + 1] = out[root * 3 + 2] = 0;
  let radius = 0.6;
  // Breadth-first from the root; a parent is placed before its children.
  const queue: number[] = [root];
  const parentless: number[] = [];
  for (let c = start; c < start + count; c++) if (c !== root && m.chunk.parent[c] < 0) parentless.push(c);
  // Stray roots (a document whose tree has several) hang off the root.
  const rootKids = [...kids[root - start], ...parentless];
  for (let q = 0; q < queue.length; q++) {
    const p = queue[q];
    const children = p === root ? rootKids : kids[p - start];
    const n = children.length;
    if (n === 0) continue;
    const px = out[p * 3];
    const py = out[p * 3 + 1];
    const pz = out[p * 3 + 2];
    if (p === root) {
      const r1 = 1.1 + 0.32 * Math.sqrt(n);
      for (let i = 0; i < n; i++) {
        const c = children[i];
        const rnd = mulberry32(fnv1a(m.chunk.id[c]));
        const d = n === 1 ? norm([rnd() - 0.5, rnd() - 0.5, rnd() - 0.5]) : fib(i, n);
        const len = r1 * (0.9 + 0.2 * rnd());
        out[c * 3] = d[0] * len;
        out[c * 3 + 1] = d[1] * len;
        out[c * 3 + 2] = d[2] * len;
        queue.push(c);
      }
      continue;
    }
    // Outward direction: away from the grandparent (or the root).
    const gp = m.chunk.parent[p];
    const gx = gp >= 0 ? out[gp * 3] : 0;
    const gy = gp >= 0 ? out[gp * 3 + 1] : 0;
    const gz = gp >= 0 ? out[gp * 3 + 2] : 0;
    const dir = norm([px - gx, py - gy, pz - gz]);
    const [u, v] = basis(dir);
    const depth = m.chunk.depth[p] + 1;
    const step = 0.85 * Math.pow(0.82, Math.max(0, depth - 2)) + 0.05 * Math.sqrt(n);
    const spread = Math.min(1.25, 0.3 + 0.11 * n);
    for (let i = 0; i < n; i++) {
      const c = children[i];
      const rnd = mulberry32(fnv1a(m.chunk.id[c]));
      const phi = GOLDEN * i + rnd() * 0.3;
      const tilt = n === 1 ? rnd() * 0.25 : spread * Math.sqrt((i + 0.5) / n);
      const ct = Math.cos(tilt);
      const st = Math.sin(tilt);
      const cp = Math.cos(phi);
      const sp = Math.sin(phi);
      const len = step * (0.88 + 0.24 * rnd());
      const dx = ct * dir[0] + st * (cp * u[0] + sp * v[0]);
      const dy = ct * dir[1] + st * (cp * u[1] + sp * v[1]);
      const dz = ct * dir[2] + st * (cp * u[2] + sp * v[2]);
      out[c * 3] = px + dx * len;
      out[c * 3 + 1] = py + dy * len;
      out[c * 3 + 2] = pz + dz * len;
      queue.push(c);
    }
  }
  for (let c = start; c < start + count; c++) {
    const r = Math.hypot(out[c * 3], out[c * 3 + 1], out[c * 3 + 2]);
    if (r > radius) radius = r;
  }
  return radius + 0.4;
}

// buildDirs turns the documents' home directories (plus the snapshot's empty
// directories) into a tree rooted at "/".
function buildDirs(m: UniverseModel): { dirs: DirNode[]; docDir: Int32Array } {
  const dirs: DirNode[] = [];
  const byPath = new Map<string, number>();
  const ensure = (path: string): number => {
    const hit = byPath.get(path);
    if (hit !== undefined) return hit;
    let parent = -1;
    let name = "/";
    if (path !== "/") {
      const cut = path.lastIndexOf("/");
      parent = ensure(cut <= 0 ? "/" : path.slice(0, cut));
      name = path.slice(cut + 1);
    }
    const i = dirs.length;
    dirs.push({
      path,
      name,
      parent,
      children: [],
      docs: [],
      docTotal: 0,
      center: [0, 0, 0],
      radius: 0,
      rho: 0,
      depth: parent >= 0 ? dirs[parent].depth + 1 : 0,
    });
    byPath.set(path, i);
    if (parent >= 0) dirs[parent].children.push(i);
    return i;
  };
  ensure("/");
  for (const p of [...m.dirs].sort()) if (p.startsWith("/")) ensure(p.length > 1 && p.endsWith("/") ? p.slice(0, -1) : p);
  const docDir = new Int32Array(m.docCount);
  for (let di = 0; di < m.docCount; di++) {
    const home = homeDir(m, di) || `/${UNFILED}`;
    const d = ensure(home);
    docDir[di] = d;
    dirs[d].docs.push(di);
  }
  // Children (and documents) in name order, so the placement is stable however
  // the pages arrived.
  for (const d of dirs) {
    d.children.sort((a, b) => (dirs[a].name < dirs[b].name ? -1 : dirs[a].name > dirs[b].name ? 1 : 0));
    d.docs.sort((a, b) => {
      const ta = m.doc.paths[a]?.[0] ?? m.doc.id[a];
      const tb = m.doc.paths[b]?.[0] ?? m.doc.id[b];
      return ta < tb ? -1 : ta > tb ? 1 : 0;
    });
  }
  // Post-order totals: a child directory always has a larger index than its parent.
  for (let i = dirs.length - 1; i >= 0; i--) {
    dirs[i].docTotal += dirs[i].docs.length;
    if (dirs[i].parent >= 0) dirs[dirs[i].parent].docTotal += dirs[i].docTotal;
  }
  return { dirs, docDir };
}

export function computeLayout(m: UniverseModel): Layout {
  const chunkPos = new Float32Array(m.chunkCount * 3);
  const docCenter = new Float32Array(m.docCount * 3);
  const docRadius = new Float32Array(m.docCount);
  // Children per chunk (doc-local index), ordered by position then id.
  const local = new Float32Array(m.chunkCount * 3);
  for (let di = 0; di < m.docCount; di++) {
    const start = m.doc.chunkStart[di];
    const count = m.doc.chunkCount[di];
    const kids: number[][] = Array.from({ length: count }, () => []);
    for (let c = start; c < start + count; c++) {
      const p = m.chunk.parent[c];
      if (p >= start && p < start + count) kids[p - start].push(c);
    }
    for (const k of kids)
      k.sort((a, b) => m.chunk.position[a] - m.chunk.position[b] || (m.chunk.id[a] < m.chunk.id[b] ? -1 : 1));
    docRadius[di] = layoutConstellation(m, di, local, kids);
  }
  const { dirs, docDir } = buildDirs(m);
  // Bottom-up radii. Children sit on fixed Fibonacci directions, so the
  // smallest sphere radius at which no two touch is exact: for each pair,
  // rho >= (r_i + r_j) / |u_i - u_j|. A very wide flat directory (thousands of
  // similar documents) uses the area estimate instead of the quadratic check.
  for (let i = dirs.length - 1; i >= 0; i--) {
    const d = dirs[i];
    const radii = [...d.children.map((c) => dirs[c].radius), ...d.docs.map((di) => docRadius[di])];
    const n = radii.length;
    let maxR = 0.6;
    for (const r of radii) maxR = Math.max(maxR, r);
    let rho = 0;
    if (n > PAIR_CHECK_MAX) {
      rho = maxR * (1.15 + 1.1 * Math.sqrt(n / Math.PI));
    } else if (n > 1) {
      const u = radii.map((_, k) => fib(k, n));
      for (let a = 0; a < n; a++) {
        for (let b = a + 1; b < n; b++) {
          const dist = Math.hypot(u[a][0] - u[b][0], u[a][1] - u[b][1], u[a][2] - u[b][2]);
          rho = Math.max(rho, ((radii[a] + radii[b]) * GAP) / Math.max(dist, 1e-6));
        }
      }
    }
    d.rho = rho;
    let outer = 0.6;
    for (const r of radii) outer = Math.max(outer, rho + r);
    d.radius = outer;
  }
  // Top-down placement.
  const place = (i: number, center: V3) => {
    const d = dirs[i];
    d.center = center;
    const items: { kind: "dir" | "doc"; idx: number }[] = [
      ...d.children.map((idx) => ({ kind: "dir" as const, idx })),
      ...d.docs.map((idx) => ({ kind: "doc" as const, idx })),
    ];
    const n = items.length;
    if (n === 0) return;
    const rho = d.rho;
    const rot = rotation(fnv1a(d.path));
    items.forEach((it, k) => {
      const off = apply(rot, fib(k, n));
      const c: V3 = [center[0] + off[0] * rho, center[1] + off[1] * rho, center[2] + off[2] * rho];
      if (it.kind === "dir") {
        place(it.idx, c);
        return;
      }
      const di = it.idx;
      docCenter[di * 3] = c[0];
      docCenter[di * 3 + 1] = c[1];
      docCenter[di * 3 + 2] = c[2];
      const r = rotation(fnv1a(m.doc.id[di]));
      const start = m.doc.chunkStart[di];
      for (let ch = start; ch < start + m.doc.chunkCount[di]; ch++) {
        const p = apply(r, [local[ch * 3], local[ch * 3 + 1], local[ch * 3 + 2]]);
        chunkPos[ch * 3] = c[0] + p[0];
        chunkPos[ch * 3 + 1] = c[1] + p[1];
        chunkPos[ch * 3 + 2] = c[2] + p[2];
      }
    });
  };
  place(0, [0, 0, 0]);
  return { chunkPos, docCenter, docRadius, dirs, docDir, radius: dirs[0].radius };
}
