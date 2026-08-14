import { useRef, useState, type FormEvent } from "react";
import type {
  BoardScope,
  GroupAxis,
  LayoutKind,
  SavedView,
  SortField,
  ViewLayout,
  ViewQuery,
} from "../types";
import { useLoomboardData } from "../lib/dataLayer";

// ViewEditor creates or edits a saved view — the query builder (the RFC BS axes:
// source / type / status / tag / under_path) + the layout picker. On save it
// persists a `type=view` Document (create) or patches its fields (edit).
export default function ViewEditor({
  scope,
  existing,
  onSaved,
  onCancel,
  onError,
}: {
  scope: BoardScope;
  existing?: SavedView;
  onSaved: () => void;
  onCancel: () => void;
  onError?: (e: unknown) => void;
}) {
  const data = useLoomboardData();
  const q0 = existing?.query;
  const l0 = existing?.layout;

  const [title, setTitle] = useState(existing?.title ?? "");
  const [source, setSource] = useState<"documents" | "chunks">(q0?.source ?? "documents");
  const [type, setType] = useState(q0?.type ?? "");
  const [status, setStatus] = useState(q0?.status ?? "");
  const [tag, setTag] = useState(q0?.tag ?? "");
  const [underPath, setUnderPath] = useState(q0?.underPath ?? "");
  const [documentId, setDocumentId] = useState(q0?.documentId ?? "");
  const [kind, setKind] = useState<LayoutKind>(l0?.kind ?? "table");
  const [groupBy, setGroupBy] = useState<GroupAxis>(l0?.groupBy ?? "status");
  const [sortBy, setSortBy] = useState<SortField>(l0?.sortBy ?? "updated");

  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  function buildQuery(): ViewQuery {
    const query: ViewQuery = { source };
    if (source === "chunks" && documentId.trim()) query.documentId = documentId.trim();
    if (underPath.trim()) query.underPath = underPath.trim();
    if (type.trim()) query.type = type.trim();
    if (status.trim()) query.status = status.trim();
    if (tag.trim()) query.tag = tag.trim();
    return query;
  }

  function buildLayout(): ViewLayout {
    return { kind, groupBy, sortBy, sortDir: "desc" };
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    if (!title.trim()) {
      setErr("A view needs a title.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      if (existing) {
        await data.updateView(scope, existing.rootChunkId, existing.revision ?? 1, {
          query: buildQuery(),
          layout: buildLayout(),
        });
      } else {
        await data.saveView(scope, { title: title.trim(), query: buildQuery(), layout: buildLayout() });
      }
      onSaved();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
      onErrorRef.current?.(e2);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="view-editor" onSubmit={onSubmit}>
      <div className="view-editor-head">{existing ? "Edit view" : "New view"}</div>

      <label className="field">
        <span>Title</span>
        <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="e.g. RFCs by status" />
      </label>

      <div className="field-row">
        <label className="field">
          <span>Source</span>
          <select value={source} onChange={(e) => setSource(e.target.value as "documents" | "chunks")}>
            <option value="documents">Documents</option>
            <option value="chunks">Chunks</option>
          </select>
        </label>
        <label className="field">
          <span>Layout</span>
          <select value={kind} onChange={(e) => setKind(e.target.value as LayoutKind)}>
            <option value="table">Table</option>
            <option value="cards">Cards</option>
            <option value="kanban">Kanban</option>
            <option value="list">List</option>
          </select>
        </label>
      </div>

      <div className="field-row">
        <label className="field">
          <span>Type</span>
          <input value={type} onChange={(e) => setType(e.target.value)} placeholder="e.g. rfc" />
        </label>
        <label className="field">
          <span>Status</span>
          <input value={status} onChange={(e) => setStatus(e.target.value)} placeholder="e.g. draft" />
        </label>
      </div>

      <div className="field-row">
        <label className="field">
          <span>Tag</span>
          <input value={tag} onChange={(e) => setTag(e.target.value)} placeholder="e.g. area/ui" />
        </label>
        <label className="field">
          <span>Under path</span>
          <input value={underPath} onChange={(e) => setUnderPath(e.target.value)} placeholder="/loomcycle/rfcs" />
        </label>
      </div>

      {source === "chunks" && (
        <label className="field">
          <span>Document id (chunks source)</span>
          <input value={documentId} onChange={(e) => setDocumentId(e.target.value)} placeholder="document_id" />
        </label>
      )}

      {kind === "kanban" && (
        <label className="field">
          <span>Group by</span>
          <select value={groupBy} onChange={(e) => setGroupBy(e.target.value as GroupAxis)}>
            <option value="status">Status</option>
            <option value="type">Type</option>
          </select>
        </label>
      )}

      <label className="field">
        <span>Sort by</span>
        <select value={sortBy} onChange={(e) => setSortBy(e.target.value as SortField)}>
          <option value="updated">Updated</option>
          <option value="title">Title</option>
          <option value="position">Position</option>
        </select>
      </label>

      {err && <div className="view-editor-error">{err}</div>}

      <div className="view-editor-actions">
        <button type="button" className="btn-ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="submit" className="btn-primary" disabled={busy}>
          {busy ? "Saving…" : existing ? "Save" : "Create"}
        </button>
      </div>
    </form>
  );
}
