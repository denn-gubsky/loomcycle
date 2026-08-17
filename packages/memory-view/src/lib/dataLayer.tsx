import { createContext, useContext, type ReactNode } from "react";
import type {
  LoomcycleClient,
  DocumentToolInput,
  MemoryScopesResponse,
  MemoryScopeIDsResponse,
  MemoryEntryResponse,
  MemoryEmbedStatsResponse,
  MemoryReembedResponse,
  MemorySearchInput,
  MemorySearchResponse,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
} from "@loomcycle/client";
import type {
  ChunkDetail,
  DocEdgesResponse,
  FactListResponse,
  MemoryEntriesResponse,
  MemoryScope,
  VerificationStats,
} from "../types";

// The two embedding-maintenance response types are DERIVED from the client's method
// signatures rather than imported by name.
//
// @loomcycle/client 1.55.0 ships the methods but does not re-export their response
// interfaces from its entry point — they are defined in its types module and missing from
// the `export type {...}` list, so a consumer cannot name them. That is fixed in the
// adapter for the next release; deriving them here is drift-free in the meantime (the
// type IS the client's, not a second copy of it) and does not make this package wait on
// a release to use methods that already exist.
type MemoryBackfillResponse = Awaited<ReturnType<LoomcycleClient["backfillEmbeddings"]>>;
type MemoryPurgeResponse = Awaited<ReturnType<LoomcycleClient["purgeStaleEmbeddings"]>>;

// FactListOptions are the browse filters `listFacts` forwards to the Document
// tool's list_facts op. All optional — an empty bag lists the whole scope's
// facts (newest first). `scopeId` is the RFC AS browse-by-subject override (a
// data-plane override sent as ?scope_id=, NOT an authority grant — the runtime
// re-authorizes the principal).
export interface FactListOptions {
  /** EXACT chunk type to match (the backend filters `c.type = ?`, not a prefix). */
  type?: string;
  /** "derived" | "evidential". */
  class?: "derived" | "evidential";
  /** Restrict to one document's facts. */
  documentId?: string;
  /** Include superseded (retired) facts; default false hides them. */
  includeRetired?: boolean;
  /** Also return facts a judge refused — withheld by default, never deleted. */
  includeRefuted?: boolean;
  /** As-of world time (unix-nanos) for a point-in-time view. */
  asOf?: number;
  /** Page size cap. */
  limit?: number;
  /** Browse-by-subject override (?scope_id=). */
  scopeId?: string;
}

