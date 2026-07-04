// Public API of @loomcycle/library — the embeddable Agents / Skills / MCP-Servers
// substrate browser. Keep this surface small and intentional; it's the contract
// consumers depend on.
//
// Styles ship separately: `import "@loomcycle/library/styles.css"`.

export { default as Library, AgentsLibrary, SkillsLibrary, McpLibrary } from "./Library";
export type { LibraryProps, LibraryTab, LibraryActionCapabilities } from "./Library";

// Data types (the shapes the components render / the data layer produces).
export type {
  DefRow,
  DefListByNameResponse,
  LibraryEntry,
  LibraryListResponse,
  SubstrateKind,
  ServerCapabilities,
  Principal,
} from "./types";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient } from "./lib/dataLayer";
export type { LibraryDataLayer } from "./lib/dataLayer";
