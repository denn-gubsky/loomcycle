import { createContext, useContext, type ReactNode } from "react";
import type {
  LoomcycleClient,
  MemoryScopesResponse,
  MemoryScopeIDsResponse,
  MemoryEntryResponse,
  MemoryEmbedStatsResponse,
  MemoryReembedResponse,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
} from "@loomcycle/client";
import type { MemoryEntriesResponse, MemoryScope } from "../types";

// MemoryDataLayer is the narrow data contract the memory console needs — the
// eight off-run reads/writes the k/v browser makes against /v1/_memory/*.
// Decoupling behind this interface lets a host inject the default client-backed
// implementation (dataLayerFromClient), or a custom one (e.g. a cookie-authed
// same-origin fetcher) without the console importing any global api module.
//
// Unlike @loomcycle/explorer's data layer, no method threads a browse-by-subject
// override: the off-run memory endpoints resolve the caller's own subject in P4a
// (the RFC AS browse-by-subject re-gate for /v1/_memory is a later stage). Keep
// the interface easy to EXTEND — P4b adds `search` / `listFacts` / `getChunk`
// here for the fact viewer + unified search; those are deliberately NOT part of
// this stage.
export interface MemoryDataLayer {
  // The kinds of scopes the server declares (agent, user, or operator yaml).
  listScopes(): Promise<MemoryScopesResponse>;
  // The scope_ids that have at least one row under a scope, with key counts.
  listScopeIDs(scope: MemoryScope): Promise<MemoryScopeIDsResponse>;
  // Entries under a (scope, scope_id) tuple. `prefix` narrows by key prefix;
  // `limit` caps the row count (the response's `truncated` flag signals a
  // clipped tail). The embedding_metadata map is present only when the data
  // source supplies it (the default client path does not — see MemoryEmbeddingMeta).
  listEntries(
    scope: MemoryScope,
    scopeId: string,
    opts?: { prefix?: string; limit?: number },
  ): Promise<MemoryEntriesResponse>;
  // One entry by (scope, scope_id, key).
  getEntry(scope: MemoryScope, scopeId: string, key: string): Promise<MemoryEntryResponse>;
  // Idempotent upsert of one entry (PUT semantics). `embed:true` triggers a
  // synchronous embed via the operator-configured embedder — the response's
  // embed_warning reports a failed embed (the k/v row still landed).
  setEntry(
    scope: MemoryScope,
    scopeId: string,
    key: string,
    input: SetMemoryEntryOptions,
  ): Promise<SetMemoryEntryResponse>;
  // Delete one entry (idempotent — a missing row is a non-error).
  deleteEntry(scope: MemoryScope, scopeId: string, key: string): Promise<void>;
  // Per-(provider, model, dimension) embedding row counts for a scope. REJECTS
  // (the runtime 503s) when the store has no vector support / no embedder
  // configured; the console catches that and renders a "not configured" hint
  // rather than an error banner.
  embedStats(scope: MemoryScope): Promise<MemoryEmbedStatsResponse>;
  // Re-embed a scope_id's rows under the configured embedder. `dryRun` (default
  // TRUE server-side; the console passes it explicitly) plans WITHOUT writing;
  // `dryRun:false` commits. The response is a discriminated union on `dry_run`.
  reembed(
    scope: MemoryScope,
    scopeId: string,
    opts: { dryRun?: boolean; limit?: number },
  ): Promise<MemoryReembedResponse>;
}

// dataLayerFromClient maps a @loomcycle/client instance onto the MemoryDataLayer.
// Every method routes to the client's off-run memory admin method 1:1; the
// client owns URL construction + auth transport. `listEntries`'s response is the
// client's (which omits embedding_metadata) — structurally assignable to the
// package's wider shape, so no cast is needed.
export function dataLayerFromClient(client: LoomcycleClient): MemoryDataLayer {
  return {
    listScopes: () => client.listMemoryScopes(),
    listScopeIDs: (scope) => client.listMemoryScopeIDs(scope),
    listEntries: (scope, scopeId, opts) =>
      client.listMemoryEntries(scope, scopeId, opts),
    getEntry: (scope, scopeId, key) => client.getMemoryEntry(scope, scopeId, key),
    setEntry: (scope, scopeId, key, input) =>
      client.setMemoryEntry(scope, scopeId, key, input),
    deleteEntry: (scope, scopeId, key) =>
      client.deleteMemoryEntry(scope, scopeId, key),
    embedStats: (scope) => client.memoryEmbedStats(scope),
    reembed: (scope, scopeId, opts) => client.reembedMemory(scope, scopeId, opts),
  };
}

// The data layer reaches the console through context — no module-global
// singleton. The root (<MemoryView>) builds it once (useMemo over connection
// identity) and provides it; the console body + edit modal read it via
// useMemoryData().
const MemoryDataContext = createContext<MemoryDataLayer | null>(null);

export function MemoryDataProvider({
  value,
  children,
}: {
  value: MemoryDataLayer;
  children: ReactNode;
}) {
  return (
    <MemoryDataContext.Provider value={value}>
      {children}
    </MemoryDataContext.Provider>
  );
}

export function useMemoryData(): MemoryDataLayer {
  const v = useContext(MemoryDataContext);
  if (!v) {
    throw new Error(
      "useMemoryData must be used within <MemoryView> (no MemoryDataLayer in context)",
    );
  }
  return v;
}
