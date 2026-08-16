import { useEffect, useState } from "react";
import type { FactRow, MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import Splitter from "./Splitter";
import ChunkDetailPanel from "./ChunkDetailPanel";
import CoverageBar from "./CoverageBar";
import RememberBox from "./RememberBox";
import { factVerdict } from "../lib/factVerdict";

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
  /** Host-supplied "run a consolidation pass" affordance; absent hides it. */
  onRunConsolidation?: (scopeId: string) => void;
}

type ClassFilter = "" | "derived" | "evidential";

export default function FactViewer({ scope = "user", scopeId, onRunConsolidation }: FactViewerProps) {
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
  // Refused facts are hidden by default, exactly as they are from an agent's recall.
  // The toggle is how an operator reads what was refused AND why — which is the whole
  // reason a refusal withholds rather than deletes.
  const [includeRefuted, setIncludeRefuted] = useState(false);
  // Bumped after a verdict is recorded, to re-run the list: withholding a fact removes
  // it from the default view, and a stale row would still offer to withhold it again.
  const [reloadKey, setReloadKey] = useState(0);

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
        includeRefuted,
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
  }, [data, scope, scopeId, typeFilter, classFilter, includeRetired, includeRefuted, reloadKey]);

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
        <label className="fact-retired-toggle">
          <input
            type="checkbox"
            checked={includeRefuted}
            onChange={(e) => setIncludeRefuted(e.target.checked)}
          />
          include refused
        </label>
      </div>
      <CoverageBar scope={scope} scopeId={scopeId} reloadKey={reloadKey} />
      <RememberBox scope={scope} scopeId={scopeId} onRemembered={() => setReloadKey((n) => n + 1)} />
      {onRunConsolidation && (
        <div className="fact-consolidate">
          <button type="button" onClick={() => onRunConsolidation(scopeId ?? "")}>
            run consolidation…
          </button>
          <span>
            reads this subject&rsquo;s unconsolidated chats and distils durable facts.
            Nothing runs until you confirm it.
          </span>
        </div>
      )}
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
                  {factVerdict(f.entity).state === "withheld" && (
                    <span className="fact-verdict-badge fact-verdict-withheld" title="refused by a verdict — withheld, not deleted">
                      refused
                    </span>
                  )}
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
              onVerdictRecorded={() => setReloadKey((n) => n + 1)}
            />
          )}
        </div>
      </Splitter>
    </div>
  );
}
