import { createContext, useContext } from "react";
import type { DocumentToolInput, LoomcycleClient } from "@loomcycle/client";
import type { BrowseScope, DocScope, GraphSnapshot, GraphSnapshotInput, SearchHit } from "../types";
import { parseUniverseView, viewFields, viewPath, type SavedUniverseView, type UniverseViewState } from "./views";
import { feedOverConnection, type FeedFrame } from "./feed";
import type { Connection } from "./createClient";

// UniverseDataLayer is the narrow contract the star map needs from the runtime:
// the paged scope snapshot, semantic search, and (optionally) the statement
// writes behind link authoring and review. Hosts inject the client-backed
// default (dataLayerFromClient) or their own (a cookie-authed same-origin
// fetcher). The runtime decides who may write: the map shows its refusal.
export interface UniverseDataLayer {
  graphSnapshot(input: GraphSnapshotInput, browse?: BrowseScope, signal?: AbortSignal): Promise<GraphSnapshot>;
  /** Document `search` (hybrid text + vector over chunk bodies). With
   *  `withProjection`, a runtime that has an embedding basis also returns the
   *  query's own projected point. */
  search(
    query: string,
    scope: DocScope,
    opts: { topK?: number; underPath?: string; withProjection?: boolean },
    browse?: BrowseScope,
    signal?: AbortSignal,
  ): Promise<SearchAnswer>;
  /** Off-run memory search (POST /v1/_memory/search): facts, notes and
   *  document chunks in one ranked list. Optional; without it the panel offers
   *  no memory mode. */
  memorySearch?(
    query: string,
    scope: DocScope,
    scopeId: string,
    opts: { topK?: number },
    signal?: AbortSignal,
  ): Promise<MemoryHit[]>;
  /** Write a link between two chunks. "propose" lands it for review
   *  (propose_statement), "assert" sets it at once (link_chunks). Optional;
   *  without it the map offers no link button. */
  writeLink?(req: LinkWrite, browse?: BrowseScope): Promise<StatementResult>;
  /** Rule on a proposed link (accept_statement / reject_statement). A
   *  rejection needs a reason. Optional; without it proposals are read-only. */
  reviewLink?(req: LinkReview, browse?: BrowseScope): Promise<StatementResult>;
  /** Saved universe views (type=view documents of layout kind "universe").
   *  Optional, as a set: without them the map offers no views menu. */
  listViews?(scope: DocScope, browse?: BrowseScope): Promise<SavedUniverseView[]>;
  getView?(scope: DocScope, documentId: string, browse?: BrowseScope): Promise<SavedUniverseView | null>;
  saveView?(scope: DocScope, title: string, state: UniverseViewState, browse?: BrowseScope): Promise<SavedUniverseView>;
  deleteView?(scope: DocScope, documentId: string, browse?: BrowseScope): Promise<void>;
  /** The member-safe document change feed (GET /v1/_document/feed): frames
   *  for documents the caller can list, in one scope. Optional; without it
   *  (or with the feed off) the map reloads when the tab comes back after a
   *  while instead. */
  documentFeed?(scope: DocScope, browse: BrowseScope | undefined, signal: AbortSignal): AsyncIterable<FeedFrame>;
}

export interface LinkWrite {
  scope: DocScope;
  fromId: string;
  toId: string;
  kind: string;
  mode: "propose" | "assert";
}

export interface LinkReview {
  scope: DocScope;
  id: string;
  verdict: "accept" | "reject";
  reason?: string;
}

export interface StatementResult {
  id: string;
  status: string;
}

export interface SearchAnswer {
  hits: SearchHit[];
  /** The query's point in the projection basis, when asked for and built. */
  queryXyz?: [number, number, number];
}

/** One memory search entry: a chunk-backed one can be placed on the map. */
export interface MemoryHit {
  key: string;
  kind: "fact" | "note" | "document";
  score: number;
  chunkId?: string;
  title?: string;
  document?: string;
}

/** A client's own documentFeed (newer @loomcycle/client), when it has one. */
interface FeedingClient {
  documentFeed(opts: {
    scope: "user" | "tenant";
    scopeId?: string;
    tenant?: string;
    signal?: AbortSignal;
  }): AsyncIterable<{ kind: string; payload: Record<string, unknown> }>;
}

async function* feedOverClient(
  client: FeedingClient,
  scope: DocScope,
  browse: BrowseScope | undefined,
  signal: AbortSignal,
): AsyncIterable<FeedFrame> {
  if (scope === "agent") return;
  for await (const f of client.documentFeed({ scope, scopeId: browse?.scopeId, tenant: browse?.tenant, signal })) {
    const p = f.payload;
    if (f.kind === "feed") yield { kind: "feed", enabled: p.enabled === true };
    else if (f.kind === "error") yield { kind: "error", code: String(p.code ?? ""), message: String(p.error ?? "") };
    else if (f.kind === "change" && typeof p.document_id === "string")
      yield { kind: "change", type: String(p.type), documentId: p.document_id, chunkId: String(p.chunk_id ?? "") };
  }
}

