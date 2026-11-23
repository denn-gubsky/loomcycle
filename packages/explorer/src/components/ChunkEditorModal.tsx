import { lazy, Suspense, useEffect, useMemo, useState, type FormEvent } from "react";
import type { EditorDiagnostic, EditorLanguage, ParseResult } from "@loomcycle/code-editor";
import "@loomcycle/code-editor/styles.css";
import type { BrowseScope, ChunkDetail, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import MermaidDiagram from "./Mermaid";
import { readImageAsBase64 } from "../lib/imageUpload";

// The shared code editor (RFC FW), loaded when a chunk is first edited: the
// reading view never fetches it. CodeEditor draws a plain textarea over the same
// text until CodeMirror itself has loaded, so the dialog is never blank.
const CodeEditor = lazy(() => import("@loomcycle/code-editor").then((m) => ({ default: m.CodeEditor })));
let parseJsonLoad: Promise<(text: string) => ParseResult> | null = null;
function loadParseJson() {
  parseJsonLoad ??= import("@loomcycle/code-editor").then((m) => m.parseJson);
  parseJsonLoad.catch(() => {
    parseJsonLoad = null;
  });
  return parseJsonLoad;
}

// ChunkEditorModal edits one chunk's content — title, type, status, tags, the
// Markdown body, and (optional) typed fields as JSON. The body and the fields
// are edited in the shared code editor: the body in its markdown mode with soft
// wrap (a Mermaid chunk's source in text mode, beside its live preview), the
// fields in json mode with the parse error marked where it is. Save is an
// optimistic-concurrency update_chunk carrying the chunk's revision; a stale
// revision returns a "revision conflict" the modal surfaces with a reload hint.
// Structural edits (reparent / reorder / link / delete) are NOT here.
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
  // Tags (RFC BS) edit as a comma-separated list; the editor is authoritative, so
  // save always replace-sets to this value (empty clears). Nested tags use a slash.
  const [tagsText, setTagsText] = useState((chunk.tags ?? []).join(", "));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);

  const isMermaid = type === "mermaid";
  const isImage = type === "image" || !!chunk.asset;
  const bodyLanguage: EditorLanguage = isMermaid || isImage ? "text" : "markdown";

  // RFC BO — a debounced copy of the body drives the live Mermaid preview so it
  // re-renders as the author edits the source, but not on every keystroke.
  const [preview, setPreview] = useState(body);
  useEffect(() => {
    if (!isMermaid) return;
    const t = setTimeout(() => setPreview(body), 400);
    return () => clearTimeout(t);
  }, [body, isMermaid]);

  // The fields' parse error, marked in the editor and blocking save. Any JSON
  // value is accepted (as JSON.parse takes it); the editor's own parser is asked
  // only where it fails, for the place and a clearer message.
  const [parseJson, setParseJson] = useState<((text: string) => ParseResult) | null>(null);
  useEffect(() => {
    let cancelled = false;
    loadParseJson()
      .then((fn) => !cancelled && setParseJson(() => fn))
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);
  const fieldsError = useMemo<EditorDiagnostic | null>(() => {
    if (fieldsText.trim() === "") return null;
    try {
      JSON.parse(fieldsText);
      return null;
    } catch (e) {
      const r = parseJson?.(fieldsText);
      if (r && !r.ok) return { from: r.error.offset, to: r.error.to, severity: "error", message: r.error.message };
      return { from: 0, to: 0, severity: "error", message: e instanceof Error ? e.message : String(e) };
    }
  }, [fieldsText, parseJson]);
  const fieldsDiagnostics = useMemo(() => (fieldsError ? [fieldsError] : []), [fieldsError]);

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
    if (fieldsError) {
      setErr(`fields must be valid JSON: ${fieldsError.message}`);
      return;
    }
    const patch: { body: string; title: string; type: string; status: string; tags: string[]; fields?: unknown } = {
      body,
      title,
      type,
      status,
      // Parse the comma-separated list; replace-set is intentional (the box shows
      // the chunk's tags, so saving makes it exactly what's in the box).
      tags: tagsText.split(",").map((t) => t.trim()).filter((t) => t !== ""),
    };
    // Send fields only when non-empty (a blank box leaves fields untouched
    // rather than clobbering them).
    if (fieldsText.trim() !== "") patch.fields = JSON.parse(fieldsText);
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
            <button type="submit" className="primary" disabled={busy || !!fieldsError}>
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
          <label className="path-field">
            <span>Tags</span>
            <input
              className="path-modal-input"
              value={tagsText}
              placeholder="area/sub, priority/high"
              title="Comma-separated; nest with a slash: area/sub, priority/high"
              onChange={(e) => setTagsText(e.target.value)}
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
        <div className={`chunk-editor-main${isMermaid ? " with-preview" : ""}`}>
          <div className="path-field chunk-editor-body">
            <span>{bodyLabel}</span>
            <Suspense fallback={<textarea className="modal-textarea" value={body} readOnly aria-label={bodyLabel} />}>
              <CodeEditor
                value={body}
                onChange={setBody}
                language={bodyLanguage}
                wrap
                ariaLabel={bodyLabel}
                className="chunk-body-input"
              />
            </Suspense>
          </div>
          {isMermaid && (
            <div className="path-field chunk-editor-preview">
              <span>Preview</span>
              <div className="chunk-mermaid-preview">
                {preview.trim() ? (
                  <MermaidDiagram code={preview} />
                ) : (
                  <p className="doc-empty-body">(enter Mermaid source)</p>
                )}
              </div>
            </div>
          )}
        </div>
        <div className="path-field chunk-editor-fields">
          <span>Fields (JSON, optional)</span>
          <Suspense
            fallback={<textarea className="modal-textarea" value={fieldsText} readOnly aria-label="Fields (JSON)" />}
          >
            <CodeEditor
              value={fieldsText}
              onChange={setFieldsText}
              language="json"
              diagnostics={fieldsDiagnostics}
              ariaLabel="Fields (JSON)"
              placeholder="{}"
              className="chunk-fields-input"
            />
          </Suspense>
          {fieldsError && <span className="chunk-fields-error">{fieldsError.message}</span>}
        </div>
      </form>
    </div>
  );
}
