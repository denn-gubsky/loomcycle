import { createContext, useContext, type ReactNode } from "react";
import type { LoomcycleClient, DocumentToolInput } from "@loomcycle/client";
import type {
  BoardScope,
  ChunkDetail,
  CreateDocumentResponse,
  ListTypesResponse,
  QueryChunksResponse,
  QueryDocumentsResponse,
  ViewLayout,
  ViewQuery,
} from "../types";

// LoomboardDataLayer is the narrow data contract the board needs: the reads that
// materialize a view and the writes that persist one. Decoupling behind this
// interface lets a host inject the default client-backed implementation
// (dataLayerFromClient) or a custom one (e.g. a cookie-authed same-origin
// fetcher) without any component importing a global api module. Every method
// routes to the single `document` meta-tool; scope + tenant are resolved
// server-side from the authenticated principal, never read from the wire.

export interface LoomboardDataLayer {
  /** Materialize a Documents board — filter by the RFC BS axes (type / status /
   *  tag / under_path). Response rows carry title/type/status/updated, so a
   *  Documents board renders without an N+1. */
  queryDocuments(scope: BoardScope, q: ViewQuery): Promise<QueryDocumentsResponse>;
  /** Materialize a Chunks board — the chunks of a document (or under a path).
   *  Response is metadata only (no fields/tags/body); rich chips need getChunk. */
  queryChunks(scope: BoardScope, q: ViewQuery): Promise<QueryChunksResponse>;
  /** One chunk's full row — the only shape carrying `fields` (a saved view's
   *  `{query, layout}`), `tags`, and `body`. */
  getChunk(scope: BoardScope, id: string): Promise<ChunkDetail>;
  /** The distinct chunk types declared in the scope — populates the query
   *  builder's `type` picker. (Advisory; `type` is a free-form column, so a
   *  board can still filter by a type never passed to define_type.) */
  listTypes(scope: BoardScope): Promise<ListTypesResponse>;

  // ---- saved views (a view is a `type=view` Document) ----

  /** All saved views in the scope. */
  listViews(scope: BoardScope): Promise<QueryDocumentsResponse>;
  /** Create a saved view: a `type=view` Document, then seed its root chunk's
   *  `fields` with `{query, layout}` (create_document can't carry root fields). */
  saveView(
    scope: BoardScope,
    input: { title: string; path?: string; query: ViewQuery; layout: ViewLayout },
  ): Promise<{ documentId: string; rootChunkId: string }>;
  /** Update a saved view's `{query, layout}`. `revision` is the root chunk's
   *  current revision (optimistic concurrency), read from a prior getChunk. */
  updateView(
    scope: BoardScope,
    rootChunkId: string,
    revision: number,
    patch: { query?: ViewQuery; layout?: ViewLayout },
  ): Promise<void>;
  /** Delete a saved view (the whole `type=view` Document). */
  deleteView(scope: BoardScope, documentId: string): Promise<void>;

  /** Move a row on a kanban: set a chunk's `status`. `revision` is the row's
   *  current revision. (P1.1 — the board renders read-first without it.) */
  setStatus(
    scope: BoardScope,
    id: string,
    revision: number,
    status: string,
  ): Promise<void>;
}

// dataLayerFromClient maps a @loomcycle/client instance onto the LoomboardDataLayer.
// Every method builds a minimal Document-tool body (only set fields sent, so the
// wire stays small and the routing tests assert exact shapes) and narrows the
// `unknown` response to the package's own type.
export function dataLayerFromClient(client: LoomcycleClient): LoomboardDataLayer {
  const doc = <T,>(input: Record<string, unknown>): Promise<T> =>
    client.document(toDocInput(input)).then((r) => r as T);

  return {
    queryDocuments: (scope, q) => {
      const input: Record<string, unknown> = { op: "query_documents", scope };
      if (q.type) input.type = q.type;
      if (q.status) input.status = q.status;
      if (q.tag) input.tag = q.tag;
      if (q.underPath) input.under_path = q.underPath;
      if (q.limit !== undefined) input.limit = q.limit;
      return doc<QueryDocumentsResponse>(input);
    },
    queryChunks: (scope, q) => {
      const input: Record<string, unknown> = { op: "query_chunks", scope };
      if (q.documentId) input.document_id = q.documentId;
      if (q.underPath) input.under_path = q.underPath;
      if (q.type) input.type = q.type;
      if (q.status) input.status = q.status;
      if (q.tag) input.tag = q.tag;
      if (q.tagPrefix) input.tag_prefix = q.tagPrefix;
      if (q.limit !== undefined) input.limit = q.limit;
      return doc<QueryChunksResponse>(input);
    },
    getChunk: (scope, id) => doc<ChunkDetail>({ op: "get_chunk", scope, id }),
    listTypes: (scope) => doc<ListTypesResponse>({ op: "list_types", scope }),

    listViews: (scope) =>
      doc<QueryDocumentsResponse>({ op: "query_documents", scope, type: "view" }),

    saveView: async (scope, input) => {
      const created = await doc<CreateDocumentResponse>({
        op: "create_document",
        scope,
        title: input.title,
        type: "view",
        ...(input.path ? { path: input.path } : {}),
      });
      // create_document seeds an empty root at revision 1, so the follow-up seed
      // write uses revision 1. `{query, layout}` are stored opaquely in `fields`.
      await doc<unknown>({
        op: "update_chunk",
        scope,
        id: created.root_chunk_id,
        revision: 1,
        fields: { query: input.query, layout: input.layout },
      });
      return { documentId: created.document_id, rootChunkId: created.root_chunk_id };
    },
    updateView: async (scope, rootChunkId, revision, patch) => {
      const fields: Record<string, unknown> = {};
      if (patch.query) fields.query = patch.query;
      if (patch.layout) fields.layout = patch.layout;
      await doc<unknown>({
        op: "update_chunk",
        scope,
        id: rootChunkId,
        revision,
        fields,
      });
    },
    deleteView: async (scope, documentId) => {
      await doc<unknown>({ op: "delete_document", scope, id: documentId });
    },

    setStatus: async (scope, id, revision, status) => {
      await doc<unknown>({ op: "update_chunk", scope, id, revision, status });
    },
  };
}

// toDocInput asserts a plain field bag onto DocumentToolInput. BoardScope is a
// narrowed string and the pinned client's op union predates some ops we send;
// the runtime passes op + scope VERBATIM and DocumentToolInput carries a
// `[extra: string]: unknown` index signature, so a Record is structurally
// assignable and the assertion is sound, not a cast past a real type error.
function toDocInput(fields: Record<string, unknown>): DocumentToolInput {
  return fields as DocumentToolInput;
}

// The data layer reaches the board through context — no module-global singleton.
// The root (<Loomboard>) builds it once (useMemo over connection identity) and
// provides it; every panel reads it with useLoomboardData().
const LoomboardDataContext = createContext<LoomboardDataLayer | null>(null);

export function LoomboardDataProvider({
  value,
  children,
}: {
  value: LoomboardDataLayer;
  children: ReactNode;
}) {
  return (
    <LoomboardDataContext.Provider value={value}>
      {children}
    </LoomboardDataContext.Provider>
  );
}

export function useLoomboardData(): LoomboardDataLayer {
  const v = useContext(LoomboardDataContext);
  if (!v) {
    throw new Error(
      "useLoomboardData must be used within <Loomboard> (no LoomboardDataLayer in context)",
    );
  }
  return v;
}
