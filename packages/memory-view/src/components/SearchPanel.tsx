import { useEffect, useMemo, useRef, useState } from "react";
import type { MemorySearchEntry, MemoryScope, MemorySource } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import { searchHitLabel } from "../lib/searchLabels";
import Splitter from "./Splitter";
import ChunkDetailPanel from "./ChunkDetailPanel";

// SearchPanel — the off-run UNIFIED semantic search (POST /v1/_memory/search). One
// ranked list; each hit is labelled by kind (RFC BW): "fact" (a consolidator
// distilled it), "note" (an agent wrote it directly), or "document" (a Document
// chunk body). A document hit opens the shared ChunkDetailPanel (its entity block
// via get_chunk); a fact/note hit shows its key + value and can deep-link back to
// the k/v console via onSelectEntry. The `source` field narrows which kinds come
// back (facts / notes / documents; empty = all planes).
//
// Search needs an embedder + a vector store. When the deployment has neither (or
// a stale dimension) the server returns 400 `search_unavailable`; we render a
// muted "not configured" note rather than an error banner — mirroring how the
// MemoryConsole treats the embed_stats 503.
export interface SearchPanelProps {
  /** Search scope (agent | user | tenant). Default "user". */
  scope?: MemoryScope;
  /** Default scope_id target. Required for agent/user; ignored for tenant (one
   *  tenant-wide keyspace). */
  scopeId?: string;
  /** The "open in Entries" action on a memory hit's detail — deep-link it in the
   *  host (e.g. the k/v console). Unwired = the action isn't offered. */
  onSelectEntry?: (scope: MemoryScope, scopeId: string, key: string) => void;
}

