import { useEffect, useMemo, useState } from "react";
import type {
  MemoryEmbedModelStats,
  MemoryEmbeddingMeta,
  MemoryEntry,
  MemoryReembedResponse,
  MemoryScopeIDSummary,
  MemoryScopeKind,
} from "../types";
import { useMemoryData } from "../lib/dataLayer";
import Splitter from "./Splitter";
import MemoryEntryEditModal from "./MemoryEntryEditModal";

// MemoryConsole — operator browse + edit view over the Memory tool's stored rows.
// The body of <MemoryView>; it reads the injected MemoryDataLayer via context
// (no global api module), so the same console renders standalone (bearer) or
// embedded in the Web UI (cookie fetch).
//
// Three-pane layout:
//   left   : scope picker (agent / user / operator-declared) + scope_id list
//            under the chosen scope, with key counts.
//   middle : key list for the selected (scope, scope_id), with a prefix filter.
//   right  : the selected entry's value pretty-printed JSON, plus timestamps + TTL.
//
// Vector Memory additions:
//   - Per-scope model-distribution badge (top of the keys pane) — which
//     embedder(s) wrote the rows in this scope.
//   - "reembed plan" → dry-run then commit flow when the badge surfaces rows
//     under a non-current embedder.
//   - An embedding indicator dot on each key row when the scope has embeddings
//     (a hint, not per-key authoritative), and a per-key embedding badge when the
//     data layer supplies embedding_metadata for that key (absent under the
//     default client path — see MemoryEmbeddingMeta).
//
// Polling, not SSE — Memory rows are slow-changing. 5 s refresh is plenty to feel
// live without hammering the store.
const REFRESH_MS = 5_000;

// The tenant scope addresses ONE keyspace per tenant — its store scope_id is
// EMPTY (the tenant_id column partitions it), so there is no scope_id to pick.
// We auto-select this placeholder when tenant is chosen so entries load directly;
// the backend drops it (adminMemoryStoreScopeID → "") and the scope_id pane shows
// a static "tenant-wide" note instead of a picker. Any non-empty value works (the
// server ignores it for tenant); Go's router just needs a non-empty {scope_id}.
const TENANT_SCOPE_ID = "-";

