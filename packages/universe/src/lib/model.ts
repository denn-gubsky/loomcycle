import type { GraphSnapshot } from "../types";

// The universe model: every page of a graph_snapshot folded into one set of
// parallel arrays. Pages carry their own code tables and index their own rows,
// so add() re-interns codes into global string tables and re-bases indexes.
// A page always holds whole documents (the runtime pages by document), so a
// document's chunks are contiguous here: [chunkStart, chunkStart + chunkCount).

export interface ModelEdge {
  id: string;
  /** Chunk index, or -1 while the endpoint's document is not loaded. */
  from: number;
  to: number;
  fromId: string;
  toId: string;
  fromDocId: string;
  toDocId: string;
  kind: string;
  status: string;
  method: string;
  confidence: number | null;
  auto: boolean;
}

export interface UniverseModel {
  /** Bumps on every page, so consumers can memoize on it. */
  version: number;
  docCount: number;
  chunkCount: number;
  doc: {
    id: string[];
    title: string[];
    type: string[];
    status: string[];
    updatedAt: number[];
    colorEnabled: boolean[];
    scheme: (Record<string, string> | null)[];
    /** Every Path name, shortest first (the runtime's order). */
    paths: string[][];
    root: number[];
    chunkStart: number[];
    chunkCount: number[];
  };
  chunk: {
    id: string[];
    doc: number[];
    parent: number[];
    depth: number[];
    position: number[];
    title: string[];
    type: string[];
    status: string[];
    updatedAt: number[];
    invalidAt: (number | null)[];
    isFact: boolean[];
    retired: boolean[];
    /** Projected xyz from the runtime's embedding basis, NaN while missing. */
    proj: number[];
  };
  edges: ModelEdge[];
  /** The embedding projection the pages carried: the embedder model and basis
   *  version (0 = none built yet), and how many chunks have coordinates. */
  projection: { model: string; basisVersion: number; placed: number; mixedBasis: boolean };
  /** Directory paths the snapshot named (so empty-but-real folders exist). */
  dirs: Set<string>;
  edgesTruncated: boolean;
  kinds: string[];
  types: string[];
  statuses: string[];
  docIndex: Map<string, number>;
  chunkIndex: Map<string, number>;
}

export function emptyModel(): UniverseModel {
  return {
    version: 0,
    docCount: 0,
    chunkCount: 0,
    doc: {
      id: [],
      title: [],
      type: [],
      status: [],
      updatedAt: [],
      colorEnabled: [],
      scheme: [],
      paths: [],
      root: [],
      chunkStart: [],
      chunkCount: [],
    },
    chunk: {
      id: [],
      doc: [],
      parent: [],
      depth: [],
      position: [],
      title: [],
      type: [],
      status: [],
      updatedAt: [],
      invalidAt: [],
      isFact: [],
      retired: [],
      proj: [],
    },
    edges: [],
    projection: { model: "", basisVersion: 0, placed: 0, mixedBasis: false },
    dirs: new Set(),
    edgesTruncated: false,
    kinds: [],
    types: [],
    statuses: [],
    docIndex: new Map(),
    chunkIndex: new Map(),
  };
}

const at = <T,>(xs: T[] | undefined, i: number, fallback: T): T =>
  xs && i >= 0 && i < xs.length && xs[i] !== undefined && xs[i] !== null ? (xs[i] as T) : fallback;

const code = (table: string[] | undefined, i: number | undefined): string =>
  table && i !== undefined && i >= 0 && i < table.length ? table[i] : "";

function addUnique(list: string[], v: string) {
  if (v && !list.includes(v)) list.push(v);
}

