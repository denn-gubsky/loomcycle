import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import type { AnnotationTarget, AssistantContext, BrowseScope, DocScope, Principal } from "./types";
import {
  ExplorerRoot,
  useResolvedDataLayer,
  type ExplorerDataSource,
} from "./components/ExplorerRoot";
import { amendCurrent, followReference, jumpTo, type DocLocation, type Trail } from "./lib/docTrail";
import DocTrail from "./components/DocTrail";
import DocumentViewerBody from "./components/DocumentViewerBody";

// DocumentViewer is the standalone, self-styling root for one chunked-graph
// document (RFC AK) — the chunk tree + Markdown view + chunk editor. Mount it
// on its own (a deep-link page) or let <PathExplorer> embed the shared body for
// a document dirent. It resolves its own data layer + wraps in the themeable
// `.loomcycle-explorer` root; styles ship separately:
// `import "@loomcycle/explorer/styles.css"`.
//
// `documentId` is the document the host opened. A reference into ANOTHER
// document is followed in place — the viewer shows that document and a
// breadcrumb back — unless the host takes navigation over with onOpenDocument.
export interface DocumentViewerProps extends ExplorerDataSource {
  documentId: string;
  /** The chunk to open on, in place of the document's root: a section a link
   *  or a search hit named. An id that is not one of the document's chunks
   *  (another document's, a deleted one) lands on the root. Changing it while
   *  documentId stays the same moves the selection without reloading. */
  chunkId?: string;
  scope: DocScope;
  /** Shown until the root chunk's title loads. */
  titleHint?: string;
  /** RFC AS browse-by-subject override threaded into every document call. */
  browse?: BrowseScope;
  /** Theming. Set → the root carries data-theme; omit → inherit an ancestor's
   *  data-theme (dark is the default palette). */
  theme?: "light" | "dark";
  /** Authenticated principal — only `subject` is read (passed to renderAssistant). */
  principal?: Principal;
  /** Optional Document Assistant slot. Provide it to show an "assistant" toggle
   *  that renders the returned node into the panel; omit → no assistant. */
  renderAssistant?: (ctx: AssistantContext) => ReactNode;
  /** Take over "open another document" (a followed cross-document reference),
   *  e.g. to route to it. `target` is the document + chunk to open; `from` is
   *  the document being left, with the chunk selected in it. Omit → the viewer
   *  swaps the document itself and keeps a breadcrumb trail back. */
  onOpenDocument?: (target: DocLocation, from: DocLocation) => void;
  /** Have an annotation written again (tenant-scope documents). The host owns
   *  how — typically by starting the annotating agent's run; the viewer reloads
   *  the annotation when the returned promise settles. Omit → no Regenerate. */
  onRegenerateAnnotation?: (target: AnnotationTarget) => void | Promise<void>;
  /** Whether the reader may edit annotations by hand (the operator plane; the
   *  runtime refuses anyone else). Default false: no Edit is offered. */
  canEditAnnotations?: boolean;
}

export default function DocumentViewer(props: DocumentViewerProps) {
  const { documentId, scope, browse, theme } = props;
  const resolved = useResolvedDataLayer(props);
  return (
    <ExplorerRoot theme={theme} dataLayer={resolved}>
      {/* Keyed on what the host opened: a new document / scope / subject from the
          host is a fresh navigation, and drops the trail. */}
      <DocumentViewerHost
        key={`${scope}:${documentId}:${browse?.scopeId ?? ""}:${browse?.tenant ?? ""}`}
        {...props}
      />
    </ExplorerRoot>
  );
}

// DocumentViewerHost owns the trail for the standalone viewer: the document on
// screen is the trail's last entry, or the host's `documentId` until a reference
// has been followed.
function DocumentViewerHost({
  documentId,
  chunkId,
  scope,
  titleHint,
  browse,
  principal,
  renderAssistant,
  onOpenDocument,
  onRegenerateAnnotation,
  canEditAnnotations,
}: DocumentViewerProps) {
  const [trail, setTrail] = useState<Trail>([]);
  const current = trail[trail.length - 1];

  // The host's chunk: the landing chunk on mount, and later changes of it
  // move the selection in the mounted document (focusChunk), or — when the
  // reader has followed a reference elsewhere — bring them back to the host's
  // document at that chunk.
  const [focus, setFocus] = useState<{ chunkId?: string; seq: number }>();
  const hostChunk = useRef(chunkId);
  useEffect(() => {
    if (chunkId === hostChunk.current) return;
    hostChunk.current = chunkId;
    if (trail.length > 0) {
      // Back to the host's document: the body remounts and lands on chunkId,
      // so no earlier focus request may be replayed into it.
      setFocus(undefined);
      setTrail([]);
    } else setFocus((f) => ({ chunkId, seq: (f?.seq ?? 0) + 1 }));
    // trail is read, not watched: only a change of the host's chunk moves here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chunkId]);

  const open = useCallback(
    (target: DocLocation, from: DocLocation) => {
      if (onOpenDocument) onOpenDocument(target, from);
      else setTrail((t) => followReference(t, from, target));
    },
    [onOpenDocument],
  );
  const jump = useCallback((index: number) => setTrail((t) => jumpTo(t, index)), []);
  const onTitle = useCallback(
    (id: string, title: string) => setTrail((t) => amendCurrent(t, id, { title })),
    [],
  );

  return (
    <>
      <DocTrail trail={trail} onJump={jump} />
      {/* Keyed on the trail depth so each step remounts the viewer and lands on
          that step's chunk. */}
      <DocumentViewerBody
        key={trail.length}
        documentId={current?.documentId ?? documentId}
        scope={scope}
        titleHint={current ? current.title : titleHint}
        browse={browse}
        principal={principal}
        renderAssistant={renderAssistant}
        initialChunkId={current ? current.chunkId : chunkId}
        focusChunk={current ? undefined : focus}
        onOpenDocument={open}
        onTitle={onTitle}
        onRegenerateAnnotation={onRegenerateAnnotation}
        canEditAnnotations={canEditAnnotations}
      />
    </>
  );
}
