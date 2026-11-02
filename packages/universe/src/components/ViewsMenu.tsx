import { useEffect, useRef, useState } from "react";
import type { SavedUniverseView } from "../lib/views";

// ViewsMenu is the toolbar's saved-views popover: the scope's universe views to
// open or delete, and a box to save what is on screen now under a title. It
// only calls back; the navigator owns the data layer and the map's state.

export default function ViewsMenu({
  load,
  onOpen,
  onSave,
  onDelete,
}: {
  load: () => Promise<SavedUniverseView[]>;
  onOpen: (v: SavedUniverseView) => void;
  onSave: (title: string) => Promise<void>;
  onDelete: (v: SavedUniverseView) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [views, setViews] = useState<SavedUniverseView[] | null>(null);
  const [title, setTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const root = useRef<HTMLDivElement | null>(null);

  const refresh = async () => {
    setError(null);
    try {
      setViews(await load());
    } catch (e) {
      setViews([]);
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  useEffect(() => {
    if (!open) return;
    void refresh();
    const onDown = (e: MouseEvent) => {
      if (root.current && !root.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
    // refresh closes over load, which the navigator memoizes on its scope.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const run = async (f: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await f();
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="lu-views" ref={root}>
      <button
        type="button"
        aria-expanded={open}
        className={open ? "active" : undefined}
        onClick={() => setOpen((o) => !o)}
        title="Saved views of this map"
      >
        views
      </button>
      {open && (
        <div className="lu-views__pop" role="menu">
          <form
            className="lu-views__save"
            onSubmit={(e) => {
              e.preventDefault();
              const t = title.trim();
              if (t) void run(async () => {
                await onSave(t);
                setTitle("");
              });
            }}
          >
            <input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Save this view as…"
              aria-label="Title for the saved view"
            />
            <button type="submit" disabled={busy || !title.trim()}>
              save
            </button>
          </form>
          {error && <div className="lu-views__error">{error}</div>}
          {views === null ? (
            <div className="lu-views__empty">Loading…</div>
          ) : views.length === 0 ? (
            <div className="lu-views__empty">No saved views in this scope.</div>
          ) : (
            <ul>
              {views.map((v) => (
                <li key={v.documentId}>
                  <button
                    type="button"
                    className="lu-views__open"
                    onClick={() => {
                      onOpen(v);
                      setOpen(false);
                    }}
                    title={`${v.state.mode} view${v.state.type ? ` · ${v.state.type}` : ""}${v.state.status ? ` · ${v.state.status}` : ""}`}
                  >
                    {v.title || v.documentId}
                  </button>
                  <button
                    type="button"
                    className="lu-views__delete"
                    disabled={busy}
                    onClick={() => void run(() => onDelete(v))}
                    title="Delete this saved view"
                    aria-label={`Delete ${v.title}`}
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
