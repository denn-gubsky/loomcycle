import type { UniverseModel } from "./model";

// Quick search: a client-side match over what is loaded — document titles,
// their Path names, and chunk headings — so finding a star by name needs no
// round trip and no embedder. Ranked prefix > word start > substring, documents
// before chunks at equal rank.

export interface QuickHit {
  chunk: number;
  score: number;
}

export function quickSearch(m: UniverseModel, query: string, limit = 50): QuickHit[] {
  const q = query.trim().toLowerCase();
  if (q.length < 2) return [];
  const rank = (text: string): number => {
    const t = text.toLowerCase();
    if (t.startsWith(q)) return 3;
    const i = t.indexOf(q);
    if (i < 0) return 0;
    return /[\s/._-]/.test(t[i - 1] ?? " ") ? 2 : 1;
  };
  const hits: QuickHit[] = [];
  for (let di = 0; di < m.docCount; di++) {
    const r = m.doc.root[di];
    if (r < 0) continue;
    let best = rank(m.doc.title[di]);
    for (const p of m.doc.paths[di]) best = Math.max(best, rank(p) - 0.5);
    if (best > 0) hits.push({ chunk: r, score: best + 0.25 });
  }
  for (let c = 0; c < m.chunkCount; c++) {
    if (m.chunk.depth[c] === 0) continue;
    const s = rank(m.chunk.title[c]);
    if (s > 0) hits.push({ chunk: c, score: s - m.chunk.depth[c] * 0.01 });
  }
  hits.sort((a, b) => b.score - a.score || a.chunk - b.chunk);
  return hits.slice(0, limit);
}