// addPage folds one snapshot page into m (mutating it) and returns m with a new
// version. A document already present (a page re-fetched by chunk_ids after a
// change) is skipped here; refreshing a constellation is a reload.
export function addPage(m: UniverseModel, page: GraphSnapshot): UniverseModel {
  const c = page.codes ?? {};
  const pd = page.documents;
  const pc = page.chunks;
  // Page doc idx → model doc idx (-1 = skipped duplicate).
  const docMap: number[] = [];
  if (pd) {
    for (let i = 0; i < pd.ids.length; i++) {
      if (m.docIndex.has(pd.ids[i])) {
        docMap.push(-1);
        continue;
      }
      const di = m.docCount++;
      docMap.push(di);
      m.docIndex.set(pd.ids[i], di);
      m.doc.id.push(pd.ids[i]);
      m.doc.title.push(at(pd.titles, i, ""));
      const type = code(c.types, pd.type_code[i]);
      const status = code(c.statuses, pd.status_code[i]);
      m.doc.type.push(type);
      m.doc.status.push(status);
      addUnique(m.types, type);
      addUnique(m.statuses, status);
      m.doc.updatedAt.push(at(pd.updated_at, i, 0));
      m.doc.colorEnabled.push(at(pd.color_enabled, i, false));
      m.doc.scheme.push(at(pd.color_scheme, i, null));
      m.doc.paths.push(at(pd.paths, i, [] as string[]));
      m.doc.root.push(-1);
      m.doc.chunkStart.push(-1);
      m.doc.chunkCount.push(0);
    }
  }
  // Page chunk idx → model chunk idx (-1 = its document was skipped).
  const chunkMap: number[] = [];
  if (pc) {
    chunkMap.length = pc.ids.length;
    chunkMap.fill(-1);
    // Ingest grouped by document (a stable sort keeps each tree's parent-first
    // order), so a document's chunks are contiguous whatever order the page used.
    const order = pc.ids.map((_, i) => i);
    order.sort((a, b) => pc.document_idx[a] - pc.document_idx[b] || a - b);
    for (const i of order) {
      const di = docMap[pc.document_idx[i]] ?? -1;
      if (di < 0 || m.chunkIndex.has(pc.ids[i])) continue;
      const ci = m.chunkCount++;
      chunkMap[i] = ci;
      if (m.doc.chunkStart[di] < 0) m.doc.chunkStart[di] = ci;
      m.chunkIndex.set(pc.ids[i], ci);
      m.chunk.id.push(pc.ids[i]);
      m.chunk.doc.push(di);
      const pp = pc.parent_idx[i];
      // The runtime emits a parent before its children; a parent index that
      // does not point backwards (or into a skipped document) reads as a root.
      const parent = pp >= 0 && pp < i ? chunkMap[pp] : -1;
      m.chunk.parent.push(parent);
      m.chunk.depth.push(parent >= 0 ? m.chunk.depth[parent] + 1 : 0);
      m.chunk.position.push(at(pc.position, i, 0));
      m.chunk.title.push(at(pc.titles, i, ""));
      const type = code(c.types, pc.type_code[i]);
      const status = code(c.statuses, pc.status_code[i]);
      m.chunk.type.push(type);
      m.chunk.status.push(status);
      addUnique(m.statuses, status);
      m.chunk.updatedAt.push(at(pc.updated_at, i, 0));
      m.chunk.invalidAt.push(at(pc.invalid_at, i, null));
      m.chunk.isFact.push(at(pc.is_fact, i, false));
      m.chunk.retired.push(at(pc.fact_retired, i, false));
      m.chunk.proj.push(NaN, NaN, NaN);
      m.doc.chunkCount[di]++;
      if (parent < 0 && m.doc.root[di] < 0) m.doc.root[di] = ci;
    }
  }
  // A document whose root chunk id the page named takes that chunk as its root
  // (the first parentless chunk is only the fallback).
  if (pd) {
    for (let i = 0; i < pd.ids.length; i++) {
      const di = docMap[i];
      if (di < 0) continue;
      const ri = m.chunkIndex.get(pd.root_chunk_ids[i]);
      if (ri !== undefined) m.doc.root[di] = ri;
    }
  }
  // The projection block holds one triple per chunk of the page, in page order;
  // `missing` names the chunks without one (not placed yet, or another model).
  const pp = page.projection;
  if (pp && pc && pp.basis_version > 0) {
    if (m.projection.basisVersion === 0) {
      m.projection.model = pp.model;
      m.projection.basisVersion = pp.basis_version;
    } else if (m.projection.basisVersion !== pp.basis_version || m.projection.model !== pp.model) {
      // A refit landed between two pages: these coordinates are in another
      // frame. The caller reloads; until then the page's chunks stay unplaced.
      m.projection.mixedBasis = true;
    }
    if (!m.projection.mixedBasis) {
      const missing = new Set(pp.missing ?? []);
      for (let i = 0; i < pc.ids.length; i++) {
        const ci = chunkMap[i];
        if (ci < 0 || missing.has(i) || pp.xyz.length < i * 3 + 3) continue;
        m.chunk.proj[ci * 3] = pp.xyz[i * 3];
        m.chunk.proj[ci * 3 + 1] = pp.xyz[i * 3 + 1];
        m.chunk.proj[ci * 3 + 2] = pp.xyz[i * 3 + 2];
        m.projection.placed++;
      }
    }
  }
  const pe = page.edges;
  if (pe) {
    const ext = new Map<number, { id: string; document_id: string }>();
    for (const x of pe.external ?? []) ext.set(x.idx, x);
    const seen = new Set(m.edges.map((e) => e.id));
    const end = (idx: number): { id: string; docId: string } | null => {
      if (idx >= 0) {
        if (!pc || idx >= pc.ids.length) return null;
        const di = pc.document_idx[idx];
        return { id: pc.ids[idx], docId: pd?.ids[di] ?? "" };
      }
      const x = ext.get(idx);
      return x ? { id: x.id, docId: x.document_id } : null;
    };
    for (let i = 0; i < pe.from_idx.length; i++) {
      const a = end(pe.from_idx[i]);
      const b = end(pe.to_idx[i]);
      if (!a || !b) continue;
      const kind = code(c.kinds, pe.kind_code[i]);
      const id = pe.ids?.[i] ?? `${a.id}>${b.id}:${kind}`;
      // An edge between two pages arrives with both: once from its source's
      // page, once as a backlink into its target's.
      if (seen.has(id)) continue;
      seen.add(id);
      addUnique(m.kinds, kind);
      m.edges.push({
        id,
        from: -1,
        to: -1,
        fromId: a.id,
        toId: b.id,
        fromDocId: a.docId,
        toDocId: b.docId,
        kind,
        status: code(c.statuses, pe.status_code?.[i]) || "asserted",
        method: code(c.methods, pe.method_code?.[i]),
        confidence: at(pe.confidence, i, null),
        auto: at(pe.auto, i, false),
      });
    }
    if (pe.truncated) m.edgesTruncated = true;
  }
  // Re-resolve every endpoint: this page may hold chunks earlier pages pointed at.
  for (const e of m.edges) {
    if (e.from < 0) e.from = m.chunkIndex.get(e.fromId) ?? -1;
    if (e.to < 0) e.to = m.chunkIndex.get(e.toId) ?? -1;
  }
  const dd = page.dirents;
  if (dd) {
    for (let i = 0; i < dd.paths.length; i++) if (dd.kinds[i] === "directory") m.dirs.add(dd.paths[i]);
  }
  m.version++;
  return m;
}

/** The directory a document is filed in: its shortest name's parent, "" = unfiled. */
export function homeDir(m: UniverseModel, di: number): string {
  const p = m.doc.paths[di]?.[0];
  if (!p) return "";
  const cut = p.lastIndexOf("/");
  return cut <= 0 ? "/" : p.slice(0, cut);
}
