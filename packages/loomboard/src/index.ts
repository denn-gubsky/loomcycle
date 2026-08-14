// Public API of @loomcycle/loomboard — the embeddable view layer over loomcycle
// Documents (RFC BT P1: saved table / cards / kanban / list views). Keep this
// surface small and intentional; it's the contract consumers depend on.
//
// Styles ship separately: `import "@loomcycle/loomboard/styles.css"`.

export { default as Loomboard } from "./Loomboard";
export type { LoomboardProps } from "./Loomboard";

// Public data types (the shapes the board renders / the data layer produces).
export type {
  BoardScope,
  ViewQuery,
  ViewLayout,
  LayoutKind,
  GroupAxis,
  SortField,
  SavedView,
  BoardRow,
  DocRow,
  ChunkRow,
  ChunkDetail,
  TypeRow,
  ListTypesResponse,
  QueryDocumentsResponse,
  QueryChunksResponse,
  CreateDocumentResponse,
} from "./types";

// Connection → client factory (the default data-source path).
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";

// The data-layer seam: inject a custom implementation, or build one from a
// @loomcycle/client instance.
export { dataLayerFromClient } from "./lib/dataLayer";
export type { LoomboardDataLayer } from "./lib/dataLayer";

// The shared data-source contract (connection | client | dataLayer), for hosts
// typing their own wrappers.
export type { LoomboardDataSource } from "./components/LoomboardRoot";

// Pure view helpers — parse a saved view's fields, normalize / group / sort rows.
// Exported for hosts composing their own surfaces over the same primitives.
export {
  parseView,
  normalizeQuery,
  normalizeLayout,
  groupRows,
  sortRows,
} from "./lib/view";
