import { useCallback, useEffect, useMemo, useState } from "react";
import { NavLink, useLocation } from "react-router-dom";
import {
  type EphemeralVolumeEntry,
  type LibraryEntry,
  type PersistentVolumeEntry,
  type VolumeDefRow,
  deleteVolume,
  listEphemeralVolumes,
  listLibraryAgents,
  listVolumes,
  purgeVolume,
} from "../api";
import VolumeEditModal from "../components/VolumeEditModal";

// VolumesView — RFC AH Phase 4 dedicated "Volumes" console tab.
//
// Two sub-tabs (nested routes, like IntegrationsView):
//   Persistent — flat table of GET /v1/_volumes. STATIC rows are operator
//     yaml (read-only). DYNAMIC rows are the tenant's VolumeDefs: Delete
//     (non-destructive unmap) + Purge (destructive RemoveAll, type-to-confirm).
//     A "bound by" column cross-references AgentDef.volumes (derived from
//     listLibraryAgents()).
//   Ephemeral — read-only flat table of GET /v1/_volumes/ephemeral (live,
//     run-scoped, auto-purged at run completion).
//
// A VolumeDef is FLAT (no versions/promote/retire) — hence a simple table, not
// the lineage tree LibraryView/IntegrationsView use. Tenant scoping is server-
// side (authoritative principal); the UI never assumes cross-tenant visibility.

const REFRESH_MS = 10_000;

type SubKey = "persistent" | "ephemeral";

