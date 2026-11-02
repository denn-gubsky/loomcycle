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
  /** When the statement stops holding (the runtime's unit), null = open. */
  invalidAt: number | null;
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
    const seen = new Set(m.edges.map((e) => e.id));
    for (const e of pageEdges(page)) {
      // An edge between two pages arrives with both: once from its source's
      // page, once as a backlink into its target's.
      if (seen.has(e.id)) continue;
      seen.add(e.id);
      addUnique(m.kinds, e.kind);
      m.edges.push(e);
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

// pageEdges decodes a page's edge columns into model edges with unresolved
// endpoints (-1); the caller resolves them against its chunk index.
function pageEdges(page: GraphSnapshot): ModelEdge[] {
  const pe = page.edges;
  if (!pe) return [];
  const c = page.codes ?? {};
  const pc = page.chunks;
  const pd = page.documents;
  const ext = new Map<number, { id: string; document_id: string }>();
  for (const x of pe.external ?? []) ext.set(x.idx, x);
  const end = (idx: number): { id: string; docId: string } | null => {
    if (idx >= 0) {
      if (!pc || idx >= pc.ids.length) return null;
      const di = pc.document_idx[idx];
      return { id: pc.ids[idx], docId: pd?.ids[di] ?? "" };
    }
    const x = ext.get(idx);
    return x ? { id: x.id, docId: x.document_id } : null;
  };
  const out: ModelEdge[] = [];
  for (let i = 0; i < pe.from_idx.length; i++) {
    const a = end(pe.from_idx[i]);
    const b = end(pe.to_idx[i]);
    if (!a || !b) continue;
    const kind = code(c.kinds, pe.kind_code[i]);
    out.push({
      id: pe.ids?.[i] ?? `${a.id}>${b.id}:${kind}`,
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
      invalidAt: at(pe.invalid_at, i, null),
      auto: at(pe.auto, i, false),
    });
  }
  return out;
}

/**
 * refreshEdges replaces every edge touching the given documents with the edges
 * a fresh snapshot page of exactly those documents carries. Such a page holds
 * all of them: the links out of its documents and the backlinks into them. So
 * a link just proposed appears, an accepted one changes status in place, and a
 * rejected or deleted one (which the snapshot no longer returns) goes.
 */
export function refreshEdges(m: UniverseModel, page: GraphSnapshot, docIds: ReadonlySet<string>): UniverseModel {
  m.edges = m.edges.filter((e) => !docIds.has(e.fromDocId) && !docIds.has(e.toDocId));
  const seen = new Set(m.edges.map((e) => e.id));
  for (const e of pageEdges(page)) {
    if (seen.has(e.id)) continue;
    seen.add(e.id);
    addUnique(m.kinds, e.kind);
    e.from = m.chunkIndex.get(e.fromId) ?? -1;
    e.to = m.chunkIndex.get(e.toId) ?? -1;
    m.edges.push(e);
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

/**
 * modelToPage writes the model's kept documents back out as one snapshot page,
 * the inverse of addPage: their chunk trees, their facts and projected points,
 * the directories, and every edge neither of whose ends lies in a dropped
 * document (an end outside the page becomes an `external` entry, as the
 * runtime sends it). Used to splice a few re-read documents into a loaded sky.
 */
export function modelToPage(m: UniverseModel, keep: (doc: number) => boolean): GraphSnapshot {
  const types: string[] = [];
  const statuses: string[] = [];
  const kinds: string[] = [];
  const methods: string[] = [];
  const intern = (t: string[], v: string) => {
    if (!v) return -1;
    const i = t.indexOf(v);
    return i >= 0 ? i : t.push(v) - 1;
  };
  const docs: number[] = [];
  for (let d = 0; d < m.docCount; d++) if (keep(d)) docs.push(d);
  const pageDoc = new Map<number, number>();
  docs.forEach((d, i) => pageDoc.set(d, i));

  const cIds: string[] = [];
  const cDoc: number[] = [];
  const cParent: number[] = [];
  const cPos: number[] = [];
  const cType: number[] = [];
  const cStatus: number[] = [];
  const cTitle: string[] = [];
  const cInvalid: (number | null)[] = [];
  const cUpdated: number[] = [];
  const cFact: boolean[] = [];
  const cRetired: boolean[] = [];
  const xyz: number[] = [];
  const missing: number[] = [];
  const pageChunk = new Map<number, number>();
  for (const d of docs) {
    const start = m.doc.chunkStart[d];
    for (let c = start; c >= 0 && c < start + m.doc.chunkCount[d]; c++) {
      const i = cIds.length;
      pageChunk.set(c, i);
      cIds.push(m.chunk.id[c]);
      cDoc.push(pageDoc.get(d)!);
      const p = m.chunk.parent[c];
      cParent.push(p >= 0 ? (pageChunk.get(p) ?? -1) : -1);
      cPos.push(m.chunk.position[c]);
      cType.push(intern(types, m.chunk.type[c]));
      cStatus.push(intern(statuses, m.chunk.status[c]));
      cTitle.push(m.chunk.title[c]);
      cInvalid.push(m.chunk.invalidAt[c]);
      cUpdated.push(m.chunk.updatedAt[c]);
      cFact.push(m.chunk.isFact[c]);
      cRetired.push(m.chunk.retired[c]);
      const x = m.chunk.proj[c * 3];
      if (Number.isNaN(x)) {
        missing.push(i);
        xyz.push(0, 0, 0);
      } else xyz.push(x, m.chunk.proj[c * 3 + 1], m.chunk.proj[c * 3 + 2]);
    }
  }

  const keptDocIds = new Set(docs.map((d) => m.doc.id[d]));
  const loadedDocIds = new Set(m.doc.id);
  const external: { idx: number; id: string; document_id: string }[] = [];
  const end = (chunk: number, id: string, docId: string) => {
    const i = chunk >= 0 ? pageChunk.get(chunk) : undefined;
    if (i !== undefined) return i;
    const idx = -(external.length + 1);
    external.push({ idx, id, document_id: docId });
    return idx;
  };
  // An edge stays when each end is in a kept document or in one the sky never
  // loaded; an end in a dropped document drops it (the re-read page brings it).
  const stays = (docId: string) => keptDocIds.has(docId) || !loadedDocIds.has(docId);
  const e = m.edges.filter((x) => stays(x.fromDocId) && stays(x.toDocId));
  const edges = {
    ids: e.map((x) => x.id),
    from_idx: e.map((x) => end(x.from, x.fromId, x.fromDocId)),
    to_idx: e.map((x) => end(x.to, x.toId, x.toDocId)),
    kind_code: e.map((x) => intern(kinds, x.kind)),
    status_code: e.map((x) => intern(statuses, x.status)),
    method_code: e.map((x) => intern(methods, x.method)),
    confidence: e.map((x) => x.confidence),
    invalid_at: e.map((x) => x.invalidAt),
    auto: e.map((x) => x.auto),
    external,
    truncated: m.edgesTruncated,
  };

  const page: GraphSnapshot = {
    page: { documents: docs.length },
    codes: { types, statuses, kinds, methods },
    documents: {
      ids: docs.map((d) => m.doc.id[d]),
      titles: docs.map((d) => m.doc.title[d]),
      root_chunk_ids: docs.map((d) => (m.doc.root[d] >= 0 ? m.chunk.id[m.doc.root[d]] : "")),
      type_code: docs.map((d) => intern(types, m.doc.type[d])),
      status_code: docs.map((d) => intern(statuses, m.doc.status[d])),
      updated_at: docs.map((d) => m.doc.updatedAt[d]),
      color_enabled: docs.map((d) => m.doc.colorEnabled[d]),
      color_scheme: docs.map((d) => m.doc.scheme[d]),
      paths: docs.map((d) => m.doc.paths[d]),
    },
    chunks: {
      ids: cIds,
      document_idx: cDoc,
      parent_idx: cParent,
      position: cPos,
      type_code: cType,
      status_code: cStatus,
      titles: cTitle,
      invalid_at: cInvalid,
      updated_at: cUpdated,
      is_fact: cFact,
      fact_retired: cRetired,
    },
    edges,
    dirents: { paths: [...m.dirs], kinds: [...m.dirs].map(() => "directory"), document_idx: [...m.dirs].map(() => -1) },
  };
  if (m.projection.basisVersion > 0)
    page.projection = { model: m.projection.model, basis_version: m.projection.basisVersion, xyz, missing };
  return page;
}

/**
 * spliceDocuments replaces the given documents with a fresh snapshot page of
 * them, in place: the model object keeps its identity (the scene frames the
 * camera once per model, so a live update never moves it), its version bumps,
 * and the rest of the sky is unchanged. A document the page no longer holds
 * (deleted, or now outside the filters) goes; one the sky did not have arrives.
 */
export function spliceDocuments(m: UniverseModel, page: GraphSnapshot, docIds: ReadonlySet<string>): UniverseModel {
  const fresh = emptyModel();
  addPage(fresh, modelToPage(m, (d) => !docIds.has(m.doc.id[d])));
  addPage(fresh, page);
  fresh.version = m.version + 1;
  Object.assign(m, fresh);
  return m;
}
