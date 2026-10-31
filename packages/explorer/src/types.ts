// Public data types for @loomcycle/explorer. These are the shapes the Path /
// Document components render and the data layer produces. They mirror the
// loomcycle Web UI's api.ts (the same off-run /v1/_path + /v1/_document endpoint
// JSON) but live here so the package has no dependency on the app's global api
// module — the runtime is reached through an injected LoomcycleClient /
// ExplorerDataLayer instead (see lib/dataLayer).

// PathScope selects WHICH dirent tree to browse. It is a subtree SELECTOR, not
// an authority grant — the runtime resolves the caller's tenant + subject from
// the authenticated principal and re-authorizes.
//
// DocScope was `agent | user` because Documents once REFUSED tenant scope, and it
// stayed that way after the runtime gained it (loomcycle v1.41.0). The cost was not
// a type error: PathExplorer had to narrow PathScope down to DocScope to hand it to
// the viewer, `"tenant"` had nowhere to go, and it collapsed to `"user"` — so every
// tenant-scope document LISTED in the Path tree and then opened with no chunks,
// because the viewer was asking the user scope for them. The two are the same set
// now, and deliberately spelled out rather than aliased: they answer different
// questions (which tree to browse vs which store a document lives in) and could
// diverge again.
export type PathScope = "agent" | "user" | "tenant";
export type DocScope = "agent" | "user" | "tenant";

// BrowseScope is the RFC AS off-run browse-by-subject override: which subject's
// tree (scopeId → ?scope_id=) and, for an admin, which tenant (tenant →
// ?tenant=). CALLER-AUTHORITATIVE only in that the SERVER re-checks it — a
// substrate:tenant principal's tenant is ignored (confined to its own tenant);
// scopeId picks any subject it may see. Unset fields are omitted, so the server
// falls back to the caller's own subject.
export interface BrowseScope {
  scopeId?: string;
  tenant?: string;
}

// PathEntry is one dirent in an `ls` listing.
export interface PathEntry {
  name: string;
  kind: string; // directory | document | volume_mount | memory_entry
  full_path: string;
  resource_ref?: unknown;
}

// ChunkRow is the structural record returned by query_chunks (no body — kept
// light; fetch the body with ExplorerDataLayer.documentGetChunk).
export interface ChunkRow {
  id: string;
  document_id: string;
  parent_id?: string;
  position: number;
  title: string;
  type?: string;
  status?: string;
  revision: number;
}

// ChunkDetail adds the Markdown body + typed fields (from get_chunk).
export interface ChunkDetail extends ChunkRow {
  body: string;
  fields?: unknown;
  // tags (RFC BS) — the chunk's tag set, returned by get_chunk (omitted when the
  // chunk has none). Nested tags use a slash (area/sub/topic).
  tags?: string[];
  // asset (RFC BO) is present on an image chunk — metadata only; the bytes are
  // fetched from GET /v1/_document/asset/{id} via documentAssetObjectUrl.
  asset?: { media_type: string; size: number };
  // entity is get_chunk's entity-metadata block, present on a chunk that carries
  // one. The viewer reads only natural_key: on a document's root chunk it marks the
  // document as a subject.
  entity?: { natural_key?: string };
  // not_announced is set on an update that changed something subscribers are told
  // about (a subject's type) when the runtime withheld the announcement, and why.
  not_announced?: string;
}

// StatementStatus is where a relation stands (RFC FN): an asserted one holds, a
// proposed one waits for an operator's ruling, a rejected one was ruled out.
export type StatementStatus = "asserted" | "proposed" | "rejected";

// EdgeEvidence is one piece of support for a statement (RFC FN): a verbatim
// `quote`, and where it came from — a chunk (with an optional character span), a
// run event (session_id + event_seq), or a memory row (scope + natural_key).
// `stale` marks a quote whose source has since changed. created_at is Unix ns.
export interface EdgeEvidence {
  id: string;
  chunk_id?: string;
  session_id?: string;
  event_seq?: number;
  scope?: string;
  natural_key?: string;
  quote: string;
  char_start?: number;
  char_end?: number;
  stale?: boolean;
  created_at: number;
}

