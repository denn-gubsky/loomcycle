// Public API of @loomcycle/memory-view — the embeddable Memory k/v console (RFC
// BV). Keep this surface small and intentional; it's the contract consumers
// depend on. P4b extends it with a FactViewer + a unified SearchPanel.
//
// Styles ship separately: `import "@loomcycle/memory-view/styles.css"`.

export { default as MemoryView } from "./MemoryView";
export type { MemoryViewProps, MemoryTab } from "./MemoryView";

// P4b — the fact viewer + unified search, exported standalone so a host
// (loomboard) can compose them without the tabbed <MemoryView> shell. The
// shared chunk/fact inspector is exported too, for a detail-only composition.
export { default as FactViewer } from "./components/FactViewer";
export type { FactViewerProps } from "./components/FactViewer";
export { default as SearchPanel } from "./components/SearchPanel";
export type { SearchPanelProps } from "./components/SearchPanel";
export { default as ChunkDetailPanel } from "./components/ChunkDetailPanel";
export type { ChunkDetailPanelProps } from "./components/ChunkDetailPanel";
export { default as ChangeFeedPanel } from "./components/ChangeFeedPanel";
export type { ChangeFeedPanelProps } from "./components/ChangeFeedPanel";

// The change-feed tail's transport + pure helpers, exported because a host with its
// own layout still wants the framing rules (a status frame that cannot be read is NOT
// enabled; a bounded buffer reports what it dropped) rather than a looser second copy.
export {
  tailChanges,
  classifyFrame,
  appendChange,
  matchesFilter,
  describeChange,
  emptyBuffer,
  isDocumentChange,
  CHANGE_BUFFER_CAP,
  CHANGE_FEED_ENV,
  CHANGE_FEED_PATHS,
} from "./lib/changeFeed";
export type {
  ChangeFamily,
  ChangeFeedFrame,
  ChangeFeedStatus,
  ChangeFilter,
  ChangeBuffer,
  MemoryChangeRow,
} from "./lib/changeFeed";

// Public data types (the shapes the console renders / the data layer produces).
export type {
  MemoryScope,
  MemoryScopeKind,
  MemoryScopesResponse,
  MemoryScopeIDSummary,
  MemoryScopeIDsResponse,
  MemoryEntry,
  MemoryEntriesResponse,
  MemoryEntryResponse,
  MemoryEmbeddingMeta,
  MemoryEmbedModelStats,
  MemoryEmbedStatsResponse,
  MemoryReembedConfigured,
  MemoryReembedResponse,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
  // P4b — unified search + the entity/fact tier shapes.
  MemorySearchInput,
  MemorySearchEntry,
  MemorySearchResponse,
  MemorySource,
  FactEntity,
  FactRow,
  FactListResponse,
  ChunkDetail,
  DocEdge,
  DocEdgesResponse,
} from "./types";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient, dataLayerFromConnection } from "./lib/dataLayer";
export type { MemoryDataLayer, FactListOptions } from "./lib/dataLayer";

// The shared data-source contract (connection | client | dataLayer), for hosts
// typing their own wrappers.
export type { MemoryDataSource } from "./components/MemoryViewRoot";
export type { MemoryBrowseOptions } from "./lib/dataLayer";
