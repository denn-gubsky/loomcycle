import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import type {
  AssistantContext,
  BrowseScope,
  ChunkDetail,
  ChunkRow,
  DocScope,
  Principal,
} from "../types";
import { useExplorerData } from "../lib/dataLayer";
import {
  docColor,
  effectiveScheme,
  tintStyle,
  type DocumentMeta,
} from "../lib/colorScheme";
import DocumentChunkTree, {
  buildChunkTree,
  findChunkNode,
  type ChunkNode,
} from "./DocumentChunkTree";
import ChunkEditorModal from "./ChunkEditorModal";
import ColorSchemeEditor from "./ColorSchemeEditor";
import Markdown from "./Markdown";

// DocumentViewerBody is the read-mostly surface for one chunked-graph document
// (RFC AK). The chunk tree stays on the left for navigation; the right pane
// renders the SELECTED chunk in one of two views, toggled per selection:
//   - chunk    — the selected chunk's own body (structured, per-chunk).
//   - markdown — the selected chunk + its sub-chunks assembled into one Markdown
//                document. Selecting the ROOT chunk therefore renders the whole
//                document; selecting a section renders just that part.
// Structural editing (move / link / delete chunks) is deliberately NOT here.
//
// This is the context-consuming body: it reads the injected ExplorerDataLayer
// via useExplorerData() and is embedded by both the standalone <DocumentViewer>
// root and the <PathExplorer> tree — neither re-wraps it in a data provider.
export interface DocumentViewerBodyProps {
  documentId: string;
  scope: DocScope;
  // titleHint shows immediately (e.g. the Path-tree name) until the root chunk
  // title loads.
  titleHint?: string;
  // browse (RFC AS) selects whose subject's document to read — threaded into the
  // off-run document calls. Unset → the caller's own subject (default).
  browse?: BrowseScope;
  // principal — only `subject` is read; passed into the renderAssistant context.
  principal?: Principal;
  // renderAssistant — an OPTIONAL host-provided Document Assistant slot. When
  // provided, an "assistant" toggle appears and this renders into the panel;
  // when omitted, no assistant affordance is shown and no run-stream machinery
  // is pulled in. The host owns the run: it reads {documentId, scope, subject}.
  renderAssistant?: (ctx: AssistantContext) => ReactNode;
}

// View mode for the selected-chunk pane (not the whole document).
type Mode = "chunks" | "markdown";

