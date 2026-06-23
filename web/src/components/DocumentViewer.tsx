import { useCallback, useEffect, useMemo, useState } from "react";
import {
  type ChunkDetail,
  type ChunkRow,
  type DocScope,
  documentExportMd,
  documentGetChunk,
  documentQueryChunks,
} from "../api";
import DocumentChunkTree, {
  buildChunkTree,
  findChunkNode,
  type ChunkNode,
} from "./DocumentChunkTree";
import ChunkEditorModal from "./ChunkEditorModal";
import DocumentAssistantPanel from "./DocumentAssistantPanel";
import Markdown from "./Markdown";

// DocumentViewer is the RFC AM Phase 2 read-mostly surface for one chunked-
// graph document. The chunk tree stays on the left for navigation; the right
// pane renders the SELECTED chunk in one of two views, toggled per selection:
//   - chunks   — the selected chunk's own body (structured, per-chunk).
//   - markdown — the selected chunk + its sub-chunks assembled into one Markdown
//                document. Selecting the ROOT chunk therefore renders the whole
//                document; selecting a section renders just that part.
// Structural editing (move / link / delete chunks) is deliberately NOT here —
// it is the document-management agent's job (Phase 3).
export interface DocumentViewerProps {
  documentId: string;
  scope: DocScope;
  // titleHint shows immediately (e.g. the Path-tree name) until the root chunk
  // title loads.
  titleHint?: string;
}

// View mode for the selected-chunk pane (not the whole document).
type Mode = "chunks" | "markdown";

