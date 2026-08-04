// Public data types for @loomcycle/memory-view. These are the shapes the memory
// console renders and the data layer produces. Most are the exact wire shapes
// the runtime returns for the off-run /v1/_memory/* endpoints, so they are
// re-exported straight from @loomcycle/client (the peer SDK) rather than
// duplicated — the package has no dependency on the app's global api module; the
// runtime is reached through an injected LoomcycleClient / MemoryDataLayer (see
// lib/dataLayer). Only the shapes the client doesn't model live here.

import type { MemoryEntriesResponse as ClientMemoryEntriesResponse } from "@loomcycle/client";

// Wire shapes owned by @loomcycle/client — re-exported so a host types its own
// wrapper without a second import.
export type {
  MemoryScopeKind,
  MemoryScopesResponse,
  MemoryScopeIDSummary,
  MemoryScopeIDsResponse,
  MemoryEntry,
  MemoryEntryResponse,
  MemoryEmbedModelStats,
  MemoryEmbedStatsResponse,
  MemoryReembedConfigured,
  MemoryReembedResponse,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
} from "@loomcycle/client";

// MemoryScope selects WHICH scope's rows to browse (agent / user — or whatever
// the operator yaml declares). It is a freeform string, not a fixed union: memory
// scopes are operator-configurable and enumerated at runtime (listScopes), unlike
// the fixed Path/Document scope set. It is a subtree SELECTOR, not an authority
// grant — the runtime resolves the caller's tenant + subject from the
// authenticated principal and re-authorizes.
export type MemoryScope = string;

// MemoryEmbeddingMeta is the per-key embedding descriptor the keys listing
// carries when the caller requested include_embedding_metadata (RFC I MR-6):
// which embedder wrote the row. Keys without an embedding are simply absent from
// the embedding_metadata map. NOTE: @loomcycle/client 1.47.0's listMemoryEntries
// does not send that query flag, so the default client-backed data layer never
// populates it (the field stays absent and the per-key badge is inert) — a
// custom MemoryDataLayer, or a future client bump, can supply it. Kept optional
// so the console renders nothing when it's absent (same as a non-vector store).
export interface MemoryEmbeddingMeta {
  provider: string;
  model: string;
  dimension: number;
}

// MemoryEntriesResponse extends the client's shape with the optional per-key
// embedding metadata map. Structurally the client's (narrower) response is
// assignable to this, so the default data layer maps straight through.
export interface MemoryEntriesResponse extends ClientMemoryEntriesResponse {
  embedding_metadata?: Record<string, MemoryEmbeddingMeta>;
}
