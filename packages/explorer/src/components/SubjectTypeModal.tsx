import { useEffect, useState, type FormEvent } from "react";
import type { BrowseScope, ChunkDetail, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import { subjectTypeChoices } from "../lib/subjectType";

// SubjectTypeModal changes the kind of a subject: the type of its document's root
// chunk, picked from the entity types in force. It is an update_chunk like any other
// edit; the runtime tells subscribers the subject was re-typed, so relations whose
// kind no longer fits get re-checked. The subject's natural key does not change.
export interface SubjectTypeModalProps {
  chunk: ChunkDetail;
  scope: DocScope;
  browse?: BrowseScope;
  onClose: () => void;
  onSaved: (updated: ChunkDetail) => void;
}

export default function SubjectTypeModal({ chunk, scope, browse, onClose, onSaved }: SubjectTypeModalProps) {
  const data = useExplorerData();
  const [inForce, setInForce] = useState<string[] | null>(null);
  const [choice, setChoice] = useState(chunk.type ?? "");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Set when the type was changed but the runtime withheld the announcement.
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    data
      .documentEntityTypes?.(scope, browse)
      .then((types) => {
        if (!cancelled) setInForce(types);
      })
      .catch((e) => {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [data, scope, browse]);

  const choices = inForce ? subjectTypeChoices(inForce, chunk.type) : [];
  const unchanged = choice.toLowerCase() === (chunk.type ?? "").toLowerCase();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (unchanged || busy) return;
    setErr(null);
    setBusy(true);
    try {
      const updated = await data.documentUpdateChunk(chunk.id, chunk.revision, { type: choice }, scope, browse);
      onSaved(updated);
      if (updated.not_announced) {
        setNotice(updated.not_announced);
        setBusy(false);
        return;
      }
      onClose();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <form className="modal" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <h3>Change type</h3>
        <p className="modal-context">
          “{chunk.title || "(untitled)"}” is a subject (<code>{chunk.entity?.natural_key}</code>). Its key
          stays as it is; relations about it are re-checked against the new type.
        </p>
        {err && <div className="modal-err">{err}</div>}
        {notice ? (
          <div className="modal-err">The type was changed, but subscribers were not told: {notice}</div>
        ) : (
          <label className="path-field">
            <span>Type</span>
            {inForce === null ? (
              <span className="doc-empty-body">{err ? "" : "loading types…"}</span>
            ) : (
              <select
                className="path-modal-input"
                value={choice}
                disabled={busy}
                onChange={(e) => setChoice(e.target.value)}
              >
                {choices.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            )}
          </label>
        )}
        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={busy}>
            {notice ? "close" : "cancel"}
          </button>
          {!notice && (
            <button type="submit" className="primary" disabled={busy || unchanged || inForce === null}>
              {busy ? "saving…" : "change type"}
            </button>
          )}
        </div>
      </form>
    </div>
  );
}
