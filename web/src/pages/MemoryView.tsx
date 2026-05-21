import { useEffect, useMemo, useState } from "react";
import {
  MemoryEmbedModelStats,
  MemoryEntry,
  MemoryReembedResponse,
  MemoryScopeIDSummary,
  MemoryScopeKind,
  listMemoryEmbedStats,
  listMemoryEntries,
  listMemoryScopeIDs,
  listMemoryScopes,
  reembedMemory,
} from "../api";
import Splitter from "../components/Splitter";

// MemoryView — operator browse-only view over the v0.8.0 Memory tool's
// stored rows.
//
// Three-pane layout:
//   left   : scope picker (agent / user) + scope_id list under the
//            chosen scope, with key counts.
//   middle : key list for the selected (scope, scope_id), with a
//            prefix filter input.
//   right  : the selected entry's value pretty-printed JSON, plus
//            timestamps and TTL.
//
// v0.9.0 additions:
//   - Per-scope model-distribution badge (top of the keys pane) —
//     shows which embedder(s) wrote the rows in this scope.
//   - "reembed missing" button when the model badge surfaces rows
//     under a NON-current embedder; opens a dry-run plan, then a
//     confirm-and-commit flow.
//   - Embedding indicator dot on each key row when the listing
//     spans a scope that has embeddings; the dot is only a hint
//     (the embed_stats endpoint reports aggregate counts, not
//     per-key presence — we render the dot when at least one
//     model exists in the scope).
//
// Polling, not SSE — Memory rows are slow-changing. 5 s refresh is
// plenty to feel live without hammering the store.
const REFRESH_MS = 5_000;

