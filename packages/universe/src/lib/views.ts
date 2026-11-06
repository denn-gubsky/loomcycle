import type { CameraPose, UniverseMode } from "../render/scene";

// A saved universe view is a `type=view` Document, the same primitive the
// boards package (@loomcycle/loomboard) keeps its saved views in: `{query,
// layout}` in the root chunk's fields. Its layout kind is "universe", which is
// how a board list tells it apart and hands it to a host that can show stars.
// Parsing is defensive: a hand-edited or partial view opens with defaults
// instead of throwing.

export interface UniverseViewState {
  mode: UniverseMode;
  planar: boolean;
  /** Hops of links drawn around the selection (1–3). */
  depth: number;
  /** Dim the stars outside the selection's neighbourhood. */
  focus: boolean;
  /** Link kinds switched off in the legend. */
  hiddenKinds: string[];
  /** The snapshot filters (a document type and status). */
  type?: string;
  status?: string;
  camera?: CameraPose;
  selection?: { documentId: string; chunkId?: string };
}

export interface SavedUniverseView {
  documentId: string;
  rootChunkId: string;
  title: string;
  revision?: number;
  state: UniverseViewState;
}

export const UNIVERSE_LAYOUT_KIND = "universe";

export const DEFAULT_VIEW_STATE: UniverseViewState = {
  mode: "structure",
  planar: false,
  depth: 1,
  focus: true,
  hiddenKinds: [],
};

/** viewFields is what a saved view stores in its root chunk's fields. */
export function viewFields(state: UniverseViewState): Record<string, unknown> {
  const query: Record<string, unknown> = { source: UNIVERSE_LAYOUT_KIND };
  if (state.type) query.type = state.type;
  if (state.status) query.status = state.status;
  const layout: Record<string, unknown> = {
    kind: UNIVERSE_LAYOUT_KIND,
    mode: state.mode,
    planar: state.planar,
    depth: state.depth,
    focus: state.focus,
    hiddenKinds: state.hiddenKinds,
  };
  if (state.camera) layout.camera = state.camera;
  if (state.selection) layout.selection = state.selection;
  return { query, layout };
}

const isVec3 = (v: unknown): v is [number, number, number] =>
  Array.isArray(v) && v.length === 3 && v.every((x) => typeof x === "number" && Number.isFinite(x));

/** parseUniverseView reads a view document's root chunk, or null when it is
 *  not a universe view (a board's table or kanban). */
export function parseUniverseView(
  documentId: string,
  root: { id: string; title?: string; revision?: number; fields?: unknown },
): SavedUniverseView | null {
  const f = (root.fields ?? {}) as { query?: unknown; layout?: unknown };
  const layout = (f.layout && typeof f.layout === "object" ? f.layout : {}) as Record<string, unknown>;
  if (layout.kind !== UNIVERSE_LAYOUT_KIND) return null;
  const query = (f.query && typeof f.query === "object" ? f.query : {}) as Record<string, unknown>;
  const state: UniverseViewState = {
    mode: layout.mode === "meaning" ? "meaning" : "structure",
    planar: layout.planar === true,
    depth: typeof layout.depth === "number" && layout.depth >= 1 && layout.depth <= 3 ? Math.round(layout.depth) : 1,
    focus: layout.focus !== false,
    hiddenKinds: Array.isArray(layout.hiddenKinds)
      ? layout.hiddenKinds.filter((k): k is string => typeof k === "string")
      : [],
  };
  if (typeof query.type === "string" && query.type) state.type = query.type;
  if (typeof query.status === "string" && query.status) state.status = query.status;
  const cam = layout.camera as Record<string, unknown> | undefined;
  if (cam && isVec3(cam.position) && isVec3(cam.target)) state.camera = { position: cam.position, target: cam.target };
  const sel = layout.selection as Record<string, unknown> | undefined;
  if (sel && typeof sel.documentId === "string" && sel.documentId) {
    state.selection = { documentId: sel.documentId };
    if (typeof sel.chunkId === "string" && sel.chunkId) state.selection.chunkId = sel.chunkId;
  }
  return { documentId, rootChunkId: root.id, title: root.title ?? "", revision: root.revision, state };
}

/** viewPath is where a new saved view is filed: under /views/universe, by a
 *  slug of its title plus a short random tail so two views may share a title. */
export function viewPath(title: string, rand: () => number = Math.random): string {
  let slug = title
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[^a-z0-9]+/g, "-");
  // Every run is one dash after the collapse, so there is at most one at each
  // end (and no /^-+|-+$/, which backtracks on a long run of dashes).
  if (slug.startsWith("-")) slug = slug.slice(1);
  if (slug.endsWith("-")) slug = slug.slice(0, -1);
  slug = slug.slice(0, 48) || "view";
  const tail = Math.floor(rand() * 0xffff)
    .toString(16)
    .padStart(4, "0");
  return `/views/universe/${slug}-${tail}`;
}
