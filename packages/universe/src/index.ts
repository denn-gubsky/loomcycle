// Public API of @loomcycle/universe — the 3D star map of a loomcycle document
// scope (RFC FQ): documents as constellations of chunk stars, Path directories
// as formations. Keep this surface small and intentional.
//
// Styles ship separately: `import "@loomcycle/universe/styles.css"`.

export { default as UniverseNavigator, useUniverseDataLayer } from "./components/UniverseNavigator";
export type { UniverseNavigatorProps, UniverseDataSource } from "./components/UniverseNavigator";
export { default as UniverseView } from "./components/UniverseView";
export type { UniverseViewProps } from "./components/UniverseView";

export { dataLayerFromClient } from "./lib/dataLayer";
export { createLoomcycleClient } from "./lib/createClient";
export type { Connection } from "./lib/createClient";
export type { UniverseDataLayer } from "./lib/dataLayer";

export type {
  BrowseScope,
  DocScope,
  GraphSnapshot,
  GraphSnapshotInput,
  SearchHit,
  SnapshotInclude,
  SnapshotExternal,
  UniverseSelection,
} from "./types";
