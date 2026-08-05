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
  // P4b — the off-run unified semantic search shapes (POST /v1/_memory/search).
  // MemorySource (RFC BW) is the `sources` selector value: "facts" | "notes" |
  // "documents". MemorySearchEntry.kind is the labelled result class.
  MemorySearchInput,
  MemorySearchEntry,
  MemorySearchResponse,
  MemorySource,
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

// ---- P4b: the entity/fact tier (RFC BL P4c + RFC BV) ------------------------
//
// These shapes are the Document tool's list_facts / get_chunk / get_edges wire
// output. @loomcycle/client models a Document response as `DocumentToolResponse =
// unknown` (it varies per op), so the fact-viewer's narrowed shapes are OWNED
// here — the data layer casts the op's `unknown` result onto them.

// FactEntity is the bi-temporal + provenance sidecar block (chunk_memory_meta,
// rendered by the backend's chunkMetaToJSON) present on a chunk that is a FACT.
// Timestamps are raw unix-NANOS. They are exact as JSON text, but a 2020s nanos
// value (~1.7e18) exceeds Number.MAX_SAFE_INTEGER (~9e15), so JSON.parse rounds
// to the nearest ~512 ns — harmless for the ms-granularity date the viewer shows.
// `retired` is ALWAYS present (the backend never omits it, so a reader
// need not infer it from an absent key); every other field is omitted when
// empty. Two time axes: valid_at/invalid_at are WORLD time (when the fact was /
// stopped being true), created_at/expired_at are BELIEF/SYSTEM time (when the
// store began / stopped believing it) — `retired` keys on expired_at.
export interface FactEntity {
  /** Always present. true once the fact has been superseded (expired_at set). */
  retired: boolean;
  /** WORLD time — when the fact became / stopped being true (unix-nanos). */
  valid_at?: number;
  invalid_at?: number;
  /** BELIEF/SYSTEM time — when the store began / stopped believing it (unix-nanos). */
  created_at?: number;
  expired_at?: number;
  /** "derived" (machine-distilled) | "evidential" (a pinned source, retention-exempt). */
  class?: string;
  /** Server-stamped provenance — "operator" (off-run) | "agent_explicit" (in a run). */
  origin?: string;
  /** The idempotency handle the entity is keyed by within its scope. */
  natural_key?: string;
  confidence?: number;
  session_id?: string;
  run_id?: string;
  event_seq?: number;
}

// FactRow is one row from Document `list_facts` — a fact's METADATA only (no
// body; the viewer fetches the body via get_chunk on click, as the backend's
// list surface returns none). Ordered newest-first (created_at DESC).
export interface FactRow {
  id: string;
  document_id: string;
  parent_id?: string;
  position: number;
  title: string;
  type?: string;
  status?: string;
  revision: number;
  entity: FactEntity;
}

// FactListResponse is the `list_facts` envelope. `truncated` signals the page
// (limit) clipped the tail.
export interface FactListResponse {
  facts: FactRow[];
  count: number;
  truncated: boolean;
}

// ChunkDetail is one chunk from Document `get_chunk` — the full body + (when the
// chunk is a fact) its `entity` block. `entity` is absent for a plain document
// chunk, which is how a fact and a plain chunk are told apart on read.
export interface ChunkDetail {
  id: string;
  document_id: string;
  title: string;
  body: string;
  type?: string;
  status?: string;
  revision: number;
  fields?: Record<string, unknown>;
  tags?: string[];
  entity?: FactEntity;
}

// DocEdge is one cross-reference edge from Document `get_edges`. `kind` is the
// edge label ("supersedes" for a supersession chain; any other for a plain
// relation). The endpoint enrichment fields (title/type/status/document_id, per
// side) are present only when non-empty — the backend omits blanks. `auto`
// marks a parser-generated [[name]] link vs a manual one.
export interface DocEdge {
  from_id: string;
  to_id: string;
  kind: string;
  auto: boolean;
  from_title?: string;
  from_type?: string;
  from_status?: string;
  from_document_id?: string;
  to_title?: string;
  to_type?: string;
  to_status?: string;
  to_document_id?: string;
}

// DocEdgesResponse is the `get_edges` envelope — every edge with an endpoint in
// the addressed DOCUMENT (both directions). The viewer filters to the edges that
// touch the fact it is showing.
export interface DocEdgesResponse {
  edges: DocEdge[];
}
