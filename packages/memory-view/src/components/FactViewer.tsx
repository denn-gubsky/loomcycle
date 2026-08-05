import { useEffect, useState } from "react";
import type { FactRow, MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import Splitter from "./Splitter";
import ChunkDetailPanel from "./ChunkDetailPanel";

// FactViewer — a browse LIST + a detail panel over a scope's entity-tier FACTS
// (RFC BL P4c / RFC BV). Deliberately minimal per RFC BV: the loomcycle UI shows
// the supersession chain + relations as a LIST (see ChunkDetailPanel), not a
// graph canvas — rich graph/timeline views belong to loomboard, which composes
// this same package.
//
// The list is metadata-only (list_facts returns no bodies); clicking a row
// fetches the body + entity block via get_chunk, exactly as the backend intends.
export interface FactViewerProps {
  /** Which scope's facts to browse. Default "user" (the entity tier's default). */
  scope?: MemoryScope;
  /** Browse-by-subject override (?scope_id=) — point the viewer at a subject. */
  scopeId?: string;
}

type ClassFilter = "" | "derived" | "evidential";

export default function FactViewer({ scope = "user", scopeId }: FactViewerProps) {
  const data = useMemoryData();
  const [facts, setFacts] = useState<FactRow[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [selectedId, setSelectedId] = useState<string>("");
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // Filters. The type box is a SERVER-SIDE exact match (the backend filters
  // `c.type = ?`), debounced so a partial-word keystroke doesn't spam the store.
  const [typeInput, setTypeInput] = useState("");
  const [typeFilter, setTypeFilter] = useState("");
  const [classFilter, setClassFilter] = useState<ClassFilter>("");
  const [includeRetired, setIncludeRetired] = useState(false);

  useEffect(() => {
    const t = setTimeout(() => setTypeFilter(typeInput.trim()), 300);
    return () => clearTimeout(t);
  }, [typeInput]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    data
      .listFacts(scope, {
        type: typeFilter || undefined,
        class: classFilter || undefined,
        includeRetired,
        scopeId,
      })
      .then((resp) => {
        if (cancelled) return;
        setFacts(resp.facts ?? []);
        setTruncated(resp.truncated);
        setErr(null);
      })
      .catch((e) => {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [data, scope, scopeId, typeFilter, classFilter, includeRetired]);

  return (
    <div className="fact-view-wrapper">
      {err && <div className="err memory-err">{err}</div>}
      <div className="fact-filters">
        <input
          type="text"
          className="fact-type-input"
          placeholder="type (exact)…"
          value={typeInput}
          onChange={(e) => setTypeInput(e.target.value)}
        />
        <select value={classFilter} onChange={(e) => setClassFilter(e.target.value as ClassFilter)}>
          <option value="">all classes</option>
          <option value="derived">derived</option>
          <option value="evidential">evidential</option>
        </select>
        <label className="fact-retired-toggle">
          <input
            type="checkbox"
            checked={includeRetired}
            onChange={(e) => setIncludeRetired(e.target.checked)}
          />
          include retired
        </label>
      </div>
      <Splitter
        className="fact-view"
        defaultLeftWidth={360}
        minLeftWidth={240}
        minRightWidth={360}
        storageKey="loomcycle.split.facts"
      >
        <div className="memory-pane fact-list-pane">
          <div className="pane-header">
            <span>
              facts <code>{scopeId ? `${scope}/${scopeId}` : scope}</code>
            </span>
            {loading && <span className="meta">loading…</span>}
          </div>
          <ul className="fact-list">
            {facts.length === 0 && (
              <li className="empty-row">{loading ? "loading…" : "no facts"}</li>
            )}
            {facts.map((f) => (
              <li
                key={f.id}
                className={f.id === selectedId ? "on" : ""}
                onClick={() => setSelectedId(f.id)}
              >
                <span className="fact-row-title">{f.title || f.id}</span>
                <span className="fact-row-badges">
                  {f.type && <span className="fact-type-badge">{f.type}</span>}
                  {f.entity.class === "evidential" && (
                    <span className="fact-class-badge fact-class-source" title="evidential — a pinned source">
                      source
                    </span>
                  )}
                  {f.entity.retired && <span className="fact-retired-badge">retired</span>}
                </span>
              </li>
            ))}
            {truncated && (
              <li className="empty-row">… more facts hidden (refine the filters or raise the limit)</li>
            )}
          </ul>
        </div>
        <div className="memory-pane fact-detail-pane">
          <div className="pane-header">fact</div>
          {!selectedId && <div className="empty">pick a fact to inspect it.</div>}
          {selectedId && (
            <ChunkDetailPanel
              scope={scope}
              scopeId={scopeId}
              chunkId={selectedId}
              onNavigate={setSelectedId}
            />
          )}
        </div>
      </Splitter>
    </div>
  );
}
