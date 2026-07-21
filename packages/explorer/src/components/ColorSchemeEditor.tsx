import { useEffect, useMemo, useState } from "react";
import type { BrowseScope, ChunkDetail, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import { DEFAULT_SCHEME, type ColorScheme } from "../lib/colorScheme";

// ColorSchemeEditor edits a document's RFC BN color settings — the color_enabled
// flag + the per-(doc-type/status) and per-(chunk-status) palette — persisted in
// the document's ROOT chunk fields. It fetches the root chunk on open (for the
// revision + any sibling fields it must preserve) and writes back with
// update_chunk. A "copy to all documents of this type" action stamps the same
// palette onto every same-type document in scope, so a tree of like documents
// stays visually consistent without per-doc editing.
export interface ColorSchemeEditorProps {
  documentId: string;
  rootChunkId: string;
  docType?: string;
  docStatus?: string;
  // chunkStatuses — the distinct statuses present in this document's chunks, so
  // the editor offers a color for each one actually in use (plus the defaults).
  chunkStatuses: string[];
  scope: DocScope;
  browse?: BrowseScope;
  onClose: () => void;
  // onSaved fires after a successful save so the viewer reloads its color meta +
  // re-tints the tree.
  onSaved: () => void;
}

// normalizeHex coerces a scheme color to the #rrggbb an <input type="color">
// requires (it rejects #rgb and names); a bad value falls back to neutral grey.
function normalizeHex(v: string | undefined): string {
  const m = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec((v ?? "").trim());
  if (!m) return "#888888";
  let h = m[1];
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  return "#" + h.toLowerCase();
}

export default function ColorSchemeEditor({
  documentId,
  rootChunkId,
  docType,
  docStatus,
  chunkStatuses,
  scope,
  browse,
  onClose,
  onSaved,
}: ColorSchemeEditorProps) {
  const data = useExplorerData();
  const [root, setRoot] = useState<ChunkDetail | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [scheme, setScheme] = useState<ColorScheme>({});
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  // The keys the editor exposes: the defaults, plus this document's own doc key
  // and every chunk-status key actually in use — so a document with a bespoke
  // type/status still gets a picker. Stable + grouped (doc.* then chunk.*).
  const keys = useMemo(() => {
    const set = new Set<string>(Object.keys(DEFAULT_SCHEME));
    if (docType) set.add(`doc.${docType}.${docStatus || "draft"}`);
    for (const s of chunkStatuses) if (s) set.add(`chunk.${s}`);
    return [...set].sort();
  }, [docType, docStatus, chunkStatuses]);

  // Load the root chunk once for its revision + existing fields + stored scheme.
  useEffect(() => {
    let cancelled = false;
    data
      .documentGetChunk(rootChunkId, scope, browse)
      .then((d) => {
        if (cancelled) return;
        setRoot(d);
        const f = (d.fields ?? {}) as Record<string, unknown>;
        setEnabled(f.color_enabled === true);
        const stored = (f.color_scheme as ColorScheme) ?? {};
        // Seed from defaults so every picker shows a sensible starting color,
        // with the document's stored overrides on top.
        setScheme({ ...DEFAULT_SCHEME, ...stored });
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [data, rootChunkId, scope, browse]);

  const setColor = (key: string, hex: string) =>
    setScheme((prev) => ({ ...prev, [key]: hex }));

  // fieldsWith merges the color settings into an existing chunk's fields, so a
  // save never clobbers other typed fields the root chunk carries.
  const fieldsWith = (existing: unknown): Record<string, unknown> => ({
    ...((existing ?? {}) as Record<string, unknown>),
    color_enabled: enabled,
    color_scheme: scheme,
  });

  const save = async () => {
    if (!root) return;
    setBusy(true);
    setErr(null);
    setStatus(null);
    try {
      await data.documentUpdateChunk(
        rootChunkId,
        root.revision,
        { fields: fieldsWith(root.fields) },
        scope,
        browse,
      );
      onSaved();
      onClose();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  // copyToType stamps THIS palette onto every OTHER document of the same type in
  // scope (best-effort; reports how many were updated). Each target is read for
  // its own root revision + fields so the write is a safe merge.
  const copyToType = async () => {
    if (!docType) {
      setErr("this document has no type to match");
      return;
    }
    setBusy(true);
    setErr(null);
    setStatus(null);
    try {
      const { documents } = await data.documentsSummary({ underPath: "/" }, scope, browse);
      const targets = (documents ?? []).filter(
        (d) => d.type === docType && d.document_id !== documentId && d.root_chunk_id,
      );
      let applied = 0;
      for (const t of targets) {
        try {
          const tr = await data.documentGetChunk(t.root_chunk_id as string, scope, browse);
          await data.documentUpdateChunk(
            t.root_chunk_id as string,
            tr.revision,
            { fields: fieldsWith(tr.fields) },
            scope,
            browse,
          );
          applied++;
        } catch {
          // Skip a target that failed (revision race / gone); keep going.
        }
      }
      setStatus(`Applied to ${applied} other ${docType} document(s).`);
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const docKeys = keys.filter((k) => k.startsWith("doc."));
  const chunkKeys = keys.filter((k) => k.startsWith("chunk."));

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal scheme-editor" onClick={(e) => e.stopPropagation()}>
        <div className="scheme-editor-head">
          <h3>Document colors</h3>
          <div className="scheme-editor-actions">
            <button type="button" onClick={onClose} disabled={busy}>
              cancel
            </button>
            <button type="button" className="primary" onClick={() => void save()} disabled={busy || !root}>
              {busy ? "saving…" : "save"}
            </button>
          </div>
        </div>
        {err && <div className="modal-err">{err}</div>}
        {!root ? (
          <p className="modal-context">Loading…</p>
        ) : (
          <>
            <label className="scheme-toggle">
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              <span>Enable coloring for this document</span>
            </label>
            <p className="modal-context">
              Document rows are tinted by type + status; chunk tiles by status. Colors apply as a
              semi-transparent wash so text stays readable.
            </p>

            <div className="scheme-group">
              <h4>Document (type · status)</h4>
              {docKeys.map((k) => (
                <ColorRow key={k} label={k.slice("doc.".length)} value={scheme[k]} onChange={(hex) => setColor(k, hex)} />
              ))}
            </div>
            <div className="scheme-group">
              <h4>Chunks (status)</h4>
              {chunkKeys.map((k) => (
                <ColorRow key={k} label={k.slice("chunk.".length)} value={scheme[k]} onChange={(hex) => setColor(k, hex)} />
              ))}
            </div>

            <div className="scheme-editor-foot">
              <button type="button" onClick={() => void copyToType()} disabled={busy || !docType} title={docType ? `Apply this palette to every other ${docType} document` : "This document has no type"}>
                copy to all {docType || "same-type"} documents
              </button>
              {status && <span className="scheme-status">{status}</span>}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function ColorRow({ label, value, onChange }: { label: string; value: string | undefined; onChange: (hex: string) => void }) {
  const hex = normalizeHex(value);
  return (
    <label className="scheme-row">
      <input type="color" value={hex} onChange={(e) => onChange(e.target.value)} />
      <span className="scheme-key">{label}</span>
      <code className="scheme-hex">{hex}</code>
    </label>
  );
}
