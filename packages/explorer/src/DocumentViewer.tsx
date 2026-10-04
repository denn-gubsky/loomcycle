import { useCallback, useState, type ReactNode } from "react";
import type { AssistantContext, BrowseScope, DocScope, Principal } from "./types";
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
  scope,
  titleHint,
  browse,
  principal,
  renderAssistant,
  onOpenDocument,
}: DocumentViewerProps) {
  const [trail, setTrail] = useState<Trail>([]);
  const current = trail[trail.length - 1];

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
        initialChunkId={current?.chunkId}
        onOpenDocument={open}
        onTitle={onTitle}
      />
    </>
  );
}
