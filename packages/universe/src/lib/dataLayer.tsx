import { createContext, useContext } from "react";
import type { DocumentToolInput, LoomcycleClient } from "@loomcycle/client";
import type { BrowseScope, DocScope, GraphSnapshot, GraphSnapshotInput, SearchHit } from "../types";

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

export function dataLayerFromClient(client: LoomcycleClient): UniverseDataLayer {
  const doc = (input: Record<string, unknown>, browse?: BrowseScope, signal?: AbortSignal) =>
    client.document(input as unknown as DocumentToolInput, {
      signal,
      scopeId: browse?.scopeId,
      tenant: browse?.tenant,
    });
  return {
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

const Ctx = createContext<UniverseDataLayer | null>(null);
export const UniverseDataProvider = Ctx.Provider;

export function useUniverseData(): UniverseDataLayer {
  const d = useContext(Ctx);
  if (!d) throw new Error("@loomcycle/universe: no data layer — wrap in <UniverseView> or pass a data source");
  return d;
}