export default function MemoryConsole() {
  const data = useMemoryData();
  const [scopes, setScopes] = useState<MemoryScopeKind[]>([]);
  const [scope, setScope] = useState<string>("");
  const [scopeIDs, setScopeIDs] = useState<MemoryScopeIDSummary[]>([]);
  const [scopeID, setScopeID] = useState<string>("");
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  // Per-key embedding metadata (provider/model/dimension). Keyed by entry key;
  // keys without an embedding are simply absent. Empty map = the data layer
  // didn't supply it (the default client path never does), so no per-row
  // indicator ever shows.
  const [embeddingMeta, setEmbeddingMeta] = useState<Record<string, MemoryEmbeddingMeta>>({});
  const [truncated, setTruncated] = useState(false);
  const [selectedKey, setSelectedKey] = useState<string>("");
  const [prefix, setPrefix] = useState("");
  const [err, setErr] = useState<string | null>(null);

  // Vector Memory state.
  // null = embed_stats rejected (vectors not configured); the UI renders a
  // "vector search not available" hint instead of the model badge.
  const [embedStats, setEmbedStats] = useState<MemoryEmbedModelStats[] | null>(null);
  const [reembedBanner, setReembedBanner] = useState<MemoryReembedResponse | null>(null);
  const [reembedBusy, setReembedBusy] = useState(false);
  // The two other embedding-maintenance ops. One `maint` slot rather than a banner each:
  // they are alternatives, and two open at once would invite committing the wrong one.
  const [maint, setMaint] = useState<
    | { kind: "backfill"; resp: Awaited<ReturnType<typeof data.backfillEmbeddings>> }
    | { kind: "purge"; resp: Awaited<ReturnType<typeof data.purgeStaleEmbeddings>> }
    | null
  >(null);
  const [maintBusy, setMaintBusy] = useState(false);

  // CRUD state.
  const [modalState, setModalState] = useState<
    | { kind: "create" }
    | { kind: "edit"; entry: MemoryEntry }
    | null
  >(null);
  const [mutationErr, setMutationErr] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);

  const triggerReload = () => setReloadTick((n) => n + 1);

  // Human label for the current (scope, scope_id) target. The tenant scope's
  // scope_id is the internal "-" placeholder, so show "tenant-wide" rather than
  // leak "-" into headers / confirmation dialogs.
  const scopeLabel = scope === "tenant" ? "tenant-wide" : `${scope}/${scopeID}`;

  const handleDelete = async (entry: MemoryEntry) => {
    if (!scope || !scopeID) return;
    if (!window.confirm(`Delete ${scopeLabel}/${entry.key}? This also removes any embedding.`)) {
      return;
    }
    setMutationErr(null);
    try {
      await data.deleteEntry(scope, scopeID, entry.key);
      if (selectedKey === entry.key) setSelectedKey("");
      triggerReload();
    } catch (e) {
      setMutationErr(e instanceof Error ? e.message : String(e));
    }
  };

  // Bootstrap: fetch the (constant) scope list once on mount.
  useEffect(() => {
    let cancelled = false;
    data
      .listScopes()
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

  // Poll scope_ids under the selected scope. The tenant scope has no scope_id
  // dimension (one tenant-wide keyspace), so we skip the fetch and leave the pane
  // to render its static "tenant-wide" note.
  useEffect(() => {
    if (!scope || scope === "tenant") {
      setScopeIDs([]);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await data.listScopeIDs(scope);
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
  }, [scope, data]);

  // Poll embed_stats for the selected scope so the UI knows which models wrote
  // what. embedStats REJECTS when vectors aren't configured (the runtime 503s);
  // catch it and surface null so the badge shows the "not configured" hint
  // instead of throwing.
  useEffect(() => {
    if (!scope) {
      setEmbedStats(null);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await data.embedStats(scope);
        if (cancelled) return;
        setEmbedStats(resp.models ?? []);
      } catch {
        if (cancelled) return;
        setEmbedStats(null);
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [scope, data]);

  // When the scope changes, reset the scope_id selection so the user picks fresh
  // under the new scope. The tenant scope has no scope_id to pick, so auto-select
  // the placeholder — entries then load directly against the tenant-wide keyspace.
  useEffect(() => {
    setScopeID(scope === "tenant" ? TENANT_SCOPE_ID : "");
    setSelectedKey("");
    setEntries([]);
    setPrefix("");
    setReembedBanner(null);
  }, [scope]);

  // Poll entries under the selected (scope, scope_id, prefix).
  useEffect(() => {
    if (!scope || !scopeID) {
      setEntries([]);
      setEmbeddingMeta({});
      setTruncated(false);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        const resp = await data.listEntries(scope, scopeID, {
          prefix: prefix || undefined,
          limit: 200,
        });
        if (cancelled) return;
        setEntries(resp.entries ?? []);
        setEmbeddingMeta(resp.embedding_metadata ?? {});
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
  }, [scope, scopeID, prefix, reloadTick, data]);

  const selectedEntry = useMemo(() => {
    if (!selectedKey) return null;
    return entries.find((e) => e.key === selectedKey) ?? null;
  }, [entries, selectedKey]);

  // Are there embeddings under this scope? Used to render the per-row indicator
  // dot. embed_stats reports aggregate counts, not per-key presence — the dot is
  // a hint that SOME rows are embedded, not which specific ones.
  const scopeHasEmbeddings = useMemo(() => {
    if (!embedStats) return false;
    return embedStats.some((m) => m.row_count > 0);
  }, [embedStats]);

  const runMaint = async (kind: "backfill" | "purge", dryRun: boolean) => {
    if (!scope || !scopeID) return;
    if (kind === "purge" && !dryRun) {
      if (!window.confirm(
        `Drop the embeddings of rows under ${scopeLabel} that have no indexable text? ` +
          `This deletes vectors; the rows themselves are untouched.`,
      )) {
        return;
      }
    }
    setMaintBusy(true);
    setMaint(null);
    try {
      const resp =
        kind === "backfill"
          ? await data.backfillEmbeddings(scope, scopeID, { dryRun })
          : await data.purgeStaleEmbeddings(scope, scopeID, { dryRun });
      setMaint(
        kind === "backfill"
          ? { kind, resp: resp as Awaited<ReturnType<typeof data.backfillEmbeddings>> }
          : { kind, resp: resp as Awaited<ReturnType<typeof data.purgeStaleEmbeddings>> },
      );
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setMaintBusy(false);
    }
  };

  const handleReembedDryRun = async () => {
    if (!scope || !scopeID) return;
    setReembedBusy(true);
    setReembedBanner(null);
    try {
      const resp = await data.reembed(scope, scopeID, { dryRun: true });
      setReembedBanner(resp);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setReembedBusy(false);
    }
  };

  const handleReembedCommit = async () => {
    if (!scope || !scopeID) return;
    if (!window.confirm(`Re-embed all rows under ${scopeLabel} using the current embedder? This calls the provider API and may incur cost.`)) {
      return;
    }
    setReembedBusy(true);
    try {
      const resp = await data.reembed(scope, scopeID, { dryRun: false });
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
        <div className="pane-header sub">{scope === "tenant" ? "tenant keyspace" : `${scope || "—"} ids`}</div>
        {scope === "tenant" ? (
          // No scope_id dimension — one keyspace per tenant. A static, always-on
          // row so the pane isn't empty and the middle keys pane loads directly.
          <ul className="scope-id-list">
            <li className="on tenant-wide">
              <code>tenant-wide</code>
              <span className="meta">one keyspace, shared across the tenant</span>
            </li>
          </ul>
        ) : (
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
        )}
      </div>
      <Splitter
        className="memory-view-inner"
        defaultLeftWidth={320}
        minLeftWidth={220}
        minRightWidth={320}
        storageKey="loomcycle.split.memory.inner"
      >
      <div className="memory-pane keys-pane">
        <div className="pane-header memory-keys-header">
          <span>keys {scopeID && <code>{scopeLabel}</code>}</span>
          <button
            type="button"
            className="memory-new-entry-btn"
            onClick={() => setModalState({ kind: "create" })}
            title="Set a new memory entry"
          >
            + New entry
          </button>
        </div>
        {mutationErr && <div className="err memory-err">{mutationErr}</div>}
        {/* Embedding model badge + reembed action. Only renders when embed_stats
            reports rows for this scope. */}
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
            {scopeID && (
              <>
                <button
                  className="embed-reembed-btn"
                  disabled={maintBusy}
                  onClick={() => void runMaint("backfill", true)}
                  title="See which rows carry NO embedding — what enabling an embedder after the rows were written leaves behind"
                >
                  {maintBusy ? "…" : "backfill plan"}
                </button>
                <button
                  className="embed-reembed-btn"
                  disabled={maintBusy}
                  onClick={() => void runMaint("purge", true)}
                  title="See which rows carry an embedding but have no indexable text to justify one"
                >
                  {maintBusy ? "…" : "purge plan"}
                </button>
              </>
            )}
          </div>
        )}
        {scope && embedStats === null && (
          <div className="embed-badge-row embed-badge-disabled">
            <span className="embed-badge-label">embeddings: not configured</span>
          </div>
        )}
        {/* Reembed banner. Shows dry-run results + commit button, or the real-run
            outcome. */}
        {reembedBanner && (
          <ReembedBanner
            banner={reembedBanner}
            busy={reembedBusy}
            onCommit={handleReembedCommit}
            onDismiss={() => setReembedBanner(null)}
          />
        )}
        {maint && (
          <MaintenanceBanner
            state={maint}
            busy={maintBusy}
            onCommit={() => void runMaint(maint.kind, false)}
            onDismiss={() => setMaint(null)}
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
              {/* Embedding indicator dot. Hint, not authoritative: shows when ANY
                  row in the scope is embedded, not whether THIS row is. */}
              {scopeHasEmbeddings && (
                <span className="embed-dot" title="scope has embeddings (use Memory.search from an agent)" />
              )}
              <code>{e.key}</code>
              {/* Per-key embedding indicator. Renders only when the data layer
                  returned embedding_metadata for THIS key; absent for plain k/v
                  rows and under the default client path. */}
              {embeddingMeta[e.key] && (
                <span
                  className="embed-key-badge"
                  title={`embedded with ${embeddingMeta[e.key].provider}/${embeddingMeta[e.key].model}, dim=${embeddingMeta[e.key].dimension}`}
                >
                  embedded · {embeddingMeta[e.key].model} · {embeddingMeta[e.key].dimension}d
                </span>
              )}
              {e.expires_at && <span className="ttl-flag" title={`expires ${e.expires_at}`}>ttl</span>}
              <span className="memory-key-actions">
                <button
                  type="button"
                  className="memory-key-action"
                  title="Edit entry"
                  onClick={(ev) => {
                    ev.stopPropagation();
                    setModalState({ kind: "edit", entry: e });
                  }}
                >
                  Edit
                </button>
                <button
                  type="button"
                  className="memory-key-action memory-key-action-danger"
                  title="Delete entry"
                  onClick={(ev) => {
                    ev.stopPropagation();
                    void handleDelete(e);
                  }}
                >
                  Delete
                </button>
              </span>
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
    {modalState && (
      <MemoryEntryEditModal
        mode={modalState.kind === "create" ? "create" : "edit"}
        scope={scope}
        scopeID={scopeID}
        existing={modalState.kind === "edit" ? modalState.entry : undefined}
        onClose={() => setModalState(null)}
        onSaved={() => {
          setModalState(null);
          triggerReload();
        }}
      />
    )}
    </div>
  );
}

// ReembedBanner renders the dry-run plan or the real-run outcome. Dry-run shape
// carries sample_keys + a "commit" button; real-run carries reembedded + failed
// counts and a dismiss button.
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

// MaintenanceBanner — the plan (or outcome) of a backfill / stale-purge, with the two
// numbers each response carries that a bare count would misrepresent.
//
// BACKFILL: `skipped_empty` rows have NO text to embed — a document root, a section
// heading — so they remain candidates permanently. Without saying so, an operator watches
// `candidates` stop falling with `embedded` at 0 and no stated reason. `more` means the
// limit was reached with work outstanding, which is the honest replacement for "run it
// until candidates hits 0" — a target unembeddable rows make unreachable.
//
// PURGE: `truncated` means the scan stopped at the limit, so a zero `stale` does NOT mean
// the scope is clean. This op deletes, and "we looked at some of it and found nothing" is
// a different statement from "there is nothing".
function MaintenanceBanner({
  state,
  busy,
  onCommit,
  onDismiss,
}: {
  state:
    | { kind: "backfill"; resp: { dry_run: boolean; candidates: number; embedded?: number; failed?: number; skipped_empty?: number; more?: boolean; sample_keys?: string[]; notes?: string[] } }
    | { kind: "purge"; resp: { dry_run: boolean; scanned: number; stale: number; purged: number; failed?: number; truncated: boolean; sample_keys?: string[]; notes?: string[] } };
  busy: boolean;
  onCommit: () => void;
  onDismiss: () => void;
}) {
  const dry = state.resp.dry_run;
  return (
    <div className={"reembed-banner " + (dry ? "reembed-dryrun" : "")}>
      <div className="reembed-summary">
        {state.kind === "backfill" ? (
          <>
            <strong>{state.resp.candidates}</strong> row
            {state.resp.candidates === 1 ? "" : "s"} carry no embedding
            {dry ? " and would be embedded" : ` · embedded ${state.resp.embedded ?? 0}`}
            {(state.resp.failed ?? 0) > 0 && <> · failed {state.resp.failed}</>}
            {(state.resp.skipped_empty ?? 0) > 0 && (
              <>
                {" "}
                · <strong>{state.resp.skipped_empty}</strong> have no text to embed and stay
                candidates permanently
              </>
            )}
            {state.resp.more && <> · the limit was reached, so another run has work</>}
          </>
        ) : (
          <>
            <strong>{state.resp.stale}</strong> of {state.resp.scanned} scanned row
            {state.resp.scanned === 1 ? "" : "s"} carry an embedding with no text to justify it
            {!dry && <> · purged {state.resp.purged}</>}
            {(state.resp.failed ?? 0) > 0 && <> · failed {state.resp.failed}</>}
            {state.resp.truncated && (
              <>
                {" "}
                · <strong>the scan stopped at the limit</strong>, so this is not the whole
                scope
              </>
            )}
          </>
        )}
      </div>
      {state.resp.sample_keys && state.resp.sample_keys.length > 0 && (
        <ul className="reembed-samples">
          {state.resp.sample_keys.slice(0, 8).map((k) => (
            <li key={k}>
              <code>{k}</code>
            </li>
          ))}
        </ul>
      )}
      {state.resp.notes?.map((n) => (
        <div key={n} className="meta">
          {n}
        </div>
      ))}
      <div className="reembed-actions">
        {dry && (
          <button className="reembed-commit-btn" disabled={busy} onClick={onCommit}>
            {state.kind === "backfill" ? "embed them" : "purge them"}
          </button>
        )}
        <button className="reembed-dismiss-btn" disabled={busy} onClick={onDismiss}>
          dismiss
        </button>
      </div>
    </div>
  );
}
