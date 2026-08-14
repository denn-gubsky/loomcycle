import { useEffect, useRef, useState } from "react";
import type { BoardScope, DocRow } from "../types";
import { useLoomboardData } from "../lib/dataLayer";

// SavedViewList is the left rail: every `type=view` Document in the scope, plus
// a "New view" affordance. `reloadKey` bumps to re-list after a save / delete.
export default function SavedViewList({
  scope,
  selectedId,
  onSelect,
  onNew,
  reloadKey,
  onError,
}: {
  scope: BoardScope;
  selectedId: string | null;
  onSelect: (row: DocRow) => void;
  onNew: () => void;
  reloadKey: number;
  onError?: (e: unknown) => void;
}) {
  const data = useLoomboardData();
  const [views, setViews] = useState<DocRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  useEffect(() => {
    let cancelled = false;
    setViews(null);
    setErr(null);
    (async () => {
      try {
        const r = await data.listViews(scope);
        if (!cancelled) setViews(r.documents ?? []);
      } catch (e) {
        if (cancelled) return;
        setErr(e instanceof Error ? e.message : String(e));
        onErrorRef.current?.(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [data, scope, reloadKey]);

  return (
    <div className="view-list">
      <div className="view-list-head">
        <span className="view-list-title">Views</span>
        <button type="button" className="view-new" onClick={onNew} title="New view">
          + New
        </button>
      </div>
      {err ? (
        <div className="view-list-error">{err}</div>
      ) : views === null ? (
        <div className="view-list-loading">Loading…</div>
      ) : views.length === 0 ? (
        <div className="view-list-empty">No saved views yet.</div>
      ) : (
        <ul className="view-list-items">
          {views.map((v) => (
            <li key={v.document_id}>
              <button
                type="button"
                className={v.document_id === selectedId ? "view-item on" : "view-item"}
                onClick={() => onSelect(v)}
              >
                {v.title || "(untitled view)"}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