/** dataLayerFromClient builds the default data layer. With the host's
 *  connection the change feed is read over it (any client version); with a
 *  client alone, through the client's documentFeed when it has one. */
export function dataLayerFromClient(client: LoomcycleClient, connection?: Connection): UniverseDataLayer {
  const feeding = client as unknown as Partial<FeedingClient>;
  const documentFeed: UniverseDataLayer["documentFeed"] = connection
    ? (scope, browse, signal) => feedOverConnection(connection, scope, browse, signal)
    : typeof feeding.documentFeed === "function"
      ? (scope, browse, signal) => feedOverClient(feeding as FeedingClient, scope, browse, signal)
      : undefined;
  const doc = (input: Record<string, unknown>, browse?: BrowseScope, signal?: AbortSignal) =>
    client.document(input as unknown as DocumentToolInput, {
      signal,
      scopeId: browse?.scopeId,
      tenant: browse?.tenant,
    });
  return {
    documentFeed,
    graphSnapshot: (input, browse, signal) =>
      doc({ op: "graph_snapshot", ...input }, browse, signal) as unknown as Promise<GraphSnapshot>,
    search: async (query, scope, opts, browse, signal) => {
      const input: Record<string, unknown> = { op: "search", scope, query, limit: opts.topK ?? 20 };
      if (opts.underPath) input.under_path = opts.underPath;
      if (opts.withProjection) input.with_projection = true;
      const res = (await doc(input, browse, signal)) as unknown as {
        chunks?: SearchHit[];
        query_xyz?: [number, number, number];
      };
      return { hits: res.chunks ?? [], queryXyz: res.query_xyz };
    },
    memorySearch: async (query, scope, scopeId, opts, signal) => {
      const res = await client.memorySearch({ query, scope, scopeId, topK: opts.topK ?? 30 }, { signal });
      return (res.entries ?? []).map((e) => ({
        key: e.key,
        kind: e.kind,
        score: e.score,
        chunkId: e.chunk_id,
        title: e.title,
        document: e.document,
      }));
    },
    writeLink: (req, browse) =>
      doc(
        {
          op: req.mode === "assert" ? "link_chunks" : "propose_statement",
          scope: req.scope,
          from_id: req.fromId,
          to_id: req.toId,
          kind: req.kind,
        },
        browse,
      ) as unknown as Promise<StatementResult>,
    listViews: async (scope, browse) => {
      const res = (await doc({ op: "query_documents", scope, type: "view", limit: 200 }, browse)) as unknown as {
        documents?: { document_id: string; root_chunk_id: string }[];
      };
      // A view's kind lives in its root's fields, which the listing does not
      // carry: read each root (views are few) and keep the universe ones.
      const roots = await Promise.all(
        (res.documents ?? []).map(async (d) => {
          const root = (await doc({ op: "get_chunk", scope, id: d.root_chunk_id }, browse)) as unknown as ViewRoot;
          return parseUniverseView(d.document_id, root);
        }),
      );
      return roots.filter((v): v is SavedUniverseView => v !== null);
    },
    getView: async (scope, documentId, browse) => {
      const d = (await doc({ op: "get_document", scope, id: documentId }, browse)) as unknown as { root_chunk_id: string };
      const root = (await doc({ op: "get_chunk", scope, id: d.root_chunk_id }, browse)) as unknown as ViewRoot;
      return parseUniverseView(documentId, root);
    },
    saveView: async (scope, title, state, browse) => {
      const created = (await doc(
        { op: "create_document", scope, title, type: "view", path: viewPath(title) },
        browse,
      )) as unknown as { document_id: string; root_chunk_id: string };
      // create_document seeds an empty root at revision 1, as the boards
      // package relies on; the view's settings go into its fields.
      await doc({ op: "update_chunk", scope, id: created.root_chunk_id, revision: 1, fields: viewFields(state) }, browse);
      return { documentId: created.document_id, rootChunkId: created.root_chunk_id, title, revision: 2, state };
    },
    deleteView: async (scope, documentId, browse) => {
      await doc({ op: "delete_document", scope, id: documentId }, browse);
    },
    reviewLink: (req, browse) =>
      doc(
        {
          op: req.verdict === "accept" ? "accept_statement" : "reject_statement",
          scope: req.scope,
          id: req.id,
          ...(req.reason ? { reason: req.reason } : {}),
        },
        browse,
      ) as unknown as Promise<StatementResult>,
  };
}

interface ViewRoot {
  id: string;
  title?: string;
  revision?: number;
  fields?: unknown;
}

const Ctx = createContext<UniverseDataLayer | null>(null);
export const UniverseDataProvider = Ctx.Provider;

export function useUniverseData(): UniverseDataLayer {
  const d = useContext(Ctx);
  if (!d) throw new Error("@loomcycle/universe: no data layer — wrap in <UniverseView> or pass a data source");
  return d;
}
