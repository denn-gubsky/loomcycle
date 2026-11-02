import { createContext, useContext } from "react";
import type { DocumentToolInput, LoomcycleClient } from "@loomcycle/client";
import type { BrowseScope, DocScope, GraphSnapshot, GraphSnapshotInput, SearchHit } from "../types";

// UniverseDataLayer is the narrow contract the star map needs from the runtime:
// the paged scope snapshot, semantic search, and one document's edges when the
// snapshot's edge budget was cut. Hosts inject the client-backed default
// (dataLayerFromClient) or their own (a cookie-authed same-origin fetcher).
export interface UniverseDataLayer {
  graphSnapshot(input: GraphSnapshotInput, browse?: BrowseScope, signal?: AbortSignal): Promise<GraphSnapshot>;
  search(
    query: string,
    scope: DocScope,
    opts: { topK?: number; underPath?: string },
    browse?: BrowseScope,
    signal?: AbortSignal,
  ): Promise<SearchHit[]>;
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
      const res = (await doc(input, browse, signal)) as unknown as { chunks?: SearchHit[] };
      return res.chunks ?? [];
    },
  };
}

const Ctx = createContext<UniverseDataLayer | null>(null);
export const UniverseDataProvider = Ctx.Provider;

export function useUniverseData(): UniverseDataLayer {
  const d = useContext(Ctx);
  if (!d) throw new Error("@loomcycle/universe: no data layer — wrap in <UniverseView> or pass a data source");
  return d;
}