// StatementFields is the RFC FN statement envelope every edge listing carries
// (get_edges, backlinks, get_edge, list_statements). Every field is optional so
// the shapes still describe an older server's plain edges. Timestamps are Unix
// nanoseconds. A listing carries at most three `evidence` rows; evidence_count is
// the total, so a reader knows how many more get_edge would return.
export interface StatementFields {
  id?: string;
  status?: StatementStatus;
  method?: string; // manual | wikilink | import | sync | canvas | runtime | …
  auto?: boolean;
  created_at?: number;
  confidence?: number;
  valid_at?: number;
  invalid_at?: number;
  expired_at?: number;
  verified_at?: number;
  actor?: string;
  run_id?: string;
  discriminator?: string;
  ontology_version?: number;
  inverse_kind?: string;
  attributes?: Record<string, unknown>;
  judged_at?: number;
  judge_reason?: string;
  judged_by?: string;
  evidence?: EdgeEvidence[];
  evidence_count?: number;
}

// DocEdge is one cross-reference edge from get_edges (RFC BN P4): a typed link
// between two chunks (possibly in different documents), enriched with each
// endpoint's title/type/status/document_id so the viewer can render a References
// list + a relationship graph without a per-endpoint get_chunk. The endpoint
// fields are omitted when empty (a dangling endpoint, or a chunk with no
// type/status). Since RFC FN an edge is a statement: it also carries the
// StatementFields envelope (get_edge / list_statements return the same shape).
export interface DocEdge extends StatementFields {
  from_id: string;
  to_id: string;
  kind: string;
  from_title?: string;
  from_type?: string;
  from_status?: string;
  from_document_id?: string;
  to_title?: string;
  to_type?: string;
  to_status?: string;
  to_document_id?: string;
}

// Backlink is one incoming link to the selected chunk, from `backlinks` (RFC BS):
// the linking (source) chunk enriched with its title/type/status/document_id, the
// edge `kind`, and `auto` — true when the link came from a `[[name]]` wikilink in
// the source body (vs a manually-authored edge). from_document_id ≠ the current
// document marks a cross-document backlink (labeled, not navigable in this
// single-document viewer). Endpoint fields are omitted when empty. Since RFC FN
// it carries the statement envelope too (backlinks omits proposals by default).
export interface Backlink extends StatementFields {
  from_id: string;
  kind: string;
  auto: boolean;
  from_title?: string;
  from_type?: string;
  from_status?: string;
  from_document_id?: string;
}

// RelatedChunk is one semantic-neighbour of the selected chunk, from `related`
// (RFC BS) — a vector-similarity match. `score` is the similarity (higher = closer).
// The `related` op REFUSES when the deployment has no embedder configured; the
// viewer surfaces that as a muted note rather than an error.
export interface RelatedChunk {
  chunk_id: string;
  score: number;
  title?: string;
  type?: string;
  document_id?: string;
}

// UnlinkedMention is a chunk whose body mentions the selected chunk's title but
// carries no explicit link to it, from `unlinked_mentions` (RFC BS) — a candidate
// link the author might want to formalize. The list is capped; the response's
// `truncated` flag (not on the row) signals more were elided.
export interface UnlinkedMention {
  chunk_id: string;
  title?: string;
  document_id?: string;
}

// ChunkRevision is one entry in a chunk's edit history, from `history` (RFC BS):
// the monotonic `revision` number, `created_at` (Unix nanoseconds — divide by 1e6
// for a JS Date), and the `actor` who wrote it. Fetch a revision's body with
// documentGetVersion, or a unified diff between two with documentDiff.
export interface ChunkRevision {
  revision: number;
  created_at: number;
  actor: string;
}

// CanvasDoc is the whole-document node/edge graph from `export_canvas` (RFC BS),
// downloadable as a `.canvas` (JSON) file. The node/edge shapes are backend-owned
// and opaque to the viewer (it only serializes them), so they stay `unknown[]`.
export interface CanvasDoc {
  nodes: unknown[];
  edges: unknown[];
}

