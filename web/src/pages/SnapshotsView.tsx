import { useEffect, useRef, useState } from "react";
import {
  createSnapshot,
  deleteSnapshot,
  exportSnapshotURL,
  listSnapshots,
  restoreSnapshotFromText,
  SnapshotListEntry,
  SnapshotRestoreResponse,
} from "../api";

// v0.8.17 PR 5 — Snapshots admin view. Shows the captured snapshots,
// supports capture / restore-from-file / export-as-download / delete.
// Polls every 10 s; capture is a manual button (snapshots are operator-
// initiated). No agent context required.
const POLL_MS = 10_000;

interface RestoreFlash {
  ok: boolean;
  message: string;
  details?: SnapshotRestoreResponse;
}

export default function SnapshotsView() {
  const [rows, setRows] = useState<SnapshotListEntry[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [captureBusy, setCaptureBusy] = useState(false);
  const [captureLabel, setCaptureLabel] = useState("");
  const [restoreBusy, setRestoreBusy] = useState(false);
  const [restoreFlash, setRestoreFlash] = useState<RestoreFlash | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const refresh = async () => {
    try {
      setLoading(true);
      const resp = await listSnapshots();
      setRows(resp.entries ?? []);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, POLL_MS);
    return () => clearInterval(t);
  }, []);

  // Auto-dismiss restore flash after 8s — long enough to read the
  // per-section counts but short enough that the next refresh shows
  // clean state.
  useEffect(() => {
    if (!restoreFlash) return;
    const t = setTimeout(() => setRestoreFlash(null), 8_000);
    return () => clearTimeout(t);
  }, [restoreFlash]);

  const doCapture = async () => {
    if (captureBusy) return;
    setCaptureBusy(true);
    try {
      const created = await createSnapshot(captureLabel.trim() || undefined);
      setRows((prev) => [
        {
          id: created.id,
          created_at: created.created_at,
          label: created.label,
          schema_version: created.schema_version,
          byte_size: created.byte_size,
        },
        ...prev,
      ]);
      setCaptureLabel("");
      setRestoreFlash({ ok: true, message: `captured ${created.id} (${formatBytes(created.byte_size)})` });
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCaptureBusy(false);
    }
  };

  const doDelete = async (id: string) => {
    if (!confirm(`Delete snapshot ${id}? This is irreversible.`)) return;
    try {
      await deleteSnapshot(id);
      setRows((prev) => prev.filter((r) => r.id !== id));
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const doRestoreFromFile = async (file: File) => {
    if (restoreBusy) return;
    if (
      !confirm(
        `Restore ${file.name} into this runtime?\n\nIdempotent: existing rows with the same PK aren't overwritten (per-section ON CONFLICT DO NOTHING). The response counters reflect rows actually written.`,
      )
    ) {
      return;
    }
    setRestoreBusy(true);
    try {
      const text = await file.text();
      const resp = await restoreSnapshotFromText(text);
      setRestoreFlash({
        ok: true,
        message: summarizeRestore(resp),
        details: resp,
      });
      setErr(null);
      // The restored data may include agent_defs etc. — refresh
      // the list to surface any new rows that were re-created.
      refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setRestoreBusy(false);
      // Reset the input so re-uploading the same file fires onChange.
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  return (
    <div className="snapshots-view">
      <div className="toolbar">
        <h2>Snapshots</h2>
        <div className="meta">
          {loading && <span className="spin">refreshing…</span>}
          <span>{rows.length} snapshot{rows.length === 1 ? "" : "s"}</span>
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      <div className="snapshot-actions">
        <div className="capture">
          <label htmlFor="snap-label">Capture:</label>
          <input
            id="snap-label"
            type="text"
            placeholder="optional description"
            value={captureLabel}
            onChange={(e) => setCaptureLabel(e.target.value)}
            disabled={captureBusy}
          />
          <button type="button" onClick={doCapture} disabled={captureBusy}>
            {captureBusy ? "capturing…" : "capture snapshot"}
          </button>
        </div>
        <div className="restore">
          <label htmlFor="snap-restore">Restore:</label>
          <input
            id="snap-restore"
            ref={fileInputRef}
            type="file"
            accept="application/json"
            disabled={restoreBusy}
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (f) doRestoreFromFile(f);
            }}
          />
          {restoreBusy && <span className="spin">restoring…</span>}
        </div>
      </div>

      {restoreFlash && (
        <div className={restoreFlash.ok ? "flash flash-ok" : "flash flash-err"}>
          <strong>{restoreFlash.message}</strong>
          {restoreFlash.details?.warnings && restoreFlash.details.warnings.length > 0 && (
            <ul className="restore-warnings">
              {restoreFlash.details.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          )}
        </div>
      )}

      <table className="snapshots-table">
        <thead>
          <tr>
            <th>id</th>
            <th>label</th>
            <th>created_at</th>
            <th>size</th>
            <th>schema</th>
            <th>actions</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && !loading && (
            <tr>
              <td colSpan={6} className="empty">
                No snapshots yet — use Capture above to record the current runtime state.
              </td>
            </tr>
          )}
          {rows.map((r) => (
            <tr key={r.id}>
              <td className="mono">{r.id}</td>
              <td>{r.label || <span className="muted">—</span>}</td>
              <td>{r.created_at}</td>
              <td>{formatBytes(r.byte_size)}</td>
              <td>v{r.schema_version}</td>
              <td className="actions">
                <a href={exportSnapshotURL(r.id)} download={`${r.id}.json`} title="Download the JSON envelope">
                  export
                </a>
                <button type="button" className="link-btn" onClick={() => doDelete(r.id)} title="Permanently delete this snapshot">
                  delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// formatBytes renders a byte count as a human-friendly string. Bounded
// at TiB so we never trigger surprise scaling on unexpectedly large
// snapshots; anything over ~1 PiB is captured incorrectly somewhere
// upstream and the operator wants to see the raw value.
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MiB`;
  if (n < 1024 ** 4) return `${(n / 1024 ** 3).toFixed(2)} GiB`;
  return `${(n / 1024 ** 4).toFixed(2)} TiB`;
}

function summarizeRestore(r: SnapshotRestoreResponse): string {
  const parts: string[] = [];
  const add = (label: string, n?: number) => {
    if (n && n > 0) parts.push(`${label}=${n}`);
  };
  add("agent_defs", r.agent_defs_restored);
  add("agent_def_active", r.agent_def_active_restored);
  add("memory", r.memory_restored);
  add("channel_messages", r.channel_messages_restored);
  add("channel_cursors", r.channel_cursors_restored);
  add("evaluations", r.evaluations_restored);
  add("paused_runs", r.paused_runs_restored);
  add("transcript_events", r.transcript_events_restored);
  add("synthesized_sessions", r.synthesized_sessions);
  if (parts.length === 0) {
    return "restored (0 new rows — every section was already in the store)";
  }
  return `restored: ${parts.join(", ")}`;
}