export default function MemoryView() {
  const [scopes, setScopes] = useState<MemoryScopeKind[]>([]);
  const [scope, setScope] = useState<string>("");
  const [scopeIDs, setScopeIDs] = useState<MemoryScopeIDSummary[]>([]);
  const [scopeID, setScopeID] = useState<string>("");
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [selectedKey, setSelectedKey] = useState<string>("");
  const [prefix, setPrefix] = useState("");
  const [err, setErr] = useState<string | null>(null);

  // v0.9.0 Vector Memory state.
  // null = endpoint returned 503 (vectors not configured); the UI
  // renders a "vector search not available" hint instead of the
  // model badge.
  const [embedStats, setEmbedStats] = useState<MemoryEmbedModelStats[] | null>(null);
  const [reembedBanner, setReembedBanner] = useState<MemoryReembedResponse | null>(null);
  const [reembedBusy, setReembedBusy] = useState(false);

  // Bootstrap: fetch the (constant) scope list once on mount.
  useEffect(() => {
    let cancelled = false;
    listMemoryScopes()
      .then((resp) => {
        if (cancelled) return;
        setScopes(resp.scopes ?? []);
        if (resp.scopes && resp.scopes.length > 0 && !scope) {
          setScope(resp.scopes[0].name);
        }
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
    // scope intentionally omitted — only run on mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Poll scope_ids under the selected scope.
  useEffect(() => {
    if (!scope) return;
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await listMemoryScopeIDs(scope);
        if (cancelled) return;
        setScopeIDs(resp.scope_ids ?? []);
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [scope]);

  // v0.9.0: poll the embed_stats endpoint for the selected scope so
  // the UI knows which models wrote what. Refreshes on the same
  // cadence as scope_ids so the badge stays in sync.
  useEffect(() => {
    if (!scope) {
      setEmbedStats(null);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      const result = await listMemoryEmbedStats(scope);
      if (cancelled) return;
      if (result.ok) {
        setEmbedStats(result.data.models ?? []);
      } else {
        // 503 = vectors not configured. Surface as null so the UI
        // renders the "not available" hint instead of an error.
        setEmbedStats(null);
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [scope]);

  // When the scope changes, blow away the scope_id selection so the
  // user always picks fresh under the new scope.
  useEffect(() => {
    setScopeID("");
    setSelectedKey("");
    setEntries([]);
    setPrefix("");
    setReembedBanner(null);
  }, [scope]);

  // Poll entries under the selected (scope, scope_id, prefix).
  useEffect(() => {
    if (!scope || !scopeID) {
      setEntries([]);
      setTruncated(false);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await listMemoryEntries(scope, scopeID, prefix || undefined, 200);
        if (cancelled) return;
        setEntries(resp.entries ?? []);
        setTruncated(resp.truncated);
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [scope, scopeID, prefix]);

  const selectedEntry = useMemo(() => {
    if (!selectedKey) return null;
    return entries.find((e) => e.key === selectedKey) ?? null;
  }, [entries, selectedKey]);

  // Are there embeddings under this scope? Used to render the
  // per-row indicator dot. The embed_stats endpoint reports
  // aggregate counts, not per-key presence — the dot is a hint that
  // SOME rows are embedded, not which specific ones.
  const scopeHasEmbeddings = useMemo(() => {
    if (!embedStats) return false;
    return embedStats.some((m) => m.row_count > 0);
  }, [embedStats]);

  const handleReembedDryRun = async () => {
    if (!scope || !scopeID) return;
    setReembedBusy(true);
    setReembedBanner(null);
    try {
      const resp = await reembedMemory(scope, scopeID, true);
      setReembedBanner(resp);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setReembedBusy(false);
    }
  };

  const handleReembedCommit = async () => {
    if (!scope || !scopeID) return;
    if (!window.confirm(`Re-embed all rows under ${scope}/${scopeID} using the current embedder? This calls the provider API and may incur cost.`)) {
      return;
    }
    setReembedBusy(true);
    try {
      const resp = await reembedMemory(scope, scopeID, false);
      setReembedBanner(resp);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setReembedBusy(false);
    }
  };

  return (
    <div className="memory-view-wrapper">
    {err && <div className="err memory-err">{err}</div>}
    <Splitter
      className="memory-view"
      defaultLeftWidth={280}
      minLeftWidth={200}
      minRightWidth={460}
      storageKey="loomcycle.split.memory.outer"
    >
      <div className="memory-pane scopes-pane">
        <div className="pane-header">scopes</div>
        <div className="scope-tabs">
          {scopes.map((sc) => (
            <button
              key={sc.name}
              className={sc.name === scope ? "on" : ""}
              onClick={() => setScope(sc.name)}
              title={sc.description}
            >
              {sc.name}
            </button>
          ))}
        </div>
        <div className="pane-header sub">{scope || "—"} ids</div>
        <ul className="scope-id-list">
          {scopeIDs.length === 0 && (
            <li className="empty-row">no rows under {scope}</li>
          )}
          {scopeIDs.map((row) => (
            <li
              key={row.scope_id}
              className={row.scope_id === scopeID ? "on" : ""}
              onClick={() => {
                setScopeID(row.scope_id);
                setSelectedKey("");
                setReembedBanner(null);
              }}
            >
              <code>{row.scope_id}</code>
              <span className="meta">
                {row.key_count} key{row.key_count === 1 ? "" : "s"} · {formatBytes(row.bytes)}
              </span>
            </li>
          ))}
        </ul>
      </div>
      <Splitter
        className="memory-view-inner"
        defaultLeftWidth={320}
        minLeftWidth={220}
        minRightWidth={320}
        storageKey="loomcycle.split.memory.inner"
      >
      <div className="memory-pane keys-pane">
        <div className="pane-header">
          keys {scopeID && <code>{scope}/{scopeID}</code>}
        </div>
        {/* v0.9.0 — embedding model badge + reembed action. Only
            renders when embed_stats reports rows for this scope. */}
        {scope && embedStats !== null && embedStats.length > 0 && (
          <div className="embed-badge-row">
            <span className="embed-badge-label">embeddings:</span>
            {embedStats.map((m) => (
              <span
                key={m.provider + "/" + m.model + "/" + m.dimension}
                className="embed-badge"
                title={`${m.row_count} row(s) under this model, dim=${m.dimension}`}
              >
                {m.provider}/{m.model}
                <span className="embed-badge-count">×{m.row_count}</span>
              </span>
            ))}
            {scopeID && (
              <button
                className="embed-reembed-btn"
                disabled={reembedBusy}
                onClick={handleReembedDryRun}
                title="See which rows would be re-embedded under the current embedder"
              >
                {reembedBusy ? "…" : "reembed plan"}
              </button>
            )}
          </div>
        )}
        {scope && embedStats === null && (
          <div className="embed-badge-row embed-badge-disabled">
            <span className="embed-badge-label">embeddings: not configured</span>
          </div>
        )}
        {/* v0.9.0 — reembed banner. Shows dry-run results + commit
            button, or the real-run outcome. */}
        {reembedBanner && (
          <ReembedBanner
            banner={reembedBanner}
            busy={reembedBusy}
            onCommit={handleReembedCommit}
            onDismiss={() => setReembedBanner(null)}
          />
        )}
        {scopeID && (
          <input
            type="text"
            className="prefix-input"
            placeholder="filter by prefix…"
            value={prefix}
            onChange={(e) => setPrefix(e.target.value)}
          />
        )}
        <ul className="key-list">
          {!scopeID && <li className="empty-row">pick a scope_id to see its keys</li>}
          {scopeID && entries.length === 0 && <li className="empty-row">no keys</li>}
          {entries.map((e) => (
            <li
              key={e.key}
              className={e.key === selectedKey ? "on" : ""}
              onClick={() => setSelectedKey(e.key)}
            >
              {/* v0.9.0 — embedding indicator dot. Hint, not
                  authoritative: shows when ANY row in the scope is
                  embedded, not whether THIS row is. Per-key embed
                  presence is a v0.9.x follow-up. */}
              {scopeHasEmbeddings && (
                <span className="embed-dot" title="scope has embeddings (use Memory.search from an agent)" />
              )}
              <code>{e.key}</code>
              {e.expires_at && <span className="ttl-flag" title={`expires ${e.expires_at}`}>ttl</span>}
            </li>
          ))}
          {truncated && (
            <li className="empty-row">… more keys hidden (raise limit or refine prefix)</li>
          )}
        </ul>
      </div>
      <div className="memory-pane detail-pane">
        <div className="pane-header">value</div>
        {!selectedEntry && <div className="empty">pick a key to inspect its value.</div>}
        {selectedEntry && (
          <div className="entry-detail">
            <div className="entry-meta">
              <div><span>key</span><code>{selectedEntry.key}</code></div>
              <div><span>created</span><code>{selectedEntry.created_at}</code></div>
              <div><span>updated</span><code>{selectedEntry.updated_at}</code></div>
              {selectedEntry.expires_at && (
                <div><span>expires</span><code>{selectedEntry.expires_at}</code></div>
              )}
            </div>
            <pre className="entry-value">{prettyJSON(selectedEntry.value)}</pre>
          </div>
        )}
      </div>
      </Splitter>
    </Splitter>
    </div>
  );
}

// ReembedBanner renders the dry-run plan or the real-run outcome.
// Dry-run shape carries sample_keys + a "commit" button; real-run
// carries reembedded + failed counts and a dismiss button.
function ReembedBanner(props: {
  banner: MemoryReembedResponse;
  busy: boolean;
  onCommit: () => void;
  onDismiss: () => void;
}) {
  const { banner, busy, onCommit, onDismiss } = props;
  if (banner.dry_run) {
    return (
      <div className="reembed-banner reembed-dryrun">
        <div className="reembed-summary">
          <strong>{banner.rows_to_reembed}</strong> row{banner.rows_to_reembed === 1 ? "" : "s"} would be re-embedded under{" "}
          <code>{banner.current_embedder.provider}/{banner.current_embedder.model}</code>.
        </div>
        {banner.sample_keys.length > 0 && (
          <div className="reembed-samples">
            sample: {banner.sample_keys.slice(0, 8).map((k) => (
              <code key={k}>{k}</code>
            ))}
            {banner.sample_keys_capped && <span className="meta">…</span>}
          </div>
        )}
        <div className="reembed-actions">
          {banner.rows_to_reembed > 0 && (
            <button onClick={onCommit} disabled={busy} className="reembed-commit-btn">
              {busy ? "re-embedding…" : `commit (${banner.rows_to_reembed} row${banner.rows_to_reembed === 1 ? "" : "s"})`}
            </button>
          )}
          <button onClick={onDismiss} disabled={busy} className="reembed-dismiss-btn">
            dismiss
          </button>
        </div>
      </div>
    );
  }
  return (
    <div className="reembed-banner reembed-realrun">
      <div className="reembed-summary">
        re-embedded <strong>{banner.rows_reembedded}</strong>
        {banner.rows_failed > 0 && (
          <>
            {" "}· <span className="meta-failed">{banner.rows_failed} failed</span>
          </>
        )}
        {" "}under{" "}
        <code>{banner.current_embedder.provider}/{banner.current_embedder.model}</code>.
      </div>
      {banner.failed_keys && banner.failed_keys.length > 0 && (
        <div className="reembed-samples">
          failed: {banner.failed_keys.slice(0, 8).map((k) => (
            <code key={k}>{k}</code>
          ))}
        </div>
      )}
      <div className="reembed-actions">
        <button onClick={onDismiss} className="reembed-dismiss-btn">
          dismiss
        </button>
      </div>
    </div>
  );
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MB`;
}

function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
