import { useMemo, useState, type FormEvent } from "react";
import type { BrowseScope, DocScope } from "../types";
import type { ExplorerDataLayer } from "../lib/dataLayer";
import {
  transferBlocked,
  transferInput,
  transferProblem,
  transferSummary,
  type TransferOp,
  type TransferRequest,
  type TransferResult,
} from "../lib/transfer";

export interface TransferModalProps {
  data: ExplorerDataLayer;
  scope: DocScope;
  browse?: BrowseScope;
  // What is being copied or moved: one document by id, or a folder's documents.
  documentId?: string;
  underPath?: string;
  // The selected path, the prefix the new path replaces.
  fromPath: string;
  onClose: () => void;
  // Called after a real copy or move, with what was asked and the answer.
  onDone: (req: TransferRequest, res: TransferResult) => void;
}

// TransferModal copies or moves a document, or every document under a folder,
// into another scope. It always runs the dry run first and shows what would
// happen, what is refused and which relations leave the set; the real call is
// offered only for the inputs that dry run checked, and a move passes the dry
// run's count so a set that changed since is refused rather than moved.
export default function TransferModal({
  data,
  scope,
  browse,
  documentId,
  underPath,
  fromPath,
  onClose,
  onDone,
}: TransferModalProps) {
  const [op, setOp] = useState<TransferOp>("copy");
  const [toScope, setToScope] = useState<DocScope>(scope === "tenant" ? "user" : "tenant");
  const [to, setTo] = useState(fromPath);
  const [dropDangling, setDropDangling] = useState(false);
  const [checked, setChecked] = useState<{ key: string; res: TransferResult } | null>(null);
  const [done, setDone] = useState<TransferResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const req = useMemo<TransferRequest>(
    () => ({ op, scope, toScope, documentId, underPath, from: fromPath, to, dropDangling, dryRun: true }),
    [op, scope, toScope, documentId, underPath, fromPath, to, dropDangling],
  );
  const key = JSON.stringify(transferInput(req));
  const problem = transferProblem(req);
  const current = checked && checked.key === key ? checked.res : null;
  const blocked = current ? transferBlocked(op, current, dropDangling) : false;
  const n = current ? (op === "move" ? (current.counts?.documents ?? 0) + (current.to_finish?.length ?? 0) : current.counts?.documents ?? 0) : 0;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (done) {
      onClose();
      return;
    }
    if (problem || !data.documentsTransfer) return;
    setBusy(true);
    setErr(null);
    try {
      if (!current) {
        setChecked({ key, res: await data.documentsTransfer(req, browse) });
      } else {
        const real: TransferRequest = { ...req, dryRun: false, expectCount: current.counts?.documents ?? 0 };
        const res = await data.documentsTransfer(real, browse);
        setDone(res);
        onDone(real, res);
      }
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
    } finally {
      setBusy(false);
    }
  };

  const label = done
    ? "close"
    : busy
      ? "working…"
      : !current
        ? "check"
        : `${op} ${n} document${n === 1 ? "" : "s"}`;
  const shown = done ?? current;
  const what = documentId ? "document" : "documents under";

  return (
    <div className="modal-overlay" onClick={busy ? undefined : onClose}>
      <form className="modal" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <div className="modal-header sticky-top">
          <h3>
            Copy or move the {what} {fromPath}
          </h3>
          <div className="modal-buttons modal-buttons-top">
            {!done && (
              <button type="button" onClick={onClose} disabled={busy}>
                cancel
              </button>
            )}
            <button
              type="submit"
              className={current && op === "move" && !done ? "danger" : "primary"}
              disabled={busy || (!done && (!!problem || blocked))}
            >
              {label}
            </button>
          </div>
        </div>
        <p className="modal-context">
          A copy gets new ids and leaves the source as it is. A move keeps every id and removes the
          documents from <code>{scope}</code>. Relations between the documents go with them.
        </p>
        <div className="paths-nav-switch" role="group" aria-label="Copy or move">
          {(["copy", "move"] as const).map((o) => (
            <button
              key={o}
              type="button"
              aria-pressed={op === o}
              className={op === o ? "active" : undefined}
              disabled={busy || !!done}
              onClick={() => setOp(o)}
            >
              {o}
            </button>
          ))}
        </div>
        <label className="path-field">
          <span>To scope</span>
          <select value={toScope} disabled={busy || !!done} onChange={(e) => setToScope(e.target.value as DocScope)}>
            <option value="user">user</option>
            <option value="agent">agent</option>
            <option value="tenant">tenant</option>
          </select>
        </label>
        <label className="path-field">
          <span>New path</span>
          <input
            className="path-modal-input"
            value={to}
            disabled={busy || !!done}
            onChange={(e) => setTo(e.target.value)}
          />
        </label>
        {op === "move" && (
          <label className="path-field">
            <span>
              <input
                type="checkbox"
                checked={dropDangling}
                disabled={busy || !!done}
                onChange={(e) => setDropDangling(e.target.checked)}
              />{" "}
              Drop relations to documents outside the set
            </span>
          </label>
        )}
        {problem && <div className="modal-err">{problem}</div>}
        {shown && (
          <div className="modal-context" aria-live="polite">
            {transferSummary(op, shown).map((line, i) =>
              line.startsWith("  ") ? (
                <div key={i}>
                  <code>{line.trim()}</code>
                </div>
              ) : (
                <div key={i}>{line}</div>
              ),
            )}
          </div>
        )}
        {err && <div className="modal-err">{err}</div>}
      </form>
    </div>
  );
}
