import type { Layout } from "./layout";
import type { ModelEdge, UniverseModel } from "./model";

// Level of detail (research decisions L1–L4). A directory whose formation is
// small on screen collapses to one star; a document whose constellation is
// small collapses to its root star; only an expanded document shows its chunk
// stars. The decision is by projected screen radius with hysteresis, so a
// constellation at the threshold does not flicker as the camera drifts. Links
// whose endpoint is hidden climb to the star that stands for it.
//
// Star indexes: [0, chunkCount) are chunks, chunkCount + d is directory d.

export const COLLAPSE_PX = 24;
export const EXPAND_PX = 40;

export interface LodView {
  cam: [number, number, number];
  /** Viewport height in px / (2·tan(fov/2)): px per world unit at distance 1. */
  pxPerUnit: number;
}

export interface Lod {
  /** 1 = this directory shows as a single star. */
  dirCollapsed: Uint8Array;
  /** The highest collapsed directory at or above each directory, or -1. */
  dirTop: Int32Array;
  /** 1 = this document shows its chunk stars. */
  docExpanded: Uint8Array;
}

function projected(cx: number, cy: number, cz: number, r: number, v: LodView): number {
  const d = Math.hypot(cx - v.cam[0], cy - v.cam[1], cz - v.cam[2]);
  // Inside (or nearly inside) the sphere: it fills the view.
  if (d <= r * 1.05) return Infinity;
  return (r * v.pxPerUnit) / d;
}

// computeLod decides each directory and document. `pinned` documents (the
// selection, search hits) stay expanded and keep their directories open.
export function computeLod(
  m: UniverseModel,
  l: Layout,
  v: LodView,
  prev: Lod | null,
  pinned: ReadonlySet<number>,
): Lod {
  const nd = l.dirs.length;
  const dirCollapsed = new Uint8Array(nd);
  const dirTop = new Int32Array(nd).fill(-1);
  const docExpanded = new Uint8Array(m.docCount);
  const keepOpen = new Uint8Array(nd);
  for (const di of pinned) {
    for (let d = l.docDir[di]; d >= 0; d = l.dirs[d].parent) keepOpen[d] = 1;
  }
  // Directories in index order: a parent always precedes its children.
  for (let d = 0; d < nd; d++) {
    const dir = l.dirs[d];
    const parentTop = dir.parent >= 0 ? dirTop[dir.parent] : -1;
    if (parentTop >= 0) {
      dirTop[d] = parentTop;
      continue;
    }
    // The root never collapses, nor does a formation of one document (its
    // document star already stands for it), nor one holding a pinned document.
    if (d === 0 || dir.docTotal <= 1 || keepOpen[d]) continue;
    const px = projected(dir.center[0], dir.center[1], dir.center[2], dir.radius, v);
    const was = prev && prev.dirCollapsed.length === nd ? prev.dirCollapsed[d] === 1 : px < COLLAPSE_PX;
    const collapsed = was ? px < EXPAND_PX : px < COLLAPSE_PX;
    if (collapsed) {
      dirCollapsed[d] = 1;
      dirTop[d] = d;
    }
  }
  for (let di = 0; di < m.docCount; di++) {
    if (dirTop[l.docDir[di]] >= 0) continue;
    if (pinned.has(di)) {
      docExpanded[di] = 1;
      continue;
    }
    const px = projected(l.docCenter[di * 3], l.docCenter[di * 3 + 1], l.docCenter[di * 3 + 2], l.docRadius[di], v);
    const was = prev && prev.docExpanded.length === m.docCount ? prev.docExpanded[di] === 1 : px > EXPAND_PX;
    docExpanded[di] = (was ? px > COLLAPSE_PX : px > EXPAND_PX) ? 1 : 0;
  }
  return { dirCollapsed, dirTop, docExpanded };
}

/** The star that stands for chunk c under lod: itself, its document's root, or
 *  its collapsed formation's star. */