// Principal is the authenticated identity behind the session (resolved by the
// host, e.g. via GET /v1/_me). The explorer only reads `subject` — it is passed
// into the renderAssistant slot's context so a host-provided Document Assistant
// can scope its agent run to the same subject the viewer shows. Kept minimal on
// purpose; a host may pass its full whoami result (extra fields are ignored).
export interface Principal {
  subject: string;
}

// AssistantContext is what the DocumentViewer's optional renderAssistant slot
// receives. The package deliberately does NOT bundle the run-stream machinery
// (LiveRunPane / useRunStream) — a host wires its own assistant panel and reads
// this context to target the right document + subject.
export interface AssistantContext {
  documentId: string;
  scope: string;
  subject?: string;
  // onChanged lets an injected assistant refresh the viewer after it mutates the
  // document (e.g. at each turn boundary) — it calls the viewer's own reload, so
  // the tree + selected chunk re-fetch. Without it the viewer would show stale
  // content until a manual reload.
  onChanged?: () => void;
}

// ---- RFC FO P5 — classify / accept_classification ----

// ClassifyWrite: `report` writes nothing; `propose` (the runtime's default)
// applies a unique rule match and files the rest for an operator's review.
export type ClassifyWrite = "report" | "propose";

// ClassifyResult — classified: a class was applied live; proposed: it waits for
// an operator; kept: the type was set by hand and classify left it; unclassified:
// nothing was decided.
export type ClassifyResult = "classified" | "proposed" | "kept" | "unclassified";

// ClassifyCandidate is one class a step considered. A rule candidate carries the
// quote it matched and where (`from`: title | source_title | section) plus the
// pattern's named groups; a vote candidate its votes and share.
export interface ClassifyCandidate {
  class: string;
  quote?: string;
  from?: string;
  groups?: Record<string, unknown>;
  share?: number;
  votes?: number;
}

// ClassifyStep is one step of a classification. `step` is "rules" | "vote" today;
// a "model" step (and `method`) arrive with a later runtime, so `step` stays an
// open string and both are rendered when present.
export interface ClassifyStep {
  step: string;
  candidates?: ClassifyCandidate[];
  chosen?: string;
  neighbours?: number;
  reason?: string;
  method?: string;
}

export interface ClassifyIssuer {
  instance: string;
  quote: string;
  subject_chunk_id?: string;
}

export interface ClassifyConcept {
  concept: string;
  tag: string;
  kind?: string;
  chunk_id?: string;
  share: number;
}

// ClassifyStatement is one statement classify wrote (tenant scope only).
export interface ClassifyStatement {
  id: string;
  status: string;
  created: boolean;
  [key: string]: unknown;
}

// ClassifyAnswer is the `classify` op's answer. The list fields are optional
// here even where the runtime always sends them, so a partial answer renders.
export interface ClassifyAnswer {
  document_id: string;
  write: ClassifyWrite;
  result: ClassifyResult;
  class?: string;
  live: boolean;
  work_key?: string;
  fields?: Record<string, unknown>;
  issued_by?: ClassifyIssuer[];
  concepts?: ClassifyConcept[];
  steps?: ClassifyStep[];
  statements?: ClassifyStatement[];
  warnings?: string[];
}

// AcceptClassificationAnswer is the `accept_classification` op's answer.
// type_pending: the proposed class was not applied (another document holds its
// identity) and stays pending. skipped_*: what the pending record named that
// did not re-check (another document's proposal, an already-ruled statement, an
// undeclared class or concept tag).
export interface AcceptClassificationAnswer {
  document_id: string;
  type?: string;
  type_pending?: boolean;
  tags?: string[];
  accepted_statements: string[];
  skipped_statements?: string[];
  skipped_type?: string;
  skipped_tags?: string[];
  warnings?: string[];
}

// ClassificationRecord is what classify keeps in a document ROOT chunk's
// `fields.classification`: what it applied and what waits for an operator.
export interface ClassificationRecord {
  by?: string;
  at?: number; // Unix ns
  applied_type?: string;
  proposed_type?: string;
  proposed_fields?: Record<string, unknown>;
  proposed_tags?: string[];
  statements?: string[];
}
