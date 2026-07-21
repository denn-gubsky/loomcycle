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
  Principal,
  AssistantContext,
} from "./types";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient } from "./lib/dataLayer";
export type { ExplorerDataLayer, ChunkPatch } from "./lib/dataLayer";

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
