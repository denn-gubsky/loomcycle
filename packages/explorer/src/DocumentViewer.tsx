import { type ReactNode } from "react";
import type { AssistantContext, BrowseScope, DocScope, Principal } from "./types";
import {
  ExplorerRoot,
  useResolvedDataLayer,
  type ExplorerDataSource,
} from "./components/ExplorerRoot";
import DocumentViewerBody from "./components/DocumentViewerBody";

// DocumentViewer is the standalone, self-styling root for one chunked-graph
// document (RFC AK) — the chunk tree + Markdown view + chunk editor. Mount it
// on its own (a deep-link page) or let <PathExplorer> embed the shared body for
// a document dirent. It resolves its own data layer + wraps in the themeable
// `.loomcycle-explorer` root; styles ship separately:
// `import "@loomcycle/explorer/styles.css"`.
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
}

export default function DocumentViewer(props: DocumentViewerProps) {
  const { documentId, scope, titleHint, browse, theme, principal, renderAssistant } = props;
  const resolved = useResolvedDataLayer(props);
  return (
    <ExplorerRoot theme={theme} dataLayer={resolved}>
      <DocumentViewerBody
        documentId={documentId}
        scope={scope}
        titleHint={titleHint}
        browse={browse}
        principal={principal}
        renderAssistant={renderAssistant}
      />
    </ExplorerRoot>
  );
}
