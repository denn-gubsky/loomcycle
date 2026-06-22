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
// graph document: a chunk sub-tree, a selected-chunk view (rendered Markdown +
// an optional "with sub-chunks" assembly), a whole-document Markdown view, MD
// download, and a single-chunk content editor. Structural editing (move / link
// / delete chunks) is deliberately NOT here — it is the document-management
// agent's job (Phase 3).
export interface DocumentViewerProps {
  documentId: string;
  scope: DocScope;
  // titleHint shows immediately (e.g. the Path-tree name) until the root chunk
  // title loads.
  titleHint?: string;
}

type Mode = "tree" | "markdown";

export default function DocumentViewer({ documentId, scope, titleHint }: DocumentViewerProps) {
  const [chunks, setChunks] = useState<ChunkRow[]>([]);
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const [selectedDetail, setSelectedDetail] = useState<ChunkDetail | null>(null);
  const [mode, setMode] = useState<Mode>("tree");
  const [mdSource, setMdSource] = useState<string>("");
  const [includeSub, setIncludeSub] = useState(false);
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

  // Reset selection + caches when the document changes.
  useEffect(() => {
    setSelectedId(undefined);
    setSelectedDetail(null);
    setIncludeSub(false);
    setSubtreeMd(null);
    setMode("tree");
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

  // Whole-document Markdown (clean, no metadata comments) for the MD mode.
  useEffect(() => {
    if (mode !== "markdown") return;
    let cancelled = false;
    documentExportMd(documentId, scope, false)
      .then((r) => !cancelled && setMdSource(r.markdown))
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [mode, documentId, scope, reload]);

  // Assemble the selected chunk + its sub-chunks into one Markdown string when
  // "with sub-chunks" is on (fetches each descendant's body).
  useEffect(() => {
    if (!includeSub || !selectedId) {
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
  }, [includeSub, selectedId, tree, scope, reload]);

  const onSelect = useCallback((n: ChunkNode) => setSelectedId(n.row.id), []);

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
          <div className="doc-mode-toggle">
            <button
              type="button"
              className={mode === "tree" ? "active" : ""}
              onClick={() => setMode("tree")}
            >
              chunks
            </button>
            <button
              type="button"
              className={mode === "markdown" ? "active" : ""}
              onClick={() => setMode("markdown")}
            >
              markdown
            </button>
          </div>
          <button type="button" onClick={() => void download()} title="Download as Markdown (.md)">
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
      {mode === "markdown" ? (
        <div className="doc-md-pane">
          <Markdown source={mdSource} />
        </div>
      ) : (
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
                    <label className="doc-subchunks">
                      <input
                        type="checkbox"
                        checked={includeSub}
                        onChange={(e) => setIncludeSub(e.target.checked)}
                      />
                      with sub-chunks
                    </label>
                    <button type="button" onClick={() => setEditing(selectedDetail)}>
                      edit
                    </button>
                  </div>
                </div>
                <div className="doc-content-body">
                  {includeSub && subtreeMd !== null ? (
                    <Markdown source={subtreeMd} />
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
      )}
      {assistantOpen && (
        <DocumentAssistantPanel
          documentId={documentId}
          scope={scope}
          selectedChunkId={selectedId}
          onChanged={refresh}
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
