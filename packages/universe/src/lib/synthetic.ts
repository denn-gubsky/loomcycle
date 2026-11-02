import { mulberry32 } from "./hash";
import type { GraphSnapshot } from "../types";

// syntheticSnapshot builds a graph_snapshot page shaped like a real scope:
// documents spread over a directory tree, each a chunk tree of `chunksPerDoc`
// chunks (a root, sections, leaves), and `edgesPerDoc` cross-document edges.
// Used by the tests and by the performance check; deterministic for a seed.
export function syntheticSnapshot(opts: {
  docs: number;
  chunksPerDoc: number;
  edgesPerDoc?: number;
  dirs?: string[];
  seed?: number;
  idPrefix?: string;
}): GraphSnapshot {
  const rnd = mulberry32(opts.seed ?? 1);
  const dirs = opts.dirs ?? ["/a", "/a/x", "/a/y", "/b", "/b/z", "/c"];
  const pre = opts.idPrefix ?? "";
  const docIds: string[] = [];
  const titles: string[] = [];
  const roots: string[] = [];
  const paths: string[][] = [];
  const ids: string[] = [];
  const docIdx: number[] = [];
  const parentIdx: number[] = [];
  const position: number[] = [];
  const ctitles: string[] = [];
  for (let d = 0; d < opts.docs; d++) {
    const id = `${pre}d${String(d).padStart(6, "0")}`;
    docIds.push(id);
    titles.push(`Document ${d}`);
    paths.push([`${dirs[d % dirs.length]}/doc-${d}`]);
    const base = ids.length;
    for (let c = 0; c < opts.chunksPerDoc; c++) {
      ids.push(`${id}c${c}`);
      docIdx.push(d);
      // Root, then ~sqrt(n) sections under it, the rest leaves under a section.
      const sections = Math.max(1, Math.round(Math.sqrt(opts.chunksPerDoc)));
      if (c === 0) parentIdx.push(-1);
      else if (c <= sections) parentIdx.push(base);
      else parentIdx.push(base + 1 + Math.floor(rnd() * sections));
      position.push(c);
      ctitles.push(c === 0 ? `Document ${d}` : `Section ${d}.${c}`);
    }
    roots.push(`${id}c0`);
  }
  const from: number[] = [];
  const to: number[] = [];
  const kind: number[] = [];
  const eids: string[] = [];
  const n = ids.length;
  for (let d = 0; d < opts.docs * (opts.edgesPerDoc ?? 0); d++) {
    const a = Math.floor(rnd() * n);
    const b = Math.floor(rnd() * n);
    if (docIdx[a] === docIdx[b]) continue;
    from.push(a);
    to.push(b);
    kind.push(d % 2);
    eids.push(`e${d}`);
  }
  return {
    page: { documents: opts.docs },
    codes: { types: ["document", "section"], statuses: ["draft", "done"], kinds: ["references", "related"] },
    documents: {
      ids: docIds,
      titles,
      root_chunk_ids: roots,
      type_code: docIds.map(() => 0),
      status_code: docIds.map((_, i) => i % 2),
      updated_at: docIds.map(() => 0),
      color_enabled: docIds.map(() => false),
      color_scheme: docIds.map(() => null),
      paths,
    },
    chunks: {
      ids,
      document_idx: docIdx,
      parent_idx: parentIdx,
      position,
      type_code: ids.map((_, i) => (parentIdx[i] < 0 ? 0 : 1)),
      status_code: ids.map(() => 0),
      titles: ctitles,
      updated_at: ids.map(() => 0),
    },
    edges: { ids: eids, from_idx: from, to_idx: to, kind_code: kind, truncated: false },
    dirents: { paths: dirs, kinds: dirs.map(() => "directory"), document_idx: dirs.map(() => -1) },
  };
}
