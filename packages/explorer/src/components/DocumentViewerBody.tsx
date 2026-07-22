import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type {
  AssistantContext,
  BrowseScope,
  ChunkDetail,
  ChunkRow,
  DocEdge,
  DocScope,
  Principal,
} from "../types";
import { readImageAsBase64, imageFileFromPaste } from "../lib/imageUpload";
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
import CrossReferences from "./CrossReferences";
import Markdown from "./Markdown";
import MermaidDiagram from "./Mermaid";

// imageAlt derives an image chunk's alt text: the fields.alt override, else the
// chunk title, else a generic label (RFC BO).
function imageAlt(chunk: ChunkDetail): string {
  const f = (chunk.fields ?? {}) as { alt?: unknown };
  if (typeof f.alt === "string" && f.alt.trim() !== "") return f.alt;
  return chunk.title || "image";
}

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
  // RFC BO — an image chunk's blob object-URL (fetched with auth) + a load error.
  const [imageUrl, setImageUrl] = useState<string | null>(null);
  const [imageErr, setImageErr] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null); // hidden upload input (RFC BO)
  const [editing, setEditing] = useState<ChunkDetail | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [reload, setReload] = useState(0);
  const [assistantOpen, setAssistantOpen] = useState(false);
  const [meta, setMeta] = useState<DocumentMeta | null>(null);
  const [colorsOpen, setColorsOpen] = useState(false);
  const [edges, setEdges] = useState<DocEdge[]>([]);

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
  // Chunk ids with at least one edge → a 🔗 badge on those tiles.
  const linkedIds = useMemo(() => {
    const s = new Set<string>();
    for (const e of edges) {
      s.add(e.from_id);
      s.add(e.to_id);
    }
    return s;
  }, [edges]);
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

  // Load the cross-reference edges (RFC BN P4) in one call — powers the 🔗 tile
  // badge, the References list, and the relationship graph. Best-effort.
  useEffect(() => {
    let cancelled = false;
    data
      .documentGetEdges(documentId, scope, browse)
      .then((r) => !cancelled && setEdges(r.edges ?? []))
      .catch(() => !cancelled && setEdges([]));
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
    setEdges([]);
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

  // RFC BO — for an image chunk, fetch its bytes WITH auth into a blob object-URL
  // for <img src> (a bare <img src=/v1/...> can't carry a bearer). Revoke the URL
  // on change/unmount so blobs don't leak.
  useEffect(() => {
    const isImage = selectedDetail?.type === "image" || !!selectedDetail?.asset;
    if (!selectedDetail || !isImage || !data.documentAssetObjectUrl) {
      setImageUrl(null);
      setImageErr(null);
      return;
    }
    let cancelled = false;
    let url: string | null = null;
    setImageErr(null);
    data
      .documentAssetObjectUrl(selectedDetail.id, scope, browse)
      .then((u) => {
        if (cancelled) {
          URL.revokeObjectURL(u);
          return;
        }
        url = u;
        setImageUrl(u);
      })
      .catch((e) => !cancelled && setImageErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
      if (url) URL.revokeObjectURL(url);
    };
  }, [data, selectedDetail, scope, browse]);

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
            const heading = "#".repeat(lvl) + " " + it.node.row.title;
            // RFC BO — render media child chunks correctly in the assembled view:
            // a mermaid chunk's body becomes a ```mermaid fence (so it draws);
            // an image chunk shows a placeholder (its bytes need the auth'd asset
            // GET, which the plain-string assembler can't inline).
            if (d.type === "mermaid" && d.body) {
              return heading + "\n\n```mermaid\n" + d.body + "\n```\n";
            }
            if (d.type === "image" || d.asset) {
              const cap = d.body ? " — " + d.body : "";
              return heading + "\n\n_🖼 image chunk_" + cap + "\n";
            }
            return heading + (d.body ? "\n\n" + d.body : "") + "\n";
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

  // RFC BO authoring — create a media chunk under the selected chunk (or the
  // root). The runtime marks type=image via set_asset; a diagram is a plain
  // type=mermaid chunk whose body is the source.
  const canCreate = !!data.documentCreateChunk && !!rootChunkId;
  const addImageFile = useCallback(
    async (file: File) => {
      if (!data.documentCreateChunk || !data.documentSetAsset) return;
      const parent = selectedId || rootChunkId;
      if (!parent) return;
      setErr(null);
      try {
        const { mediaType, data: b64 } = await readImageAsBase64(file);
        const created = await data.documentCreateChunk(
          documentId,
          parent,
          { title: file.name || "image", type: "image" },
          scope,
          browse,
        );
        await data.documentSetAsset(created.id, mediaType, b64, file.name, scope, browse);
        refresh();
        setSelectedId(created.id);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      }
    },
    [data, documentId, scope, browse, selectedId, rootChunkId, refresh],
  );
  const addDiagram = useCallback(async () => {
    if (!data.documentCreateChunk) return;
    const parent = selectedId || rootChunkId;
    if (!parent) return;
    setErr(null);
    try {
      const created = await data.documentCreateChunk(
        documentId,
        parent,
        { title: "Diagram", type: "mermaid", body: "graph TD\n  A[Start] --> B[End]" },
        scope,
        browse,
      );
      refresh();
      setSelectedId(created.id);
      setEditing(created); // open the editor (with live preview) on the new diagram
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [data, documentId, scope, browse, selectedId, rootChunkId, refresh]);

  // Paste a screenshot anywhere in the viewer → a new image chunk. Ignored while
  // a modal or a text field is focused (so an editor paste isn't hijacked).
  useEffect(() => {
    if (!canCreate) return;
    const onPaste = (e: ClipboardEvent) => {
      if (editing || colorsOpen) return;
      const ae = document.activeElement;
      const tag = ae?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || (ae as HTMLElement)?.isContentEditable) return;
      const file = imageFileFromPaste(e.clipboardData?.items);
      if (file) {
        e.preventDefault();
        void addImageFile(file);
      }
    };
    window.addEventListener("paste", onPaste);
    return () => window.removeEventListener("paste", onPaste);
  }, [canCreate, editing, colorsOpen, addImageFile]);

  return (
    <div className="doc-viewer">
      <input
        ref={fileInputRef}
        type="file"
        accept="image/png,image/jpeg,image/gif,image/webp"
        style={{ display: "none" }}
        onChange={(e) => {
          const f = e.target.files?.[0];
          e.target.value = ""; // allow re-selecting the same file
          if (f) void addImageFile(f);
        }}
      />
      <div className="doc-toolbar" style={toolbarTint}>
        <strong className="doc-title" title={rootTitle}>
          {rootTitle}
        </strong>
        <div className="doc-toolbar-actions">
          {canCreate && (
            <>
              <button
                type="button"
                onClick={() => fileInputRef.current?.click()}
                title="Add an image chunk (upload — or paste a screenshot into the view)"
              >
                + image
              </button>
              <button
                type="button"
                onClick={() => void addDiagram()}
                title="Add a Mermaid diagram chunk"
              >
                + diagram
              </button>
            </>
          )}
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
              linkedIds={linkedIds}
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
                {selectedDetail.type === "mermaid" ? (
                  // RFC BO — a diagram chunk: body IS the Mermaid source.
                  selectedDetail.body ? (
                    <>
                      <MermaidDiagram code={selectedDetail.body} />
                      {mode === "markdown" && (
                        <pre className="md-pre doc-mermaid-source">
                          <code>{selectedDetail.body}</code>
                        </pre>
                      )}
                    </>
                  ) : (
                    <p className="doc-empty-body">(empty diagram)</p>
                  )
                ) : selectedDetail.type === "image" || selectedDetail.asset ? (
                  // RFC BO — an image chunk: bytes come from the auth'd asset GET.
                  imageErr ? (
                    <p className="doc-empty-body">Image failed to load: {imageErr}</p>
                  ) : imageUrl ? (
                    <figure className="doc-image">
                      <img src={imageUrl} alt={imageAlt(selectedDetail)} />
                      {selectedDetail.body && <figcaption>{selectedDetail.body}</figcaption>}
                    </figure>
                  ) : (
                    <p className="doc-empty-body">Loading image…</p>
                  )
                ) : mode === "markdown" ? (
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
              <CrossReferences
                edges={edges}
                documentId={documentId}
                selectedId={selectedId}
                onSelectChunk={setSelectedId}
              />
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