export default function DocumentViewer({ documentId, scope, titleHint }: DocumentViewerProps) {
  const [chunks, setChunks] = useState<ChunkRow[]>([]);
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const [selectedDetail, setSelectedDetail] = useState<ChunkDetail | null>(null);
  const [mode, setMode] = useState<Mode>("chunks");
  const [subtreeMd, setSubtreeMd] = useState<string | null>(null);
  const [editing, setEditing] = useState<ChunkDetail | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [reload, setReload] = useState(0);
  const [assistantOpen, setAssistantOpen] = useState(false);

  // Stable refresh for the assistant panel (avoids re-firing its turn-boundary
  // effect every render).
  const refresh = useCallback(() => setReload((n) => n + 1), []);

  const tree = useMemo(() => buildChunkTree(chunks), [chunks]);
  const rootTitle = useMemo(() => {
    const root = chunks.find((c) => !c.parent_id);
    return root?.title || titleHint || "document";
  }, [chunks, titleHint]);
  const selectedIsRoot = useMemo(
    () => !!selectedDetail && !selectedDetail.parent_id,
    [selectedDetail],
  );

  // Load the chunk list when the document / scope / reload changes. Default the
  // selection to the root chunk.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    documentQueryChunks(documentId, scope)
      .then((resp) => {
        if (cancelled) return;
        const rows = resp.chunks ?? [];
        setChunks(rows);
        setSelectedId((cur) => cur ?? rows.find((c) => !c.parent_id)?.id);
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [documentId, scope, reload]);

  // Reset selection + view when the document changes.
  useEffect(() => {
    setSelectedId(undefined);
    setSelectedDetail(null);
    setSubtreeMd(null);
    setMode("chunks");
  }, [documentId, scope]);

  // Fetch the selected chunk's body.
  useEffect(() => {
    if (!selectedId) {
      setSelectedDetail(null);
      return;
    }
    let cancelled = false;
    documentGetChunk(selectedId, scope)
      .then((d) => !cancelled && setSelectedDetail(d))
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [selectedId, scope, reload]);

  // Assemble the selected chunk + its sub-chunks into one Markdown string for the
  // markdown view (fetches each descendant's body; heading depth is relative to
  // the selected chunk, so it reads as a standalone part — or the whole document
  // when the root is selected). Only runs while the markdown view is active.
  useEffect(() => {
    if (mode !== "markdown" || !selectedId) {
      setSubtreeMd(null);
      return;
    }
    const node = findChunkNode(tree, selectedId);
    if (!node) return;
    let cancelled = false;
    const items: { node: ChunkNode; depth: number }[] = [];
    const walk = (n: ChunkNode, depth: number) => {
      items.push({ node: n, depth });
      n.children.forEach((c) => walk(c, depth + 1));
    };
    walk(node, 0);
    Promise.all(items.map((it) => documentGetChunk(it.node.row.id, scope)))
      .then((details) => {
        if (cancelled) return;
        const md = items
          .map((it, idx) => {
            const lvl = Math.min(it.depth + 1, 6);
            const d = details[idx];
            return "#".repeat(lvl) + " " + it.node.row.title + (d.body ? "\n\n" + d.body : "") + "\n";
          })
          .join("\n");
        setSubtreeMd(md);
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [mode, selectedId, tree, scope, reload]);

  const onSelect = useCallback((n: ChunkNode) => setSelectedId(n.row.id), []);

  // Download the WHOLE document as canonical, round-trippable Markdown (with the
  // metadata comments), independent of the selected-chunk view above.
  const download = useCallback(async () => {
    try {
      const r = await documentExportMd(documentId, scope, true);
      const blob = new Blob([r.markdown], { type: "text/markdown" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = (r.title || "document").replace(/[^\w.-]+/g, "_") + ".md";
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [documentId, scope]);

  return (
    <div className="doc-viewer">
      <div className="doc-toolbar">
        <strong className="doc-title" title={rootTitle}>
          {rootTitle}
        </strong>
        <div className="doc-toolbar-actions">
          <button type="button" onClick={() => void download()} title="Download the whole document as Markdown (.md)">
            ↓ .md
          </button>
          <button
            type="button"
            className={assistantOpen ? "active" : ""}
            onClick={() => setAssistantOpen((o) => !o)}
            title="Document Assistant — instruct an agent to restructure/import/link chunks"
          >
            assistant
          </button>
          <button type="button" onClick={refresh} title="Reload">
            ↻
          </button>
        </div>
      </div>
      {err && <div className="paths-err">{err}</div>}
      <div className="doc-split">
        <div className="doc-chunktree">
          {loading && chunks.length === 0 ? (
            <div className="empty">
              <p>Loading…</p>
            </div>
          ) : (
            <DocumentChunkTree tree={tree} selectedId={selectedId} onSelect={onSelect} />
          )}
        </div>
        <div className="doc-content">
          {selectedDetail ? (
            <>
              <div className="doc-content-head">
                <h3>{selectedDetail.title || "(untitled)"}</h3>
                <div className="doc-content-meta">
                  {selectedDetail.type && <span className="chunk-badge">{selectedDetail.type}</span>}
                  {selectedDetail.status && (
                    <span className="chunk-badge chunk-status">{selectedDetail.status}</span>
                  )}
                  <span className="chunk-rev">rev {selectedDetail.revision}</span>
                </div>
                <div className="doc-content-controls">
                  {/* The chunks/markdown switch is per-selected-chunk: markdown
                      assembles this chunk + its sub-chunks (the root → the whole
                      document). */}
                  <div className="doc-mode-toggle">
                    <button
                      type="button"
                      className={mode === "chunks" ? "active" : ""}
                      onClick={() => setMode("chunks")}
                      title="This chunk's own content"
                    >
                      chunk
                    </button>
                    <button
                      type="button"
                      className={mode === "markdown" ? "active" : ""}
                      onClick={() => setMode("markdown")}
                      title={
                        selectedIsRoot
                          ? "Whole document as Markdown"
                          : "This chunk + its sub-chunks as Markdown"
                      }
                    >
                      markdown
                    </button>
                  </div>
                  <button type="button" onClick={() => setEditing(selectedDetail)}>
                    edit
                  </button>
                </div>
              </div>
              <div className="doc-content-body">
                {mode === "markdown" ? (
                  subtreeMd !== null ? (
                    <Markdown source={subtreeMd} />
                  ) : (
                    <p className="doc-empty-body">Assembling Markdown…</p>
                  )
                ) : selectedDetail.body ? (
                  <Markdown source={selectedDetail.body} />
                ) : (
                  <p className="doc-empty-body">(empty chunk)</p>
                )}
              </div>
            </>
          ) : (
            <div className="empty">
              <p>Select a chunk on the left.</p>
            </div>
          )}
        </div>
      </div>
      {assistantOpen && (
        <DocumentAssistantPanel
          documentId={documentId}
          scope={scope}
          selectedChunkId={selectedId}
          chunks={chunks}
          selectedChunk={selectedDetail}
          onChanged={refresh}
          onStopped={(e) => {
            setAssistantOpen(false);
            if (e) setErr("Assistant: " + e);
          }}
        />
      )}
      {editing && (
        <ChunkEditorModal
          chunk={editing}
          scope={scope}
          onClose={() => setEditing(null)}
          onSaved={refresh}
        />
      )}
    </div>
  );
}
