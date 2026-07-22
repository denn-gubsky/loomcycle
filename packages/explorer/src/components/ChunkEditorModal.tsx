import { useEffect, useState, type FormEvent } from "react";
import type { BrowseScope, ChunkDetail, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import MermaidDiagram from "./Mermaid";
import { readImageAsBase64 } from "../lib/imageUpload";

// ChunkEditorModal edits one chunk's content — title, type, status, Markdown
// body, and (optional) typed fields as JSON. Save is an optimistic-concurrency
// update_chunk carrying the chunk's revision; a stale revision returns a
// "revision conflict" the modal surfaces with a reload hint. Structural edits
// (reparent / reorder / link / delete) are NOT here — those are the
// document-management agent's job.
export interface ChunkEditorModalProps {
  chunk: ChunkDetail;
  scope: DocScope;
  // browse (RFC AS) — edit a chunk under the browsed subject's document, not
  // necessarily the caller's own (server re-authorizes). Unset → own subject.
  browse?: BrowseScope;
  onClose: () => void;
  onSaved: (updated: ChunkDetail) => void;
}

export default function ChunkEditorModal({ chunk, scope, browse, onClose, onSaved }: ChunkEditorModalProps) {
  const data = useExplorerData();
  const [title, setTitle] = useState(chunk.title);
  const [type, setType] = useState(chunk.type ?? "");
  const [status, setStatus] = useState(chunk.status ?? "");
  const [body, setBody] = useState(chunk.body ?? "");
  const [fieldsText, setFieldsText] = useState(
    chunk.fields ? JSON.stringify(chunk.fields, null, 2) : "",
  );
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);

  const isMermaid = type === "mermaid";
  const isImage = type === "image" || !!chunk.asset;

  // RFC BO — a debounced copy of the body drives the live Mermaid preview so it
  // re-renders as the author edits the source, but not on every keystroke.
  const [preview, setPreview] = useState(body);
  useEffect(() => {
    if (!isMermaid) return;
    const t = setTimeout(() => setPreview(body), 400);
    return () => clearTimeout(t);
  }, [body, isMermaid]);

  // RFC BO — replace an image chunk's bytes in place (set_asset bumps the
  // revision, so we hand the updated chunk back and close rather than risk a
  // stale-revision save afterward).
  const replaceImage = async (file: File) => {
    setErr(null);
    setBusy(true);
    try {
      const { mediaType, data: b64 } = await readImageAsBase64(file);
      const updated = await data.documentSetAsset(chunk.id, mediaType, b64, file.name, scope, browse);
      onSaved(updated);
      onClose();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };

  const bodyLabel = isMermaid ? "Mermaid source" : isImage ? "Caption (optional)" : "Body (Markdown)";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    setConflict(false);
    const patch: { body: string; title: string; type: string; status: string; fields?: unknown } = {
      body,
      title,
      type,
      status,
    };
    // Send fields only when non-empty + valid JSON (a blank box leaves fields
    // untouched rather than clobbering them).
    if (fieldsText.trim() !== "") {
      try {
        patch.fields = JSON.parse(fieldsText);
      } catch {
        setErr("fields must be valid JSON");
        return;
      }
    }
    setBusy(true);
    try {
      const updated = await data.documentUpdateChunk(chunk.id, chunk.revision, patch, scope, browse);
      onSaved(updated);
      onClose();
    } catch (ex) {
      const msg = ex instanceof Error ? ex.message : String(ex);
      setErr(msg);
      setConflict(/revision conflict/i.test(msg));
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <form className="modal chunk-editor" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        {/* RFC BN P1: Save/Cancel pinned to the top so a long chunk body never
            needs scrolling to reach the save button. */}
        <div className="modal-header sticky-top">
          <h3>Edit chunk</h3>
          <div className="modal-buttons modal-buttons-top">
            <button type="button" onClick={onClose} disabled={busy}>
              cancel
            </button>
            <button type="submit" className="primary" disabled={busy}>
              {busy ? "saving…" : "save"}
            </button>
          </div>
        </div>
        {err && (
          <div className="modal-err">
            {err}
            {conflict && " — reopen the chunk to load the latest, then reapply your edit."}
          </div>
        )}
        <label className="path-field">
          <span>Title</span>
          <input className="path-modal-input" value={title} onChange={(e) => setTitle(e.target.value)} />
        </label>
        <div className="chunk-editor-row">
          <label className="path-field">
            <span>Type</span>
            <input
              className="path-modal-input"
              value={type}
              placeholder="(none)"
              onChange={(e) => setType(e.target.value)}
            />
          </label>
          <label className="path-field">
            <span>Status</span>
            <input
              className="path-modal-input"
              value={status}
              placeholder="(none)"
              onChange={(e) => setStatus(e.target.value)}
            />
          </label>
        </div>
        {isImage && (
          <label className="path-field">
            <span>Replace image</span>
            <input
              type="file"
              accept="image/png,image/jpeg,image/gif,image/webp"
              disabled={busy}
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) void replaceImage(f);
              }}
            />
          </label>
        )}
        <label className="path-field">
          <span>{bodyLabel}</span>
          <textarea
            className="modal-textarea chunk-body-input"
            rows={isMermaid ? 8 : 12}
            value={body}
            onChange={(e) => setBody(e.target.value)}
          />
        </label>
        {isMermaid && (
          <div className="path-field">
            <span>Preview</span>
            <div className="chunk-mermaid-preview">
              {preview.trim() ? (
                <MermaidDiagram code={preview} />
              ) : (
                <p className="doc-empty-body">(enter Mermaid source above)</p>
              )}
            </div>
          </div>
        )}
        <label className="path-field">
          <span>Fields (JSON, optional)</span>
          <textarea
            className="modal-textarea chunk-fields-input"
            rows={4}
            value={fieldsText}
            placeholder="{}"
            onChange={(e) => setFieldsText(e.target.value)}
          />
        </label>
      </form>
    </div>
  );
}