export default function DocumentViewerBody({
  documentId,
  scope,
  titleHint,
  browse: browseProp,
  principal,
  renderAssistant,
}: DocumentViewerBodyProps) {
  const data = useExplorerData();

  // Memoize browse on its primitives so a parent passing an inline object
  // literal (`browse={{scopeId}}`) doesn't churn the fetch effects every render.
  const browse = useMemo<BrowseScope | undefined>(
    () =>
      browseProp && (browseProp.scopeId || browseProp.tenant)
        ? { scopeId: browseProp.scopeId, tenant: browseProp.tenant }
        : undefined,
    [browseProp?.scopeId, browseProp?.tenant],
  );

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
  const [meta, setMeta] = useState<DocumentMeta | null>(null);
  const [colorsOpen, setColorsOpen] = useState(false);

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

  // Color (RFC BN). rootChunkId comes from the meta (or the loaded root chunk);
  // chunkStatuses seeds the editor's per-status pickers; the toolbar tints to the
  // document's own doc.<type>.<status> color when coloring is enabled.
  const rootChunkId = useMemo(
    () => meta?.root_chunk_id ?? chunks.find((c) => !c.parent_id)?.id,
    [meta, chunks],
  );
  const chunkStatuses = useMemo(
    () => [...new Set(chunks.map((c) => c.status).filter((s): s is string => !!s))],
    [chunks],
  );
  const colorEnabled = !!meta?.color_enabled;
  const toolbarTint = useMemo(
    () =>
      colorEnabled
        ? tintStyle(docColor(meta?.type, meta?.status, effectiveScheme(meta?.color_scheme)))
        : undefined,
    [colorEnabled, meta?.type, meta?.status, meta?.color_scheme],
  );

  // Load the chunk list when the document / scope / reload changes. Default the
  // selection to the root chunk.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    data
      .documentQueryChunks(documentId, scope, browse)
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
  }, [data, documentId, scope, reload, browse]);

  // Load the document's color metadata (RFC BN) alongside the chunk list. It
  // drives the chunk-tree tint, the toolbar tint, and seeds the color editor.
  // Best-effort — a failure just leaves the document uncolored.
  useEffect(() => {
    let cancelled = false;
    data
      .documentGet(documentId, scope, browse)
      .then((m) => !cancelled && setMeta(m))
      .catch(() => !cancelled && setMeta(null));
    return () => {
      cancelled = true;
    };
  }, [data, documentId, scope, reload, browse]);

  // Reset selection + view when the document changes.
  useEffect(() => {
    setSelectedId(undefined);
    setSelectedDetail(null);
    setSubtreeMd(null);
    setMode("chunks");
    setMeta(null);
  }, [documentId, scope]);

  // Fetch the selected chunk's body.
  useEffect(() => {
    if (!selectedId) {
      setSelectedDetail(null);
      return;
    }
    let cancelled = false;
    data
      .documentGetChunk(selectedId, scope, browse)
      .then((d) => !cancelled && setSelectedDetail(d))
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [data, selectedId, scope, reload, browse]);

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
    Promise.all(items.map((it) => data.documentGetChunk(it.node.row.id, scope, browse)))
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
  }, [data, mode, selectedId, tree, scope, reload, browse]);

  const onSelect = useCallback((n: ChunkNode) => setSelectedId(n.row.id), []);

  // Download the WHOLE document as canonical, round-trippable Markdown (with the
  // metadata comments), independent of the selected-chunk view above.
  const download = useCallback(async () => {
    try {
      const r = await data.documentExportMd(documentId, scope, true, browse);
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
  }, [data, documentId, scope, browse]);

  return (
    <div className="doc-viewer">
      <div className="doc-toolbar" style={toolbarTint}>
        <strong className="doc-title" title={rootTitle}>
          {rootTitle}
        </strong>
        <div className="doc-toolbar-actions">
          <button
            type="button"
            className={colorEnabled ? "active" : ""}
            onClick={() => setColorsOpen(true)}
            disabled={!rootChunkId}
            title="Colors — tint chunk tiles + the Path tree by type/status"
          >
            colors
          </button>
          <button type="button" onClick={() => void download()} title="Download the whole document as Markdown (.md)">
            ↓ .md
          </button>
          {renderAssistant && (
            <button
              type="button"
              className={assistantOpen ? "active" : ""}
              onClick={() => setAssistantOpen((o) => !o)}
              title="Document Assistant — instruct an agent to restructure/import/link chunks"
            >
              assistant
            </button>
          )}
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
            <DocumentChunkTree
              tree={tree}
              selectedId={selectedId}
              onSelect={onSelect}
              colorEnabled={colorEnabled}
              scheme={meta?.color_scheme}
            />
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
                  {/* The chunk/markdown switch is per-selected-chunk: markdown
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
      {assistantOpen && renderAssistant && (
        <div className="doc-assistant">
          {renderAssistant({
            documentId,
            scope,
            subject: principal?.subject,
            // Pass the viewer's own reload so the injected assistant can refresh
            // the tree + selected chunk after a turn mutates the document.
            onChanged: refresh,
          })}
        </div>
      )}
      {editing && (
        <ChunkEditorModal
          chunk={editing}
          scope={scope}
          browse={browse}
          onClose={() => setEditing(null)}
          onSaved={refresh}
        />
      )}
      {colorsOpen && rootChunkId && (
        <ColorSchemeEditor
          documentId={documentId}
          rootChunkId={rootChunkId}
          docType={meta?.type}
          docStatus={meta?.status}
          chunkStatuses={chunkStatuses}
          scope={scope}
          browse={browse}
          onClose={() => setColorsOpen(false)}
          onSaved={refresh}
        />
      )}
    </div>
  );
}
