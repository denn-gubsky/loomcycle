import { useState } from "react";
import type { MemorySearchEntry, MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import Splitter from "./Splitter";
import ChunkDetailPanel from "./ChunkDetailPanel";

// SearchPanel — the off-run UNIFIED semantic search (POST /v1/_memory/search). One
// ranked list spanning both plain k/v entries (kind:"memory") and document-chunk
// bodies (kind:"document"). A document hit opens the shared ChunkDetailPanel (its
// entity block via get_chunk); a memory hit deep-links back to the k/v console
// through the onSelectEntry callback.
//
// Search needs an embedder + a vector store. When the deployment has neither (or
// a stale dimension) the server returns 400 `search_unavailable`; we render a
// muted "not configured" note rather than an error banner — mirroring how the
// MemoryConsole treats the embed_stats 503.
export interface SearchPanelProps {
  /** Search scope (agent | user). Default "user". */
  scope?: MemoryScope;
  /** Default scope_id target (search requires one). */
  scopeId?: string;
  /** A memory hit was clicked — deep-link it in the host (e.g. the k/v console). */
  onSelectEntry?: (scope: MemoryScope, scopeId: string, key: string) => void;
}

export default function SearchPanel({ scope: scopeProp = "user", scopeId: scopeIdProp = "", onSelectEntry }: SearchPanelProps) {
  const data = useMemoryData();
  const [query, setQuery] = useState("");
  const [scope, setScope] = useState<MemoryScope>(scopeProp);
  const [scopeId, setScopeId] = useState(scopeIdProp);
  const [results, setResults] = useState<MemorySearchEntry[] | null>(null);
  const [selected, setSelected] = useState<string>("");
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [loading, setLoading] = useState(false);

  const runSearch = async () => {
    if (!query.trim() || !scopeId.trim()) return;
    setLoading(true);
    setErr(null);
    setUnavailable(false);
    setSelected("");
    try {
      const resp = await data.search({ query: query.trim(), scope, scopeId: scopeId.trim() });
      setResults(resp.entries ?? []);
    } catch (e) {
      // The embedder-unavailable refusal is a deployment STATE, not a failure —
      // surface it as a muted hint (like the console's embed_stats 503), and let
      // every other error keep the red banner.
      if (isSearchUnavailable(e)) {
        setUnavailable(true);
        setResults(null);
      } else {
        setErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="search-view-wrapper">
      <form
        className="search-form"
        onSubmit={(e) => {
          e.preventDefault();
          void runSearch();
        }}
      >
        <select value={scope} onChange={(e) => setScope(e.target.value)}>
          <option value="user">user</option>
          <option value="agent">agent</option>
        </select>
        <input
          type="text"
          className="search-scopeid-input"
          placeholder="scope_id (e.g. alice)…"
          value={scopeId}
          onChange={(e) => setScopeId(e.target.value)}
        />
        <input
          type="text"
          className="search-query-input"
          placeholder="semantic query…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <button type="submit" className="search-submit-btn" disabled={loading || !query.trim() || !scopeId.trim()}>
          {loading ? "searching…" : "search"}
        </button>
      </form>

      {err && <div className="err memory-err">{err}</div>}
      {unavailable && (
        <div className="search-unavailable">
          semantic search isn't configured on this deployment (no embedder / vector store).
        </div>
      )}

      {results !== null && !unavailable && (
        <Splitter
          className="search-view"
          defaultLeftWidth={420}
          minLeftWidth={280}
          minRightWidth={320}
          storageKey="loomcycle.split.search"
        >
          <div className="memory-pane search-results-pane">
            <div className="pane-header">
              <span>results</span>
              <span className="meta">{results.length}</span>
            </div>
            <ul className="search-result-list">
              {results.length === 0 && <li className="empty-row">no matches</li>}
              {results.map((hit) => {
                const rowKey = hit.kind + ":" + hit.key;
                const isDoc = hit.kind === "document";
                return (
                  <li
                    key={rowKey}
                    className={rowKey === selected ? "on" : ""}
                    onClick={() => {
                      if (isDoc && hit.chunk_id) {
                        setSelected(rowKey);
                      } else {
                        onSelectEntry?.(scope, scopeId.trim(), hit.key);
                      }
                    }}
                  >
                    <div className="search-hit-head">
                      <span className={"search-kind-badge " + (isDoc ? "search-kind-doc" : "search-kind-mem")}>
                        {hit.kind}
                      </span>
                      <span className="search-hit-key">
                        {isDoc ? hit.chunk_id ?? hit.key : hit.key}
                      </span>
                      <span className="search-hit-scores" title="cosine similarity · hybrid rank score">
                        {hit.score.toFixed(3)} · {hit.rank_score.toFixed(3)}
                      </span>
                    </div>
                    {!isDoc && <div className="search-hit-value">{fmtValue(hit.value)}</div>}
                  </li>
                );
              })}
            </ul>
          </div>
          <div className="memory-pane search-detail-pane">
            <div className="pane-header">chunk</div>
            {!selected && <div className="empty">click a document hit to inspect its chunk.</div>}
            {selected &&
              (() => {
                const hit = results.find((h) => h.kind + ":" + h.key === selected);
                if (!hit || !hit.chunk_id) return null;
                return (
                  <ChunkDetailPanel
                    scope={scope}
                    scopeId={scopeId.trim() || undefined}
                    chunkId={hit.chunk_id}
                  />
                );
              })()}
          </div>
        </Splitter>
      )}
    </div>
  );
}

// isSearchUnavailable detects the server's 400 `search_unavailable` refusal
// (no embedder / no vector index / stale dimension). The client raises it as an
// InvalidArgumentError whose message IS the JSON error body, so a substring
// check on the message is transport-robust without importing the client's error
// classes. `bodyText` is checked too for a custom data layer that surfaces the
// raw body separately.
function isSearchUnavailable(e: unknown): boolean {
  if (typeof e !== "object" || e === null) return false;
  const anyE = e as { message?: unknown; bodyText?: unknown };
  const hay = `${typeof anyE.message === "string" ? anyE.message : ""} ${
    typeof anyE.bodyText === "string" ? anyE.bodyText : ""
  }`;
  return hay.includes("search_unavailable");
}

function fmtValue(v: unknown): string {
  if (typeof v === "string") return truncate(v);
  let s: string;
  try {
    // JSON.stringify returns undefined at runtime for undefined/function values
    // (tsc types it as string); `?? ""` catches that without a string-vs-
    // undefined comparison (which tsc rejects as a no-overlap error).
    s = JSON.stringify(v) ?? "";
  } catch {
    s = String(v);
  }
  return truncate(s);
}

function truncate(s: string): string {
  return s.length > 240 ? s.slice(0, 240) + "…" : s;
}
