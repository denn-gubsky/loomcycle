// Public API of @loomcycle/memory-view — the embeddable Memory k/v console (RFC
// BV). Keep this surface small and intentional; it's the contract consumers
// depend on. P4b extends it with a FactViewer + a unified SearchPanel.
//
// Styles ship separately: `import "@loomcycle/memory-view/styles.css"`.

export { default as MemoryView } from "./MemoryView";
export type { MemoryViewProps } from "./MemoryView";

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
} from "./types";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient } from "./lib/dataLayer";
export type { MemoryDataLayer } from "./lib/dataLayer";

// The shared data-source contract (connection | client | dataLayer), for hosts
// typing their own wrappers.
export type { MemoryDataSource } from "./components/MemoryViewRoot";