export function representative(m: UniverseModel, l: Layout, lod: Lod, c: number): number {
  const di = m.chunk.doc[c];
  const top = lod.dirTop[l.docDir[di]];
  if (top >= 0) return m.chunkCount + top;
  if (!lod.docExpanded[di]) return m.doc.root[di];
  return c;
}

/** Whether star s is drawn under lod. */
export function starVisible(m: UniverseModel, l: Layout, lod: Lod, s: number): boolean {
  if (s >= m.chunkCount) {
    const d = s - m.chunkCount;
    return lod.dirCollapsed[d] === 1 && lod.dirTop[d] === d;
  }
  const di = m.chunk.doc[s];
  if (lod.dirTop[l.docDir[di]] >= 0) return false;
  return lod.docExpanded[di] === 1 || m.doc.root[di] === s;
}

export interface DrawnLink {
  a: number;
  b: number;
  kind: string;
  /** Edges merged into this line (parallel links between collapsed stars). */
  count: number;
  /** Any of them proposed (drawn dashed). */
  proposed: boolean;
}

// drawnLinks maps each edge's endpoints to their representatives and merges
// the parallel ones, so a hundred links between two far formations draw as one
// line whose weight says a hundred. A link that folds into a single star (both
// ends inside one collapsed document or formation) is not drawn.
export function drawnLinks(m: UniverseModel, l: Layout, lod: Lod, edges: readonly ModelEdge[]): DrawnLink[] {
  const merged = new Map<string, DrawnLink>();
  for (const e of edges) {
    if (e.from < 0 || e.to < 0) continue;
    const a = representative(m, l, lod, e.from);
    const b = representative(m, l, lod, e.to);
    if (a === b) continue;
    const key = `${a}|${b}|${e.kind}`;
    const hit = merged.get(key);
    if (hit) {
      hit.count++;
      if (e.status === "proposed") hit.proposed = true;
    } else merged.set(key, { a, b, kind: e.kind, count: 1, proposed: e.status === "proposed" });
  }
  return [...merged.values()];
}

// neighbourhood collects the edges within `depth` hops of the seed chunks. A
// seed that is a document's root stands for the whole document (selecting a
// document shows every link out of it); hops past the first follow chunks only,
// so a depth-3 walk does not swallow every document a hub touches.
export function neighbourhood(
  m: UniverseModel,
  adjacency: ReadonlyMap<number, number[]>,
  seeds: readonly number[],
  depth: number,
  kinds?: ReadonlySet<string>,
): { edges: ModelEdge[]; chunks: Set<number> } {
  const chunks = new Set<number>();
  let frontier: number[] = [];
  for (const s of seeds) {
    const di = m.chunk.doc[s];
    if (m.doc.root[di] === s) {
      const start = m.doc.chunkStart[di];
      for (let c = start; c < start + m.doc.chunkCount[di]; c++) frontier.push(c);
    } else frontier.push(s);
  }
  for (const c of frontier) chunks.add(c);
  const picked = new Set<number>();
  for (let hop = 0; hop < depth && frontier.length > 0; hop++) {
    const next: number[] = [];
    for (const c of frontier) {
      for (const ei of adjacency.get(c) ?? []) {
        const e = m.edges[ei];
        if (kinds && !kinds.has(e.kind)) continue;
        if (picked.has(ei)) continue;
        picked.add(ei);
        for (const o of [e.from, e.to]) {
          if (o >= 0 && !chunks.has(o)) {
            chunks.add(o);
            next.push(o);
          }
        }
      }
    }
    frontier = next;
  }
  return { edges: [...picked].map((i) => m.edges[i]), chunks };
}

/** adjacencyOf indexes the model's resolved edges by chunk, both directions. */
export function adjacencyOf(m: UniverseModel): Map<number, number[]> {
  const adj = new Map<number, number[]>();
  const add = (c: number, ei: number) => {
    if (c < 0) return;
    const l = adj.get(c);
    if (l) l.push(ei);
    else adj.set(c, [ei]);
  };
  m.edges.forEach((e, i) => {
    add(e.from, i);
    add(e.to, i);
  });
  return adj;
}
