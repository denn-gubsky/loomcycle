import { useEffect, useMemo, useState } from "react";
import {
  MemoryEntry,
  MemoryScopeIDSummary,
  MemoryScopeKind,
  listMemoryEntries,
  listMemoryScopeIDs,
  listMemoryScopes,
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
// Polling, not SSE — Memory rows are slow-changing (counters tick on
// the order of one-per-iteration; preferences barely change). 5 s
// refresh is plenty to feel live without hammering the store.
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

  // Bootstrap: fetch the (constant) scope list once on mount, default
  // the selection to "agent" so the page lands on actual data.
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

  // When the scope changes, blow away the scope_id selection so the
  // user always picks fresh under the new scope.
  useEffect(() => {
    setScopeID("");
    setSelectedKey("");
    setEntries([]);
    setPrefix("");
  }, [scope]);

  // Poll entries under the selected (scope, scope_id, prefix). Debounce
  // the prefix input by piggy-backing on the polling clock — typing
  // doesn't fire a request per keystroke; the next tick picks up the
  // new prefix.
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

  // Resolve the currently selected entry from the in-memory list — no
  // separate fetch required because the list response already carries
  // the value. (Drops one round-trip and one source of staleness.)
  const selectedEntry = useMemo(() => {
    if (!selectedKey) return null;
    return entries.find((e) => e.key === selectedKey) ?? null;
  }, [entries, selectedKey]);

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