// MemoryDataLayer is the narrow data contract the memory console needs — the
// eight off-run reads/writes the k/v browser makes against /v1/_memory/*.
// Decoupling behind this interface lets a host inject the default client-backed
// implementation (dataLayerFromClient), or a custom one (e.g. a cookie-authed
// same-origin fetcher) without the console importing any global api module.
//
// The k/v (P4a) reads resolve the caller's OWN subject — the off-run
// /v1/_memory/* endpoints take no browse-by-subject override. P4b adds the
// entity/fact tier (search / listFacts / getChunk / getEdges), and those DO
// thread an optional `scopeId` (?scope_id=) override for the Document tool's
// RFC AS browse-by-subject reach — a data-plane selector, never an authority
// grant (the runtime re-authorizes the principal). search itself takes no
// override: /v1/_memory/search resolves the caller's own subject.
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
  // Embed rows that carry NO embedding — what enabling an embedder AFTER the rows were
  // written leaves behind. Same dry-run posture as reembed: plan, then commit.
  backfillEmbeddings(
    scope: MemoryScope,
    scopeId: string,
    opts: { dryRun?: boolean; limit?: number },
  ): Promise<MemoryBackfillResponse>;
  // Drop embeddings from rows that have no indexable text. THIS ONE DELETES, so the plan
  // matters more than on its siblings — and `truncated` must be read, because a zero
  // `stale` from a scan cut short at the limit does not mean the scope is clean.
  purgeStaleEmbeddings(
    scope: MemoryScope,
    scopeId: string,
    opts: { dryRun?: boolean; limit?: number },
  ): Promise<MemoryPurgeResponse>;

  // ---- P4b: unified search + the entity/fact tier -------------------------

  // Off-run unified semantic search over a (scope, scope_id): ONE ranked list
  // spanning both plain k/v entries (kind:"memory") AND document-chunk bodies
  // (kind:"document", carrying chunk_id). REJECTS with a 400 `search_unavailable`
  // when the deployment has no embedder / no vector store / a stale dimension —
  // the SearchPanel catches that and renders a "not configured" hint.
  search(input: MemorySearchInput): Promise<MemorySearchResponse>;

  // Browse a scope's FACTS (chunks that carry a chunk_memory_meta sidecar) as
  // metadata only, newest-first. The detail (body + entity block) comes from
  // getChunk on click, exactly as the backend's list surface returns no body.
  listFacts(scope: MemoryScope, opts?: FactListOptions): Promise<FactListResponse>;

  // One chunk's full body + (when the chunk is a fact) its `entity` block.
  // `scopeId` is the browse-by-subject override.
  getChunk(scope: MemoryScope, id: string, opts?: { scopeId?: string }): Promise<ChunkDetail>;
  // judgeFact records a verdict against a fact's stored span. The caller supplies only
  // the WORD and a reason: the server maps the word to a confidence, stamps the time,
  // and stamps who judged from the call's own context. A wrong verdict is corrected by
  // judging again, never by deleting anything.
  judgeFact(
    scope: MemoryScope,
    id: string,
    verdict: "supported" | "unclear" | "unsupported" | "mistyped",
    reason: string,
    opts?: { scopeId?: string },
  ): Promise<unknown>;
  // remember stores a statement a PERSON supplied. The text becomes both the claim and
  // its own source span — the server writes both from this one field, so a client cannot
  // point a claim at evidence that does not contain it.
  remember(
    scope: MemoryScope,
    text: string,
    opts?: { scopeId?: string; type?: string; subject?: string },
  ): Promise<unknown>;
  // verificationStats reports how much of a scope's fact store carries evidence and a
  // verdict — the number that says whether verification is doing anything at all.
  verificationStats(scope: MemoryScope, opts?: { scopeId?: string }): Promise<VerificationStats>;

  // Every cross-reference edge with an endpoint in a DOCUMENT (get_edges is
  // document-scoped, not chunk-scoped — the caller passes the fact's
  // document_id, from getChunk, then filters to the edges touching the fact).
  // Carries the supersession chain (kind:"supersedes") + any plain relations.
  getEdges(scope: MemoryScope, documentId: string, opts?: { scopeId?: string }): Promise<DocEdgesResponse>;

  // ---- Scope-id suggestions (OPTIONAL) ---------------------------------
  // Populate the search panel's scope_id combobox. Optional so a data layer that
  // only knows memory need not implement them — the field then degrades to a
  // free-text input. dataLayerFromClient wires them to whoami / the users list /
  // the Library. None reaches beyond the caller's tenant (server-side scoped).

  /** The current principal — pre-fills scope_id (the tenant name for tenant
   *  scope, the caller's own subject for user scope) and floats "you" to the top
   *  of the user suggestions. */
  whoami?(): Promise<{ subject: string; tenant: string }>;
  /** The user ids (subjects) whose per-user memory can be browsed. */
  listUserIds?(): Promise<string[]>;
  /** The names of ACTIVE, non-retired agents. */
  listAgentNames?(): Promise<string[]>;
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
    backfillEmbeddings: (scope, scopeId, opts) => client.backfillEmbeddings(scope, scopeId, opts),
    purgeStaleEmbeddings: (scope, scopeId, opts) =>
      client.purgeStaleEmbeddings(scope, scopeId, opts),

    // ---- P4b -----------------------------------------------------------
    search: (input) => client.memorySearch(input),
    listFacts: (scope, opts) => {
      const input: Record<string, unknown> = { op: "list_facts", scope };
      if (opts?.type) input.type = opts.type;
      if (opts?.class) input.class = opts.class;
      if (opts?.documentId) input.document_id = opts.documentId;
      // Only send include_retired when true — false is the server default, so
      // omitting it keeps the wire body minimal (and the test assertions exact).
      if (opts?.includeRetired) input.include_retired = true;
      // Refused facts are withheld from this list by default, exactly as they are from
      // an agent's recall. Asking for them is how an operator reads what was refused AND
      // why, which is the entire point of withholding rather than deleting. Same
      // send-only-when-true rule as its neighbour.
      if (opts?.includeRefuted) input.include_refuted = true;
      if (opts?.asOf !== undefined) input.as_of = opts.asOf;
      if (opts?.limit !== undefined) input.limit = opts.limit;
      return client
        .document(toDocInput(input), docOpts(opts?.scopeId))
        .then((r) => r as FactListResponse);
    },
    getChunk: (scope, id, opts) =>
      client
        .document(toDocInput({ op: "get_chunk", scope, id }), docOpts(opts?.scopeId))
        .then((r) => r as ChunkDetail),
    judgeFact: (scope, id, verdict, reason, opts) =>
      client.document(
        toDocInput({ op: "judge_fact", scope, id, verdict, reason }),
        docOpts(opts?.scopeId),
      ),
    remember: (scope, text, opts) => {
      const input: Record<string, unknown> = { op: "remember", scope, text };
      if (opts?.type) input.type = opts.type;
      if (opts?.subject) input.subject = opts.subject;
      return client.document(toDocInput(input), docOpts(opts?.scopeId));
    },
    verificationStats: (scope, opts) =>
      client
        .document(toDocInput({ op: "verification_stats", scope }), docOpts(opts?.scopeId))
        .then((r) => r as VerificationStats),
    getEdges: (scope, documentId, opts) =>
      client
        .document(
          // get_edges is DOCUMENT-scoped: it reads `document_id`, not a chunk
          // `id`. The FactViewer sources this from the fact chunk's document_id.
          toDocInput({ op: "get_edges", scope, document_id: documentId }),
          docOpts(opts?.scopeId),
        )
        .then((r) => r as DocEdgesResponse),

    // ---- Scope-id suggestions ------------------------------------------
    whoami: () => client.whoami().then((w) => ({ subject: w.subject, tenant: w.tenant_id })),
    listUserIds: () => client.listUsers().then((r) => (r.users ?? []).map((u) => u.user_id)),
    listAgentNames: () =>
      client.listLibraryAgents().then((r) =>
        (r.entries ?? [])
          // "active, non-retired": a static agent (statics are never retired) OR
          // a dynamic agent that still has an active version — retiring the last
          // version clears active_def_id (v1.6.4 soft-reclaim).
          .filter((e) => e.in_static || Boolean(e.active_def_id))
          .map((e) => e.name),
      ),
  };
}

// docOpts builds the Document tool's browse-override opts, or undefined when
// there's no override (so the client sends no ?scope_id= and resolves the
// caller's own subject).
function docOpts(scopeId?: string): { scopeId: string } | undefined {
  return scopeId ? { scopeId } : undefined;
}

// toDocInput asserts a plain field bag onto DocumentToolInput. Two mismatches
// force the assertion rather than a direct literal: MemoryScope is a freeform
// string (DocumentToolInput.scope is the narrow "agent"|"user"|"tenant" union),
// and @loomcycle/client 1.47.0's op union predates the entity-tier `list_facts`
// op. The runtime passes scope + op VERBATIM over the wire, and DocumentToolInput
// carries a `[extra: string]: unknown` index signature — so a Record is
// structurally assignable to it and the assertion is sound, not a cast past a
// real type error. (A client bump that models list_facts would let these become
// plain literals; keeping the bag localized here makes that a one-spot change.)
function toDocInput(fields: Record<string, unknown>): DocumentToolInput {
  return fields as DocumentToolInput;
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
