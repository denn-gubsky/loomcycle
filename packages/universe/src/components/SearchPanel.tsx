import { useEffect, useRef, useState, type FormEvent } from "react";
import type { UniverseDataLayer } from "../lib/dataLayer";
import type { UniverseModel } from "../lib/model";
import { quickSearch } from "../lib/search";
import { chunkRgb, rgbHex } from "../lib/colors";
import type { BrowseScope, DocScope } from "../types";

// SearchPanel sits under the map. "names" matches titles and paths among the
// loaded stars as you type; "content" asks the runtime's Document search
// (hybrid full-text + vector over chunk bodies) and, in the meaning view, also
// where the query itself lands; "memory" searches the scope's memory (facts,
// notes and document chunks) and places the chunk-backed hits. Clicking a row
// flies there and opens it; ticking rows selects several at once, and the map
// draws the union of their links.

export interface ResultRow {
  /** Star (chunk index), or -1 for a hit whose document is not on the map. */
  chunk: number;
  chunkId: string;
  title: string;
  documentTitle: string;
  path?: string;
  score: number;
  color: string;
}

type Mode = "names" | "content" | "memory";

export default function SearchPanel(props: {
  model: UniverseModel;
  version: number;
  data: UniverseDataLayer | null;
  scope: DocScope;
  browse?: BrowseScope;
  underPath?: string;
  results: ResultRow[];
  onResults: (rows: ResultRow[]) => void;
  checked: ReadonlySet<number>;
  onChecked: (next: ReadonlySet<number>) => void;
  onPick: (row: ResultRow) => void;
  onFrame: (stars: number[]) => void;
  /** Ask content search for the query's projected point (the meaning view). */
  withProjection: boolean;
  onQueryPoint: (xyz: [number, number, number] | null) => void;
  /** The memory scope id to search (the subject, for user scope); undefined =
   *  no memory mode (agent scope, or a data layer without memory search). */
  memoryScopeId?: string;
}) {
  const {
    model: m,
    version,
    data,
    scope,
    browse,
    underPath,
    results,
    onResults,
    checked,
    onChecked,
    onPick,
    onFrame,
    withProjection,
    onQueryPoint,
    memoryScopeId,
  } = props;
  const [mode, setMode] = useState<Mode>("names");
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const seq = useRef(0);

  const row = (c: number, score: number): ResultRow => {
    const di = m.chunk.doc[c];
    const isRoot = m.doc.root[di] === c;
    return {
      chunk: c,
      chunkId: m.chunk.id[c],
      title: (isRoot ? m.doc.title[di] : m.chunk.title[c]) || m.chunk.id[c],
      documentTitle: m.doc.title[di],
      path: m.doc.paths[di]?.[0],
      score,
      color: rgbHex(chunkRgb(m, c)),
    };
  };

  // Names mode answers as you type (and again when more of the scope loads).
  useEffect(() => {
    if (mode !== "names") return;
    const t = setTimeout(() => {
      onResults(quickSearch(m, query).map((h) => row(h.chunk, h.score)));
    }, 120);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, query, version]);

  const offMap = (chunkId: string, title: string, documentTitle: string, score: number): ResultRow => ({
    chunk: -1,
    chunkId,
    title,
    documentTitle,
    score,
    color: "#888888",
  });

  const runSearch = async (e?: FormEvent) => {
    e?.preventDefault();
    if (mode === "names" || !data || query.trim().length < 2) return;
    const my = ++seq.current;
    setBusy(true);
    setErr(null);
    try {
      if (mode === "memory") {
        if (!data.memorySearch || memoryScopeId === undefined) return;
        const hits = await data.memorySearch(query.trim(), scope, memoryScopeId, { topK: 30 });
        if (my !== seq.current) return;
        onQueryPoint(null);
        onResults(
          hits.map((h) => {
            const c = h.chunkId ? m.chunkIndex.get(h.chunkId) : undefined;
            if (c !== undefined) return row(c, h.score);
            return offMap(h.chunkId ?? h.key, h.title || h.key, h.document ?? h.kind, h.score);
          }),
        );
        return;
      }
      const ans = await data.search(query.trim(), scope, { topK: 30, underPath, withProjection }, browse);
      if (my !== seq.current) return;
      onQueryPoint(ans.queryXyz ?? null);
      onResults(
        ans.hits.map((h) => {
          const c = m.chunkIndex.get(h.chunk_id);
          if (c !== undefined) return row(c, h.score);
          return offMap(h.chunk_id, h.title || h.chunk_id, "", h.score);
        }),
      );
    } catch (x) {
      if (my === seq.current) setErr(x instanceof Error ? x.message : String(x));
    } finally {
      if (my === seq.current) setBusy(false);
    }
  };

  const toggle = (c: number) => {
    const next = new Set(checked);
    if (next.has(c)) next.delete(c);
    else next.add(c);
    onChecked(next);
  };

  const onMap = results.filter((r) => r.chunk >= 0);
  return (
    <div className="lu-search">
      <form className="lu-search-bar" onSubmit={runSearch}>
        <select
          value={mode}
          onChange={(e) => {
            setMode(e.target.value as Mode);
            onQueryPoint(null);
          }}
          title="Search by name, by content, or the scope's memory"
        >
          <option value="names">names</option>
          <option value="content">content</option>
          {data?.memorySearch && memoryScopeId !== undefined && <option value="memory">memory</option>}
        </select>
        <input
          type="search"
          value={query}
          placeholder={
            mode === "names"
              ? "Find a document or heading…"
              : mode === "content"
                ? "Describe what you are looking for, Enter"
                : "Ask the scope's memory, Enter"
          }
          onChange={(e) => setQuery(e.target.value)}
        />
        {mode !== "names" && (
          <button type="submit" disabled={busy || query.trim().length < 2}>
            {busy ? "…" : "search"}
          </button>
        )}
        <button
          type="button"
          disabled={onMap.length === 0}
          onClick={() => onFrame(checked.size ? [...checked] : onMap.map((r) => r.chunk))}
          title="Frame the ticked results (or all of them)"
        >
          frame
        </button>
        <button
          type="button"
          disabled={results.length === 0 && checked.size === 0}
          onClick={() => {
            setQuery("");
            onResults([]);
            onChecked(new Set());
            onQueryPoint(null);
          }}
        >
          clear
        </button>
      </form>
      {err && <div className="lu-search-err">{err}</div>}
      {results.length > 0 && (
        <ul className="lu-results">
          {results.map((r) => (
            <li key={r.chunkId} className={r.chunk < 0 ? "lu-result lu-result--off" : "lu-result"}>
              <input
                type="checkbox"
                disabled={r.chunk < 0}
                checked={r.chunk >= 0 && checked.has(r.chunk)}
                onChange={() => toggle(r.chunk)}
                aria-label={`Select ${r.title}`}
              />
              <button type="button" onClick={() => onPick(r)} disabled={r.chunk < 0} title={r.path ?? r.chunkId}>
                <i className="lu-chip" style={{ background: r.color }} />
                <span className="lu-result-title">{r.title}</span>
                {r.documentTitle && r.documentTitle !== r.title && (
                  <span className="lu-result-doc">{r.documentTitle}</span>
                )}
                {r.path && <code className="lu-result-path">{r.path}</code>}
              </button>
              {mode !== "names" && <span className="lu-result-score">{r.score.toFixed(2)}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
