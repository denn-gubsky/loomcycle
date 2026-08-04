import { useEffect, useState } from "react";
import type { BrowseScope, ChunkRevision, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";

// HistoryModal is the RFC BS read-only edit-history surface for one chunk. It
// lists the chunk's revisions (number + time + actor); clicking a revision shows
// that version's body verbatim, and a from→to picker renders a unified diff
// between any two. Restore is deliberately NOT here (this pass is read-only) —
// mirroring ChunkEditorModal, which owns the write path.
export interface HistoryModalProps {
  chunkId: string;
  chunkTitle?: string;
  scope: DocScope;
  // browse (RFC AS) — read the history under the browsed subject's document.
  browse?: BrowseScope;
  onClose: () => void;
}

// Which output pane is showing: a single version's body, or a two-revision diff.
type OutputMode = "none" | "version" | "diff";

// formatTs renders a revision timestamp. `created_at` is Unix NANOSECONDS (RFC BS
// history), so divide by 1e6 for a JS Date; a stray string is tolerated via
// Date.parse. Falls back to the raw value when it isn't a usable instant.
function formatTs(createdAt: number): string {
  const ms = typeof createdAt === "number" ? createdAt / 1e6 : Date.parse(String(createdAt));
  if (!Number.isFinite(ms) || ms <= 0) return String(createdAt ?? "");
  return new Date(ms).toLocaleString();
}

export default function HistoryModal({
  chunkId,
  chunkTitle,
  scope,
  browse,
  onClose,
}: HistoryModalProps) {
  const data = useExplorerData();
  const [revisions, setRevisions] = useState<ChunkRevision[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const [mode, setMode] = useState<OutputMode>("none");
  const [selectedRev, setSelectedRev] = useState<number | null>(null);
  const [body, setBody] = useState<string>("");
  const [busy, setBusy] = useState(false); // a version/diff fetch is in flight

  const [fromRev, setFromRev] = useState<number | null>(null);
  const [toRev, setToRev] = useState<number | null>(null);
  const [diff, setDiff] = useState<string>("");

  // Load the revision list on open. Default the diff picker to the full span
  // (oldest → newest) so a single "diff" click is meaningful without fiddling.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    data
      .documentHistory(chunkId, scope, browse)
      .then((r) => {
        if (cancelled) return;
        const revs = r.revisions ?? [];
        setRevisions(revs);
        if (revs.length >= 2) {
          const nums = revs.map((x) => x.revision);
          setFromRev(Math.min(...nums));
          setToRev(Math.max(...nums));
        }
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [data, chunkId, scope, browse]);

  const viewVersion = async (rev: number) => {
    setErr(null);
    setBusy(true);
    setSelectedRev(rev);
    setMode("version");
    try {
      const r = await data.documentGetVersion(chunkId, rev, scope, browse);
      setBody(r.body ?? "");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const runDiff = async () => {
    if (fromRev === null || toRev === null) return;
    setErr(null);
    setBusy(true);
    setMode("diff");
    try {
      const r = await data.documentDiff(chunkId, fromRev, toRev, scope, browse);
      setDiff(r.diff ?? "");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal history-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header sticky-top">
          <h3>History{chunkTitle ? ` — ${chunkTitle}` : ""}</h3>
          <div className="modal-buttons modal-buttons-top">
            <button type="button" onClick={onClose}>
              close
            </button>
          </div>
        </div>
        {err && <div className="modal-err">{err}</div>}

        {loading ? (
          <p className="doc-empty-body">loading…</p>
        ) : revisions.length === 0 ? (
          <p className="doc-empty-body">No history recorded for this chunk.</p>
        ) : (
          <>
            <div className="doc-hist-revs">
              {revisions.map((rev) => (
                <button
                  key={rev.revision}
                  type="button"
                  className={
                    "doc-hist-rev" +
                    (mode === "version" && selectedRev === rev.revision ? " active" : "")
                  }
                  onClick={() => void viewVersion(rev.revision)}
                >
                  <span className="doc-hist-rev-num">rev {rev.revision}</span>
                  <span className="doc-hist-rev-time">{formatTs(rev.created_at)}</span>
                  <span className="doc-hist-rev-actor">{rev.actor || "unknown"}</span>
                </button>
              ))}
            </div>

            {revisions.length >= 2 && (
              <div className="doc-hist-diffbar">
                <span>Compare</span>
                <select
                  value={fromRev ?? ""}
                  onChange={(e) => setFromRev(e.target.value === "" ? null : Number(e.target.value))}
                  aria-label="diff from revision"
                >
                  {revisions.map((r) => (
                    <option key={r.revision} value={r.revision}>
                      rev {r.revision}
                    </option>
                  ))}
                </select>
                <span aria-hidden>→</span>
                <select
                  value={toRev ?? ""}
                  onChange={(e) => setToRev(e.target.value === "" ? null : Number(e.target.value))}
                  aria-label="diff to revision"
                >
                  {revisions.map((r) => (
                    <option key={r.revision} value={r.revision}>
                      rev {r.revision}
                    </option>
                  ))}
                </select>
                <button
                  type="button"
                  onClick={() => void runDiff()}
                  disabled={busy || fromRev === null || toRev === null}
                >
                  diff
                </button>
              </div>
            )}

            <div className="doc-hist-output">
              {busy ? (
                <p className="doc-empty-body">loading…</p>
              ) : mode === "diff" ? (
                <DiffView text={diff} />
              ) : mode === "version" ? (
                <pre className="md-pre doc-hist-version">
                  <code>{body || "(empty)"}</code>
                </pre>
              ) : (
                <p className="doc-empty-body">
                  Select a revision to view it, or compare two.
                </p>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// DiffView renders a unified diff, coloring added (+) / removed (-) / hunk (@@)
// lines. `+++`/`---` file headers stay neutral so they don't read as content
// changes. Line breaks come from the explicit "\n" text nodes inside the <pre>,
// so spacing stays single (a per-line <div> would double it).
function DiffView({ text }: { text: string }) {
  if (!text.trim()) return <p className="doc-empty-body">(no differences)</p>;
  const lines = text.split("\n");
  return (
    <pre className="md-pre doc-hist-diff">
      {lines.map((ln, i) => {
        const cls =
          ln.startsWith("+") && !ln.startsWith("+++")
            ? "diff-add"
            : ln.startsWith("-") && !ln.startsWith("---")
              ? "diff-del"
              : ln.startsWith("@@")
                ? "diff-hunk"
                : "";
        return (
          <span key={i} className={cls}>
            {ln}
            {"\n"}
          </span>
        );
      })}
    </pre>
  );
}