export default function VolumesView() {
  const [persistent, setPersistent] = useState<PersistentVolumeEntry[]>([]);
  const [ephemeral, setEphemeral] = useState<EphemeralVolumeEntry[]>([]);
  // agentName -> volume names it binds (for the "bound by" cross-reference).
  const [bindings, setBindings] = useState<Record<string, string[]>>({});
  const [err, setErr] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);
  const refreshNow = useCallback(() => setRefreshKey((k) => k + 1), []);

  const [showCreate, setShowCreate] = useState(false);
  const [purgeTarget, setPurgeTarget] = useState<PersistentVolumeEntry | null>(null);

  const loc = useLocation();
  const sub: SubKey = loc.pathname.startsWith("/volumes/ephemeral")
    ? "ephemeral"
    : "persistent";

  useEffect(() => {
    let cancelled = false;
    const fetchAll = async () => {
      try {
        const [pv, ev, agents] = await Promise.all([
          listVolumes(),
          listEphemeralVolumes(),
          // Bindings are best-effort: an agent-list failure shouldn't blank the
          // volume tables, so it's tolerated (bindings stay empty).
          listLibraryAgents().catch(() => ({ entries: [] as LibraryEntry[] })),
        ]);
        if (cancelled) return;
        setPersistent(pv.entries ?? []);
        setEphemeral(ev.entries ?? []);
        setBindings(deriveBindings(agents.entries ?? []));
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    };
    fetchAll();
    const t = setInterval(fetchAll, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [refreshKey]);

  // boundBy maps a volume name -> the agents that bind it.
  const boundBy = useMemo(() => {
    const out: Record<string, string[]> = {};
    for (const [agent, vols] of Object.entries(bindings)) {
      for (const v of vols) {
        (out[v] ??= []).push(agent);
      }
    }
    for (const k of Object.keys(out)) out[k].sort();
    return out;
  }, [bindings]);

  const handleDelete = async (row: PersistentVolumeEntry) => {
    if (
      !window.confirm(
        `Delete (unmap) volume "${row.name}"? This removes the mapping but LEAVES the files on disk. Use Purge to also delete the directory tree.`,
      )
    ) {
      return;
    }
    setActionErr(null);
    try {
      await deleteVolume(row.name);
      refreshNow();
    } catch (e) {
      setActionErr(`Delete failed: ${liftRefusal(e)}`);
    }
  };

  return (
    <div className="library-view">
      <div className="library-subtabs">
        <NavLink to="/volumes/persistent" end className={subtabClass}>
          Persistent <span className="library-subtab-count">{persistent.length}</span>
        </NavLink>
        <NavLink to="/volumes/ephemeral" end className={subtabClass}>
          Ephemeral <span className="library-subtab-count">{ephemeral.length}</span>
        </NavLink>
      </div>

      {err && <div className="error-banner">Failed to load volumes: {err}</div>}
      {actionErr && <div className="error-banner">{actionErr}</div>}

      {sub === "persistent" ? (
        <PersistentTab
          rows={persistent}
          boundBy={boundBy}
          onCreate={() => setShowCreate(true)}
          onDelete={handleDelete}
          onPurge={(row) => setPurgeTarget(row)}
        />
      ) : (
        <EphemeralTab rows={ephemeral} />
      )}

      {showCreate && (
        <VolumeEditModal
          existingNames={persistent.map((p) => p.name)}
          onClose={() => setShowCreate(false)}
          onSaved={() => {
            setShowCreate(false);
            refreshNow();
          }}
        />
      )}
      {purgeTarget && (
        <PurgeConfirmModal
          volume={purgeTarget}
          onClose={() => setPurgeTarget(null)}
          onPurged={() => {
            setPurgeTarget(null);
            refreshNow();
          }}
          onError={(m) => {
            setPurgeTarget(null);
            setActionErr(`Purge failed: ${m}`);
          }}
        />
      )}
    </div>
  );
}

const subtabClass = ({ isActive }: { isActive: boolean }) =>
  isActive ? "library-subtab library-subtab-active" : "library-subtab";

// ---- Persistent sub-tab ----

function PersistentTab({
  rows,
  boundBy,
  onCreate,
  onDelete,
  onPurge,
}: {
  rows: PersistentVolumeEntry[];
  boundBy: Record<string, string[]>;
  onCreate: () => void;
  onDelete: (row: PersistentVolumeEntry) => void;
  onPurge: (row: PersistentVolumeEntry) => void;
}) {
  return (
    <div className="library-content">
      <div className="volumes-toolbar">
        <span className="muted">
          Static volumes are operator yaml (read-only — the shared bind floor).
          Dynamic volumes are this tenant's — Delete unmaps (keeps files); Purge
          deletes the directory tree.
        </span>
        <button type="button" className="primary" onClick={onCreate}>
          Create volume
        </button>
      </div>
      <table className="snapshots-table volumes-table">
        <thead>
          <tr>
            <th>name</th>
            <th>source</th>
            <th>path</th>
            <th>mode</th>
            <th>default</th>
            <th>bound by</th>
            <th>actions</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={7} className="empty">
                No volumes. Static volumes come from the loomcycle yaml; dynamic
                volumes are created here.
              </td>
            </tr>
          )}
          {rows.map((r) => {
            const agents = boundBy[r.name] ?? [];
            return (
              <tr key={`${r.source}:${r.name}`}>
                <td className="mono">{r.name}</td>
                <td>
                  <span className={`source-chip source-chip-${r.source === "static" ? "static-only" : "dynamic-only"}`}>
                    {r.source}
                  </span>
                  {r.dynamic_root && (
                    <span className="badge badge-label" title="Dynamic volumes are provisioned inside this root">
                      dynamic_root
                    </span>
                  )}
                </td>
                <td className="mono volumes-path">{r.path || <span className="muted">—</span>}</td>
                <td className="mono">{r.mode}</td>
                <td>{r.default ? "✓" : <span className="muted">—</span>}</td>
                <td>
                  {agents.length === 0 ? (
                    <span className="muted">—</span>
                  ) : (
                    <span className="volumes-boundby" title={agents.join(", ")}>
                      {agents.join(", ")}
                    </span>
                  )}
                </td>
                <td className="actions">
                  {r.source === "dynamic" ? (
                    <>
                      <button
                        type="button"
                        className="link-btn"
                        title="Unmap the volume (removes the mapping, keeps files on disk)"
                        onClick={() => onDelete(r)}
                      >
                        delete
                      </button>
                      <button
                        type="button"
                        className="link-btn link-btn-danger"
                        title="Destructive: removes the mapping AND deletes the directory tree"
                        onClick={() => onPurge(r)}
                      >
                        purge
                      </button>
                    </>
                  ) : (
                    <span className="muted" title="Static volumes are operator yaml — edit the yaml + restart">
                      read-only
                    </span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ---- Ephemeral sub-tab ----

function EphemeralTab({ rows }: { rows: EphemeralVolumeEntry[] }) {
  return (
    <div className="library-content">
      <div className="volumes-toolbar">
        <span className="muted">
          Ephemeral volumes are live and run-scoped — created mid-run, inherited
          by sub-agents, and auto-purged when the run completes. Read-only here.
        </span>
      </div>
      <table className="snapshots-table volumes-table">
        <thead>
          <tr>
            <th>name</th>
            <th>run_id</th>
            <th>path</th>
            <th>mode</th>
            <th>created_at</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={5} className="empty">
                No live ephemeral volumes. They appear here while a run that
                created one is active.
              </td>
            </tr>
          )}
          {rows.map((r) => (
            <tr key={`${r.root_run_id}:${r.name}`}>
              <td className="mono">{r.name}</td>
              <td className="mono">{r.root_run_id}</td>
              <td className="mono volumes-path">{r.path || <span className="muted">—</span>}</td>
              <td className="mono">{r.mode}</td>
              <td>{r.created_at}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---- Purge confirm (type-to-confirm — the dangerous one) ----

function PurgeConfirmModal({
  volume,
  onClose,
  onPurged,
  onError,
}: {
  volume: PersistentVolumeEntry;
  onClose: () => void;
  onPurged: (row: VolumeDefRow) => void;
  onError: (msg: string) => void;
}) {
  const [typed, setTyped] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const confirmed = typed === volume.name;

  const submit = async () => {
    if (!confirmed) return;
    setSubmitting(true);
    try {
      const row = await purgeVolume(volume.name);
      onPurged(row);
    } catch (e) {
      onError(liftRefusal(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>Purge volume · {volume.name}</h3>
        <div className="warning-banner">
          This is <strong>destructive</strong>. Purge removes the mapping AND
          recursively deletes the directory tree (<code className="mono">{volume.path}</code>)
          on disk. This cannot be undone. To unmap without deleting files, use
          Delete instead.
        </div>
        <div className="library-modal-fields">
          <label className="library-modal-field">
            <span>
              Type the volume name <code className="mono">{volume.name}</code> to confirm
            </span>
            <input
              type="text"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={volume.name}
              autoFocus
              autoComplete="off"
            />
          </label>
        </div>
        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button
            type="button"
            className="primary primary-danger"
            onClick={submit}
            disabled={!confirmed || submitting}
          >
            {submitting ? "Purging…" : "Purge"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ---- helpers ----

// deriveBindings maps each agent name -> its bound volume names, read from the
// agent's static_definition.volumes (the library endpoint surfaces it). Agents
// with no bindings are omitted.
function deriveBindings(entries: LibraryEntry[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const e of entries) {
    const def = e.static_definition;
    if (def && typeof def === "object" && "volumes" in def) {
      const vols = (def as { volumes?: unknown }).volumes;
      if (Array.isArray(vols)) {
        const names = vols.filter((v): v is string => typeof v === "string");
        if (names.length > 0) out[e.name] = names;
      }
    }
  }
  return out;
}

// liftRefusal extracts the human-readable text from the substrate's
// {"code":"tool_refused","error":"…"} 422 envelope surfaced in a thrown Error.
function liftRefusal(e: unknown): string {
  const msg = e instanceof Error ? e.message : String(e);
  const match = msg.match(/"error":"([^"]+)"/);
  return match && match[1] ? match[1] : msg;
}
