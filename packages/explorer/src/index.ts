// Public API of @loomcycle/explorer — the embeddable Path VFS (RFC AL) + chunked-
// graph Document (RFC AK) browser. Keep this surface small and intentional; it's
// the contract consumers depend on.
//
// Styles ship separately: `import "@loomcycle/explorer/styles.css"`.

export { default as PathExplorer } from "./PathExplorer";
export type { PathExplorerProps } from "./PathExplorer";

export { default as DocumentViewer } from "./DocumentViewer";
export type { DocumentViewerProps } from "./DocumentViewer";

// Data types (the shapes the components render / the data layer produces) + the
// injected-slot context.
export type {
  PathScope,
  DocScope,
  BrowseScope,
  PathEntry,
  ChunkRow,
  ChunkDetail,
  DocEdge,
  Backlink,
  StatementStatus,
  StatementFields,
  EdgeEvidence,
  RelatedChunk,
  UnlinkedMention,
  ChunkRevision,
  CanvasDoc,
  Principal,
  AssistantContext,
  ClassifyWrite,
  ClassifyResult,
  ClassifyCandidate,
  ClassifyStep,
  ClassifyIssuer,
  ClassifyConcept,
  ClassifyStatement,
  ClassifyAnswer,
  AcceptClassificationAnswer,
  ClassificationRecord,
} from "./types";

// A stop on the document trail — the argument shape of DocumentViewer's
// onOpenDocument (a followed cross-document reference).
export type { DocLocation } from "./lib/docTrail";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient, assetFetchFromConnection, jsonPostFromConnection, RequestError } from "./lib/createClient";
export type { Connection, AssetFetch, JsonPost } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient } from "./lib/dataLayer";
export type {
  ExplorerDataLayer,
  ChunkPatch,
  DocEdgesPage,
  PathLsPage,
  PathLsResult,
  DocRulingFilter,
  DocRulingAnswer,
  DocRevertAnswer,
} from "./lib/dataLayer";
export { loadDocumentEdges, EDGE_PAGE_CAP } from "./lib/edgePages";

// RFC FS — a bulk ruling on proposed statements, run as every surface runs it
// (dry run first, expect_count, chunks of 1000), and its undo.
export { runRuling, undoRulings, isQueueChanged, chunkCount, RULING_CHUNK, QUEUE_CHANGED_RETRIES } from "./lib/ruling";
export type {
  RulingAction,
  RulingScope,
  RulingItem,
  RulingRequest,
  RulingCounts,
  RulingApi,
  RulingSelection,
  ConfirmReason,
  RulingProgress,
  RulingOutcome,
  RulingHooks,
  UndoOutcome,
} from "./lib/ruling";

// RFC BN per-document color schemes: the metadata shapes the data layer produces
// (documents_summary / get_document) + the palette resolvers, for a host that
// renders its own colored view.
export type { ColorScheme, DocSummary, DocumentMeta } from "./lib/colorScheme";
export {
  DEFAULT_SCHEME,
  effectiveScheme,
  docColor,
  chunkColor,
  hexToTint,
  tintStyle,
} from "./lib/colorScheme";

// The shared data-source contract (connection | client | dataLayer), for hosts
// typing their own wrappers.
export type { ExplorerDataSource } from "./components/ExplorerRoot";
