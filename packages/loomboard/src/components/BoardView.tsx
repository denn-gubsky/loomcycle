import { useEffect, useRef, useState } from "react";
import type { BoardRow, BoardScope, SavedView } from "../types";
import { useLoomboardData } from "../lib/dataLayer";
import { docRowToBoardRow, chunkRowToBoardRow } from "../lib/view";
import { BoardLayout } from "./layouts";

// BoardView materializes a saved view's query (documents or chunks) and renders
// the chosen layout. Load state / errors are surfaced inline; a runtime refusal
// (e.g. SQL Memory not enabled) arrives as a 422 message, shown as a soft hint
// rather than a crash. It also forwards failures to the host's onError.
export default function BoardView({
  view,
  scope,
  onError,
}: {
  view: SavedView;
  scope: BoardScope;
  onError?: (e: unknown) => void;
}) {
  const data = useLoomboardData();
  const [rows, setRows] = useState<BoardRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  // Hold onError in a ref so an inline parent callback doesn't re-run the load.
  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  const sig = JSON.stringify(view.query);
  useEffect(() => {
    let cancelled = false;
    setRows(null);
    setErr(null);
    (async () => {
      try {
        let out: BoardRow[];
        if (view.query.source === "chunks") {
          const r = await data.queryChunks(scope, view.query);
          out = (r.chunks ?? []).map(chunkRowToBoardRow);
        } else {
          const r = await data.queryDocuments(scope, view.query);
          out = (r.documents ?? []).map(docRowToBoardRow);
        }
        if (!cancelled) setRows(out);
      } catch (e) {
        if (cancelled) return;
        setErr(e instanceof Error ? e.message : String(e));
        onErrorRef.current?.(e);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, scope, view.rootChunkId, sig]);

  return (
    <div className="board-view">
      <div className="board-view-head">
        <span className="board-view-title">{view.title}</span>
        <span className="board-view-kind">{view.layout.kind}</span>
      </div>
      {err ? (
        <div className="board-error">{err}</div>
      ) : rows === null ? (
        <div className="board-loading">Loading…</div>
      ) : (
        <BoardLayout rows={rows} layout={view.layout} />
      )}
    </div>
  );
}