export default function SearchPanel({ scope: scopeProp = "user", scopeId: scopeIdProp = "", onSelectEntry }: SearchPanelProps) {
  const data = useMemoryData();
  const [query, setQuery] = useState("");
  const [scope, setScope] = useState<MemoryScope>(scopeProp);
  const [scopeId, setScopeId] = useState(scopeIdProp);
  // RFC BW `sources` selector (facts | notes | documents). Empty = every plane.
  // A single-value field never hits the invalid_sources 400 (which only rejects
  // mixing "documents" with just ONE of facts/notes). Free text is accepted — the
  // datalist suggests the valid values and the server drops anything unknown.
  const [source, setSource] = useState("");
  const [results, setResults] = useState<MemorySearchEntry[] | null>(null);
  // The (scope, scope_id) the current `results` were fetched under — snapshotted
  // at search time so the detail panel + deep-link use the scope the hits came
  // from, NOT whatever the inputs say now (a hit's chunk_id is scope-partitioned,
  // so inspecting it under a since-changed scope would dead-end on "no chunk").
  const [searched, setSearched] = useState<{ scope: MemoryScope; scopeId: string } | null>(null);
  const [selected, setSelected] = useState<string>("");
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [loading, setLoading] = useState(false);
  // Monotonic request token: only the LATEST in-flight search may write results,
  // so a slow earlier search can't overwrite a faster later one.
  const reqRef = useRef(0);

  // scope_id suggestions (optional data-layer methods; absent → free-text only).
  // identity pre-fills the field + floats "you" to the top of the user list.
  const [identity, setIdentity] = useState<{ subject: string; tenant: string } | null>(null);
  const [users, setUsers] = useState<string[]>([]);
  const [agents, setAgents] = useState<string[]>([]);

  // Fetch the suggestion sources once. Each is optional + best-effort — a failure
  // (or an unimplemented method) just leaves that scope's field as free text.
  useEffect(() => {
    let cancelled = false;
    data.whoami?.().then((w) => !cancelled && setIdentity(w)).catch(() => {});
    data.listUserIds?.().then((u) => !cancelled && setUsers(u)).catch(() => {});
    data.listAgentNames?.().then((a) => !cancelled && setAgents(a)).catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [data]);

  // The tenant scope has no scope_id (one tenant-wide keyspace), so it does not
  // require one; every other scope does.
  const needsScopeId = scope !== "tenant";

  // Pre-fill scope_id when the scope changes (or when the data seeding its default
  // first loads): tenant → the tenant name (shown read-only), user → your own
  // subject. Split from the agent effect below so an agent-list load never
  // clobbers a user/tenant value (and vice-versa); once loaded the deps are
  // stable, so it stops firing and a typed value survives.
  useEffect(() => {
    if (scope === "tenant") setScopeId(identity?.tenant ?? "");
    else if (scope === "user") setScopeId(identity?.subject ?? "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope, identity]);
  useEffect(() => {
    if (scope === "agent") setScopeId(agents[0] ?? "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope, agents]);

  // The scope_id combobox suggestions: users (yourself first) for user scope,
  // active agents for agent scope, none for tenant (its field is read-only).
  const scopeIdOptions = useMemo(() => {
    if (scope === "user") {
      const me = identity?.subject;
      const rest = users.filter((u) => u !== me);
      const ordered = me ? [me, ...rest] : users;
      return ordered.map((u) => ({ value: u, label: u === me ? `${u} (you)` : u }));
    }
    if (scope === "agent") return agents.map((a) => ({ value: a, label: a }));
    return [];
  }, [scope, identity, users, agents]);
  const runSearch = async () => {
    if (!query.trim() || (needsScopeId && !scopeId.trim())) return;
    const token = ++reqRef.current;
    const target = { scope, scopeId: needsScopeId ? scopeId.trim() : "" };
    setLoading(true);
    setErr(null);
    setUnavailable(false);
    setSelected("");
    try {
      const resp = await data.search({
        query: query.trim(),
        scope: target.scope,
        scopeId: target.scopeId,
        // A free-typed unknown source is dropped server-side (→ all planes), so
        // the cast is safe: the datalist offers the valid values, the server validates.
        sources: source.trim() ? [source.trim() as MemorySource] : undefined,
      });
      if (token !== reqRef.current) return; // a newer search superseded this one
      setResults(resp.entries ?? []);
      setSearched(target);
    } catch (e) {
      if (token !== reqRef.current) return;
      // The embedder-unavailable refusal is a deployment STATE, not a failure —
      // surface it as a muted hint (like the console's embed_stats 503), and let
      // every other error keep the red banner.
      if (isSearchUnavailable(e)) {
        setUnavailable(true);
        setResults(null);
        setSearched(null);
      } else {
        setErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      if (token === reqRef.current) setLoading(false);
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
          <option value="tenant">tenant</option>
        </select>
        {/* scope_id: a combobox pre-filled per scope with suggestions — users
            (you first) for user, active agents for agent; read-only tenant name
            for tenant. Still free-text editable for user/agent. */}
        <Combobox
          value={scopeId}
          onChange={setScopeId}
          options={scopeIdOptions}
          placeholder={needsScopeId ? "scope_id (e.g. alice)…" : "tenant-wide"}
          disabled={!needsScopeId}
          ariaLabel="scope id"
        />
        {/* Source: a combobox (visible dropdown of the known kinds + free text).
            Empty = every plane. */}
        <Combobox
          value={source}
          onChange={setSource}
          options={SOURCE_OPTIONS}
          placeholder="source: all"
          ariaLabel="source filter"
        />
        <input
          type="text"
          className="search-query-input"
          placeholder="semantic query…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <button type="submit" className="search-submit-btn" disabled={loading || !query.trim() || (needsScopeId && !scopeId.trim())}>
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
                    onClick={() => setSelected(rowKey)}
                  >
                    <div className="search-hit-head">
                      <span className={`search-kind-badge search-kind-${hit.kind}`}>
                        {hit.kind}
                      </span>
                      {/* A document hit is named by its document + heading when the
                          server could resolve them; the opaque id stays on the
                          tooltip so it is still readable and copyable. */}
                      <span
                        className="search-hit-key"
                        title={isDoc ? hit.chunk_id ?? hit.key : hit.key}
                      >
                        {searchHitLabel(hit)}
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
            <div className="pane-header">detail</div>
            {!selected && <div className="empty">click a hit to inspect it.</div>}
            {selected &&
              searched &&
              (() => {
                const hit = results.find((h) => h.kind + ":" + h.key === selected);
                if (!hit) return null;
                if (hit.kind === "document" && hit.chunk_id) {
                  // Inspect under the scope the SEARCH ran (searched), not the live
                  // inputs — the chunk_id is scope-partitioned.
                  return (
                    <ChunkDetailPanel
                      scope={searched.scope}
                      scopeId={searched.scopeId || undefined}
                      chunkId={hit.chunk_id}
                    />
                  );
                }
                // A k/v memory hit — no chunk to inspect; show its key + full value,
                // and offer to jump to it in the console when the host wired the hook.
                return (
                  <div className="search-mem-detail">
                    <div className="search-mem-key">
                      <code>{hit.key}</code>
                      {onSelectEntry && (
                        <button
                          type="button"
                          className="search-mem-open-btn"
                          onClick={() => onSelectEntry(searched.scope, searched.scopeId, hit.key)}
                        >
                          open in Entries
                        </button>
                      )}
                    </div>
                    <pre className="search-mem-value">{fmtFullValue(hit.value)}</pre>
                  </div>
                );
              })()}
          </div>
        </Splitter>
      )}
    </div>
  );
}

// The RFC BW source kinds, plus an explicit "all" (empty selector = every plane).
const SOURCE_OPTIONS: { value: string; label: string }[] = [
  { value: "", label: "all sources" },
  { value: "facts", label: "facts" },
  { value: "notes", label: "notes" },
  { value: "documents", label: "documents" },
];

// Combobox — a VISIBLE dropdown of suggested values that is also free-text
// editable. A bare <datalist> reads as an empty text box (its choices only
// surface on focus, browser-dependent), so this is an explicit combobox: a text
// input + a chevron that opens the list on click, with outside-click / Escape to
// close. Typing passes straight through — the server validates/ignores unknowns.
// Reused by the source filter and the scope_id field. When `disabled` (tenant
// scope_id) it's a plain read-only input; with no `options` it's a free-text box
// with no chevron.
function Combobox({
  value,
  onChange,
  options,
  placeholder,
  disabled,
  ariaLabel,
}: {
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  placeholder?: string;
  disabled?: boolean;
  ariaLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const canOpen = !disabled && options.length > 0;

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  return (
    <div className="mv-combobox" ref={wrapRef}>
      <input
        type="text"
        className="mv-combobox-input"
        placeholder={placeholder}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        onFocus={() => canOpen && setOpen(true)}
        onKeyDown={(e) => e.key === "Escape" && setOpen(false)}
        aria-label={ariaLabel}
      />
      {canOpen && (
        <button
          type="button"
          className="mv-combobox-toggle"
          aria-label="show suggestions"
          tabIndex={-1}
          onClick={() => setOpen((o) => !o)}
        >
          ▾
        </button>
      )}
      {open && canOpen && (
        <ul className="mv-combobox-menu" role="listbox">
          {options.map((o) => (
            <li
              key={o.value || "__default__"}
              role="option"
              aria-selected={value === o.value}
              className={value === o.value ? "on" : ""}
              // onMouseDown + preventDefault so the pick registers before the
              // input's blur would close the menu.
              onMouseDown={(e) => {
                e.preventDefault();
                onChange(o.value);
                setOpen(false);
              }}
            >
              {o.label}
            </li>
          ))}
        </ul>
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

// fmtFullValue renders a value in FULL (no truncation) for the detail pane —
// pretty-printed JSON for objects, the raw string for strings.
function fmtFullValue(v: unknown): string {
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v, null, 2) ?? String(v);
  } catch {
    return String(v);
  }
}
