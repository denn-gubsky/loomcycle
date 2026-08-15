import { useState } from "react";
import { Link } from "react-router-dom";
import { documentGetChunk, searchMemory, type BrowseScope, type MemorySearchHit } from "../api";
import { factVerdict, type FactEntity } from "../lib/factVerdict";
import { hitText, judgeable, SEARCH_SOURCES, sourcesParam } from "../lib/memorySearch";

// Semantic search over a scope's memory — the front door to the facts below it.
//
// WHY IT EARNS ITS PLACE next to a list that already shows every fact: the list is
// ordered by recency and capped, so "what do we know about X" is answerable by scrolling
// only while a store is small. Search is also how an operator finds the ONE fact they
// came here to correct, which is the panel's whole reason for existing.
//
// A hit's verification state is fetched per document hit, not for every row: only a
// document-chunk hit can carry a verdict at all, and the k/v plane has no entity block
// to read.
export default function MemorySearchPanel({
  scopeId,
  browse,
}: {
  scopeId: string;
  browse?: BrowseScope;
}) {
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<MemorySearchHit[] | null>(null);
  const [entities, setEntities] = useState<Record<string, FactEntity>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const all = SEARCH_SOURCES.map((s) => s.id as string);

  const run = async () => {
    if (!query.trim()) return;
    setBusy(true);
    try {
      const r = await searchMemory(query, "user", scopeId, {
        sources: sourcesParam(selected, all),
        topK: 20,
      });
      const entries = r.entries ?? [];
      setHits(entries);
      setErr(null);
      // Verification state for the hits that can have one. Bounded by top_k, and only
      // the document hits — a handful of reads, not one per row.
      const next: Record<string, FactEntity> = {};
      await Promise.all(
        entries.filter(judgeable).map(async (h) => {
          try {
            const c = await documentGetChunk(h.chunk_id!, "user", browse);
            const e = (c as { entity?: FactEntity }).entity;
            if (e) next[h.chunk_id!] = e;
          } catch {
            // A hit whose entity will not load still shows its text and score; it
            // simply cannot show a verdict.
          }
        }),
      );
      setEntities(next);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setHits(null);
    } finally {
      setBusy(false);
    }
  };

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <div className="memory-search">
      <div className="settings-row">
        <input
          value={query}
          placeholder="what do we know about…"
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void run();
          }}
        />
        <button type="button" onClick={() => void run()} disabled={busy || !query.trim()}>
          {busy ? "searching…" : "Search"}
        </button>
      </div>

      <div className="settings-row search-sources">
        {SEARCH_SOURCES.map((s) => (
          <label key={s.id} title={s.hint}>
            <input
              type="checkbox"
              checked={selected.size === 0 || selected.has(s.id)}
              onChange={() => toggle(s.id)}
            />
            {s.label}
          </label>
        ))}
        <span className="settings-muted">
          {selected.size === 0 || selected.size === all.length
            ? "searching everything remembered"
            : "narrowed"}
        </span>
      </div>

      {err && <div className="settings-error">{err}</div>}
      {hits && hits.length === 0 && (
        <div className="settings-muted">Nothing matched.</div>
      )}

      {hits?.map((h) => {
        const entity = h.chunk_id ? entities[h.chunk_id] : undefined;
        const v = factVerdict(entity);
        return (
          <div key={h.key} className="search-hit">
            <div className="search-hit-head">
              <span className={"fact-badge search-kind-" + h.kind}>{h.kind}</span>
              {/* The score, because a weak top hit and a strong one look identical in a
                  list — and this floor is not calibrated for anyone's embedder. */}
              <span className="settings-muted">{h.rank_score.toFixed(2)}</span>
              {entity && <span className={"fact-badge fact-badge-" + v.state}>{v.state}</span>}
              {judgeable(h) && (
                <Link
                  className="settings-action-link"
                  to={`/documents/${encodeURIComponent(h.chunk_id ?? "")}?scope=user`}
                >
                  open →
                </Link>
              )}
            </div>
            <div className="search-hit-text">{hitText(h.value)}</div>
            {entity?.source_quote && (
              <blockquote className="fact-span">{entity.source_quote}</blockquote>
            )}
          </div>
        );
      })}
    </div>
  );
}
