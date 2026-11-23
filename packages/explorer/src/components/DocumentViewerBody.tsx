import { memo, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type {
  Annotation,
  AnnotationLevel,
  AnnotationTarget,
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
import { loadDocumentEdges } from "../lib/edgePages";
import { landingChunk, type DocLocation } from "../lib/docTrail";
import { isSubjectRoot } from "../lib/subjectType";
import { bulkReviewScope, documentProposals, reviewPanel } from "../lib/bulkReview";
import { annotationLevelFor, annotationsApply, loadAnnotations } from "../lib/annotation";
import { pendingClassification } from "../lib/classify";
import { copyToClipboard } from "../lib/clipboard";
import { downloadEntries, runDownload, type DownloadKind } from "../lib/documentDownload";
import {
  docColor,
  effectiveScheme,
  statusDotColor,
  tintStyle,
  type DocumentMeta,
} from "../lib/colorScheme";
import DocumentChunkTree, { buildChunkTree, type ChunkNode } from "./DocumentChunkTree";
import AnnotationPanel, { AnnotationBrief } from "./AnnotationPanel";
import ChunkEditorModal from "./ChunkEditorModal";
import SubjectTypeModal from "./SubjectTypeModal";
import Classification from "./Classification";
import ColorSchemeEditor from "./ColorSchemeEditor";
import Connections from "./Connections";
import CrossReferences from "./CrossReferences";
import DocUnavailable from "./DocUnavailable";
import HistoryModal, { HistoryView } from "./HistoryModal";
import Markdown from "./Markdown";
import MermaidDiagram from "./Mermaid";
import MoreMenu, { type MoreMenuItem } from "./MoreMenu";
import Splitter from "./Splitter";
import { TreeMarks } from "./TreeRow";
import { CollapseButton, PaneIcon, PaneRail, type PaneIconKind } from "./Pane";
import { CardScopeContext, useButtonCard, type CardScope, type CardSpec } from "./AnnotationCard";
import { shortcutApplies, usePersistedChoice, usePersistedFlag } from "../lib/persisted";
import { useNarrow } from "../lib/useNarrow";

// imageAlt derives an image chunk's alt text: the fields.alt override, else the
// chunk title, else a generic label (RFC BO).
function imageAlt(chunk: ChunkDetail): string {
  const f = (chunk.fields ?? {}) as { alt?: unknown };
  if (typeof f.alt === "string" && f.alt.trim() !== "") return f.alt;
  return chunk.title || "image";
}

// DETAIL_BATCH is how many chunk bodies are read at once. The chunk list
// carries no bodies, so the page reads each chunk; a batch keeps a long
// document from opening hundreds of requests at a time, and in document order
// the text at the top arrives first.
const DETAIL_BATCH = 8;

// DocumentViewerBody is the reading-first view of one chunked-graph document
// (RFC AK, laid out by RFC GZ): one header line (path, title, marks, edit and a
// "more" menu), the chunk outline on the left, and the whole document on the
// right as one scrolling page — every chunk in order, headed by its depth.
// Picking a chunk in the outline scrolls the page to it; scrolling moves the
// outline's current row. Each chunk carries its own action bar (on hover or
// keyboard focus).
//
// The annotation, references, classification and history panels open one at a
// time beside the body, from a rail of icons at its right edge that shows a
// count or a dot for each. The outline collapses to a rail of its own.
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
  // provided, an "assistant" entry appears in the menu and this renders into the
  // panel; when omitted, no assistant affordance is shown. The host owns the
  // run: it reads {documentId, scope, subject}.
  renderAssistant?: (ctx: AssistantContext) => ReactNode;
  // initialChunkId — the chunk to land on instead of the root, for a document
  // opened AT a chunk (a followed reference, or a return along the trail). It is
  // read when the document loads; the host remounts the body to land again.
  initialChunkId?: string;
  // onOpenDocument — the host's "leave this document" handler. `target` is the
  // referenced document + chunk; `from` is this document as the reader leaves it
  // (the current chunk and the loaded title), which is what a way back needs.
  // Omitted → cross-document targets are labels only.
  onOpenDocument?: (target: DocLocation, from: DocLocation) => void;
  // onTitle reports this document's title once its root chunk has loaded, so a
  // host that opened it by id can name it.
  onTitle?: (documentId: string, title: string) => void;
  // onChunkSelect reports the chunk the reader is on as it changes (undefined =
  // the document's root) — picked or scrolled to — so a host navigator can
  // follow it.
  onChunkSelect?: (chunkId: string | undefined) => void;
  // focusChunk moves to a chunk inside the mounted document without a remount:
  // each new `seq` scrolls to `chunkId` (absent = the root). A host navigator
  // uses it when the reader picks another chunk of the open document.
  focusChunk?: { chunkId?: string; seq: number };
  // onRegenerateAnnotation — the host's way to have an annotation written again
  // (it owns the run). Omitted → no Regenerate. Its promise settling reloads the
  // annotation; a rejection is shown in the panel.
  onRegenerateAnnotation?: (target: AnnotationTarget) => void | Promise<void>;
  // canEditAnnotations — whether to offer Edit (the operator plane writes
  // annotations by hand; the runtime refuses anyone else). Default false.
  canEditAnnotations?: boolean;
  // onAnnotationChanged — told after an edit or a regenerate settles, so a host
  // showing briefs elsewhere (the Path tree) can read them again.
  onAnnotationChanged?: () => void;
  // pathLabel — where the document sits, shown first in the header line (the
  // Path console passes the path). Omitted → the header starts with the title.
  pathLabel?: ReactNode;
  // documentActions — the host's actions on the document's Path entry, listed
  // in the "more" menu. Each is offered only when given.
  documentActions?: {
    rename?: () => void;
    transfer?: () => void;
    delete?: () => void;
  };
}

// The side panels (RFC GZ): one open at a time, closed by default, the choice
// remembered per browser.
type Panel = "annotation" | "references" | "classification" | "history";
const PANELS: readonly Panel[] = ["annotation", "references", "classification", "history"];
const PANEL_LABEL: Record<Panel, string> = {
  annotation: "annotation",
  references: "references",
  classification: "classification",
  history: "history",
};

// Where the layout is remembered, beside the splitter widths.
const PANEL_KEY = "loomcycle.explorer.panel";
const OUTLINE_KEY = "loomcycle.explorer.collapsed.outline";
// Below this width the outline folds to its rail and a side panel overlays
// the body instead of pushing it.
const NARROW_PX = 640;
// Below this width an open side panel lies over the body: pushing it would
// leave the outline and the text too little room to read.
const OVERLAY_PX = 960;

export default function DocumentViewerBody({
  documentId,
  scope,
  titleHint,
  browse: browseProp,
  principal,
  renderAssistant,
  initialChunkId,
  onOpenDocument,
  onTitle,
  onChunkSelect,
  focusChunk,
  onRegenerateAnnotation,
  canEditAnnotations,
  onAnnotationChanged,
  pathLabel,
  documentActions,
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
  // selectedId is the chunk the reader is on: picked in the outline, scrolled
  // to, or landed on. Chunk actions in the menu act on it.
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  // Every chunk's body, by id, read after the list (which carries none). A
  // chunk is read again only when its revision moves.
  const [details, setDetails] = useState<Map<string, ChunkDetail>>(() => new Map());
  const [detailErrs, setDetailErrs] = useState<Map<string, string>>(() => new Map());
  // The chunks whose Markdown source is shown in place of the rendered text.
  const [sourceOpen, setSourceOpen] = useState<Set<string>>(() => new Set());
  const fileInputRef = useRef<HTMLInputElement>(null); // hidden upload input (RFC BO)
  const [editing, setEditing] = useState<ChunkDetail | null>(null);
  const [retyping, setRetyping] = useState<ChunkDetail | null>(null);
  const [historyFor, setHistoryFor] = useState<ChunkDetail | null>(null); // RFC BS chunk history
  const [confirmDelete, setConfirmDelete] = useState(false); // RFC BP delete confirm
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // Which document the chunk list on hand belongs to, and why a load failed —
  // together they tell "still loading" from "cannot be read".
  const [loadedFor, setLoadedFor] = useState<string | undefined>(undefined);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [reload, setReload] = useState(0);
  const [assistantOpen, setAssistantOpen] = useState(false);
  const [meta, setMeta] = useState<DocumentMeta | null>(null);
  const [colorsOpen, setColorsOpen] = useState(false);
  const [edges, setEdges] = useState<DocEdge[]>([]);
  // The edge load stopped before the end of the list (its page cap).
  const [edgesIncomplete, setEdgesIncomplete] = useState(false);
  // Bumped after an operator rules on a proposed statement: refetches the edges
  // (and an open Connections panel) without reloading the whole document.
  const [edgesReload, setEdgesReload] = useState(0);
  // RFC FO — the Classify report: non-zero while requested (each change runs a
  // fresh report), 0 closed. rootFields is the root chunk's fields, where a
  // pending classification waits.
  const [classifyKey, setClassifyKey] = useState(0);
  const [rootFields, setRootFields] = useState<unknown>(undefined);
  // The annotations of the document (its root chunk's records) and of the
  // current chunk when that is not the root. Tenant scope only; otherwise
  // they stay empty and nothing is fetched.
  const [docAnnotations, setDocAnnotations] = useState<Annotation[]>([]);
  const [chunkAnnotations, setChunkAnnotations] = useState<Annotation[]>([]);
  const [annotationReload, setAnnotationReload] = useState(0);
  const [panel, setPanel] = usePersistedChoice<Panel>(PANEL_KEY, PANELS);
  const [outlineCollapsed, setOutlineCollapsed] = usePersistedFlag(OUTLINE_KEY);
  // On a narrow body the outline is a rail unless the reader opens it, and
  // then it takes the width until a chunk is picked.
  const viewerRef = useRef<HTMLDivElement>(null);
  const narrow = useNarrow(viewerRef, NARROW_PX);
  const overlay = useNarrow(viewerRef, OVERLAY_PX);
  const [narrowOutline, setNarrowOutline] = useState(false);
  const outlineHidden = narrow ? !narrowOutline : outlineCollapsed;
  const setOutlineHidden = useCallback(
    (hide: boolean) => (narrow ? setNarrowOutline(!hide) : setOutlineCollapsed(hide)),
    [narrow, setOutlineCollapsed],
  );
  const [downloading, setDownloading] = useState<string | undefined>(undefined);
  const [copied, setCopied] = useState<string | null>(null);

  const refresh = useCallback(() => setReload((n) => n + 1), []);
  // A reload the reader asked for reads every body again, not only the ones
  // whose revision moved.
  const hardReload = useCallback(() => {
    setDetails(new Map());
    setDetailErrs(new Map());
    setReload((n) => n + 1);
  }, []);

  // Host callbacks + the landing chunk through refs: none of them should re-run
  // a fetch effect when a host passes a fresh closure each render.
  const initialChunkRef = useRef(initialChunkId);
  initialChunkRef.current = initialChunkId;
  const onTitleRef = useRef(onTitle);
  onTitleRef.current = onTitle;

  // The page scroller, and the chunk the page is held on: set when the reader
  // picks a chunk (or lands on one), it is scrolled to — again as the text above
  // it loads and moves it — until the reader scrolls the page themselves.
  const pageRef = useRef<HTMLDivElement>(null);
  const pinRef = useRef<string | null>(null);
  // Where the page was last scrolled to on the pin's behalf: a scroll that
  // ends anywhere else came from the reader (or a script), whatever moved it.
  const pinnedTop = useRef<number | null>(null);
  const [pinSeq, setPinSeq] = useState(0);
  const goTo = useCallback((id: string | undefined) => {
    if (!id) return;
    pinRef.current = id;
    setSelectedId(id);
    setPinSeq((n) => n + 1);
    // On a narrow body the outline was laid over the page to pick from.
    setNarrowOutline(false);
  }, []);

  const tree = useMemo(() => buildChunkTree(chunks), [chunks]);
  // The page order: the outline walked depth first.
  const flat = useMemo(() => {
    const out: { row: ChunkRow; depth: number }[] = [];
    const walk = (nodes: ChunkNode[], depth: number) =>
      nodes.forEach((n) => {
        out.push({ row: n.row, depth });
        walk(n.children, depth + 1);
      });
    walk(tree, 0);
    return out;
  }, [tree]);
  const rootTitle = useMemo(() => {
    const root = chunks.find((c) => !c.parent_id);
    return root?.title || titleHint || "document";
  }, [chunks, titleHint]);
  const selectedDetail = (selectedId && details.get(selectedId)) || null;

  // Color (RFC BN). rootChunkId comes from the meta (or the loaded root chunk);
  // chunkStatuses seeds the editor's per-status pickers; the header tints to the
  // document's own doc.<type>.<status> color when coloring is enabled.
  const rootChunkId = useMemo(
    () => meta?.root_chunk_id ?? chunks.find((c) => !c.parent_id)?.id,
    [meta, chunks],
  );
  const rootDetail = (rootChunkId && details.get(rootChunkId)) || null;
  // Report the chunk the reader is on; the root reads as the document itself.
  const onChunkSelectRef = useRef(onChunkSelect);
  onChunkSelectRef.current = onChunkSelect;
  useEffect(() => {
    if (!selectedId) return;
    onChunkSelectRef.current?.(selectedId === rootChunkId ? undefined : selectedId);
  }, [selectedId, rootChunkId]);

  const chunkStatuses = useMemo(
    () => [...new Set(chunks.map((c) => c.status).filter((s): s is string => !!s))],
    [chunks],
  );
  const colorEnabled = !!meta?.color_enabled;
  const tintScheme = colorEnabled ? effectiveScheme(meta?.color_scheme) : undefined;
  // Chunk ids with at least one edge → a link mark on those outline rows.
  const linkedIds = useMemo(() => {
    const s = new Set<string>();
    for (const e of edges) {
      s.add(e.from_id);
      s.add(e.to_id);
    }
    return s;
  }, [edges]);
  // The document's own colour: the header's tint, the root row's in the chunk
  // outline, and the wash a chunk without a status colour inherits.
  const documentColor = useMemo(
    () => (colorEnabled ? docColor(meta?.type, meta?.status, effectiveScheme(meta?.color_scheme)) : undefined),
    [colorEnabled, meta?.type, meta?.status, meta?.color_scheme],
  );
  const headerTint = useMemo(() => tintStyle(documentColor), [documentColor]);
  // The document's type and status: from its meta, else its root chunk.
  const rootRow = useMemo(() => chunks.find((c) => !c.parent_id), [chunks]);
  const docType = meta?.type ?? rootRow?.type;
  const docStatus = meta?.status ?? rootRow?.status;

  // Load the chunk list when the document / scope / reload changes. Default the
  // selection to the landing chunk.
  const selectedIdRef = useRef(selectedId);
  selectedIdRef.current = selectedId;
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    setLoadErr(null);
    data
      .documentQueryChunks(documentId, scope, browse)
      .then((resp) => {
        if (cancelled) return;
        const rows = resp.chunks ?? [];
        setChunks(rows);
        setLoadedFor(`${scope}:${documentId}`);
        if (pinRef.current === null && !selectedIdRef.current) goTo(landingChunk(rows, initialChunkRef.current));
      })
      .catch((e) => {
        if (cancelled) return;
        // Drop the previous document's chunks: a failed load must not leave them
        // on screen under the new document's name.
        setChunks([]);
        setLoadErr(e instanceof Error ? e.message : String(e));
      })
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [data, documentId, scope, reload, browse, goTo]);

  // Read the bodies the page does not have yet, or whose revision moved, in
  // document order and a batch at a time.
  const detailsRef = useRef(details);
  detailsRef.current = details;
  useEffect(() => {
    const want = flat
      .map((f) => f.row)
      .filter((r) => r.document_id === documentId && detailsRef.current.get(r.id)?.revision !== r.revision);
    if (want.length === 0) return;
    let cancelled = false;
    (async () => {
      for (let i = 0; i < want.length && !cancelled; i += DETAIL_BATCH) {
        const batch = want.slice(i, i + DETAIL_BATCH);
        const got = await Promise.allSettled(batch.map((r) => data.documentGetChunk(r.id, scope, browse)));
        if (cancelled) return;
        setDetails((prev) => {
          const next = new Map(prev);
          got.forEach((g, k) => g.status === "fulfilled" && next.set(batch[k].id, g.value));
          return next;
        });
        setDetailErrs((prev) => {
          const next = new Map(prev);
          got.forEach((g, k) => {
            if (g.status === "rejected") {
              next.set(batch[k].id, g.reason instanceof Error ? g.reason.message : String(g.reason));
            } else next.delete(batch[k].id);
          });
          return next;
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [data, flat, documentId, scope, browse]);

  // A document always has a root chunk, so a load that failed — or answered with
  // no chunks at all — is a document that is not there to read (deleted, another
  // scope, no access). Said in place rather than left as an empty pane.
  const unavailable =
    !loading &&
    (loadErr !== null || (loadedFor === `${scope}:${documentId}` && chunks.length === 0));

  // Report the title once THIS document's root chunk is on hand (the list still
  // holds the previous document's rows until the new load lands).
  const loadedTitle = useMemo(
    () => chunks.find((c) => !c.parent_id && c.document_id === documentId)?.title,
    [chunks, documentId],
  );
  useEffect(() => {
    if (loadedTitle) onTitleRef.current?.(documentId, loadedTitle);
  }, [documentId, loadedTitle]);

  // Load the document's color metadata (RFC BN) alongside the chunk list. It
  // drives the outline tint, the header tint and marks, and seeds the color
  // editor. Best-effort — a failure just leaves the document uncolored.
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

  // Load the cross-reference edges (RFC BN P4), every page up to the cap —
  // powers the outline's link marks, the References list, and the relationship
  // graph. Best-effort.
  useEffect(() => {
    let cancelled = false;
    loadDocumentEdges(data, documentId, scope, browse)
      .then((r) => {
        if (cancelled) return;
        setEdges(r.edges);
        setEdgesIncomplete(r.incomplete);
      })
      .catch(() => {
        if (cancelled) return;
        setEdges([]);
        setEdgesIncomplete(false);
      });
    return () => {
      cancelled = true;
    };
  }, [data, documentId, scope, reload, browse, edgesReload]);

  // RFC FO — the root chunk's fields, for the pending-classification block. The
  // chunk list carries no fields; the root's body read above does. Only when
  // the data layer can accept a classification (nothing to act on otherwise).
  useEffect(() => {
    if (!data.documentAcceptClassification || !rootDetail || rootDetail.document_id !== documentId) {
      setRootFields(undefined);
      return;
    }
    setRootFields(rootDetail.fields);
  }, [data, documentId, rootDetail]);
  // How many classification items wait for a ruling: the proposed type, each
  // proposed tag, each proposed statement. 0 → nothing pending.
  const pendingCount = useMemo(() => {
    if (!data.documentAcceptClassification) return 0;
    const p = pendingClassification(rootFields);
    return p ? (p.proposedType ? 1 : 0) + p.proposedTags.length + p.statementCount : 0;
  }, [data, rootFields]);
  const classificationPending = pendingCount > 0;

  // The document's annotation, read from its root chunk. Best-effort: a failed
  // read shows no annotation rather than an error over the document.
  useEffect(() => {
    if (!rootChunkId || !annotationsApply(scope)) {
      setDocAnnotations([]);
      return;
    }
    let cancelled = false;
    loadAnnotations(data, rootChunkId, scope, browse)
      .then((r) => !cancelled && setDocAnnotations(r))
      .catch(() => !cancelled && setDocAnnotations([]));
    return () => {
      cancelled = true;
    };
  }, [data, rootChunkId, scope, reload, browse, annotationReload]);

  // The current chunk's own branch annotation (the root's is the document's,
  // above) — read only while the annotation panel is open, since the current
  // chunk moves as the reader scrolls.
  const annotationOpen = panel === "annotation";
  useEffect(() => {
    if (!annotationOpen || !selectedId || selectedId === rootChunkId || !annotationsApply(scope)) {
      setChunkAnnotations([]);
      return;
    }
    let cancelled = false;
    setChunkAnnotations([]);
    loadAnnotations(data, selectedId, scope, browse)
      .then((r) => !cancelled && setChunkAnnotations(r))
      .catch(() => !cancelled && setChunkAnnotations([]));
    return () => {
      cancelled = true;
    };
  }, [data, annotationOpen, selectedId, rootChunkId, scope, reload, browse, annotationReload]);

  // Edit and Regenerate. Edit needs a data layer that can write one; Regenerate
  // needs the host's callback. Neither is offered outside the tenant scope.
  const saveAnnotation = useMemo(() => {
    if (!canEditAnnotations || !data.documentSetAnnotation || !annotationsApply(scope)) return undefined;
    return (chunkId: string) => async (level: AnnotationLevel, brief: string, abstract: string) => {
      await data.documentSetAnnotation!({ id: chunkId, level, brief, abstract }, scope, browse);
      setAnnotationReload((n) => n + 1);
      onAnnotationChanged?.();
    };
  }, [data, scope, browse, canEditAnnotations, onAnnotationChanged]);
  const regenerateAnnotation = useMemo(() => {
    if (!onRegenerateAnnotation || !annotationsApply(scope)) return undefined;
    return (chunkId: string) => async (level: AnnotationLevel) => {
      // A folder's record names its folder: that, not this document, is what a
      // path-level Regenerate rewrites.
      const folder = [...docAnnotations, ...chunkAnnotations].find(
        (a) => a.chunk_id === chunkId && a.level === level,
      )?.folder;
      try {
        await onRegenerateAnnotation({ documentId, chunkId, level, scope, ...(folder ? { folder } : {}) });
      } finally {
        setAnnotationReload((n) => n + 1);
        onAnnotationChanged?.();
      }
    };
  }, [onRegenerateAnnotation, documentId, scope, docAnnotations, chunkAnnotations, onAnnotationChanged]);
  // The document's brief is the document record's; a path record on the same
  // root describes the folder. A folder's _annotation document has only the
  // path record: that is its brief.
  const documentBrief =
    docAnnotations.find((a) => a.level === "document") ?? docAnnotations.find((a) => a.level === "path");
  // The current chunk's records, matched by id: the list can still be the
  // previous chunk's for a moment after the reader moves on.
  const selectedAnnotations = useMemo(
    () => chunkAnnotations.filter((a) => a.chunk_id === selectedId),
    [chunkAnnotations, selectedId],
  );
  const annotationOffered =
    !!rootChunkId && (docAnnotations.length > 0 || !!saveAnnotation || !!regenerateAnnotation);

  // RFC FN — the operator's ruling on a proposed statement. Offered only when the
  // data layer implements both rulings (they are optional on a custom one). A
  // refusal propagates to the row, which shows it inline.
  // Called through `data` (not detached) so a class-based data layer keeps `this`.
  const ruleStatement = useMemo(() => {
    if (!data.documentAcceptStatement || !data.documentRejectStatement) return undefined;
    return async (id: string, ok: boolean, reason?: string) => {
      if (ok) await data.documentAcceptStatement!(id, scope, browse, reason);
      else await data.documentRejectStatement!(id, reason ?? "", scope, browse);
      setEdgesReload((n) => n + 1);
    };
  }, [data, scope, browse]);

  // RFC FS — bulk review of this document's proposals in the Connections panel:
  // offered when the data layer can rule in bulk and the review routes reach
  // this document's scope (reviewPanel).
  const reviewScope = useMemo(() => bulkReviewScope(data, scope), [data, scope]);
  const proposals = useMemo(() => documentProposals(edges, documentId), [edges, documentId]);
  const onReviewChanged = useCallback(() => setEdgesReload((n) => n + 1), []);
  const review = useMemo(
    () => reviewPanel(reviewScope, proposals, edgesIncomplete, onReviewChanged),
    [reviewScope, proposals, edgesIncomplete, onReviewChanged],
  );
  // The references of the current chunk: what its panel toggle counts.
  const refCount = useMemo(
    () => (selectedId ? edges.filter((e) => e.from_id === selectedId || e.to_id === selectedId).length : 0),
    [edges, selectedId],
  );

  // A host's focus request for the open document. Declared before the reset
  // below on purpose: on a document change the reset runs after it and the
  // landing chunk (initialChunkId) decides instead.
  const focusSeq = focusChunk?.seq;
  const focusRef = useRef(focusChunk);
  focusRef.current = focusChunk;
  const rootRef = useRef(rootChunkId);
  rootRef.current = rootChunkId;
  const chunksRef = useRef(chunks);
  chunksRef.current = chunks;
  useEffect(() => {
    if (focusSeq === undefined) return;
    const wanted = focusRef.current?.chunkId;
    // Once the chunk list is in, an id that is not one of this document's
    // chunks (another document's, a deleted one) lands on the root rather than
    // selecting nothing; before it is in, the request is taken as given.
    const target =
      chunksRef.current.length > 0 ? landingChunk(chunksRef.current, wanted) : (wanted ?? rootRef.current);
    goTo(target);
  }, [focusSeq, goTo]);

  // Reset the view when the document changes.
  useEffect(() => {
    pinRef.current = null;
    setSelectedId(undefined);
    setDetails(new Map());
    setDetailErrs(new Map());
    setSourceOpen(new Set());
    setMeta(null);
    setEdges([]);
    setEdgesIncomplete(false);
    setClassifyKey(0);
    setRootFields(undefined);
    setDocAnnotations([]);
    setChunkAnnotations([]);
  }, [documentId, scope]);

  // Scroll the page to the held chunk — on a pick, and again as the bodies
  // above it arrive and push it down. Instant, so the outline and the page
  // never disagree for the length of an animation.
  useEffect(() => {
    const page = pageRef.current;
    const id = pinRef.current;
    if (!page || !id) return;
    const el = page.querySelector<HTMLElement>(`[data-chunk-id="${id}"]`);
    if (!el) return;
    const top = el.getBoundingClientRect().top - page.getBoundingClientRect().top + page.scrollTop;
    page.scrollTop = Math.max(0, top - 8);
    pinnedTop.current = page.scrollTop;
  }, [pinSeq, details, flat]);

  // The reader's own scrolling lets go of the held chunk; from then on the
  // chunk at the top of the page is the current one. Wheel, touch, keys and
  // a press say so at once; a scroll that fires none of them (a scrollbar
  // drag, a host's script) says so by ending somewhere the pin did not put the
  // page. The pin's own scroll lands where it was told, so it never lets go of
  // itself.
  useEffect(() => {
    const page = pageRef.current;
    if (!page) return;
    const release = () => {
      pinRef.current = null;
      pinnedTop.current = null;
    };
    const onScroll = () => {
      if (pinRef.current === null) return;
      const at = pinnedTop.current;
      if (at === null || Math.abs(page.scrollTop - at) > 2) {
        release();
        followRef.current(); // the outline takes up the reading position
      }
    };
    const opts = { passive: true } as const;
    page.addEventListener("wheel", release, opts);
    page.addEventListener("touchstart", release, opts);
    page.addEventListener("keydown", release);
    page.addEventListener("pointerdown", release);
    page.addEventListener("scroll", onScroll, opts);
    return () => {
      page.removeEventListener("wheel", release);
      page.removeEventListener("touchstart", release);
      page.removeEventListener("keydown", release);
      page.removeEventListener("pointerdown", release);
      page.removeEventListener("scroll", onScroll);
    };
  }, [unavailable]);

  // The outline follows the reading position: the current chunk is the first
  // whose section crosses the top third of the page. One observer for every
  // section, so a long document costs nothing while it is not scrolled.
  // followRef recomputes the current chunk from what the observer last saw —
  // for a pin let go by a scroll that crossed no section boundary.
  const followRef = useRef<() => void>(() => {});
  useEffect(() => {
    const page = pageRef.current;
    if (!page || typeof IntersectionObserver === "undefined" || flat.length === 0) return;
    const order = new Map(flat.map((f, i) => [f.row.id, i]));
    const visible = new Set<string>();
    const follow = () => {
      if (pinRef.current) return;
      let best: string | undefined;
      for (const id of visible) if (best === undefined || order.get(id)! < order.get(best)!) best = id;
      if (best) setSelectedId((cur) => (cur === best ? cur : best));
    };
    followRef.current = follow;
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          const id = (e.target as HTMLElement).dataset.chunkId;
          if (!id) continue;
          if (e.isIntersecting) visible.add(id);
          else visible.delete(id);
        }
        follow();
      },
      { root: page, rootMargin: "0px 0px -66% 0px", threshold: 0 },
    );
    page.querySelectorAll<HTMLElement>("[data-chunk-id]").forEach((el) => io.observe(el));
    return () => {
      io.disconnect();
      followRef.current = () => {};
    };
  }, [flat]);

  const onSelect = useCallback((n: ChunkNode) => goTo(n.row.id), [goTo]);

  // Leaving for another document: hand the host the target and a snapshot of
  // where the reader is now. The target's title is the CHUNK's — a stand-in until
  // that document loads and reports its own.
  const openDocument = useCallback(
    (targetDocumentId: string, chunkId: string, title?: string) =>
      onOpenDocument?.(
        { documentId: targetDocumentId, chunkId, title },
        { documentId, chunkId: selectedId, title: rootTitle },
      ),
    [onOpenDocument, documentId, selectedId, rootTitle],
  );

  // RFC BO authoring — create a media chunk under the current chunk (or the
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
        goTo(created.id);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      }
    },
    [data, documentId, scope, browse, selectedId, rootChunkId, refresh, goTo],
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
      goTo(created.id);
      setEditing(created); // open the editor (with live preview) on the new diagram
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [data, documentId, scope, browse, selectedId, rootChunkId, refresh, goTo]);

  // RFC BP — "add text": a plain text chunk immediately AFTER the current chunk
  // (via after_id) when a non-root chunk is current; otherwise appended under
  // the root. Opens the editor on the new chunk.
  const currentIsRoot = !!selectedId && selectedId === rootChunkId;
  const addText = useCallback(async () => {
    if (!data.documentCreateChunk) return;
    const insertAfter = selectedId && !currentIsRoot ? selectedId : undefined;
    const parent = selectedId || rootChunkId;
    if (!parent) return;
    setErr(null);
    try {
      const created = await data.documentCreateChunk(
        documentId,
        insertAfter ? "" : parent, // after_id resolves the parent server-side
        { title: "Text", body: "" },
        scope,
        browse,
        insertAfter,
      );
      refresh();
      goTo(created.id);
      setEditing(created);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [data, documentId, scope, browse, selectedId, currentIsRoot, rootChunkId, refresh, goTo]);

  // "add child": a chunk NESTED UNDER the current one.
  //
  // "add text" inserts a SIBLING (after_id), which is right for prose flow and
  // wrong for structure — without this a document's hierarchy could only be
  // created by import_md or by an agent, which left the ontology's subclasses (a
  // child chunk is a subclass) unauthorable by the operator who owns them.
  //
  // Passing parent_id with no after_id appends it as the current chunk's last child.
  const addChild = useCallback(async () => {
    if (!data.documentCreateChunk || !selectedId) return;
    setErr(null);
    try {
      const created = await data.documentCreateChunk(
        documentId,
        selectedId,
        { title: "Text", body: "" },
        scope,
        browse,
      );
      refresh();
      goTo(created.id);
      setEditing(created);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [data, documentId, scope, browse, selectedId, refresh, goTo]);

  // RFC BP — the current chunk's siblings (same parent) in position order, to
  // gate move up / down at the level boundaries.
  const siblingBounds = useMemo(() => {
    const row = chunks.find((c) => c.id === selectedId);
    if (!row || !row.parent_id) return { canUp: false, canDown: false };
    const sibs = chunks
      .filter((c) => (c.parent_id ?? "") === row.parent_id)
      .sort((a, b) => a.position - b.position || a.id.localeCompare(b.id));
    const i = sibs.findIndex((c) => c.id === row.id);
    return { canUp: i > 0, canDown: i >= 0 && i < sibs.length - 1 };
  }, [chunks, selectedId]);

  const reorderSelected = useCallback(
    async (direction: "up" | "down") => {
      if (!data.documentReorderChunk || !selectedId) return;
      setErr(null);
      try {
        await data.documentReorderChunk(selectedId, direction, scope, browse);
        refresh();
        goTo(selectedId);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      }
    },
    [data, selectedId, scope, browse, refresh, goTo],
  );

  const deleteSelected = useCallback(async () => {
    if (!data.documentDeleteChunk || !selectedDetail) return;
    setErr(null);
    const parentId = selectedDetail.parent_id;
    try {
      await data.documentDeleteChunk(selectedDetail.id, scope, browse);
      setConfirmDelete(false);
      goTo(parentId || rootChunkId);
      refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setConfirmDelete(false);
    }
  }, [data, selectedDetail, scope, browse, rootChunkId, refresh, goTo]);

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

  // Copy an id or a link, and say so for a moment in the header.
  const copy = useCallback(async (text: string, what: string) => {
    const ok = await copyToClipboard(text);
    setCopied(ok ? `${what} copied` : `could not copy the ${what}`);
    window.setTimeout(() => setCopied(null), 1500);
  }, []);

  // The page's per-chunk handlers take the chunk's id, so they stay the same
  // function across renders and a chunk that did not change is not drawn again.
  const pickChunk = useCallback((id: string) => setSelectedId((cur) => (cur === id ? cur : id)), []);
  const toggleSource = useCallback(
    (id: string) =>
      setSourceOpen((s) => {
        const next = new Set(s);
        if (!next.delete(id)) next.add(id);
        return next;
      }),
    [],
  );
  const copyChunkId = useCallback((id: string) => void copy(id, "chunk id"), [copy]);

  const download = useCallback(
    async (kind: DownloadKind, label: string) => {
      setDownloading(`Preparing ${label}`);
      try {
        await runDownload(data, kind, documentId, scope, browse, rootTitle);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      } finally {
        setDownloading(undefined);
      }
    },
    [data, documentId, scope, browse, rootTitle],
  );

  const togglePanel = (p: Panel) => setPanel((cur) => (cur === p ? null : p));
  const classifyOffered = !!data.documentClassify || classificationPending;
  // The rail: every panel this document can show. A panel remembered from
  // another document that this one does not offer stays closed here.
  const offered: Panel[] = [
    ...(annotationOffered ? (["annotation"] as const) : []),
    "references",
    ...(classifyOffered ? (["classification"] as const) : []),
    "history",
  ];
  const openPanel = panel && offered.includes(panel) ? panel : null;

  // Keyboard: "{" folds the outline, "]" the side panel (the last one open,
  // else references). The Path console takes "[" for its tree.
  const lastPanel = useRef<Panel>("references");
  if (openPanel) lastPanel.current = openPanel;
  const keysRef = useRef<(e: KeyboardEvent) => void>(() => {});
  keysRef.current = (e: KeyboardEvent) => {
    if (unavailable || !shortcutApplies(e, viewerRef.current)) return;
    if (e.key === "{") {
      setOutlineHidden(!outlineHidden);
    } else if (e.key === "]") {
      setPanel(openPanel ? null : offered.includes(lastPanel.current) ? lastPanel.current : "references");
    } else return;
    e.preventDefault();
  };
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => keysRef.current(e);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The rail's annotation icon carries the document's card.
  const docCard: CardSpec = {
    title: rootTitle,
    type: docType,
    status: docStatus,
    brief: documentBrief?.brief,
    abstract: documentBrief?.abstract,
    stale: documentBrief?.stale || undefined,
  };
  const annotationStale = docAnnotations.some((a) => a.stale);
  const cardScope = useMemo<CardScope>(() => ({ data, scope, browse }), [data, scope, browse]);

  // Everything on the document that is not "edit" (RFC GZ). The chunk actions
  // act on the current chunk — the one picked or scrolled to — as the toolbar's
  // did on the selected one.
  const currentTitle = selectedDetail?.title || chunks.find((c) => c.id === selectedId)?.title || "(untitled)";
  const menuGroups: MoreMenuItem[][] = [
    canCreate
      ? [
          {
            key: "add-text",
            label: "add text",
            title: `Add a text chunk after “${currentTitle}”, at the same level`,
            run: () => void addText(),
          },
          {
            key: "add-child",
            label: "add child",
            title: selectedId
              ? `Add a chunk nested under “${currentTitle}” (in the ontology, a child is a subclass)`
              : "Select a chunk first — this nests a new chunk under it",
            disabled: !selectedId,
            run: () => void addChild(),
          },
          {
            key: "add-image",
            label: "add image",
            title: "Add an image chunk (upload — or paste a screenshot into the view)",
            run: () => fileInputRef.current?.click(),
          },
          { key: "add-diagram", label: "add diagram", title: "Add a Mermaid diagram chunk", run: () => void addDiagram() },
        ]
      : [],
    selectedId && !currentIsRoot
      ? [
          {
            key: "chunk-up",
            label: "move chunk up",
            title: `Move “${currentTitle}” up within its level`,
            disabled: !siblingBounds.canUp,
            run: () => void reorderSelected("up"),
          },
          {
            key: "chunk-down",
            label: "move chunk down",
            title: `Move “${currentTitle}” down within its level`,
            disabled: !siblingBounds.canDown,
            run: () => void reorderSelected("down"),
          },
          {
            key: "chunk-delete",
            label: "delete chunk…",
            title: `Delete “${currentTitle}” and its sub-chunks`,
            danger: true,
            disabled: !selectedDetail,
            run: () => setConfirmDelete(true),
          },
        ]
      : [],
    [
      {
        key: "colors",
        label: colorEnabled ? "colours (on)" : "colours",
        title: "Colours — tint chunk rows and the Path tree by type/status",
        disabled: !rootChunkId,
        run: () => setColorsOpen(true),
      },
      ...(data.documentClassify
        ? [
            {
              key: "classify",
              label: "classify",
              title:
                "Classify — what this document is, by the confirmed ontology (a report first; nothing is written until Apply)",
              run: () => {
                setClassifyKey((n) => n + 1);
                setPanel("classification");
              },
            },
          ]
        : []),
      ...(data.documentEntityTypes && rootDetail && isSubjectRoot(rootDetail)
        ? [
            {
              key: "change-type",
              label: "change subject type",
              title: "Change this subject's type to another type in force",
              run: () => setRetyping(rootDetail),
            },
          ]
        : []),
      ...(renderAssistant
        ? [
            {
              key: "assistant",
              label: assistantOpen ? "close assistant" : "assistant",
              title: "Document Assistant — instruct an agent to restructure/import/link chunks",
              run: () => setAssistantOpen((o) => !o),
            },
          ]
        : []),
    ],
    downloadEntries(data).map((e) => ({
      key: `download-${e.kind}`,
      label: `download ${e.label}`,
      title: e.title,
      run: () => void download(e.kind, e.label),
    })),
    [
      ...(documentActions?.rename ? [{ key: "rename", label: "rename / move", run: documentActions.rename }] : []),
      ...(documentActions?.transfer
        ? [
            {
              key: "transfer",
              label: "copy / move to…",
              title: "Copy or move to another scope",
              run: documentActions.transfer,
            },
          ]
        : []),
      {
        key: "copy-id",
        label: "copy document id",
        title: "Copy this document's id, for a workflow over the MCP/HTTP API",
        run: () => void copy(documentId, "document id"),
      },
      {
        key: "copy-link",
        label: "copy link",
        title: "Copy a loom:doc link to this document, the form agents cite it by",
        run: () => void copy(docLink(documentId, scope), "link"),
      },
      { key: "reload", label: "reload", title: "Read the document again", run: hardReload },
    ],
    documentActions?.delete
      ? [{ key: "delete", label: "delete document…", danger: true, run: documentActions.delete }]
      : [],
  ];

  return (
    <CardScopeContext.Provider value={cardScope}>
      <div className={`doc-viewer${narrow ? " narrow" : ""}${overlay ? " panel-over" : ""}`} ref={viewerRef}>
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
        <div className="doc-header" style={headerTint}>
          {pathLabel && <div className="doc-header-path">{pathLabel}</div>}
          <h1 className="doc-title" title={rootTitle}>
            {rootTitle}
          </h1>
          <TreeMarks type={docType} status={docStatus} statusColor={statusDotColor(docStatus, tintScheme)} />
          <div className="doc-header-actions">
            {copied && (
              <span className="doc-header-note" role="status">
                {copied}
              </span>
            )}
            <button
              type="button"
              className="doc-edit"
              onClick={() => rootDetail && setEditing(rootDetail)}
              disabled={!rootDetail}
              title="Edit the document's title, type, status and opening text"
            >
              <BarIcon kind="edit" />
              <span className="doc-edit-label">edit</span>
            </button>
            <MoreMenu groups={menuGroups} busy={downloading} label="Document actions" />
          </div>
        </div>
        {err && <div className="paths-err">{err}</div>}
        {unavailable ? (
          <DocUnavailable documentId={documentId} scope={scope} detail={loadErr ?? undefined} />
        ) : (
          <div className="doc-body">
            {/* The outline | page border is draggable and its width remembered
                per browser; the outline also folds to a rail with its count. */}
            <Splitter
              className="doc-split"
              defaultLeftWidth={240}
              minLeftWidth={160}
              minRightWidth={320}
              storageKey="loomcycle.explorer.split.doc"
              hideLeft={outlineHidden}
              hideRight={narrow && !outlineHidden}
              onHandleDoubleClick={() => setOutlineHidden(true)}
              leftRail={
                <PaneRail
                  side="left"
                  label="chunk outline"
                  icon="outline"
                  count={chunks.length}
                  countLabel={chunks.length === 1 ? "chunk" : "chunks"}
                  shortcut="{"
                  onExpand={() => setOutlineHidden(false)}
                />
              }
            >
              <div className="doc-chunktree">
                <div className="pane-head">
                  <span className="pane-head-label">Outline</span>
                  <span className="pane-head-count">{chunks.length}</span>
                  <CollapseButton label="chunk outline" side="left" shortcut="{" onClick={() => setOutlineHidden(true)} />
                </div>
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
                    documentColor={documentColor}
                    linkedIds={linkedIds}
                    documentType={docType}
                    documentStatus={docStatus}
                  />
                )}
              </div>
              <div className="doc-page" ref={pageRef} tabIndex={-1}>
                <article className="doc-article" aria-label={rootTitle}>
                  {flat.map(({ row, depth }) => (
                    <ChunkSection
                      key={row.id}
                      row={row}
                      depth={depth}
                      detail={details.get(row.id)}
                      error={detailErrs.get(row.id)}
                      current={row.id === selectedId}
                      source={sourceOpen.has(row.id)}
                      statusColor={statusDotColor(row.status, tintScheme)}
                      scope={scope}
                      browse={browse}
                      onPick={pickChunk}
                      onToggleSource={toggleSource}
                      onEdit={setEditing}
                      onHistory={setHistoryFor}
                      onCopyId={copyChunkId}
                    />
                  ))}
                </article>
              </div>
            </Splitter>
            {openPanel && (
              <aside className="doc-side" id="doc-side" aria-label={`${PANEL_LABEL[openPanel]} panel`}>
                <div className="pane-head">
                  <span className="pane-head-label">{PANEL_LABEL[openPanel]}</span>
                  <CollapseButton label={`${PANEL_LABEL[openPanel]} panel`} side="right" shortcut="]" onClick={() => setPanel(null)} />
                </div>
                <div className="doc-side-body">
                  {openPanel === "annotation" && rootChunkId && (
                    <>
                      <div className="doc-annotation">
                        <h2 className="doc-panel-heading">Document</h2>
                        <AnnotationBrief record={documentBrief} />
                        <AnnotationPanel
                          key={rootChunkId}
                          records={docAnnotations}
                          newLevel={annotationLevelFor(true)}
                          onSave={saveAnnotation?.(rootChunkId)}
                          onRegenerate={regenerateAnnotation?.(rootChunkId)}
                        />
                      </div>
                      {selectedId && selectedId !== rootChunkId && (
                        <div className="doc-annotation">
                          <h2 className="doc-panel-heading">{currentTitle}</h2>
                          <AnnotationBrief record={selectedAnnotations[0]} />
                          <AnnotationPanel
                            key={selectedId}
                            records={selectedAnnotations}
                            newLevel={annotationLevelFor(false)}
                            onSave={saveAnnotation?.(selectedId)}
                            onRegenerate={regenerateAnnotation?.(selectedId)}
                          />
                        </div>
                      )}
                    </>
                  )}
                  {openPanel === "references" && (
                    <div className="doc-links-pane">
                      {edges.length === 0 && <p className="doc-refs-empty">This document has no links.</p>}
                      <CrossReferences
                        edges={edges}
                        incomplete={edgesIncomplete}
                        documentId={documentId}
                        selectedId={selectedId}
                        onSelectChunk={goTo}
                        onOpenDocument={onOpenDocument ? openDocument : undefined}
                        onRuleStatement={ruleStatement}
                      />
                      {/* RFC BS — backlinks / related / unlinked mentions for the
                          current chunk, lazily fetched when its section opens. */}
                      <Connections
                        documentId={documentId}
                        selectedId={selectedId}
                        scope={scope}
                        browse={browse}
                        onSelectChunk={goTo}
                        onOpenDocument={onOpenDocument ? openDocument : undefined}
                        refreshKey={edgesReload}
                        review={review}
                      />
                    </div>
                  )}
                  {openPanel === "classification" &&
                    (classifyKey === 0 && !classificationPending ? (
                      <div className="doc-classify">
                        <p className="doc-classify-hint">Nothing is waiting for review.</p>
                        {data.documentClassify && (
                          <button type="button" onClick={() => setClassifyKey((n) => n + 1)}>
                            classify this document
                          </button>
                        )}
                      </div>
                    ) : (
                      <Classification
                        key={`${scope}:${documentId}`}
                        documentId={documentId}
                        scope={scope}
                        browse={browse}
                        rootFields={rootFields}
                        requestKey={classifyKey}
                        onClose={() => setClassifyKey(0)}
                        onChanged={refresh}
                      />
                    ))}
                  {openPanel === "history" &&
                    (selectedId ? (
                      <>
                        <h2 className="doc-panel-heading">{currentTitle}</h2>
                        <HistoryView key={selectedId} chunkId={selectedId} scope={scope} browse={browse} />
                      </>
                    ) : (
                      <p className="doc-empty-body">Select a chunk to see its history.</p>
                    ))}
                </div>
              </aside>
            )}
            <nav className="doc-rail" aria-label="Panels">
              {offered.map((p) => (
                <RailButton
                  key={p}
                  panel={p}
                  open={openPanel === p}
                  onClick={() => togglePanel(p)}
                  count={p === "references" ? refCount : p === "classification" ? pendingCount : undefined}
                  dot={p === "annotation" && annotationStale}
                  card={p === "annotation" ? docCard : undefined}
                />
              ))}
            </nav>
          </div>
        )}
        {assistantOpen && renderAssistant && (
          <div className="doc-assistant">
            {renderAssistant({
              documentId,
              scope,
              subject: principal?.subject,
              // Pass the viewer's own reload so the injected assistant can refresh
              // the outline + page after a turn mutates the document.
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
        {retyping && (
          <SubjectTypeModal
            chunk={retyping}
            scope={scope}
            browse={browse}
            onClose={() => setRetyping(null)}
            onSaved={refresh}
          />
        )}
        {historyFor && (
          <HistoryModal
            chunkId={historyFor.id}
            chunkTitle={historyFor.title}
            scope={scope}
            browse={browse}
            onClose={() => setHistoryFor(null)}
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
        {confirmDelete && selectedDetail && (
          <div className="modal-overlay" onClick={() => setConfirmDelete(false)}>
            <div className="modal chunk-delete-confirm" onClick={(e) => e.stopPropagation()}>
              <h3>Delete chunk</h3>
              <p className="modal-context">
                Delete “{selectedDetail.title || "(untitled)"}” and all of its sub-chunks? This
                cannot be undone.
              </p>
              <div className="modal-buttons">
                <button type="button" onClick={() => setConfirmDelete(false)}>
                  cancel
                </button>
                <button type="button" className="danger" onClick={() => void deleteSelected()}>
                  delete
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </CardScopeContext.Provider>
  );
}

// docLink is the link agents cite a document by (help topic Document/citing):
// loom:doc/<id>, with the scope when it is not the user's own.
export function docLink(documentId: string, scope: DocScope): string {
  return `loom:doc/${documentId}${scope === "user" ? "" : `?scope=${scope}`}`;
}

interface ChunkSectionProps {
  row: ChunkRow;
  depth: number;
  detail?: ChunkDetail;
  error?: string;
  current: boolean;
  source: boolean;
  statusColor?: string;
  scope: DocScope;
  browse?: BrowseScope;
  onPick: (id: string) => void;
  onToggleSource: (id: string) => void;
  onEdit: (d: ChunkDetail) => void;
  onHistory: (d: ChunkDetail) => void;
  onCopyId: (id: string) => void;
}

// ChunkSection is one chunk on the page: its heading (by depth; the root's
// title is the document's, shown once in the header), its body, and its action
// bar, which shows on hover or when anything in the chunk has the keyboard
// focus. Memoized: moving the current chunk redraws two sections, not the
// whole document.
const ChunkSection = memo(function ChunkSection({
  row,
  depth,
  detail,
  error,
  current,
  source,
  statusColor,
  scope,
  browse,
  onPick,
  onToggleSource,
  onEdit,
  onHistory,
  onCopyId,
}: ChunkSectionProps) {
  const title = row.title || "(untitled)";
  const level = Math.min(depth + 1, 6);
  const Heading = `h${level}` as "h2";
  return (
    <section
      className={`doc-chunk${current ? " current" : ""}`}
      data-chunk-id={row.id}
      data-depth={depth}
      aria-label={depth === 0 ? undefined : title}
      onClick={() => onPick(row.id)}
    >
      <div className="doc-chunk-bar" role="toolbar" aria-label={`${title} — chunk actions`}>
        <TreeMarks type={row.type} status={row.status} statusColor={statusColor} />
        <span className="doc-chunk-rev" title="Revision">
          rev {row.revision}
        </span>
        {detail?.tags?.map((t) => (
          <span className="chunk-tag" key={t}>
            {t}
          </span>
        ))}
        <button
          type="button"
          className="doc-chunk-icon"
          aria-pressed={source}
          onClick={() => onToggleSource(row.id)}
          disabled={!detail}
          aria-label="view source"
          title="Show this chunk's Markdown source"
        >
          <BarIcon kind="source" />
        </button>
        <button
          type="button"
          className="doc-chunk-icon"
          onClick={() => detail && onHistory(detail)}
          disabled={!detail}
          aria-label="history"
          title="This chunk's edit history and diffs"
        >
          <BarIcon kind="history" />
        </button>
        <button
          type="button"
          className="doc-chunk-icon"
          onClick={() => onCopyId(row.id)}
          aria-label="copy id"
          title="Copy this chunk's id"
        >
          <BarIcon kind="copy" />
        </button>
        <button type="button" className="doc-chunk-edit" onClick={() => detail && onEdit(detail)} disabled={!detail}>
          <BarIcon kind="edit" />
          edit
        </button>
      </div>
      {depth > 0 && <Heading className="doc-chunk-title">{title}</Heading>}
      <ChunkBody detail={detail} error={error} source={source} scope={scope} browse={browse} />
    </section>
  );
});

const RAIL_ICON: Record<Panel, PaneIconKind> = {
  annotation: "annotation",
  references: "references",
  classification: "classification",
  history: "history",
};

// RailButton opens one side panel. Its label says what its count counts, so
// a screen reader hears "references, 3" rather than a bare number; a quiet
// count (0) is not drawn.
function RailButton({
  panel,
  open,
  onClick,
  count,
  dot,
  card,
}: {
  panel: Panel;
  open: boolean;
  onClick: () => void;
  count?: number;
  dot?: boolean;
  card?: CardSpec;
}) {
  const c = useButtonCard(card);
  const what =
    panel === "references"
      ? `${count ?? 0} on this chunk`
      : panel === "classification"
        ? count
          ? `${count} waiting for review`
          : "nothing waiting"
        : panel === "annotation" && dot
          ? "out of date"
          : undefined;
  return (
    <>
      <button
        type="button"
        className={`doc-rail-button${count ? " counted" : ""}`}
        aria-pressed={open}
        aria-controls={open ? "doc-side" : undefined}
        aria-label={`${PANEL_LABEL[panel]}${what ? `, ${what}` : ""}`}
        title={card ? undefined : PANEL_LABEL[panel]}
        aria-keyshortcuts={open ? "]" : undefined}
        onClick={onClick}
        {...c.props}
      >
        <PaneIcon kind={RAIL_ICON[panel]} />
        {count ? <span className="doc-rail-count">{count}</span> : null}
        {dot && <span className="doc-rail-dot" aria-hidden="true" />}
      </button>
      {c.card}
    </>
  );
}

// The chunk bar's icons, drawn like the tree icons so they follow the theme.
function BarIcon({ kind }: { kind: "source" | "history" | "copy" | "edit" }) {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {kind === "source" && <path d="M5.5 4.5L2 8l3.5 3.5M10.5 4.5L14 8l-3.5 3.5" />}
      {kind === "history" && (
        <>
          <circle cx="8" cy="8" r="5.75" />
          <path d="M8 5v3.25l2 1.25" />
        </>
      )}
      {kind === "edit" && <path d="M10.75 2.75l2.5 2.5L5.5 13H3v-2.5zM9.25 4.25l2.5 2.5" />}
      {kind === "copy" && (
        <>
          <rect x="5.25" y="5.25" width="8" height="8" rx="1" />
          <path d="M10.75 5.25v-1.5a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h1.5" />
        </>
      )}
    </svg>
  );
}

function ChunkBody({
  detail,
  error,
  source,
  scope,
  browse,
}: {
  detail?: ChunkDetail;
  error?: string;
  source: boolean;
  scope: DocScope;
  browse?: BrowseScope;
}) {
  if (!detail) {
    return error ? (
      <p className="doc-empty-body">This chunk could not be read: {error}</p>
    ) : (
      <p className="doc-chunk-loading" aria-busy="true">
        Loading…
      </p>
    );
  }
  if (source) {
    return (
      <pre className="md-pre doc-chunk-source">
        <code>{detail.body}</code>
      </pre>
    );
  }
  if (detail.type === "mermaid") {
    // RFC BO — a diagram chunk: body IS the Mermaid source.
    return detail.body ? <MermaidDiagram code={detail.body} /> : <p className="doc-empty-body">(empty diagram)</p>;
  }
  if (detail.type === "image" || detail.asset) {
    return <ChunkImage detail={detail} scope={scope} browse={browse} />;
  }
  return detail.body ? <Markdown source={detail.body} /> : null;
}

// ChunkImage shows an image chunk (RFC BO): its bytes are fetched WITH auth into
// a blob object-URL for <img src> (a bare <img src=/v1/...> can't carry a
// bearer), revoked when the chunk changes or leaves the page.
function ChunkImage({ detail, scope, browse }: { detail: ChunkDetail; scope: DocScope; browse?: BrowseScope }) {
  const data = useExplorerData();
  const [url, setUrl] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    if (!data.documentAssetObjectUrl) return;
    let cancelled = false;
    let made: string | null = null;
    setErr(null);
    data
      .documentAssetObjectUrl(detail.id, scope, browse)
      .then((u) => {
        if (cancelled) {
          URL.revokeObjectURL(u);
          return;
        }
        made = u;
        setUrl(u);
      })
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
      if (made) URL.revokeObjectURL(made);
    };
    // The revision moves when the image is replaced.
  }, [data, detail.id, detail.revision, scope, browse]);
  if (err) return <p className="doc-empty-body">Image failed to load: {err}</p>;
  if (!url) return <p className="doc-chunk-loading">Loading image…</p>;
  return (
    <figure className="doc-image">
      <img src={url} alt={imageAlt(detail)} />
      {detail.body && <figcaption>{detail.body}</figcaption>}
    </figure>
  );
}
