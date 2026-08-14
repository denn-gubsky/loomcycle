import { useCallback, useState } from "react";
import type { BoardScope, DocRow, SavedView } from "../types";
import { useLoomboardData } from "../lib/dataLayer";
import { parseView } from "../lib/view";
import Splitter from "./Splitter";
import SavedViewList from "./SavedViewList";
import BoardView from "./BoardView";
import ViewEditor from "./ViewEditor";

type Mode = "empty" | "view" | "editor";

// LoomboardBody is the shell inside <LoomboardRoot>: a scope toggle + a splitter
// with the saved-view rail on the left and the active board / editor on the
// right. It owns the selection + edit state; every child reads the injected data
// layer via useLoomboardData().
export default function LoomboardBody({
  defaultScope = "user",
  onError,
}: {
  defaultScope?: BoardScope;
  onError?: (e: unknown) => void;
}) {
  const data = useLoomboardData();
  const [scope, setScope] = useState<BoardScope>(defaultScope);
  const [mode, setMode] = useState<Mode>("empty");
  const [active, setActive] = useState<SavedView | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const [err, setErr] = useState<string | null>(null);

  const openView = useCallback(
    async (row: DocRow) => {
      setErr(null);
      try {
        const root = await data.getChunk(scope, row.root_chunk_id);
        setActive(parseView(row.document_id, root));
        setMode("view");
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
        onError?.(e);
      }
    },
    [data, scope, onError],
  );

  const onDelete = useCallback(async () => {
    if (!active) return;
    setErr(null);
    try {
      await data.deleteView(scope, active.documentId);
      setActive(null);
      setMode("empty");
      setReloadKey((k) => k + 1);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      onError?.(e);
    }
  }, [data, scope, active, onError]);

  return (
    <div className="loomboard-body">
      <div className="loomboard-toolbar">
        <div className="scope-toggle" role="tablist" aria-label="Scope">
          {(["user", "tenant"] as const).map((s) => (
            <button
              key={s}
              type="button"
              role="tab"
              aria-selected={scope === s}
              className={scope === s ? "on" : ""}
              onClick={() => {
                setScope(s);
                setActive(null);
                setMode("empty");
              }}
            >
              {s}
            </button>
          ))}
        </div>
        {mode === "view" && active && (
          <div className="loomboard-actions">
            <button type="button" className="btn-ghost" onClick={() => setMode("editor")}>
              Edit
            </button>
            <button type="button" className="btn-ghost danger" onClick={onDelete}>
              Delete
            </button>
          </div>
        )}
      </div>

      {err && <div className="loomboard-error">{err}</div>}

      <Splitter storageKey="loomboard.split" defaultLeftWidth={260} className="loomboard-split">
        <SavedViewList
          scope={scope}
          selectedId={active?.documentId ?? null}
          onSelect={openView}
          onNew={() => {
            setMode("editor");
          }}
          reloadKey={reloadKey}
          onError={onError}
        />
        <div className="loomboard-main">
          {mode === "editor" ? (
            <ViewEditor
              scope={scope}
              existing={active ?? undefined}
              onSaved={() => {
                setMode(active ? "view" : "empty");
                setReloadKey((k) => k + 1);
              }}
              onCancel={() => setMode(active ? "view" : "empty")}
              onError={onError}
            />
          ) : active ? (
            <BoardView view={active} scope={scope} onError={onError} />
          ) : (
            <div className="loomboard-placeholder">
              Select a view, or create one with <strong>+ New</strong>.
            </div>
          )}
        </div>
      </Splitter>
    </div>
  );
}
