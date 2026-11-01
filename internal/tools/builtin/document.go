package builtin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Document is the RFC AK Document primitive (Phase 1 core): a chunked-graph
// document where each chunk is a first-class unit. Content/structure split:
// chunk BODIES + fields live in Memory keyed by the chunk UUID; chunk
// STRUCTURE (parent/position/type/status/title/revision + edges + type
// schemas) lives in SQL Memory so agents query it. A Document is named in the
// Path tree (RFC AL) via a `document` dirent.
//
// v1 supports agent + user scopes (SQL Memory's durable scopes); tenant-scoped
// Documents are deferred (SQL Memory has no tenant scope). Tenant ISOLATION for
// agent/user docs comes free via the SQL Memory ScopeKey.Tenant axis.
//
// Gated by tools:[Document]. Requires SQL Memory enabled
// (LOOMCYCLE_SQLMEM_ENABLED=1) — the structure tables live there.
type Document struct {
	Store  store.Store
	SqlMem *sqlmem.Manager
	Bus    *channels.Bus
	// Writer writes the change feed through the channel writer; nil writes
	// straight to Store (a tool built for a unit test).
	Writer channels.Writer
	// MaxAssetBytes caps the DECODED size of an image asset (set_asset); 0 = a
	// conservative built-in default. The wire (base64) payload is bounded
	// separately by the /v1/_document request-body cap. RFC BO.
	MaxAssetBytes int
	// Embedder makes chunk BODIES semantically searchable: a prose chunk's body
	// is embedded on write, so `memory op=search` with prefix "doc.chunk:" finds
	// it. Before this, the searchable half of a document (the SQL chunks table)
	// held no body text while the half holding the text (the k/v plane) had no
	// index — so document prose was unreachable by any agent-visible search.
	//
	// OPTIONAL, and deliberately so. Nil means bodies are written exactly as
	// before: unsearchable, never lost. Several internal construction sites
	// (ontology provisioning, the tenant-root probe) neither need nor have an
	// embedder, and a required field would force them to fabricate one.
	Embedder providers.Embedder
	// Classifier is memory.classifier: what op=classify asks when its rules and its
	// neighbourhood vote leave a document undecided. nil = classify never calls a model.
	Classifier memrank.DocumentClassifier

	// Backend serves op=search: the SAME hybrid retrieval `Memory op=search` runs
	// (vector and full-text legs fused, then ranked), so a document search and a
	// memory search over the same chunks return the same order. main.go sets the
	// operator-default in-process backend here — one instance, shared with the
	// Memory tool, so whatever that backend is configured with applies to both.
	// Nil builds an in-process backend over Store + Embedder per call, which is
	// what every ad-hoc &Document{} gets.
	//
	// Being the same search, it has the same side effect: a returned chunk body's
	// access count is bumped (the ranker's frequency term reads it), exactly as a
	// `Memory op=search` that returns that chunk already did.
	Backend memrank.Backend

	// Cfg is the loaded operator config, used to resolve `document_sources:`
	// (a peer loomcycle) for the RFC CE set_remote / sync ops. Set in main.go
	// (documentTool.Cfg = cfg); nil disables remote document sources.
	Cfg *config.Config

	// PeerCredentials resolves a "$cred:<name>" api_key_env on a document
	// source, in the source def's owning tenant. Nil = such a reference
	// resolves to nothing, so sync / diff_remote fail.
	PeerCredentials *PeerCredentials

	// reindexJobs tracks background subtree re-indexes (see reindexSubtree), so a test
	// can wait for their end state. A Document is only ever used by pointer.
	reindexJobs sync.WaitGroup
	// reindexing holds one entry per document with a subtree re-index in flight: the
	// chunks whose subtrees were asked for while it ran, which the running pass
	// re-indexes before it lets go (see reindexSubtree). Guarded by reindexMu.
	reindexMu  sync.Mutex
	reindexing map[reindexDocKey]map[string]struct{}

	// editions serialises the reimports of one work (see document_editions.go).
	editions editionLocks
}

func (d *Document) Name() string { return "Document" }

// UsageHint implements tools.HintedTool — surfaced by Context op=guide.
func (d *Document) UsageHint() string {
	return "To reach a chunk you don't have the id for, find it first with op=search or op=query_chunks — don't invent ids. op=upsert_chunk keys on natural_key. Pass op + scope every call. When the answer depends on what a passage cites, defines or implements, follow the relations: op=graph_recall seed_ids=[the passage] kinds=[cites, defines, …] walks them, and op=search with expand={} adds what the top hits are related to."
}

func (d *Document) Description() string {
	return "A chunked-graph document: each chunk is a first-class unit (UUID, hierarchy, type, fields, graph edges, Markdown body) that agents and humans co-author and query. Ops: create_document/get_document/documents_summary (per-document type/status + display metadata for a set of ids or a Path subtree)/query_documents (filter documents by type/status/tag/under_path)/delete_document/set_path, create_chunk (optional after_id inserts right after a sibling)/get_chunk/update_chunk/delete_chunk/move_chunk/reorder_chunk (move up|down within a level), the fact tier — upsert_chunk (write a fact by natural_key)/supersede_chunk (correct one without deleting it)/list_facts/graph_recall (recall across relations, optionally as of a past instant)/remember (store a statement a PERSON supplied as a fact that cites ITSELF — the text becomes both the claim and its source span, filed as evidential; additive only, and never a way to delete)/verbatim_answer (answer a LOOKUP question with a stored fact quoted verbatim plus the source span it was verified against, and NO generated text — returns an answer only when one verified fact clearly matches, and explains itself when it declines)/verification_stats (how much of this scope's fact store carries a span and a verdict)/judge_fact (record whether a fact is supported by the source span recorded on it — an unsupported fact is withheld from list_facts and graph_recall rather than deleted, and stays readable with include_refuted)/propose_entity (suggest an entity type for the tenant ontology; inert until an operator accepts it), link_chunks/unlink_chunks/get_edges (the relations touching a document — each a statement with its status, provenance and evidence quotes, enriched with its endpoints' titles/types/statuses)/get_edge (one relation with all its evidence)/propose_statement (suggest a relation for review; nothing live changes until an operator accepts it)/list_statements (the review queue)/accept_statement/reject_statement (an operator's ruling; refused inside a run)/judge_statements (record a judge's verdicts on proposals and rule on those the review policy routes to the judge, at or above its floor; only for a code agent granted the judge statement method)/maintain_statement (upkeep of a statement by a code agent granted statement methods, or an operator: verify, expire, demote, merge, mark or re-point an evidence row, flag; never asserts or deletes)/resolve_anchor (find a work by its key and a part of it by anchor, as of a date)/describe_ontology (the tenant ontology in force, parsed: classes with their effective directives and fields, relation kinds, grammars, concept schemes, instances with the subject each was adopted as)/extract_citations (apply the document class's citation grammar to a document's or a chunk's text and resolve each citation it finds; read-only)/classify (say what a document is from the confirmed ontology — its class and work key by the classes' patterns, its issuer, the classes and concepts of its neighbours, and, where those leave it open, the operator's classifier model; one class matching by rule is applied, the rest proposed; write:report writes nothing)/accept_classification (an operator's ruling on a document's pending classification; refused inside a run)/backlinks (the chunks that link TO a chunk — both manual links and inline [[name]] links)/related (the chunks whose bodies are semantically closest to a chunk's — ranked by score; needs a configured embedder)/unlinked_mentions (the chunks whose body text mentions a chunk's title but do NOT already link to it), history/get_version/diff (a chunk's body-change log: list the revisions its body changed at, read one revision's exact body, or unified-diff two of them), search (search chunk BODIES for free text, by meaning and, where the server has a word index, by the words too — the entry point when you do not know which document holds the answer; returns chunk_id + title/type/document_id, so no key format to learn; aim it — and related — at part of the scope with under_path(s), exclude_under_path(s), document_ids, exclude_document_ids, type(s), status(es), tag, tag_prefix or as_of, applied before ranking), query_chunks (structured filters incl. tag/tag_prefix + a raw sql escape hatch), add_tags/remove_tags (incremental tags on a chunk (id) or document (document_id))/list_tags (distinct tags + counts for a chunk, a document, or the whole scope), define_type/list_types, set_asset (attach an image's bytes to a chunk → type=image, served by GET /v1/_document/asset/{id})/get_asset (asset metadata), export_md (render the document to Markdown), import_md (build a document from export_md-shaped Markdown; reimport:true reconciles a new edition onto the held one part by part), export_canvas (render the document as a JSON Canvas v1.0 spatial graph — content chunks as nodes + their cross-reference edges, for Obsidian Canvas / spatial views)/import_canvas (build a new document from a JSON Canvas), set_remote (bind this document to a peer loomcycle declared in the operator's document_sources — pass source + remote_ref)/sync (reconcile the bound peer document's keyed chunks with this one by natural_key — carrying body, tags, hierarchy (each chunk under its keyed parent, at its sibling position) and manual links — direction:pull (default) copies the peer's chunks in, direction:push writes this document's chunks up to the peer)/diff_remote (dry-run: what a sync would change — keyed chunks only_local/only_remote/diverged/retagged/reparented, without touching either side). Scope is agent, user (the default) or tenant (tenant needs the operator's grant); documents are named in the Path tree (path:) — create_document defaults to /documents/<title> if you omit one, and set_path attaches/re-homes a path for an existing document."
}

// documentInputSchema is a package const so the LoomCycle MCP server can
// source the wrapper's advertised inputSchema verbatim (via
// MCPWrapperInputSchema) rather than restating it — the same pattern as
// memoryInputSchema.
const documentInputSchema = `{
	"type": "object",
	"properties": {
		"op":          {"type": "string", "enum": ["create_document","get_document","documents_summary","query_documents","delete_document","set_path","create_chunk","propose_entity","propose_subject","judge_fact","upsert_chunk","get_chunk","update_chunk","delete_chunk","supersede_chunk","graph_recall","list_facts","verbatim_answer","verification_stats","remember","move_chunk","reorder_chunk","link_chunks","unlink_chunks","get_edges","get_edge","propose_statement","accept_statement","reject_statement","list_statements","judge_statements","maintain_statement","resolve_anchor","lease","release","describe_ontology","extract_citations","classify","accept_classification","query_chunks","add_tags","remove_tags","list_tags","define_type","list_types","set_asset","get_asset","export_md","import_md","export_canvas","import_canvas","history","get_version","diff","backlinks","search","related","unlinked_mentions","set_remote","sync","diff_remote"]},
		"scope":       {"type": "string", "enum": ["agent","user","tenant"], "description": "Which store (default user). ONE scope per call — a search, graph_recall or list_facts reads ONLY the scope you name, so a user-scope search never returns tenant chunks and vice versa; consult both by making two calls. agent = this agent; user = this end-user (needs a user_id on the run); tenant = shared by every user and agent in the tenant — anything written here is read by all of them, so use it for curated reference material, not for anything derived from untrusted text. tenant requires the operator to grant BOTH memory_scopes and sql_scopes with the tenant value."},
		"id":          {"type": "string", "description": "Document id (get/delete_document, set_path) or chunk id (get/update/delete/move_chunk), or a statement id (get_edge, unlink_chunks, accept/reject_statement, maintain_statement, judge_fact; propose_statement: the statement a competing proposal revises)."},
		"path":        {"type": "string", "description": "create_document: name the doc in the Path tree (default /documents/<title> if omitted). set_path: the path to attach to an existing document (by id). get/delete_document: address by path instead of id."},
		"title":       {"type": "string", "description": "The document's or chunk's title. update_chunk on the root chunk (root_chunk_id) renames the document; its path stays put (Path op=mv moves it)."},
		"document_id": {"type": "string", "description": "The document an op works on. search / related: return only this document's chunks. list_statements: only statements with either end in this document."},
		"document_ids": {"type": "array", "items": {"type": "string"}, "description": "documents_summary: the document ids to summarize (combine with or instead of under_path). The response is bounded (default 500, max 5000) and reports truncated:true when it clips — an under_path over a subject-homed fact store is as large as the tenant's entity count, so page the directory with path op=ls and pass each page's ids here. search / related: return only chunks of these documents."},
		"exclude_document_ids": {"type": "array", "items": {"type": "string"}, "description": "search / related: never return chunks of these documents — e.g. the document under review, so its own text is not found as evidence for itself."},
		"parent":      {"type": "string", "description": "propose_entity: the IN-FORCE entity type this one is a kind of, BY NAME (omit for a new top-level type). Use a name from the entity types listed in your instructions."},
		"section":     {"type": "string", "enum": ["classes","relations","schemes","instances"], "description": "propose_entity: what is being proposed — classes (default) for an entity type, relations for a relation kind (a name for how two things are related, e.g. supervises), schemes for a topic or classification concept, instances for a real organisation, unit or post the tenant should know. parent then names an in-force entry of the same section; for a concept, the scheme or broader concept it belongs under (a name or a tag such as topics/risk-management); for an instance, its class."},
		"parent_id":   {"type": "string", "description": "create_chunk: the parent chunk. Omit it to add the chunk under the document's title (the root chunk)."},
		"new_parent_id": {"type": "string", "description": "move_chunk: the new parent. Empty moves the chunk directly under the document's title (the root chunk)."},
		"after_id":    {"type": "string", "description": "create_chunk: insert the new chunk immediately AFTER this sibling (same parent; shifts later siblings). Overrides parent_id/position."},
		"direction":   {"type": "string", "enum": ["up","down","pull","push","out","in","both"], "description": "get_edges: out (statements leaving the document), in (arriving) or both (default). graph_recall: follow relations out of (out), into (in) or both ways from (both, default) each chunk. reorder_chunk: move the chunk up or down within its current level. sync: pull (default) copies the peer document's keyed chunks into this one; push writes this document's keyed chunks up to the peer (reconciled by natural_key, carrying body/tags/hierarchy/manual-links; the losing side keeps the overwritten body in its history)."},
		"type":        {"type": "string", "description": "Optional supertag-like chunk type. list_facts (browse the scope's facts — chunks that carry entity metadata — newest first, metadata only, no bodies): return only facts of this type. On list_facts, query_chunks, search and related the filter INCLUDES SUBTYPES — asking for a general type also returns the more specific kinds of it, so filter by the broadest type that still answers your question. The response reports type_expanded_to when this widened the filter."},
		"types":       {"type": "array", "items": {"type": "string"}, "description": "search / related: return only chunks of any of these types, each including its subtypes (with type, the union of both)."},
		"body":        {"type": "string", "description": "Markdown body."},
		"seed_ids":  {"type": "array", "items": {"type": "string"}, "description": "graph_recall: chunk ids to start from. Use this to hand in results you already found some other way (a Memory search, a previous recall) and follow the graph out from them."},
		"query":     {"type": "string", "description": "search: free text matched against chunk BODIES by meaning (and by words, where the server indexes them) — the way into a document when you do not know where to look. graph_recall: find starting chunks whose title matches this text (use seed_ids instead when you already know where to start)."},
		"hops":      {"type": "integer", "description": "graph_recall: how far to follow relations from each starting chunk. A hop is ONE edge, so a fact→entity→fact step costs two: a two-hop question needs 2, a three-hop needs 4. 0 = the starting chunks only, 1 = their neighbours (default), 6 = the maximum. What bounds a long walk is budget_chars, not this."},
		"seeds":     {"type": "integer", "description": "graph_recall: how many of the best-ranked facts start the walk (default 5). Small on purpose — seeding from every hit spends the budget before the walk contributes anything."},
		"budget_chars": {"type": "integer", "description": "graph_recall: cap the answer on CONTENT rather than on rows, and backfill. A walk fetches more rows than a search by construction, so a row limit lets it win on volume instead of on the relations; and when the graph runs dry the walk under-fills, which scores worse than not walking at all. Set this and whatever budget the walk leaves is spent on the best-ranked facts it did not reach. 0 = the row-limited behaviour."},
		"body_chars": {"type": "integer", "description": "graph_recall: how much of each chunk's text to return. Omit it and chunks that are not facts (clauses, sections, notes) carry the first 600 characters as body, while facts stay title-only because their title is the claim. 0 = titles only, the cheapest structural walk. N (at most 4000) = up to N characters on every row, facts included. A cut body ends on a word and is marked body_truncated; budget_chars counts body characters too."},
		"as_of":     {"type": "integer", "description": "graph_recall / list_facts: answer as of this moment (unix nanos) instead of now — returns what was true then, including facts since corrected. A chunk with its own valid_at/invalid_at must also contain the moment. search / related / query_chunks: return only chunks whose stated validity contains this moment (the chunk's own valid_at/invalid_at and, for a fact, its fact dates too); a chunk with no stated validity always matches. get_edges / backlinks: only statements whose own valid_at/invalid_at contain the moment (undated ones always match). extract_citations: read the parts in force then, and resolve each citation as of then (default: each citing part's own edition date)."},
		"include_retired": {"type": "boolean", "description": "graph_recall / list_facts: also return facts that have been superseded, and on graph_recall chunks whose own valid_at/invalid_at put them out of force now. Off by default, so you get only what is currently true. get_edges / backlinks / list_statements: also return retired statements (list_statements: expired ones and ones whose invalid_at has passed)."},
		"text":        {"type": "string", "description": "remember: the statement to store, as ONE self-contained sentence. It is stored verbatim AND becomes its own source span, so what you type is both the fact and the evidence for it — write what you mean recorded, not an instruction about it."},
		"min_score":   {"type": "number", "description": "verbatim_answer: how close a match must be before it can be quoted as an answer (default 0.6). Cosine scale is a property of the embedding model, so tune this against your own — the response always reports the actual score, including when it refuses."},
		"include_refuted": {"type": "boolean", "description": "list_facts / graph_recall: also return facts a judge marked unsupported. They are withheld by default and never deleted, so this is how you read what was refused and why (each carries judge_reason). graph_recall also follows relations a judge marked unsupported, never one an operator rejected. get_edges / backlinks: also return statements that were rejected or judged unsupported."},
		"about":       {"type": "string", "description": "list_facts: the facts about ONE SUBJECT — pass the subject's entity chunk id (a subject document's root_chunk_id, from get_document). Returns the facts filed under it AND the facts filed elsewhere that reference it, because a fact about two things lives in one of their documents and points at the other: filtering by document alone would lose it from the second subject entirely."},
		"claims_only": {"type": "boolean", "description": "list_facts: return only CLAIMS, dropping the entity identity nodes (the subject a fact is about — 'Ollama', 'the user'). A claim carries a sentence and can carry a source span; an identity node is a name and never can, so a listing meant for reading or for verification coverage wants this on. Off by default because the default listing is every chunk carrying entity metadata, which is what document sync reconciles."},
		"verdict":      {"type": "string", "enum": ["supported","unclear","unsupported","mistyped"], "description": "judge_fact: whether the fact's recorded source span supports it. supported = the span carries the claim; unclear = it partly does; unsupported = it does not; mistyped = the span DOES support the claim but it is filed under a type the ontology does not say it is (the fix is to retype it, not to drop it, so it stays visible). The confidence each maps to is set by the server, and an unsupported fact stops being returned by the fact surfaces without being deleted."},
		"reason":       {"type": "string", "description": "judge_fact: one sentence on WHY, quoted back to the operator. A verdict nobody can act on is a verdict nobody trusts. reject_statement: why the relation is wrong (required). maintain_statement: why (required for expire and demote; flag records it)."},
		"subject":      {"type": "string", "description": "upsert_chunk: the thing this entity assertion is ABOUT, paired with type — emit both or neither. type says what kind of thing it is, subject names it. A plain document chunk has no subject; passing the pair is what marks a write as an entity the ontology governs."},
		"source_quote": {"type": "string", "description": "upsert_chunk: the EXACT text this fact was derived from, copied verbatim from the source you read — not a paraphrase. It is what a later pass checks the claim against, and what an operator sees when they ask why the store believes this. Omit only when there is no source text (material you are recording as evidence in its own right)."},
		"source_chunk_id": {"type": "string", "description": "upsert_chunk: the chunk of this scope the fact was taken from. The runtime links the fact to it (derived_from) with source_quote as evidence, so the fact can be checked against the passage and recall can point back to it. source_quote must occur in that chunk; char_start/char_end say where (or, with no source_quote, select it)."},
		"char_start":  {"type": "integer", "description": "upsert_chunk with source_chunk_id: where the quote starts in the source chunk's text (characters)."},
		"char_end":    {"type": "integer", "description": "upsert_chunk with source_chunk_id: where the quote ends (exclusive)."},
		"about_ids":   {"type": "array", "items": {"type": "string"}, "description": "upsert_chunk: the subject nodes (entity chunks) this fact is about; the runtime writes the fact's about edges to them. A subject that is not a chunk of this scope is skipped and counted (about_skipped)."},
		"relation":    {"type": "string", "description": "upsert_chunk with two or more about_ids: the kind of relation the fact states between its first two subjects (e.g. works_at). The runtime records it as a relation whose evidence is the fact; a kind the tenant has not declared is recorded for review."},
		"across_scopes": {"type": "boolean", "description": "get_document / list_facts about: also look for this SUBJECT in the other scopes you can read (agent, user, tenant), matching on its natural_key. A subject exists once per scope, so its facts can be split across them — this is how \"what do we know about X\" stops depending on which scope you happened to read. Off by default: it costs up to three queries, and it never reaches another USER's scope."},
		"natural_key": {"type": "string", "description": "create_document: pair it with type+subject to make the new document's ROOT the entity node itself, so /facts/<subject> IS the subject and its facts are its children. upsert_chunk: the stable identity of this entity or fact. Upserting twice with the same key updates ONE chunk instead of adding a second — use a derived form such as person:ada-lovelace, or subject|predicate|object for a fact. Unique within the scope. May be omitted when the chunk's type is a class whose identity fields are declared in the tenant ontology: the key is then built from those fields."},
		"supersedes_id": {"type": "string", "description": "supersede_chunk: the id of the chunk being RETIRED by this one. The retired chunk is not deleted — it stays queryable so that questions about an earlier point in time still have an answer."},
		"valid_at":   {"type": "integer", "description": "When the content became true IN THE WORLD (unix nanos). upsert_chunk: the fact's start. create_chunk / update_chunk: the chunk's own start, e.g. when an edition or clause came into force — it does not make the chunk a fact; update_chunk with null clears it. Omit when unknown — an undated chunk is honest and still matches an as_of question; a guessed instant is not. Distinct from when it was recorded. link_chunks / propose_statement: when the relation began to hold."},
		"from_pending": {"type": "string", "description": "upsert_chunk: the id of a pending item you drained, so this fact records what produced it. The server fills in the origin and the source ids from that row — you cannot set those yourself. Unknown or unowned ids are ignored and the write still succeeds."},
		"source_session_id": {"type": "string", "description": "create_document / upsert_chunk (with subject + natural_key): the chat a fact was distilled from, when it came off a transcript rather than a drained pending item (from_pending supplies it then, and wins). Provenance only, never used for access."},
		"source_run_id": {"type": "string", "description": "list_facts: only the facts distilled from this run — what one conversation produced."},
		"observed_at": {"type": "integer", "description": "When the thing was SAID or written (unix nanos), as distinct from when it became true. \"Yesterday I met them in Boston\", said on the 4th, is observed_at the 4th and valid_at the 3rd. Omit when unknown."},
		"invalid_at": {"type": "integer", "description": "When the content STOPPED being true in the world (unix nanos, exclusive). upsert_chunk: the fact's end. create_chunk / update_chunk: the chunk's own end, e.g. when an edition was replaced; update_chunk with null clears it. Must be after valid_at. Leave unset for something still true. link_chunks / propose_statement: when the relation stopped holding."},
		"class":      {"type": "string", "enum": ["derived","evidential"], "description": "derived = distilled from something else (the default). evidential = source material, exempt from age-based pruning. list_facts: filter to facts of this class."},
		"confidence": {"type": "number", "description": "0..1, how sure you are of this fact or relation (upsert_chunk, link_chunks, propose_statement)."},
		"fields":      {"type": "object", "description": "Type-specific structured fields."},
		"status":      {"type": "string", "description": "The chunk's status label. query_chunks / search / related: return only chunks with exactly this status. list_statements: which statements to list (default proposed)."},
		"statuses":    {"type": "array", "items": {"type": "string"}, "description": "search / related: return only chunks with any of these exact statuses (with status, the union of both)."},
		"position":    {"type": "integer"},
		"revision":    {"type": "integer", "description": "update_chunk: the chunk's current revision (optimistic concurrency). get_version: which body-change revision to read."},
		"from_revision": {"type": "integer", "description": "diff: the earlier revision to compare (from history)."},
		"to_revision":   {"type": "integer", "description": "diff: the later revision to compare (from history)."},
		"from_id":     {"type": "string"},
		"to_id":       {"type": "string"},
		"kind":        {"type": "string", "description": "link/unlink_chunks / propose_statement: the relation kind (cites, based_on, defines, implements, related, …). Kinds the runtime writes are refused: about, supersedes, references, mentions, derived_from, supported_by. get_edges / backlinks / list_statements: return only this kind. graph_recall / related (related_to): follow only this kind."},
		"kinds":       {"type": "array", "items": {"type": "string"}, "description": "get_edges / backlinks / list_statements: return only these relation kinds (with kind, the union). graph_recall / related (related_to): follow only these kinds."},
		"attributes":  {"type": "object", "description": "link_chunks / propose_statement: the relation's qualifiers (e.g. aspect, coverage), at most 4 KB of JSON."},
		"method":      {"type": "string", "description": "propose_statement / upsert_chunk (its relation): how the statement was made, rule:<name> or extractor:<name> (name: 1-64 of a-z 0-9 _ . / -). maintain_statement: the same, recorded as the judge of what it changes. Only an agent granted that kind of method may send it; otherwise it is refused (method_not_granted). Omit it and the statement is recorded as manual."},
		"action":      {"type": "string", "enum": ["verify","expire","demote","merge","stale_evidence","relocate_evidence","flag"], "description": "maintain_statement: verify (stamp verified_at), expire (retire it, reason required), demote (asserted back to proposed, reason required), merge (move its evidence onto survivor_id and expire it), stale_evidence (mark evidence_id stale), relocate_evidence (re-point evidence_id to chunk_id, where its quote must occur), flag (attributes.needs_review). A statement a person wrote takes only verify and flag, and so does one an operator ruled on, from inside a run. expire, demote and merge need the link permission on both ends."},
		"survivor_id": {"type": "string", "description": "maintain_statement merge: the live statement of the same from_id, to_id, kind and status that keeps the evidence."},
		"evidence_id": {"type": "string", "description": "maintain_statement stale_evidence / relocate_evidence: the evidence row, by its id from get_edge."},
		"chunk_id":    {"type": "string", "description": "maintain_statement relocate_evidence: the chunk the evidence's quote is in now."},
		"evidence":    {"type": "array", "description": "link_chunks / propose_statement: the text that supports the relation. Each item names ONE source — chunk_id (a chunk of this scope), session_id + event_seq (a chat turn), or scope + natural_key (another scope) — and the verbatim quote. A quote that does not occur in its chunk or turn is refused.", "items": {"type": "object", "properties": {"chunk_id": {"type": "string"}, "session_id": {"type": "string"}, "event_seq": {"type": "integer"}, "scope": {"type": "string", "enum": ["agent","user","tenant"]}, "natural_key": {"type": "string"}, "quote": {"type": "string"}, "char_start": {"type": "integer"}, "char_end": {"type": "integer"}}, "required": ["quote"]}},
		"include_proposed": {"type": "boolean", "description": "get_edges (default true) / backlinks (default false): include statements proposed for review and not yet accepted. graph_recall (default false): also follow them."},
		"after":       {"type": "string", "description": "get_edges / backlinks / list_statements: continue a truncated listing — pass the next_cursor from the previous page (list_statements: with the same order)."},
		"evidence_document_id": {"type": "string", "description": "list_statements: only statements with an evidence row quoting a chunk of this document."},
		"method_prefix": {"type": "string", "description": "list_statements: only statements whose method starts with exactly this text (e.g. rule: or rule:cite); case-sensitive, no wildcards."},
		"order":       {"type": "string", "enum": ["created","verified"], "description": "list_statements: created (default) is newest first; verified is least recently verified first, never-verified ones before any."},
		"from_document_id": {"type": "string", "description": "list_statements: only statements whose FROM end is in this document."},
		"to_document_id":   {"type": "string", "description": "list_statements: only statements whose TO end is in this document."},
		"from_under_path":  {"type": "string", "description": "list_statements: only statements whose FROM end is in a document named at or under this Path."},
		"to_under_path":    {"type": "string", "description": "list_statements: only statements whose TO end is in a document named at or under this Path."},
		"actor":            {"type": "string", "description": "list_statements: only statements written by this actor: the agent that wrote it in a run, else the user."},
		"min_confidence":   {"type": "number", "description": "list_statements: only statements whose writer's own confidence is at least this (0..1); one with no confidence is left out."},
		"created_before":   {"type": "integer", "description": "list_statements: only statements created before this instant (unix nanoseconds, as created_at reads)."},
		"created_after":    {"type": "integer", "description": "list_statements: only statements created after this instant (unix nanoseconds)."},
		"has_judge_verdict": {"type": "boolean", "description": "list_statements: true keeps only statements a judge recorded a verdict on (attributes.judge), false only those with none."},
		"judge_verdict":    {"type": "string", "description": "list_statements: only statements whose recorded judge verdict (attributes.judge.verdict) is exactly this, e.g. accept, reject or unsure."},
		"summarize":        {"type": "boolean", "description": "list_statements: instead of the statements, return counts grouped by kind, method, the FROM document's path prefix (its first two segments) and judge verdict, plus the total. The filters apply; after, limit and order are ignored."},
		"method_exact":     {"type": "string", "description": "list_statements: only statements whose method is exactly this (rule:cite, not rule:citation); case-sensitive."},
		"kind_exact":       {"type": "string", "description": "list_statements: only statements of exactly this kind; unlike kind, never widened to the kind's declared sub-kinds."},
		"from_path_prefix": {"type": "string", "description": "list_statements: only statements whose FROM document's path prefix is exactly this, as summarize computes it: the first two segments of the document's first Path name in sort order (/board/regulatory-external)."},
		"from_unfiled":     {"type": "boolean", "description": "list_statements: true keeps only statements whose FROM document is filed under no Path name (or is gone): summarize's empty from_path_prefix."},
		"walk":        {"type": "string", "enum": ["declared","all"], "description": "graph_recall: which relations to follow. declared (the default where the tenant declares relation kinds) follows the kinds declared for walking — citations, definitions, what implements what — and never mentions or about; all follows every relation. An explicit kind / kinds overrides it; any other value is refused."},
		"decay":       {"type": "number", "description": "graph_recall with budget_chars: each hop may spend at most decay^hop of the budget, so far hops cannot crowd out near ones. 0 < decay <= 1; default 0.7 for a declared walk, else 1 (no decay). Refused without budget_chars."},
		"include_hierarchy": {"type": "string", "enum": ["up","down","both","none"], "description": "graph_recall: also step along the document tree — up to a chunk's parent (part_of), down to its children (has_part). Default none."},
		"expand":      {"type": "object", "description": "search: after ranking, add the chunks the top hits are related to — one hop, within a budget — each marked expanded_via {kind, from_chunk_id, quote}. Use it when a passage depends on what it cites or defines. The search's filter arguments apply to the added chunks too.", "properties": {"kinds": {"type": "array", "items": {"type": "string"}}, "hops": {"type": "integer", "enum": [1]}, "budget_chars": {"type": "integer"}}},
		"related_to":  {"type": "string", "description": "related: a chunk id whose neighbours BY RELATION (citations, definitions, …) to return as linked, beside — or, without id, instead of — the neighbours by meaning. Needs no embedder. The filter arguments (under_path, type, …) narrow it too."},
		"reimport":    {"type": "boolean", "description": "import_md: the markdown is a NEW EDITION of the document named by document_id or work_key. Parts are matched by their text, then their heading path: unchanged parts keep their ids and links, a part whose text moved under another heading keeps them too, changed ones are superseded (the old one closes at edition_date), removed ones close, new ones open at edition_date. Nothing is deleted."},
		"edition_date": {"type": "integer", "description": "import_md with reimport: when the new edition came into force (unix nanos; default now). Not earlier than the edition in force."},
		"work_key":    {"type": "string", "description": "import_md with reimport / resolve_anchor: the work's key, e.g. law:ua/2297-vi — the document holding it, or (import) the one the first edition creates."},
		"amending_document_id": {"type": "string", "description": "import_md with reimport and work_key: the document of this scope that made the edition — the amending act. Each part the edition renumbered or moved gets an amends statement from its root, mod_type renumbering, dated edition_date. You must be able to read and link it."},
		"anchor":      {"type": "string", "description": "resolve_anchor: a part of the work, by its anchor (its heading path, e.g. article-12/paragraph-3) or the anchor it first had."},
		"create_placeholder": {"type": "boolean", "description": "resolve_anchor: file a placeholder document for a work this scope does not hold yet, so citations of it have a target; importing the work later fills it in."},
		"media_type":  {"type": "string", "description": "set_asset: the image MIME type (image/png, image/jpeg, image/gif, image/webp)."},
		"data":        {"type": "string", "description": "set_asset: the image bytes as standard base64 (no data: prefix)."},
		"filename":    {"type": "string", "description": "set_asset: an optional original filename (metadata only)."},
		"under_path":  {"type": "string", "description": "query_chunks / query_documents / documents_summary: restrict to documents at/under this Path-tree path. search / related: return only chunks of documents named at or under it — applied before ranking, so the best matches come from that part of the tree. list_statements: only statements with either end in a document named at or under it."},
		"under_paths": {"type": "array", "items": {"type": "string"}, "description": "search / related: return only chunks of documents named at or under ANY of these Path-tree paths (with under_path, the union of both)."},
		"exclude_under_path":  {"type": "string", "description": "search / related: never return chunks of documents named at or under this Path-tree path."},
		"exclude_under_paths": {"type": "array", "items": {"type": "string"}, "description": "search / related: never return chunks of documents named at or under any of these Path-tree paths."},
		"tags":        {"type": "array", "items": {"type": "string"}, "description": "The chunk's or document's tags. create_chunk/update_chunk/upsert_chunk/create_document replace-set the whole tag set (omit to leave unchanged; [] to clear). add_tags/remove_tags take the tags to add/remove incrementally. Nested tags use a slash: area/sub/topic."},
		"tag":         {"type": "string", "description": "query_chunks / query_documents / search / related: return only chunks/documents carrying exactly this tag."},
		"tag_prefix":  {"type": "string", "description": "query_chunks / search / related: return chunks whose tag equals this OR is nested under it (prefix + '/'), e.g. tag_prefix=area matches area and area/sub."},
		"concept":        {"type": "string", "description": "search / related: return only chunks about this concept of the tenant ontology's concept schemes, or about any concept narrower than it. Name it by its chunk id, its name, or its tag (<scheme>/<path>, e.g. topics/risk-management); a name two concepts share is refused, so pass the tag. In the tenant scope a chunk matches by its subject relation to the concept; in a user or agent scope, by carrying the concept's tag (or a tag under it). The echo's concept_matched_by says which."},
		"concept_prefix": {"type": "string", "description": "search / related: like concept, for every concept whose tag is this path or lies under it, e.g. concept_prefix=topics covers the whole topics scheme."},
		"sql":         {"type": "string", "description": "query_chunks: raw read-only SELECT against the chunk tables (escape hatch; validator-gated)."},
		"limit":       {"type": "integer", "description": "The most rows to return. The default depends on the op (graph_recall: 50 chunks); each op's help article gives its own."},
		"name":        {"type": "string", "description": "define/list_types: the type name. lease / release: the lease's name, e.g. linker (1–64 of a-z 0-9 _ . : / -)."},
		"ttl_ms":      {"type": "integer", "description": "lease: how long to hold it, in milliseconds (default 60000, capped at one hour). The lease expires on its own; the holder re-leases to extend it."},
		"write":       {"type": "string", "enum": ["propose","report","rule","recommend"], "description": "classify: propose (default) applies an unambiguous rule match and files the rest for review; report writes nothing and returns the same answer. judge_statements: rule (default) rules each routed proposal whose verdict clears the floor; recommend only records the verdicts."},
		"judgements":  {"type": "array", "description": "judge_statements: 1-100 verdicts, each {id (a statement), verdict (accept|reject|unsure), confidence (0..1), rationale (one sentence), verdict_run_id? (the run that judged it, from the sub-agent result header)}.", "items": {"type": "object", "properties": {"id": {"type": "string"}, "verdict": {"type": "string", "enum": ["accept","reject","unsure"]}, "confidence": {"type": "number"}, "rationale": {"type": "string"}, "verdict_run_id": {"type": "string"}}, "required": ["id","verdict","confidence","rationale"]}},
		"include_metadata": {"type": "boolean", "description": "export_md: embed round-trippable chunk metadata + edges as HTML comments (default true). false = clean human-facing Markdown."},
		"markdown":    {"type": "string", "description": "import_md: an export_md-shaped Markdown document (headings = hierarchy; <!-- loom: ... --> metadata; <!-- loom-edges: ... --> trailer). Omit document_id to create a new document; pass document_id (+ optional parent_id) to import under an existing chunk."},
		"canvas":      {"type": "object", "description": "import_canvas: a JSON Canvas v1.0 object ({\"nodes\":[...],\"edges\":[...]}) — e.g. the contents of an Obsidian .canvas file. Each node becomes a chunk (positioned via its x/y/width/height); each edge becomes a link. Pass title/path to name the new document."},
		"source":      {"type": "string", "description": "set_remote: the document_sources name (a peer loomcycle declared in operator config) to bind this document to."},
		"remote_ref":  {"type": "string", "description": "set_remote: the remote document's path (or id) on the peer. sync reconciles that document's keyed chunks with this one."},
		"source_url":  {"type": "string", "description": "import_md: a link to where the text came from (the email, the SharePoint file, the web page): an http(s) URL, at most 2048 bytes, stored with any signature or token in its query masked. Naming a source marks the document as imported: its text is then never served as a remembered fact. A new document only."},
		"source_kind": {"type": "string", "enum": ["sharepoint", "email", "attachment", "web", "other"], "description": "import_md: what kind of place the text came from (default other when a source is given). Also tags the document source/<kind>."},
		"source_title": {"type": "string", "description": "import_md: the source's own name, e.g. an email's subject. One line, at most 300 characters."}
	},
	"required": ["op"]
}`

func (d *Document) InputSchema() json.RawMessage { return json.RawMessage(documentInputSchema) }

type docInput struct {
	Op          string   `json:"op"`
	Scope       string   `json:"scope"`
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	DocumentID  string   `json:"document_id"`
	DocumentIDs []string `json:"document_ids"`
	ParentID    string   `json:"parent_id"`
	NewParentID string   `json:"new_parent_id"`
	// AfterID (create_chunk, RFC BP) inserts the new chunk immediately after this
	// sibling — insert-and-shift, overriding parent_id/position. Direction
	// (reorder_chunk) is "up" | "down".
	AfterID   string          `json:"after_id"`
	Direction string          `json:"direction"`
	Type      string          `json:"type"`
	Body      string          `json:"body"`
	Fields    json.RawMessage `json:"fields"`
	Status    string          `json:"status"`
	// Parent names an entity by NAME (propose_entity). Distinct from ParentID, which
	// addresses a chunk: a curator knows the type it read in its prompt, not a uuid.
	Parent string `json:"parent"`
	// Section is which part of the ontology a proposal belongs to (propose_entity):
	// "" or "classes" for an entity type, "relations" for a relation kind, "schemes" for
	// a concept, "instances" for a real thing.
	Section string `json:"section"`
	// SourceQuote is the span a fact was derived from (upsert_chunk). Persisted on the
	// entity row so a later pass can check the claim against it, and so an operator
	// asking "why do you believe this" has an answer.
	SourceQuote string `json:"source_quote"`
	// SourceChunkID / CharStart / CharEnd (upsert_chunk): the chunk of this scope a
	// fact was taken from and where in it; the runtime records a derived_from
	// statement with the quote and span as evidence. AboutIDs: the subject nodes
	// the fact is about — the runtime writes the `about` edges (RFC FN P3a).
	SourceChunkID string   `json:"source_chunk_id,omitempty"`
	CharStart     *int     `json:"char_start,omitempty"`
	CharEnd       *int     `json:"char_end,omitempty"`
	AboutIDs      []string `json:"about_ids,omitempty"`
	// Relation (upsert_chunk): the kind of relation the fact states between its
	// first two about_ids subjects; the runtime records it as a statement whose
	// evidence is the fact itself (RFC FN P3b).
	Relation string `json:"relation,omitempty"`
	// Subject is WHAT an entity assertion is about, travelling with Type as a pair.
	// First-class rather than encoded in the natural key, so it can be read, queried and
	// checked — and so the tool can tell an entity assertion from a document write.
	Subject string `json:"subject"`
	// Verdict and Reason are judge_fact's inputs. The verdict is a WORD, never a number:
	// the confidence it maps to is derived server-side so one place owns the scale.
	// Text is the statement `remember` stores. Named apart from `body` because the two
	// are not interchangeable here: this text becomes the fact AND its own citation, so
	// the server writes both from it rather than letting a caller supply them separately.
	Text string `json:"text"`
	// MinScore overrides verbatim_answer's similarity floor. Caller-supplied because
	// cosine scale is a property of the embedding model, so the shipped default is
	// right for at most one of them.
	MinScore float64 `json:"min_score"`
	// IncludeRefuted surfaces facts a judge refused. Off by default and available on
	// purpose: "quarantine, never delete" is only meaningful if the quarantine can be read.
	IncludeRefuted bool `json:"include_refuted"`
	// About narrows a fact listing to ONE SUBJECT, by its entity chunk id. It is not
	// a document filter and must not become one: a subject's facts are the ones filed
	// under it PLUS the ones filed elsewhere that reference it, and a relation-fact is
	// only ever filed under one of the two subjects it names.
	About string `json:"about,omitempty"`
	// ClaimsOnly drops entity IDENTITY nodes from a fact listing, leaving the claims.
	// Opt-in rather than the default because list_facts' contract is "chunks that carry
	// entity metadata", and the document-federation reconcile depends on that breadth —
	// narrowing it silently would stop syncing subject nodes. See identityNodeExclusion.
	ClaimsOnly bool `json:"claims_only"`
	// SourceRunID answers the INVERSE of "where did this fact come from": what
	// did we learn from this conversation. The forward direction is the pointer
	// recall returns; without the inverse an operator holding a transcript has no
	// way to ask what it produced, which is half the relation missing.
	SourceRunID string `json:"source_run_id,omitempty"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
	// Statement inputs (RFC FN): link_chunks / propose_statement take the
	// relation's attributes and evidence; get_edges / backlinks / list_statements
	// filter by kinds and page with after.
	Attributes      map[string]any   `json:"attributes,omitempty"`
	Evidence        []edgeEvidenceIn `json:"evidence,omitempty"`
	Kinds           []string         `json:"kinds,omitempty"`
	After           string           `json:"after,omitempty"`
	IncludeProposed *bool            `json:"include_proposed,omitempty"`
	// list_statements selection: evidence_document_id keeps a statement with an
	// evidence row citing a chunk of that document, method_prefix an exact prefix
	// of its method; order is created (newest first) or verified (least recently
	// verified first, never-verified before any).
	EvidenceDocumentID string `json:"evidence_document_id,omitempty"`
	MethodPrefix       string `json:"method_prefix,omitempty"`
	Order              string `json:"order,omitempty"`
	// The review queue's filters (document_review.go): each end's document or
	// subtree, the writer, its confidence, when it was filed, and a judge's
	// recorded verdict; summarize returns grouped counts instead of rows.
	FromDocumentID  string   `json:"from_document_id,omitempty"`
	ToDocumentID    string   `json:"to_document_id,omitempty"`
	FromUnderPath   string   `json:"from_under_path,omitempty"`
	ToUnderPath     string   `json:"to_under_path,omitempty"`
	Actor           string   `json:"actor,omitempty"`
	MinConfidence   *float64 `json:"min_confidence,omitempty"`
	CreatedBefore   *int64   `json:"created_before,omitempty"`
	CreatedAfter    *int64   `json:"created_after,omitempty"`
	HasJudgeVerdict *bool    `json:"has_judge_verdict,omitempty"`
	JudgeVerdict    string   `json:"judge_verdict,omitempty"`
	Summarize       bool     `json:"summarize,omitempty"`
	// The exact selectors a summary group is selected by: the method and the
	// kind as stored (no prefix, no sub-kinds) and the from-document's path
	// prefix as summarize computes it, or filed nowhere.
	MethodExact    string `json:"method_exact,omitempty"`
	KindExact      string `json:"kind_exact,omitempty"`
	FromPathPrefix string `json:"from_path_prefix,omitempty"`
	FromUnfiled    bool   `json:"from_unfiled,omitempty"`
	// Kind-aware retrieval (RFC FN P4). graph_recall: walk (declared | all),
	// decay (per-hop share of the content budget), include_hierarchy (up | down |
	// both | none). search: expand (one hop over relations from the top hits).
	// related: related_to (neighbours by relation rather than by embedding).
	Walk             string        `json:"walk,omitempty"`
	Decay            *float64      `json:"decay,omitempty"`
	IncludeHierarchy string        `json:"include_hierarchy,omitempty"`
	Expand           *searchExpand `json:"expand,omitempty"`
	RelatedTo        string        `json:"related_to,omitempty"`
	// Editions (RFC FN P2). import_md reimport:true reconciles an incoming edition
	// onto the document named by document_id or work_key, part by part;
	// edition_date dates the changes. resolve_anchor finds a work and a part of it
	// by work_key + anchor, and create_placeholder files a work not held yet.
	Reimport          bool   `json:"reimport,omitempty"`
	EditionDate       *int64 `json:"edition_date,omitempty"`
	WorkKey           string `json:"work_key,omitempty"`
	Anchor            string `json:"anchor,omitempty"`
	CreatePlaceholder bool   `json:"create_placeholder,omitempty"`
	// AmendingDocumentID names the document that made a reimported edition: the
	// source of the `amends` statement each moved part gets.
	AmendingDocumentID string `json:"amending_document_id,omitempty"`
	// Method is the method a code-js run names for the statement it writes
	// (rule:<name> | extractor:<name>), checked against its agent's
	// statement_methods grant by stampStatementMethod, which copies it into
	// edgeMethod. Never read past that check.
	Method string `json:"method,omitempty"`
	// maintain_statement: the action, the statement a merge keeps
	// (survivor_id), the evidence row an evidence action moves or marks
	// (evidence_id), and the chunk relocate_evidence moves it to (chunk_id).
	Action     string `json:"action,omitempty"`
	SurvivorID string `json:"survivor_id,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	ChunkID    string `json:"chunk_id,omitempty"`
	// Write is classify's write mode: "propose" (the default) files what it finds,
	// "report" writes nothing. judge_statements: "rule" (the default) rules a
	// routed proposal at or above the floor, "recommend" only records.
	Write string `json:"write,omitempty"`
	// Judgements are judge_statements' verdicts (document_review_judge.go).
	Judgements []judgementIn `json:"judgements,omitempty"`
	// edgeMethod is the statement's method when an internal path (import, sync,
	// canvas) writes it, or the wire Method once it passed the grant check.
	// UNEXPORTED: a caller must not name how its own link was made.
	edgeMethod string
	Position   *int `json:"position"`
	Revision   *int `json:"revision"`
	// FromRevision / ToRevision select the two body-change revisions to diff (RFC
	// BS Phase 3a). Pointers so an omitted bound is distinguishable from a value
	// (the log is 1-based, so 0 is never a real revision, but the pointer keeps the
	// parse honest and the missing-field error precise).
	FromRevision *int   `json:"from_revision"`
	ToRevision   *int   `json:"to_revision"`
	FromID       string `json:"from_id"`
	ToID         string `json:"to_id"`
	Kind         string `json:"kind"`

	// Tag facet (RFC BS Phase 1). Tags is a plain slice so JSON presence is
	// self-distinguishing: an omitted `tags` decodes to nil (leave existing tags
	// untouched), a present `[]` decodes to a non-nil empty slice (clear all) — the
	// unset-vs-empty distinction update_chunk relies on. Tag/TagPrefix are the
	// query filters (exact / slash-prefix).
	Tags      []string `json:"tags"`
	Tag       string   `json:"tag"`
	TagPrefix string   `json:"tag_prefix"`

	// Entity-tier fields (RFC BL P4c). NaturalKey is the idempotency handle:
	// upsert_chunk keys on it, and it is UNIQUE per scope.
	NaturalKey string `json:"natural_key"`
	// Across asks a subject read to look in the OTHER scopes this caller can reach,
	// matching on natural_key. See document_cross_scope.go for why no reference has to
	// be stored for that to work.
	Across bool `json:"across_scopes"`
	// SupersedesID names the chunk being retired by supersede_chunk. It is an
	// explicitly-named field rather than a reuse of from_id/to_id on purpose: a
	// caller who transposed those would invalidate the NEW fact and leave the stale
	// one current — a silent inversion of history, which is worse than one more
	// field to document.
	SupersedesID string `json:"supersedes_id"`
	// Pointers so "unset" is distinguishable from zero — valid_at=0 is a real
	// instant (the unix epoch), not a missing value.
	ValidAt   *int64 `json:"valid_at"`
	InvalidAt *int64 `json:"invalid_at"`
	// ObservedAt is WHEN THE THING WAS SAID, which is a different timeline from
	// when it was true. The k/v memory plane has carried it since RFC CL and the
	// chunk plane had no column for it at all — so a fact mirrored into the graph
	// silently lost the one temporal field that is actually populated (measured
	// 103/103 on a real corpus, against 27/103 for valid_at).
	ObservedAt *int64 `json:"observed_at"`
	// bodyOrigin is the origin to stamp on this chunk's BODY row, threaded from
	// the fact-tier write path. UNEXPORTED on purpose: origin names the writer, so
	// a caller must not be able to claim one — an exported field would be settable
	// from the tool's JSON input and the column would stop being trustworthy.
	bodyOrigin string
	// FromPending attributes this write to a pending-queue row the caller drained,
	// so the SERVER stamps the origin and source ids from that row. The id is the
	// only thing a caller may pass — the fields themselves stay unsettable, which
	// is what keeps origin an unforgeable statement about who wrote a fact.
	FromPending string `json:"from_pending,omitempty"`
	// SourceSessionID names the chat this fact was distilled FROM, for a fact that
	// came off the transcript path rather than a drained queue row (where
	// from_pending supplies it server-side). Descriptive metadata like the rest of
	// the provenance block — never consulted for authorization — but the erasure
	// report keys on it, so a fact without one cannot be surfaced as residue for
	// the subject who produced it.
	SourceSessionID string `json:"source_session_id,omitempty"`
	// pendingProv is what the server resolved from that row. UNEXPORTED, like
	// bodyOrigin, so the tool's JSON input can never supply it — the id is the
	// caller's to pass, the provenance is the server's to decide.
	pendingProv *store.MemoryProvenance
	Confidence  *float64 `json:"confidence"`
	// Class is 'derived' | 'evidential' — the retention-exemption signal.
	Class string `json:"class"`

	// graph_recall inputs.
	SeedIDs []string `json:"seed_ids"`
	Query   string   `json:"query"`
	Hops    *int     `json:"hops"`
	// Seeds is how many of the ranked facts start a graph_recall walk. Small on
	// purpose: seeding from every hit spends the budget before the walk
	// contributes anything. 0 = the default.
	Seeds int `json:"seeds"`
	// BudgetChars caps graph_recall on the CONTENT handed back rather than on the
	// row count, and turns on the backfill. A walk fetches more rows than a search
	// by construction, so a row limit lets a walk win on volume; and when the graph
	// runs dry the walk under-fills, which is worse than not walking. 0 = the
	// old row-limited behaviour, unchanged.
	BudgetChars int `json:"budget_chars"`
	// BodyChars is the excerpt length on graph_recall rows. nil = the default
	// (an excerpt on non-fact rows only), 0 = no bodies (the titles-only response),
	// N = an excerpt of up to N characters on every row. A pointer, because 0 and
	// unset ask for different things.
	BodyChars      *int   `json:"body_chars"`
	AsOf           *int64 `json:"as_of"`
	IncludeRetired bool   `json:"include_retired"`
	// MediaType/Data/Filename carry an image asset for set_asset (RFC BO). Data
	// is standard base64 (no data: prefix); it is decoded to raw bytes and stored
	// in the chunk_assets BYTEA/BLOB table.
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	Filename  string `json:"filename"`
	UnderPath string `json:"under_path"`
	// Structural filters on search / related, beside the singular fields above
	// that query_chunks already takes (see document_search_filter.go). Plural
	// twins rather than string-or-list unions: the singular fields are plain
	// strings shared by every op, and a union type is a schema shape some
	// providers' function declarations reject. Concept / ConceptPrefix are named
	// apart from Subject, which is upsert_chunk's entity input.
	UnderPaths         []string `json:"under_paths"`
	ExcludeUnderPath   string   `json:"exclude_under_path"`
	ExcludeUnderPaths  []string `json:"exclude_under_paths"`
	ExcludeDocumentIDs []string `json:"exclude_document_ids"`
	Types              []string `json:"types"`
	Statuses           []string `json:"statuses"`
	Concept            string   `json:"concept"`
	ConceptPrefix      string   `json:"concept_prefix"`
	SQL                string   `json:"sql"`
	Limit              int      `json:"limit"`
	Name               string   `json:"name"`
	// TTLMs is the lease op's hold duration in milliseconds (0 = default).
	TTLMs int `json:"ttl_ms"`
	// IncludeMetadata gates export_md's round-trip comments (default true when
	// omitted; a pointer so an explicit `false` is distinguishable from unset).
	IncludeMetadata *bool `json:"include_metadata"`
	// Markdown is the import_md source (an export_md-shaped document).
	Markdown string `json:"markdown"`
	// Canvas is the import_canvas source: a JSON Canvas v1.0 object
	// ({"nodes":[...],"edges":[...]}). Kept as RawMessage so a caller can hand in
	// an object verbatim (e.g. the contents of an Obsidian .canvas file) and it is
	// decoded into the canvas struct in import_canvas.
	Canvas    json.RawMessage `json:"canvas"`
	Source    string          `json:"source,omitempty"`     // set_remote: the document_sources name
	RemoteRef string          `json:"remote_ref,omitempty"` // set_remote: the remote doc's path or id
	// SourceURL / SourceKind / SourceTitle are import_md's link to where the
	// document came from (an email, a SharePoint file). Three fields rather
	// than one `source` object because `source` is set_remote's string.
	SourceURL   string `json:"source_url,omitempty"`
	SourceKind  string `json:"source_kind,omitempty"`
	SourceTitle string `json:"source_title,omitempty"`
}

// docSchemaDDL is portable across SQL Memory's sqlite + postgres tiers: BIGINT
// for unix-nanos timestamps, TEXT/INTEGER otherwise, no foreign keys (cascade
// is done explicitly in Go so it also cleans the Memory bodies and doesn't
// depend on per-backend FK enforcement).
var docSchemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS documents (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, root_chunk_id TEXT NOT NULL,
		created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chunks (
		id TEXT PRIMARY KEY, document_id TEXT NOT NULL, parent_id TEXT,
		position INTEGER NOT NULL, type TEXT, status TEXT, title TEXT NOT NULL,
		created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
		valid_at BIGINT, invalid_at BIGINT)`,
	// chunk_edges — a relation is a STATEMENT (RFC FN): its own id, the envelope
	// (status, method, validity, provenance, verdict) and evidence rows in
	// chunk_edge_evidence. See document_statements.go for the columns and for
	// migrateEdgeStatements, which rebuilds a scope created before them.
	`CREATE TABLE IF NOT EXISTS chunk_edges (` + chunkEdgesColumns + `)`,
	`CREATE TABLE IF NOT EXISTS chunk_edge_evidence (` + chunkEdgeEvidenceColumns + `)`,
	`CREATE TABLE IF NOT EXISTS chunk_types (
		document_id TEXT NOT NULL, name TEXT NOT NULL, fields TEXT NOT NULL,
		created_at BIGINT NOT NULL, PRIMARY KEY (document_id, name))`,
	// chunk_memory_meta — the bi-temporal + provenance sidecar for the entity tier.
	// Created UNCONDITIONALLY: the tier is opt-in at the level of "does this scope
	// have entities", not at the level of "does this scope have the table". Gating
	// the DDL would let a scope exist in two shapes and force every later read to
	// ask which one it got; one empty table per scope is the cheaper answer.
	//
	// `confidence` is DOUBLE PRECISION, not REAL, and the difference is not
	// cosmetic: postgres REAL is 4-byte float4, so a confidence written as 0.9 read
	// back as 0.8999999761581421 there while sqlite (whose REAL is always 8-byte)
	// returned 0.9. The two tiers disagreed on a stored value, which is how a shared
	// contract quietly stops being one — and a `WHERE confidence >= 0.9` filter would
	// have excluded a row written as exactly 0.9 on postgres only. sqlite gives
	// DOUBLE PRECISION the same REAL affinity, so one spelling serves both.
	// migrateConfidencePrecision widens the column on scopes created before this.
	//
	// TWO timelines, which is the whole point:
	//   valid_at / invalid_at    — when the fact was true IN THE WORLD
	//   created_at / expired_at  — when the SYSTEM learned and retired it
	// Keeping them apart is what lets "as of June…" stay answerable after a
	// correction: a contradicting fact closes the old row's invalid_at/expired_at
	// and links `supersedes`, rather than deleting it.
	//
	// natural_key is the idempotency handle ({scope}:{type}:{canonical-name} for an
	// entity, {subject}|{predicate}|{object} for a fact). It lives HERE, in SQL,
	// rather than in the chunk's `fields` as the design first specified — `fields`
	// is stored inside the chunkBody JSON blob in the Memory k/v plane, so
	// "does this entity already exist?" would have meant reading every chunk body
	// out of Memory and parsing it: O(n) on the one operation an entity graph does
	// constantly.
	//
	// No foreign key, matching the rest of this schema — cascade is explicit in Go
	// (see deleteChunk / delete_document) so it also cleans the Memory bodies and
	// does not depend on per-backend FK enforcement.
	`CREATE TABLE IF NOT EXISTS chunk_memory_meta (
		chunk_id TEXT PRIMARY KEY,
		valid_at BIGINT, invalid_at BIGINT,
		created_at BIGINT, expired_at BIGINT,
		class TEXT, origin TEXT, confidence DOUBLE PRECISION,
		session_id TEXT, run_id TEXT, event_seq BIGINT,
		natural_key TEXT, source_quote TEXT, subject TEXT,
		judged_at BIGINT, judge_reason TEXT, judged_by TEXT)`,
	// UNIQUE per SCOPE, not per document: each scope owns its own database (a
	// sqlite file / a postgres schema), so a bare UNIQUE index on the column IS
	// scope-wide — one entity per tenant regardless of which document holds it.
	//
	// The column is NULLABLE and almost every chunk leaves it unset. Both tiers
	// treat NULLs as DISTINCT in a unique index, so ordinary chunks never collide;
	// that portability assumption is asserted by a test rather than trusted.
	`CREATE UNIQUE INDEX IF NOT EXISTS chunk_memory_meta_natural_key ON chunk_memory_meta(natural_key)`,
	`CREATE INDEX IF NOT EXISTS chunks_doc_parent_pos ON chunks(document_id, parent_id, position)`,
	`CREATE INDEX IF NOT EXISTS chunks_doc_type_status ON chunks(document_id, type, status)`,
	// The REVERSE edge index. chunk_edges' primary key (from_id, to_id, kind)
	// serves a forward walk only, so every reverse hop of a graph expansion would
	// scan the scope's whole edge table. Absent this, retrieval degrades only once
	// the graph is large enough to matter — the failure that shows up in
	// production and not in a test.
	`CREATE INDEX IF NOT EXISTS chunk_edges_to_kind ON chunk_edges(to_id, kind)`,
	// chunk_tags / document_tags — the multi-valued tag facet (RFC BS Phase 1).
	// Tags are a JOIN table (not a chunk `fields` key) precisely because they are a
	// query axis: query_chunks / query_documents filter on them, and a `fields` key
	// lives inside the chunkBody JSON blob in the Memory k/v plane where SQL cannot
	// reach it. A document's tags are INDEPENDENT of its root chunk's tags — they
	// live in a separate table. Nested tags (area/sub/topic) are stored as the full
	// slash string; the hierarchy is a prefix query (query_chunks tag_prefix). No
	// foreign key — cascade is explicit in Go (deleteChunk / deleteDocument),
	// matching the rest of this schema.
	`CREATE TABLE IF NOT EXISTS chunk_tags (
		chunk_id TEXT NOT NULL, tag TEXT NOT NULL, PRIMARY KEY (chunk_id, tag))`,
	`CREATE INDEX IF NOT EXISTS chunk_tags_tag ON chunk_tags(tag)`,
	`CREATE TABLE IF NOT EXISTS document_tags (
		document_id TEXT NOT NULL, tag TEXT NOT NULL, PRIMARY KEY (document_id, tag))`,
	`CREATE INDEX IF NOT EXISTS document_tags_tag ON document_tags(tag)`,
	// chunk_revisions — the append-only body-change log (RFC BS Phase 3a). The
	// store's Memory API is OVERWRITE-only: MemorySet upserts and there is no
	// version-history read, so a chunk's prior BODIES have nowhere to live unless
	// they are kept here. One row per body WRITE: create_chunk seeds revision 1 and
	// each body-bearing update_chunk appends the post-bump revision. A metadata-only
	// update writes no row (its body is unchanged from the last snapshot), so this
	// table lists exactly the revisions at which the BODY changed. No foreign key —
	// cascade is explicit in Go (deleteChunk / deleteDocument), matching the rest of
	// this schema; the (chunk_id, revision) primary key makes a re-run's snapshot
	// idempotent (recordRevision's guarded INSERT relies on it).
	`CREATE TABLE IF NOT EXISTS chunk_revisions (
		chunk_id TEXT NOT NULL, revision INTEGER NOT NULL, created_at BIGINT NOT NULL,
		actor TEXT, body TEXT NOT NULL, PRIMARY KEY (chunk_id, revision))`,
	`CREATE INDEX IF NOT EXISTS chunk_revisions_chunk ON chunk_revisions(chunk_id)`,
	// chunk_layout — a chunk's spatial coordinates for the JSON Canvas view
	// (export_canvas / import_canvas). This is VIEW metadata (where a node sits on
	// a canvas), NOT a query axis, so it is its own thin table rather than a
	// chunks column: a chunk without a layout row is the normal case (export
	// falls back to a deterministic auto-grid), and only a chunk placed on a
	// canvas ever gets a row. x/y/width/height are integers per the JSON Canvas
	// spec; color is a nullable preset id ("1".."6") or hex ("#RRGGBB"). No
	// foreign key — cascade is explicit in Go (deleteChunk / deleteDocument),
	// matching the rest of this schema.
	`CREATE TABLE IF NOT EXISTS chunk_layout (
		chunk_id TEXT PRIMARY KEY, x INTEGER NOT NULL, y INTEGER NOT NULL,
		width INTEGER NOT NULL, height INTEGER NOT NULL, color TEXT)`,
	// document_leases: a named lease on this scope (document_lease.go).
	documentLeasesDDL,
	// statement_rulings / statement_ruling_items: each bulk ruling and what it
	// changed, so it can be undone together (document_review.go).
	statementRulingsDDL,
	statementRulingItemsDDL,
}

// maxChunkDepth caps the ancestor walk in move_chunk (cycle detection) so a
// corrupt tree can't hang it.
const maxChunkDepth = 10000

func newDocID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("id%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (d *Document) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if d.Store == nil || d.SqlMem == nil {
		return errBusiness("Document tool: not configured — requires the Store backend and SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)", "An operator must enable SQL Memory on this server; until then the Document tool cannot run."), nil
	}
	var in docInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errValidation("invalid input JSON: "+err.Error(), "Resend the call as one JSON object matching the Document input schema."), nil
	}
	return d.executeInput(ctx, in, raw)
}

// executeInput is Execute after the input is decoded: every gate, then the op.
// An in-process caller that builds its input in Go (the review queue) enters
// here, so it passes the same gates without a JSON round trip, which would let
// any field it set reach every other field's use.
func (d *Document) executeInput(ctx context.Context, in docInput, raw json.RawMessage) (tools.Result, error) {
	if d.Store == nil || d.SqlMem == nil {
		return errBusiness("Document tool: not configured — requires the Store backend and SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)", "An operator must enable SQL Memory on this server; until then the Document tool cannot run."), nil
	}
	// Before any lookup: a cut id is refused on its syntax alone.
	if r, cut := refuseCutIDs(in); cut {
		return r, nil
	}
	if refused := stampStatementMethod(ctx, &in); refused != nil {
		return *refused, nil
	}
	key, mscope, err := d.resolveScope(ctx, in.Scope)
	if err != nil {
		return docFail("", err), nil
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return errFrom("document: schema init: "+err.Error(), err), nil
	}
	// RFC CA: an agent may PROPOSE an ontology entity, never decide one. Checked at the
	// single dispatch point rather than inside each op — a per-op check is one op away
	// from being forgotten, and the ops that would need it (create/update/delete/move/
	// reorder) are exactly the ones a future change is likely to add a sibling to.
	if refused := d.guardOntologyWrite(ctx, key, in.Op, in); refused != nil {
		return *refused, nil
	}
	// RFC EZ §1.5 seam 5: the caller's access-control set, asked about the
	// document(s) this call resolves to; see document_acl.go.
	acl, refused, err := d.guardDocumentACL(ctx, key, in)
	if err != nil {
		return errFrom("document: access control: "+err.Error(), err), nil
	}
	if refused != nil {
		return *refused, nil
	}
	// RFC FO P1a: typed classes — field warnings and identity keys for the four write
	// ops; see document_ontology_fields.go. Pre-write so a collision leaves nothing behind.
	classRules, refused := d.applyClassRules(ctx, key, mscope, acl, &in, raw)
	if refused != nil {
		return *refused, nil
	}
	res, err := d.dispatch(ctx, key, mscope, in, raw)
	if err != nil {
		return res, err
	}
	res = d.finishClassRules(ctx, key, in, classRules, res)
	return acl.filterListing(ctx, in.Op, res), nil
}

// dispatch runs one op; Execute has resolved the scope and asked the gates.
func (d *Document) dispatch(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, raw json.RawMessage) (tools.Result, error) {
	switch in.Op {
	case "create_document":
		return d.createDocument(ctx, key, mscope, in)
	case "get_document":
		return d.getDocument(ctx, key, mscope, in)
	case "documents_summary":
		return d.documentsSummary(ctx, key, mscope, in)
	case "query_documents":
		return d.queryDocuments(ctx, key, in)
	case "delete_document":
		return d.deleteDocument(ctx, key, mscope, in)
	case "set_path":
		return d.setPath(ctx, key, in)
	case "judge_fact":
		return d.judgeFact(ctx, key, mscope, in)
	case "propose_entity":
		// RFC CA: files an INERT ontology proposal. Resolves its own tenant scope, so it
		// is deliberately not routed through the caller's `scope` argument.
		return d.proposeEntity(ctx, in)
	case "propose_subject":
		return d.proposeSubject(ctx, in)
	case "create_chunk":
		return d.createChunk(ctx, key, mscope, in)
	case "get_chunk":
		return d.getChunk(ctx, key, mscope, in)
	case "update_chunk":
		return d.updateChunk(ctx, key, mscope, in, raw)
	case "delete_chunk":
		return d.deleteChunk(ctx, key, mscope, in)
	case "move_chunk":
		return d.moveChunk(ctx, key, in)
	case "reorder_chunk":
		return d.reorderChunk(ctx, key, mscope, in)
	case "upsert_chunk":
		return d.upsertChunkOp(ctx, key, mscope, in, raw)
	case "supersede_chunk":
		return d.supersedeChunk(ctx, key, in)
	case "graph_recall":
		return d.graphRecall(ctx, key, in)
	case "remember":
		return d.rememberFact(ctx, key, mscope, in)
	case "verbatim_answer":
		return d.verbatimAnswer(ctx, key, mscope, in)
	case "verification_stats":
		return d.verificationStats(ctx, key)
	case "list_facts":
		return d.listFacts(ctx, key, in)
	case "link_chunks":
		return d.linkChunks(ctx, key, mscope, in)
	case "unlink_chunks":
		return d.unlinkChunks(ctx, key, mscope, in)
	case "get_edges":
		return d.getEdges(ctx, key, in)
	case "get_edge":
		return d.getEdge(ctx, key, in)
	case "propose_statement":
		return d.proposeStatement(ctx, key, mscope, in)
	case "accept_statement":
		return d.reviewStatement(ctx, key, mscope, in, true)
	case "reject_statement":
		return d.reviewStatement(ctx, key, mscope, in, false)
	case "list_statements":
		return d.listStatements(ctx, key, in)
	case "judge_statements":
		return d.judgeStatements(ctx, key, mscope, in)
	case "maintain_statement":
		return d.maintainStatement(ctx, key, mscope, in)
	case "resolve_anchor":
		return d.resolveAnchorOp(ctx, key, mscope, in)
	case "lease":
		return d.leaseOp(ctx, key, in)
	case "release":
		return d.releaseOp(ctx, key, in)
	case "describe_ontology":
		// Reads the TENANT ontology whatever `scope` says, as propose_entity files there.
		return d.describeOntology(ctx)
	case "classify":
		return d.classify(ctx, key, mscope, in)
	case "accept_classification":
		return d.acceptClassification(ctx, key, mscope, in)
	case "extract_citations":
		return d.extractCitations(ctx, key, mscope, in)
	case "query_chunks":
		return d.queryChunks(ctx, key, in)
	case "add_tags":
		return d.addTags(ctx, key, in)
	case "remove_tags":
		return d.removeTags(ctx, key, in)
	case "list_tags":
		return d.listTags(ctx, key, in)
	case "define_type":
		return d.defineType(ctx, key, in)
	case "list_types":
		return d.listTypes(ctx, key, in)
	case "set_asset":
		return d.setAsset(ctx, key, mscope, in)
	case "get_asset":
		return d.getAsset(ctx, key, in)
	case "export_md":
		return d.exportMD(ctx, key, mscope, in)
	case "import_md":
		return d.importMD(ctx, key, mscope, in)
	case "export_canvas":
		return d.exportCanvas(ctx, key, mscope, in)
	case "import_canvas":
		return d.importCanvas(ctx, key, mscope, in)
	case "history":
		return d.chunkHistory(ctx, key, in)
	case "get_version":
		return d.getVersion(ctx, key, in)
	case "diff":
		return d.diffVersions(ctx, key, in)
	case "backlinks":
		return d.backlinks(ctx, key, in)
	case "search":
		return d.searchChunks(ctx, key, mscope, in)
	case "related":
		return d.related(ctx, key, mscope, in)
	case "unlinked_mentions":
		return d.unlinkedMentions(ctx, key, mscope, in)
	case "set_remote":
		return d.setRemote(ctx, key, mscope, in)
	case "sync":
		return d.syncDocument(ctx, key, mscope, in)
	case "diff_remote":
		return d.diffRemote(ctx, key, mscope, in)
	case "":
		return errValidation("missing required field: op", "Pass `op`, e.g. op=query_documents to list documents or op=get_document to read one."), nil
	default:
		return errValidation(fmt.Sprintf("unknown op %q", in.Op), "Pass one of the ops in the Document tool's `op` enum."), nil
	}
}

func (d *Document) resolveScope(ctx context.Context, requested string) (sqlmem.ScopeKey, store.MemoryScope, error) {
	// SQL Memory rejects an empty tenant (it sanitizes the tenant into a path/
	// identifier); canonicalize ""→"default" exactly like the Memory tool's SQL
	// ops. NOTE: the dirent ops use the RAW tenant instead (see direntTenant) so
	// Document's Path-tree entries interoperate with the Path/Memory/Volume
	// dirents, which all key on the raw RunIdentity tenant.
	sqlTenant := sqlScopeTenant(ctx)
	if requested == "" {
		requested = "user"
	}
	// RFC BX P2b: an isolated member (substrate:user) may author/read only its own
	// user/agent-scoped documents — refuse the tenant-shared scope even when the
	// agent holds the tenant grants below. Server-derived from the run's Isolated
	// bit; a non-isolated run is unaffected.
	if err := tools.ConfineIsolatedScope(ctx, store.MemoryScope(requested)); err != nil {
		return sqlmem.ScopeKey{}, "", docClassify(err, errPermission, "Use scope=user or scope=agent.")
	}
	switch requested {
	case "agent":
		name := tools.AgentName(ctx)
		if name == "" {
			return sqlmem.ScopeKey{}, "", docClassify(fmt.Errorf("Document: scope=agent requires a yaml-declared agent"),
				errBusiness, "Use scope=user.")
		}
		return sqlmem.ScopeKey{Tenant: sqlTenant, Scope: "agent", ScopeID: name}, store.MemoryScopeAgent, nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return sqlmem.ScopeKey{}, "", docClassify(fmt.Errorf("Document: scope=user requires a user_id on the run"),
				errBusiness, "Use scope=agent, or start the run with a user_id.")
		}
		return sqlmem.ScopeKey{Tenant: sqlTenant, Scope: "user", ScopeID: uid}, store.MemoryScopeUser, nil
	case "tenant":
		// The two planes take DIFFERENT scope ids for the same logical scope, and
		// the asymmetry is load-bearing rather than an oversight:
		//
		//   sqlmem  ScopeID = the tenant — pgScopeNames hashes
		//           sha256(tenant \x1f scope \x1f scope_id) into a schema + LOGIN
		//           role name and REJECTS an empty component.
		//   Memory  scope_id = ""       — the tenant_id column already carries the
		//           identity, matching global's convention and the Path tree's
		//           tenant dirent.
		//
		// GATED, unlike agent and user above. A tenant document is readable AND
		// writable by every user and agent in the tenant, so an ungated one lets any
		// agent holding `tools: [Document]` publish state the whole tenant then reads
		// as ground truth — which is the poisoning surface, reached without the
		// operator ever granting anything.
		//
		// The check is deliberately scoped to `tenant` and does NOT retrofit onto
		// agent/user. Those have been ungated on this path since the tool shipped, so
		// requiring a grant for them would break every existing agent that holds
		// Document without declaring scopes; that is a separate, breaking change and
		// does not belong in the commit that introduces a new scope. What must not
		// happen is a NEW, cross-user scope inheriting the ungated posture.
		//
		// BOTH grants are required, because a document write touches BOTH planes: the
		// chunk structure goes to SQL Memory (sql_scopes) and the chunk bodies to k/v
		// Memory (memory_scopes). Granting one and not the other would let an agent
		// reach half of a tenant store — and a half-written document is not a partial
		// failure, it is structure with no text, which is the shape this store has
		// already been broken into once.
		//
		// Requiring both also keeps one rule for the whole tenant scope: whichever
		// tool an operator reasons about, the same two lines of yaml are what opens
		// it. A single-grant gate here would mean Document needed less authority than
		// the Memory tool does for the same data.
		missing := make([]string, 0, 2)
		if !contains(tools.PolicyOf(ctx).Memory.AllowedScopes, "tenant") {
			missing = append(missing, "memory_scopes: [tenant]")
		}
		if !contains(tools.PolicyOf(ctx).SqlMem.AllowedScopes, "tenant") {
			missing = append(missing, "sql_scopes: [tenant]")
		}
		if len(missing) > 0 {
			return sqlmem.ScopeKey{}, "", docClassify(fmt.Errorf("Document: scope=tenant is not granted to this agent — "+
				"a tenant document is readable and writable by every user and agent in the tenant, so the "+
				"operator must opt in. Missing on the agent: %s (a document needs both — structure lives in "+
				"SQL Memory, chunk bodies in Memory)", strings.Join(missing, " and ")),
				errPermission, "Use scope=user or scope=agent, or ask an operator to grant the tenant scope.")
		}
		return sqlmem.ScopeKey{Tenant: sqlTenant, Scope: "tenant", ScopeID: sqlTenant}, store.MemoryScopeTenant, nil
	default:
		return sqlmem.ScopeKey{}, "", docClassify(fmt.Errorf("Document: unknown scope %q (agent | user | tenant)", requested),
			errValidation, "Pass scope as agent, user or tenant.")
	}
}

// docClassified is an error this file built whose failure category is already
// known where it was made — a refused scope, a malformed path. A helper returns
// it as an error, and the op that surfaces it prefixes its own name; docFail
// recovers the category so the prefix does not cost the model the kind of
// failure. Its text is the wrapped error's, unchanged.
type docClassified struct {
	err  error
	kind func(msg, next string) tools.Result
	next string
}

func (e *docClassified) Error() string { return e.err.Error() }
func (e *docClassified) Unwrap() error { return e.err }

func docClassify(err error, kind func(msg, next string) tools.Result, next string) error {
	return &docClassified{err: err, kind: kind, next: next}
}

// docPathFix is the next step for a Path-tree path that is not well formed.
const docPathFix = "Pass an absolute path below the root, e.g. /documents/<name>."

// docFail is the failure prefix+err.Error(). An error built by docClassify keeps
// its category; any other came back from a store or backend and is classified
// by its type (errFrom).
func docFail(prefix string, err error) tools.Result {
	msg := prefix + err.Error()
	var dc *docClassified
	if errors.As(err, &dc) {
		return dc.kind(msg, dc.next)
	}
	return errFrom(msg, err)
}

// docRawSQLFail is a failure of query_chunks' raw `sql` escape hatch. The SQL
// is the caller's own, so a statement the read-only validator refuses is the
// caller's to fix; a driver or backend fault is classified by its type.
func docRawSQLFail(msg string, err error) tools.Result {
	var se *sqlmem.ErrStatement
	if errors.As(err, &se) {
		return errValidation(msg, "Send one read-only SELECT statement, or use query_chunks' filters (document_id, type, status, tag) instead of `sql`.")
	}
	return errFrom(msg, err)
}

// direntTenant is the tenant used for Path-tree dirents — the RAW
// RunIdentity tenant (NOT the SQL-canonicalized one), so Document's document
// dirents share the same namespace as the Path/Memory/Volume dirents (which
// all key on the raw tenant). In open mode this is "" for dirents while SQL
// uses "default"; both consistently represent the single/default tenant.
func direntTenant(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID }

// ensureSchema provisions the document tables for a scope, ONCE per process.
//
// It used to run in full on every op: 16 CREATE … IF NOT EXISTS statements, an
// asset DDL and 9 migration probes ahead of the actual work. Measured on the sqlite
// tier that was 322µs of a 453µs get_chunk — 71% of the call — and on postgres each
// statement is a network round trip. A single benchmark run issued 915 Document
// calls, so ~24k round trips existed only to re-assert a schema already there.
//
// The memo lives on the sqlmem Manager, not on this struct: &Document{} is built
// ad-hoc in a dozen places (the Memory tool's placement helpers, Context, the HTTP
// describe handler) and they all share one Manager pointer, so a per-struct cache
// would miss every one of them.
// docSQL is the SQL Memory plane the document tables live on. On postgres it is
// a schema and login role of its own, which an agent's SQL cannot reach (see
// sqlmem.Manager.DocPlane); on sqlite it is the scope's one file.
func (d *Document) docSQL() *sqlmem.Manager { return d.SqlMem.DocPlane() }

func (d *Document) ensureSchema(ctx context.Context, key sqlmem.ScopeKey) error {
	return d.docSQL().EnsureOnce(key, func() error { return d.provisionSchema(provisioning(ctx), key) })
}

// provisioningCtxKey marks a context as already inside schema provisioning.
//
// ⚠️ WITHOUT THIS THE RETRY BELOW IS INFINITELY RECURSIVE, and the shape is not
// obvious: the migrations probe for a column with `SELECT col FROM t WHERE 1=0` and
// read the ERROR as the answer — "no such column" IS how tableHasColumn says no. The
// staleness retry treats that same error as a stale memo, re-provisions, which runs
// the migrations, which probe again. One statement whose failure is expected turns
// into a loop that never terminates.
type provisioningCtxKey struct{}

func provisioning(ctx context.Context) context.Context {
	return context.WithValue(ctx, provisioningCtxKey{}, true)
}

func isProvisioning(ctx context.Context) bool {
	v, _ := ctx.Value(provisioningCtxKey{}).(bool)
	return v
}

// reensureSchema re-provisions after the memo has been invalidated. See the call
// site in query/exec: a scope dropped underneath a running process leaves the memo
// asserting tables that are gone.
func (d *Document) reensureSchema(ctx context.Context, key sqlmem.ScopeKey) error {
	if isProvisioning(ctx) {
		return errAlreadyProvisioning
	}
	d.docSQL().ForgetEnsured(key)
	return d.ensureSchema(ctx, key)
}

// errAlreadyProvisioning short-circuits the staleness retry for statements issued
// BY provisioning itself. Returned rather than nil so the caller does not retry.
var errAlreadyProvisioning = errors.New("document: already provisioning")

func (d *Document) provisionSchema(ctx context.Context, key sqlmem.ScopeKey) error {
	// A scope created before the document tables had a plane of their own keeps
	// them on the agent plane. They are brought to the current shape where they
	// are first, so every migration's backfill runs on the old rows, then the
	// rows move into the fresh tables provisioned on the document plane.
	held, err := d.docSQL().AgentPlaneTables(ctx, key, documentTableNames)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		if err := d.provisionTables(ctx, key.AgentPlane()); err != nil {
			return err
		}
	}
	if err := d.provisionTables(ctx, key); err != nil {
		return err
	}
	// Even with nothing to move: the call records that the scope has its document
	// plane, after which nothing on the agent plane is ever copied over.
	return d.docSQL().AdoptAgentTables(ctx, key, documentTableNames)
}

// provisionTables creates the document tables on the key's plane and runs every
// migration on them.
func (d *Document) provisionTables(ctx context.Context, key sqlmem.ScopeKey) error {
	for _, ddl := range docSchemaDDL {
		if _, err := d.docSQL().Exec(ctx, key, ddl, nil, 0); err != nil {
			return err
		}
	}
	// chunk_assets (RFC BO) holds image chunk bytes as TRUE binary. The binary
	// column type is the ONE non-portable part of the doc schema — sqlite BLOB vs
	// postgres BYTEA — so it is built per-tier here rather than in the static
	// docSchemaDDL. Bytes are bound/scanned as []byte; base64 exists only on the
	// wire (set_asset payload / export_md data-URL), decoded before storage.
	binType := "BLOB"
	if d.docSQL().Tier() == "postgres" {
		binType = "BYTEA"
	}
	assetDDL := `CREATE TABLE IF NOT EXISTS chunk_assets (
		chunk_id TEXT PRIMARY KEY, media_type TEXT NOT NULL, bytes ` + binType + ` NOT NULL,
		size BIGINT NOT NULL, created_at BIGINT NOT NULL)`
	if _, err := d.docSQL().Exec(ctx, key, assetDDL, nil, 0); err != nil {
		return err
	}
	if err := d.migrateConfidencePrecision(ctx, key); err != nil {
		return err
	}
	if err := d.migrateDocumentFacets(ctx, key); err != nil {
		return err
	}
	if err := d.backfillDocumentTitles(ctx, key); err != nil {
		return err
	}
	if err := d.migrateAssetDescription(ctx, key); err != nil {
		return err
	}
	if err := d.migrateFactObservation(ctx, key); err != nil {
		return err
	}
	if err := d.migrateChunkValidity(ctx, key); err != nil {
		return err
	}
	if err := d.migrateEdgeAuto(ctx, key); err != nil {
		return err
	}
	if err := d.migrateEdgeStatements(ctx, key); err != nil {
		return err
	}
	if err := d.migrateSourceQuote(ctx, key); err != nil {
		return err
	}
	if err := d.migrateSubject(ctx, key); err != nil {
		return err
	}
	if err := d.migrateVerdict(ctx, key); err != nil {
		return err
	}
	if err := d.migrateJudgedBy(ctx, key); err != nil {
		return err
	}
	if err := d.migrateIdentityKey(ctx, key); err != nil {
		return err
	}
	if err := d.migrateAnchors(ctx, key); err != nil {
		return err
	}
	if err := d.migrateSourceGone(ctx, key); err != nil {
		return err
	}
	if err := d.migrateMentionKinds(ctx, key); err != nil {
		return err
	}
	return d.migrateOntologyVersion(ctx, key)
}

// migrateConfidencePrecision widens chunk_memory_meta.confidence from postgres
// float4 to float8 on a scope provisioned before the column was declared DOUBLE
// PRECISION.
//
// Postgres-only: sqlite's REAL is already 8-byte, so its rows never lost precision,
// and sqlite cannot ALTER a column type at all. A `CREATE TABLE IF NOT EXISTS`
// silently leaves an existing table alone, so without this a scope created in the
// window where the column was REAL keeps float4 forever — and "some scopes are
// float4" is exactly the two-shapes problem the sidecar's own comment argues against
// above.
//
// It CHECKS before it alters rather than running an unconditional ALTER TYPE. The
// unconditional form takes an ACCESS EXCLUSIVE lock and can rewrite the table, and
// ensureSchema runs on every Document op — so the guard is what keeps this from
// being a lock on the hot path. The catalog read is one round trip among the ~9 DDL
// statements already issued here.
func (d *Document) migrateConfidencePrecision(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.docSQL().Tier() != "postgres" {
		return nil
	}
	res, err := d.query(ctx, key,
		`SELECT data_type FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND table_name = 'chunk_memory_meta' AND column_name = 'confidence'`)
	if err != nil || len(res.Rows) == 0 {
		// A missing row means the table is not there yet, which the DDL above just
		// handled; either way this is best-effort widening, not a correctness gate.
		return err
	}
	if !strings.EqualFold(asStr(res.Rows[0][0]), "real") {
		return nil // already double precision
	}
	_, err = d.docSQL().Exec(ctx, key,
		`ALTER TABLE chunk_memory_meta ALTER COLUMN confidence TYPE DOUBLE PRECISION`, nil, 0)
	return err
}

// migrateDocumentFacets adds the denormalized `type`/`status` columns to an
// existing `documents` table (RFC BS Phase 1) and backfills them from each
// document's root chunk. Like migrateConfidencePrecision it PROBES before it
// alters, because ensureSchema is on every op's hot path: the fast case is a
// single 0-row SELECT.
//
// Why ALTER rather than declaring the columns in docSchemaDDL's CREATE TABLE? A
// `CREATE TABLE IF NOT EXISTS` leaves a table provisioned before this change
// untouched, so an existing scope would keep the old 5-column shape forever. The
// columns therefore arrive via ALTER on every scope, new and old alike — the one
// path that reaches both.
//
// The column probe is a leading-SELECT (`SELECT <col> FROM documents WHERE 1=0`)
// rather than a bare `PRAGMA table_info`: the SQL Memory validator denies a
// leading PRAGMA (see internal/sqlmem/validate.go), and a WHERE-1=0 select is both
// validator-safe AND tier-portable (no information_schema / pragma-function
// split), erroring iff the column is absent.
func (d *Document) migrateDocumentFacets(ctx context.Context, key sqlmem.ScopeKey) error {
	// Fast path: both columns present → nothing to do (the common case).
	if _, err := d.query(ctx, key, `SELECT type, status FROM documents WHERE 1=0`); err == nil {
		return nil
	}
	// At least one column is missing — add whichever is absent. Per-column probes
	// avoid a duplicate-column error when only one is missing (a crash between the
	// two ALTERs on a prior run).
	if !d.documentsHasColumn(ctx, key, "type") {
		if err := d.exec(ctx, key, `ALTER TABLE documents ADD COLUMN type TEXT`); err != nil {
			return err
		}
	}
	if !d.documentsHasColumn(ctx, key, "status") {
		if err := d.exec(ctx, key, `ALTER TABLE documents ADD COLUMN status TEXT`); err != nil {
			return err
		}
	}
	// The composite index only becomes creatable once the columns exist, so it
	// lives here rather than in docSchemaDDL (which runs before this migration).
	if err := d.exec(ctx, key, `CREATE INDEX IF NOT EXISTS documents_type_status ON documents(type, status)`); err != nil {
		return err
	}
	// Backfill from each document's root chunk, per column and only where still
	// NULL — cheap once populated, and crash-safe if a prior run added one column
	// and backfilled it before adding the other.
	if err := d.exec(ctx, key,
		`UPDATE documents SET type = (SELECT type FROM chunks WHERE chunks.id = documents.root_chunk_id) WHERE type IS NULL`); err != nil {
		return err
	}
	return d.exec(ctx, key,
		`UPDATE documents SET status = (SELECT status FROM chunks WHERE chunks.id = documents.root_chunk_id) WHERE status IS NULL`)
}

// backfillDocumentTitles repairs documents renamed before mirrorRootFacets carried
// the title: update_chunk/upsert_chunk renamed the root chunk but left
// documents.title — the copy every listing reads — on the old name.
//
// Unlike the type/status backfill above it cannot key on NULL (the drifted row
// holds a stale value, not a missing one), and it cannot hide behind
// migrateDocumentFacets' fast path (those columns exist on every scope it must
// repair). So it runs on every provisioning — once per scope per process, behind
// the ensureSchema memo — and writes only rows that differ, which makes a second
// run a no-op. Cost is one PK lookup into chunks per document. A blank root title
// is skipped, matching the mirror: the row keeps its last name.
func (d *Document) backfillDocumentTitles(ctx context.Context, key sqlmem.ScopeKey) error {
	return d.exec(ctx, key,
		`UPDATE documents SET title = (SELECT c.title FROM chunks c WHERE c.id = documents.root_chunk_id)
		 WHERE EXISTS (SELECT 1 FROM chunks c WHERE c.id = documents.root_chunk_id
		                 AND TRIM(c.title) <> '' AND c.title <> documents.title)`)
}

// migrateFactObservation adds chunk_memory_meta.observed_at, access_count and
// last_accessed_at to a scope provisioned before the chunk plane had to carry
// them.
//
// MEASURED, and it is why this exists: on one real corpus every one of 103 facts
// carried observed_at in the k/v memory row and the chunk sidecar had NO COLUMN
// FOR IT. observed_at is not a spare field — it is the predicate `when` narrows
// on (RFC CL) and the only temporal value that is fully populated, so mirroring a
// fact into the graph dropped the one date it actually had. valid_at, by
// contrast, was 100% populated in the chunk plane and 26% in the k/v one, because
// this table's writer defaulted it to now: three quarters of those instants were
// write timestamps wearing world-time's column.
//
// access_count / last_accessed_at are here and are NOT the mechanism they were
// added for. The reasoning that put them here was that access_count feeds the
// recall ranker's frequency term and would have nowhere to live once a fact's home
// moved. That premise was wrong, and checking it is what showed why: the counter is
// maintained per (tenant, scope, scope_id, KEY) on the k/v row itself, and the row
// a collapsed fact lives in is its `doc.chunk:<hex>` BODY row — a k/v row, which
// already has these columns and which the access tracker already bumps for any
// returned hit, key-agnostically. So the telemetry follows the fact for free.
//
// They are left in place rather than dropped because a DROP COLUMN migration on
// every provisioned scope is a worse trade than two nullable unused columns, and
// this comment is the record so the next reader does not build on them. The real
// requirement they were standing in for belongs to the collapse: when the k/v fact
// row is deleted its accumulated access_count must be COPIED onto the body row, or
// every existing fact's frequency term silently resets to zero — measured at 696
// accesses across 84 facts on one corpus against 0 on their bodies.
//
// ALTER on every scope, new and old alike, and NOT declared in docSchemaDDL — a
// CREATE TABLE IF NOT EXISTS leaves an existing table untouched, so the DDL alone
// would reach only fresh scopes. Same shape as migrateAssetDescription: a
// leading-SELECT fast path, then per-column probes so a crash between two ALTERs
// does not become a duplicate-column error on the next run.
func (d *Document) migrateFactObservation(ctx context.Context, key sqlmem.ScopeKey) error {
	// Fast path: all three present → nothing to do (the common case).
	if _, err := d.query(ctx, key, `SELECT observed_at, access_count, last_accessed_at FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	for _, col := range []string{"observed_at", "access_count", "last_accessed_at"} {
		if d.tableHasColumn(ctx, key, "chunk_memory_meta", col) {
			continue
		}
		// BIGINT and NULLABLE for all three. Nullable matters: a zero access_count
		// is a real count and a zero instant is 1970, so "never set" has to stay
		// distinguishable from "set to nothing" — the same argument int64Arg makes.
		if err := d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN `+col+` BIGINT`); err != nil {
			return err
		}
	}
	return nil
}

// migrateChunkValidity adds chunks.valid_at / invalid_at — an ordinary chunk's own
// validity interval, independent of the fact tier's sidecar — to a scope
// provisioned before the columns existed. docSchemaDDL declares them for a fresh
// scope; a CREATE TABLE IF NOT EXISTS leaves an existing chunks table untouched, so
// this is the one path that reaches both.
//
// Same shape as migrateFactObservation: a leading-SELECT fast path, then a
// per-column probe so a crash between the two ALTERs is not a duplicate-column
// error on the next run. NULLABLE and never defaulted: the interval is when the
// content was true in the world, and a write timestamp in its place is a wrong
// answer to every as_of question.
//
// The index lives here rather than in docSchemaDDL for the reason
// migrateDocumentFacets gives: docSchemaDDL runs before this migration, so on an
// old scope the columns would not yet exist to index.
func (d *Document) migrateChunkValidity(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT valid_at, invalid_at FROM chunks WHERE 1=0`); err != nil {
		for _, col := range []string{"valid_at", "invalid_at"} {
			if d.tableHasColumn(ctx, key, "chunks", col) {
				continue
			}
			if err := d.exec(ctx, key, `ALTER TABLE chunks ADD COLUMN `+col+` BIGINT`); err != nil {
				return err
			}
		}
	}
	return d.exec(ctx, key, `CREATE INDEX IF NOT EXISTS chunks_validity ON chunks(valid_at, invalid_at)`)
}

// migrateAssetDescription adds chunk_assets.description + described_at (RFC BU
// phase 4) to a scope provisioned before image descriptions existed.
//
// A CREATE TABLE IF NOT EXISTS silently leaves an existing table alone, so without
// this every scope created before phase 4 would keep the old three-column shape
// forever — the same trap migrateConfidencePrecision exists for.
//
// described_at is separate from `description IS NOT NULL` on purpose: it records
// that a describe pass RAN. A model that looked at an image and produced nothing
// useful is a different state from one that was never asked, and without the
// timestamp a sweep cannot tell them apart — it would re-describe the same
// unproductive image on every run.
func (d *Document) migrateAssetDescription(ctx context.Context, key sqlmem.ScopeKey) error {
	// Fast path: both columns present → nothing to do (the common case).
	if _, err := d.query(ctx, key, `SELECT description, described_at FROM chunk_assets WHERE 1=0`); err == nil {
		return nil
	}
	// Per-column probes, so a crash between the two ALTERs on a prior run does not
	// turn into a duplicate-column error on the next.
	if !d.tableHasColumn(ctx, key, "chunk_assets", "description") {
		if err := d.exec(ctx, key, `ALTER TABLE chunk_assets ADD COLUMN description TEXT`); err != nil {
			return err
		}
	}
	if !d.tableHasColumn(ctx, key, "chunk_assets", "described_at") {
		if err := d.exec(ctx, key, `ALTER TABLE chunk_assets ADD COLUMN described_at BIGINT`); err != nil {
			return err
		}
	}
	return nil
}

// documentsHasColumn reports whether the documents table has `col` — a
// validator-safe, tier-portable existence probe (a leading SELECT that errors iff
// the column is absent). col is a hardcoded literal ("type" | "status"), never
// caller input.
func (d *Document) documentsHasColumn(ctx context.Context, key sqlmem.ScopeKey, col string) bool {
	_, err := d.query(ctx, key, `SELECT `+col+` FROM documents WHERE 1=0`)
	return err == nil
}

// tableHasColumn is documentsHasColumn for any table. Both interpolate their
// arguments, which is safe ONLY because every caller passes a literal — a probe
// built from caller-supplied text would be an injection. Keep it that way.
// ⚠️ Deliberately NOT d.query: this probe's ERROR is its answer ("no such column"
// means no), and d.query reads that same error as a stale schema memo and
// re-provisions. Going through the retry path here is what makes the migrations
// call themselves. The raw call has no retry and no recursion.
func (d *Document) tableHasColumn(ctx context.Context, key sqlmem.ScopeKey, table, col string) bool {
	_, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(`SELECT `+col+` FROM `+table+` WHERE 1=0`), nil)
	return err == nil
}

// migrateEdgeAuto adds the `auto` flag to chunk_edges (RFC BS Phase 2a): 1 marks
// a parser-generated inline-`[[name]]`-link edge (reconcileNameLinks owns it and
// re-derives it on every body write); 0 marks a manual link_chunks edge (never
// touched by the parser). It PROBES before it alters, like migrateDocumentFacets,
// because ensureSchema is on every op's hot path — the fast case is a single
// 0-row SELECT. The column arrives via ALTER on every scope (new and old alike),
// since a `CREATE TABLE IF NOT EXISTS` leaves an existing chunk_edges untouched.
//
// The probe is a leading-SELECT (validator-safe; a leading PRAGMA is denied) and
// tier-portable; `ADD COLUMN … NOT NULL DEFAULT 0` fills existing rows on both
// tiers, so every edge predating this migration reads back as manual (auto=0),
// which is what it is.
// migrateSourceQuote adds the span a fact was derived from.
//
// A fact could say who wrote it (`origin`, `run_id`) but never what it was based on,
// so nothing downstream could ask whether the source supported the claim — not the
// consolidator, not retrieval, not an operator reading the memory page. Three separate
// failures on a live deployment traced to that: a curator inventing counts, an extractor
// typing the subject of "the user resides in Cluj-Napoca" as a location, and three entity
// writes refused for a class-where-a-type-belongs. None of them was checkable.
//
// NULLABLE, and existing rows stay NULL. A fact written before this column cannot be
// verified retroactively and must not be treated as if it were: no span means unverified,
// which is honest, and it stays recallable.
//
// PROBES before it alters, like its siblings — ensureSchema is on every op's hot path, so
// the fast case is one 0-row SELECT.
// migrateSubject adds the subject an entity assertion is ABOUT.
//
// Until now the subject existed only inside the natural key and the title —
// `location:user` with title "user" — so nothing could read it as data. Two
// consequences, both observed: the tool could not tell an entity assertion from an
// idempotent DOCUMENT write (both carry a type and a natural key, and `type` holds a
// document's structural vocabulary too), and a mistyped subject was invisible to any
// query. `location:user`, asserting that the user is a location, is only findable by
// parsing a key.
//
// Nullable. A document write carries no subject and never will, which is exactly what
// makes the pair a usable discriminator.
// migrateVerdict adds when a fact was last judged, and why.
//
// `confidence` is the verdict AXIS — one number for how much this is trusted, rather than
// a second status that can disagree with it. These two columns are the PROVENANCE of that
// number, not a competing verdict: they answer "who set this, and on what grounds".
//
// The distinction they exist for is the one withholding depends on. A never-judged fact
// and a judged-and-refuted one are different states, and confidence alone cannot separate
// them: a refuted claim at 0.0 and a hedged one at 0.1 would look the same, and a store
// where nothing has been judged has NULL everywhere. judged_at is what says a verdict was
// actually reached.
func (d *Document) migrateVerdict(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT judged_at FROM chunk_memory_meta WHERE 1=0`); err != nil {
		if aerr := d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN judged_at BIGINT`); aerr != nil {
			return aerr
		}
	}
	if _, err := d.query(ctx, key, `SELECT judge_reason FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN judge_reason TEXT`)
}

// migrateJudgedBy adds the column recording WHO reached a verdict.
//
// Probe-then-ALTER like its siblings. An existing row keeps a NULL, which reads as
// "unknown" rather than as either party — a verdict recorded before this column existed
// was reached by the judge or by an operator and the store genuinely cannot say which,
// so it must not claim one.
func (d *Document) migrateJudgedBy(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT judged_by FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN judged_by TEXT`)
}

func (d *Document) migrateSubject(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT subject FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN subject TEXT`)
}

func (d *Document) migrateSourceQuote(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT source_quote FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN source_quote TEXT`)
}

func (d *Document) migrateEdgeAuto(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT auto FROM chunk_edges WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_edges ADD COLUMN auto INTEGER NOT NULL DEFAULT 0`)
}

// --- chunk body (Memory) helpers ---

type chunkBody struct {
	Body   string          `json:"body"`
	Fields json.RawMessage `json:"fields,omitempty"`
}

func (d *Document) writeBody(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID, chunkType, body string, fields json.RawMessage) error {
	return d.writeBodyAs(ctx, mscope, key, chunkID, chunkType, body, fields, "")
}

// writeBodyAs is writeBody with the row's ORIGIN — who produced this content.
//
// An ordinary document chunk passes "" and the row stays provenance-free, which is
// what marks it as prose. An ENTITY write passes the same origin its sidecar
// records, and that is the whole point: a fact's body row was previously
// indistinguishable from a document's, because both are `doc.chunk:<hex>` keys with
// an empty origin. Retrieval therefore had only the key prefix to go on, and the
// key prefix says "chunk body", not "fact" — so `sources=documents` returned facts
// and labelled them documents.
//
// It has to be ORIGIN rather than a new column. origin is already server-stamped
// and deliberately absent from the `Memory set` input schema BECAUSE it names the
// writer, so a caller cannot claim one; a fresh `row_class` column would add a
// writer-forgot-to-set failure mode, which is the class of bug a derived
// discriminator exists to avoid. And it cannot be a lookup: chunk_memory_meta lives
// in SQL Memory — a DIFFERENT database from the k/v plane — so consulting it per
// candidate would be a cross-database round trip from each of the four call sites
// that label a row.
func (d *Document) writeBodyAs(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID, chunkType, body string, fields json.RawMessage, origin string) error {
	return d.putBody(ctx, mscope, key, chunkID, chunkType, body, fields, origin, nil)
}

// putBody is writeBodyAs with the import provenance the server stamps on an
// imported document's root (stamped; import_md alone passes it). Every other
// write is a caller's: the provenance field it gives is dropped and the
// stored one carried forward, so no write path — create_chunk, update_chunk,
// set_asset, sync, an import's Markdown metadata — can set or change where a
// document says it came from. This is the one place every body write goes
// through, which is why the rule lives here.
func (d *Document) putBody(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID, chunkType, body string, fields json.RawMessage, origin string, stamped map[string]any) error {
	scopeID := docMemScopeID(mscope, key)
	// RFC BL: key chunk bodies on the same tenant the document's dirent + SQL
	// structure use (direntTenant = the raw RunIdentity tenant), so bodies and
	// structure never drift across the tenant axis.
	tenant := direntTenant(ctx)
	bodyKey := chunkBodyKey(chunkID)
	// A BODY REWRITE NEVER ERASES PROVENANCE. MemorySet delegates to
	// MemorySetProvenance with the extra arguments zeroed, so a plain set over an
	// existing row CLEARS its origin — verified, not assumed. That matters because
	// the origin on a body row is now what distinguishes a fact from prose: without
	// this, an operator editing a fact through update_chunk (which writes the body
	// with no origin of its own) would strip the column and silently reclassify the
	// fact as a document. The text would still be right; the fact would just stop
	// being a fact.
	//
	// This is the same rule writeChunkMeta already applies to the sidecar — "an
	// operator correcting a fact's wording must not strip the span it was derived
	// from" — applied to the other half of the same fact. A caller that supplies an
	// origin still wins; absence means "I have nothing to say about provenance",
	// never "erase it".
	//
	// The whole provenance struct is carried, not just the origin, so a re-set does
	// not clear class or the source ids either. The extra point read costs nothing
	// next to the embedding call this function already makes.
	prov := store.MemoryProvenance{Origin: origin}
	prev, perr := d.Store.MemoryProvenanceGet(ctx, tenant, mscope, scopeID, bodyKey)
	if perr == nil {
		if prov.Origin == "" {
			prov.Origin = prev.Origin
		}
		prov.Class = prev.Class
		prov.SourceSessionID = prev.SourceSessionID
		prov.SourceRunID = prev.SourceRunID
	}
	// The stored `principal` binding wins over whatever the write carries; see
	// keepStoredPrincipal. Held until the body is written.
	fields, release, err := d.keepStoredPrincipal(ctx, key, mscope, chunkID, fields)
	if err != nil {
		return err
	}
	defer release()
	if stamped != nil {
		fields = setProvenanceField(fields, stamped)
	} else {
		// Only an imported body can hold provenance, so only its rewrite reads
		// the stored fields. A failed read refuses the write rather than
		// erase the provenance the caller could not see.
		var stored json.RawMessage
		if perr == nil && prev.Origin == store.OriginImport {
			cb, err := d.readBody(ctx, mscope, key, chunkID)
			if err != nil {
				return err
			}
			stored = cb.Fields
		}
		fields = carryProvenanceField(fields, stored)
	}
	v, _ := json.Marshal(chunkBody{Body: body, Fields: fields})
	if prov.Origin == "" && prov.Class == "" && prov.SourceSessionID == "" && prov.SourceRunID == "" {
		if err := d.Store.MemorySet(ctx, tenant, mscope, scopeID, bodyKey, v, 0); err != nil {
			return err
		}
	} else if err := d.Store.MemorySetProvenance(ctx, tenant, mscope, scopeID, bodyKey, v, 0, prov); err != nil {
		return err
	}
	// The body is durable at this point. Embedding is a SEPARATE, best-effort
	// step and its failure is never returned: an unembedded chunk is
	// unsearchable, but a chunk whose write was rejected because an embedder was
	// cold is lost work the author has to redo. Authoring must not become
	// embedder-dependent.
	d.embedBody(ctx, tenant, mscope, key, bodyKey, chunkID, chunkType, body)
	return nil
}

// embedBody indexes a chunk for semantic search, best-effort.
//
// The text is chunkIndexText's — the header naming where the chunk sits, then the
// type-specific content — and nothing here second-guesses it. The admin backfill,
// re-embed and purge derive the same text through the same function, so what a scope
// returns cannot depend on which path last touched a row. See document_index_text.go.
//
// THE TYPE IS PASSED IN, NOT SNIFFED FROM THE BODY. A mermaid chunk STORES its bare
// source (export re-adds the fence), so sniffing the body for a ```mermaid fence
// embedded a natively created diagram as raw source while an imported one was
// skipped; and a sniffed kind would misread a prose chunk that opens with the word
// "pie". The chunk type is the only authoritative answer, so callers supply it.
func (d *Document) embedBody(ctx context.Context, tenant string, mscope store.MemoryScope, key sqlmem.ScopeKey, bodyKey, chunkID, chunkType, body string) {
	scopeID := docMemScopeID(mscope, key)
	if d.Embedder == nil {
		return
	}
	// "" means index nothing — no content and no usable header, or a bodyless root.
	// Embedding punctuation or a placeholder is worse than embedding nothing, because a
	// row that exists ranks against every query.
	text := d.chunkIndexText(ctx, key, chunkID, chunkType, body)
	if text == "" {
		// Nothing to index — so nothing may STAY indexed either. A rename to a
		// letterless heading, or a body edited down to scaffolding, would otherwise keep
		// the vector of what the chunk used to say. Deleting an absent row is a no-op.
		_ = d.Store.MemoryEmbedDelete(ctx, tenant, mscope, scopeID, bodyKey)
		return
	}
	vec, err := d.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vec) == 0 {
		// One log line, not a returned error — see writeBody. The admin re-embed
		// surface is how an operator recovers a scope that was written while the
		// embedder was down.
		slog.WarnContext(ctx, "document: embed body failed for a chunk", "chunk_id", chunkID, "err", err)
		return
	}
	if err := d.Store.MemoryEmbedSet(ctx, tenant, mscope, scopeID, bodyKey, store.MemoryEmbedding{
		Provider:  d.Embedder.Provider(),
		Model:     d.Embedder.Model(),
		Dimension: len(vec[0]),
		Vector:    vec[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		slog.WarnContext(ctx, "document: store embedding failed for a chunk", "chunk_id", chunkID, "err", err)
	}
}

// readBody loads a chunk's body + fields from Memory.
//
// A chunk with NO body row is normal and reports as an empty body with a nil
// error: section/parent chunks never get one. Every other store failure is
// returned, because writeBody rewrites body AND fields together — so a caller
// that read-modify-writes a zero chunkBody derived from a failed read erases
// whichever half it did not supply. That is not hypothetical: it blanked a
// document root in production when a tenant-scoping change (v1.33.0) made the
// body row unreadable and a fields-only `update_chunk` wrote the emptiness
// back. A transient store error reaches the same end, which is why the guard
// belongs here rather than in the tenant fix.
func (d *Document) readBody(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID string) (chunkBody, error) {
	tenant := direntTenant(ctx)
	entry, err := d.Store.MemoryGet(ctx, tenant, mscope, docMemScopeID(mscope, key), chunkBodyKey(chunkID))
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return chunkBody{}, nil
		}
		return chunkBody{}, err
	}
	var cb chunkBody
	_ = json.Unmarshal(entry.Value, &cb)
	return cb, nil
}

// chunkBodyKey namespaces chunk bodies in the Memory keyspace so they don't
// collide with an agent's own k/v keys.
// chunkBodyKeyPrefix is the k/v namespace for chunk bodies. One definition, because
// both the Document tool and the admin backfill parse keys out of it.
const chunkBodyKeyPrefix = "doc.chunk:"

func chunkBodyKey(chunkID string) string { return chunkBodyKeyPrefix + chunkID }

// ChunkIDFromBodyKey returns the chunk id a body key addresses, or "" when the key
// is not a chunk body. Exported so the admin backfill can recognise document rows
// without restating the prefix.
func ChunkIDFromBodyKey(memKey string) string {
	if !strings.HasPrefix(memKey, chunkBodyKeyPrefix) {
		return ""
	}
	return strings.TrimPrefix(memKey, chunkBodyKeyPrefix)
}

// chunkTitle reads one chunk's title, or "" on any fault. Best-effort like the rest
// of the embed path: a title that cannot be read costs searchability, never a write.
func (d *Document) chunkTitle(ctx context.Context, key sqlmem.ScopeKey, chunkID string) string {
	if d.SqlMem == nil {
		return ""
	}
	res, err := d.query(ctx, key, `SELECT title FROM chunks WHERE id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}

// recordRevision appends a body snapshot to the chunk_revisions log (RFC BS
// Phase 3a). It is a BODY-CHANGE log: called only where a chunk's body is
// actually (re)written — create_chunk (revision 1) and a body-bearing
// update_chunk (the post-bump revision). A metadata-only update_chunk does NOT
// call this: its body is unchanged from the last snapshot, so history lists
// exactly the revisions at which the BODY changed.
//
// The INSERT is guarded (SELECT … WHERE NOT EXISTS) so a re-run cannot violate
// the (chunk_id, revision) primary key — the same portable idempotence pattern
// addTagRows uses. actor is the acting principal (RunIdentity.UserID), matching
// publishChange's actor field.
//
// Deliberately NOT snapshotting yet (follow-ups): update_chunk-via-upsert_chunk
// (updateChunkForUpsert writes the body directly), import_md's in-place body
// writes, set_asset, and createDocument's empty root body do not append a
// revision. Any body write that flows through create_chunk (incl. upsert_chunk's
// create path and import_md's child chunks) does seed revision 1 naturally.
func (d *Document) recordRevision(ctx context.Context, key sqlmem.ScopeKey, chunkID string, revision int, body string) error {
	actor := tools.RunIdentity(ctx).UserID
	now := time.Now().UnixNano()
	stmt := `INSERT INTO chunk_revisions (chunk_id, revision, created_at, actor, body)
	         SELECT ?, ?, ?, ?, ? WHERE NOT EXISTS (
	             SELECT 1 FROM chunk_revisions WHERE chunk_id = ? AND revision = ?)`
	return d.exec(ctx, key, stmt, chunkID, revision, now, actor, body, chunkID, revision)
}

// --- SQL helpers ---

// schemaMissing reports the errors that mean "the tables this statement needs are
// not there", which is what a stale provisioning memo looks like from a statement.
//
// ⚠️ NARROW ON PURPOSE. Its only job is to decide whether re-provisioning and
// retrying ONCE is worth it; anything broader would retry real failures and turn a
// clear error into a slow one. The retry is safe precisely because these errors mean
// the statement did nothing — there is no half-applied write to repeat.
//
// Both tiers, and three shapes: postgres raises 3F000 when the SCHEMA is gone (the
// search_path names nothing), 42P01 when the schema exists but the TABLE does not,
// and 42703 for a missing COLUMN; sqlite says "no such table" / "no such column".
//
// ⚠️ THE COLUMN CASE IS THE ONE THE MEMO NEEDS MOST. `CREATE TABLE IF NOT EXISTS`
// leaves an existing table alone, so a column added by a later release arrives only
// through the ALTER in provisionSchema — and memoising provisioning means that ALTER
// runs once per process. If anything leaves a scope on the old shape after that
// point, every write of the new column fails forever. Treating "no such column" as
// staleness makes the memo SELF-HEALING: the statement fails, provisioning re-runs
// (migrations included), and the retry succeeds. Without it the memo would convert a
// recoverable pending migration into a permanent failure.
func schemaMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"no schema has been selected", // postgres 3F000
		"3f000",
		"does not exist", // postgres 42P01 relation / 42703 column ... does not exist
		"42p01",
		"42703",
		"no such table",  // sqlite
		"no such column", // sqlite — a column a migration adds, not yet applied
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// exec/query run the tool's OWN statements (written with portable `?`
// placeholders) — Rebind converts `?`→`$N` on the postgres tier. The raw `sql:`
// escape hatch does NOT go through these (it calls the Manager directly with
// the model's dialect-native SQL).
func (d *Document) exec(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) error {
	_, err := d.docSQL().Exec(ctx, key, d.docSQL().Rebind(stmt), args, 0)
	if schemaMissing(err) && d.reensureSchema(ctx, key) == nil {
		_, err = d.docSQL().Exec(ctx, key, d.docSQL().Rebind(stmt), args, 0)
	}
	return err
}

func (d *Document) query(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) (*sqlmem.QueryResult, error) {
	res, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(stmt), args)
	if schemaMissing(err) && d.reensureSchema(ctx, key) == nil {
		return d.docSQL().Query(ctx, key, d.docSQL().Rebind(stmt), args)
	}
	return res, err
}

// withSqlTxn runs fn inside a FRESH, independent SQL Memory transaction —
// committing on success, rolling back on any error. A unique txn id means it
// never nests onto an agent's explicit sql_begin, so the delete is its own
// atomic unit: a mid-cascade failure rolls back the whole SQL side, leaving the
// chunk graph untouched (no half-deleted mess). The chunk Memory BODIES live in
// a separate store and can't join this txn — callers delete them AFTER a
// successful commit (an orphaned body is invisible dead k/v; an orphaned row
// would be visible, so SQL-first is least-bad).
func (d *Document) withSqlTxn(ctx context.Context, key sqlmem.ScopeKey, fn func(txnID string) error) error {
	txnID := "doc-tx:" + newDocID()
	if _, err := d.docSQL().BeginTxn(ctx, txnID, tools.RunIdentity(ctx).RootRunID, key); err != nil {
		return err
	}
	if err := fn(txnID); err != nil {
		_, _ = d.docSQL().RollbackTxn(txnID)
		return err
	}
	if _, err := d.docSQL().CommitTxn(txnID); err != nil {
		_, _ = d.docSQL().RollbackTxn(txnID)
		return err
	}
	return nil
}

func (d *Document) execTxn(ctx context.Context, txnID, stmt string, args ...any) error {
	_, err := d.docSQL().ExecTxn(ctx, txnID, d.docSQL().Rebind(stmt), args, 0)
	return err
}

func (d *Document) queryTxn(ctx context.Context, txnID, stmt string, args ...any) (*sqlmem.QueryResult, error) {
	return d.docSQL().QueryTxn(ctx, txnID, d.docSQL().Rebind(stmt), args)
}

// chunkRow is the SQL-side chunk record (body/fields come from Memory).
type chunkRow struct {
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	ParentID   string `json:"parent_id,omitempty"`
	Position   int    `json:"position"`
	Type       string `json:"type,omitempty"`
	Status     string `json:"status,omitempty"`
	Title      string `json:"title"`
	Revision   int    `json:"revision"`
	// ValidAt / InvalidAt are the chunk's own [valid_at, invalid_at) interval in
	// unix nanoseconds; nil = unbounded on that side. Pointers because 0 is an
	// instant (the epoch), not "unset".
	ValidAt   *int64 `json:"valid_at,omitempty"`
	InvalidAt *int64 `json:"invalid_at,omitempty"`
}

const chunkSelectCols = `id, document_id, parent_id, position, type, status, title, revision, valid_at, invalid_at`

func scanChunkRow(cols []string, row []any) chunkRow {
	m := map[string]any{}
	for i, c := range cols {
		if i < len(row) {
			m[c] = row[i]
		}
	}
	return chunkRow{
		ID:         asStr(m["id"]),
		DocumentID: asStr(m["document_id"]),
		ParentID:   asStr(m["parent_id"]),
		Position:   asInt(m["position"]),
		Type:       asStr(m["type"]),
		Status:     asStr(m["status"]),
		Title:      asStr(m["title"]),
		Revision:   asInt(m["revision"]),
		ValidAt:    asInt64Ptr(m["valid_at"]),
		InvalidAt:  asInt64Ptr(m["invalid_at"]),
	}
}

// putValidity adds a chunk's interval to a response map, only the ends that are
// set — so an undated chunk's response carries no key at all.
func putValidity(m map[string]any, validAt, invalidAt *int64) {
	if validAt != nil {
		m["valid_at"] = *validAt
	}
	if invalidAt != nil {
		m["invalid_at"] = *invalidAt
	}
}

func asStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int64:
		return int(t)
	case int:
		return t
	case float64:
		return int(t)
	default:
		return 0
	}
}

// asBytes extracts raw bytes from a SQL cell — a BYTEA/BLOB column comes back as
// []byte via database/sql; a string form (defensive) preserves the bytes. Used
// only for the chunk_assets.bytes column (RFC BO); asStr would work too since a
// Go string holds arbitrary bytes, but []byte is the honest type.
func asBytes(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	default:
		return nil
	}
}

func (d *Document) getChunkRow(ctx context.Context, key sqlmem.ScopeKey, id string) (chunkRow, bool, error) {
	res, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return chunkRow{}, false, err
	}
	if len(res.Rows) == 0 {
		return chunkRow{}, false, nil
	}
	return scanChunkRow(res.Columns, res.Rows[0]), true, nil
}

// --- board surface (chunk.status as a durable walk position) ---

// GetChunkStatus reads a chunk's status field. It is the narrow read half of the
// Document-board surface a TeamDef `op=run` uses to RESUME from a persisted walk
// position (chunk.status = the team state to continue from). scope is "agent" or
// "user" (default user); the ScopeKey + tenant come from ctx exactly as the
// tool's own ops resolve them. ok=false when the chunk doesn't exist in this
// scope (so a caller can distinguish "no such chunk" from "status is empty").
func (d *Document) GetChunkStatus(ctx context.Context, scope, chunkID string) (status string, ok bool, err error) {
	if d.Store == nil || d.SqlMem == nil {
		return "", false, fmt.Errorf("Document board: requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return "", false, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return "", false, err
	}
	row, found, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return row.Status, true, nil
}

// SetChunkStatus sets a chunk's status field — the write half of the board
// surface, called on each state transition so chunk.status durably reflects the
// walk's current position. It reuses the same atomic, revision-guarded bump as
// update_chunk (read the current revision, then UPDATE ... WHERE revision = ?)
// so a concurrent human/agent edit of the same chunk is a clean conflict rather
// than a silent lost write. Reusing the tool's SQL path keeps the chunk-schema
// logic in one place (no hand-rolled board SQL to drift). A best-effort change
// event lets the Web UI board follow along live.
func (d *Document) SetChunkStatus(ctx context.Context, scope, chunkID, status string) error {
	if d.Store == nil || d.SqlMem == nil {
		return fmt.Errorf("Document board: requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return err
	}
	row, found, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no such chunk: %s", chunkID)
	}
	now := time.Now().UnixNano()
	res, err := d.docSQL().Exec(ctx, key, d.docSQL().Rebind(`UPDATE chunks SET status = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`), []any{nullIfEmpty(status), now, chunkID, row.Revision}, 0)
	if err != nil {
		return err
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("chunk %s: revision conflict setting status (changed by a concurrent write)", chunkID)
	}
	// If this board write lands on a document's root chunk, mirror the new status
	// onto the documents row so get_document / query_documents (which read that
	// row) stay consistent with the root chunk. A no-op for a non-root chunk.
	if err := d.mirrorRootFacets(ctx, key, chunkID); err != nil {
		return err
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), row.DocumentID, "update_chunk", chunkID)
	return nil
}

// --- ops: document lifecycle ---

func (d *Document) createDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.Title == "" {
		return errValidation("create_document: missing required field: title", "Pass `title`, the document's name."), nil
	}
	// A malformed `path` is refused BEFORE the document exists. It used to be
	// checked only when the dirent was registered, after the write: the call
	// succeeded with a path_warning, the document landed outside the Path tree,
	// and a model that read the success created a second one. Measured: two
	// local models passed the title as a segment ("/documents/zz-eval Trip plan")
	// and each left an orphan behind.
	if in.Path != "" {
		if _, perr := normalizePath(in.Path); perr != nil {
			return errValidation("create_document: "+perr.Error()+". Nothing was created.",
				fmt.Sprintf("Use a path of letters, digits and . _ - segments, such as %q, or omit `path` for /documents/<title>.",
					suggestDocPath(in.Path))), nil
		}
	}
	now := time.Now().UnixNano()
	docID := newDocID()
	// The name the document will be filed under, decided before it exists so
	// the caller's access-control set can refuse it with nothing left behind
	// (RFC EZ §1.5 seam 5). Every path that makes a document comes here.
	docPath := in.Path
	if docPath == "" {
		docPath = "/documents/" + docDefaultPathSegment(in.Title, docID)
	}
	if canonical, perr := normalizePath(docPath); perr == nil {
		if refused := d.aclRefuseNewName(ctx, key, "create_document", canonical); refused != nil {
			return *refused, nil
		}
	}
	// THE ONTOLOGY GATE APPLIES HERE TOO, now that a root can be an entity. It used
	// to live on upsert_chunk alone, which was the only way to make one; a
	// subject-homed store creates its entities through create_document, so leaving
	// the gate behind would let any caller mint an entity of an undeclared kind
	// simply by creating a document — curation (RFC CA) bypassed by a different op.
	//
	// Gated BEFORE the document exists, so a refusal leaves nothing behind.
	if strings.TrimSpace(in.Subject) != "" && strings.TrimSpace(in.NaturalKey) != "" {
		if refused := d.gateEntityType(ctx, in); refused != nil {
			return *refused, nil
		}
		// AND THE KEY MUST BE FREE. chunk_memory_meta.natural_key is UNIQUE per scope,
		// so a root claiming a key another chunk holds fails — and it used to fail
		// AFTER the document and its root were inserted, leaving an empty, path-less
		// document behind every time. Measured on a store whose subject nodes predate
		// subject-homing: one orphan per subject per consolidation pass, growing
		// forever, because the caller retries on the next pass and fails identically.
		//
		// Refused here, before anything exists, with the holder named — a caller that
		// hits this is looking at a store whose entities have not been moved into
		// their own documents yet, and that is what the message has to say.
		if held, herr := d.chunkIDByNaturalKey(ctx, key, in.NaturalKey); herr != nil {
			return errFrom("create_document: natural key lookup: "+herr.Error(), herr), nil
		} else if held != "" {
			return errBusiness("create_document: chunk "+held+" already holds the natural key "+
				strconv.Quote(in.NaturalKey)+", and a key names ONE thing per scope. That entity "+
				"has not been moved into its own document yet — run the subject-homing migration "+
				"(POST /v1/_memory/home_facts), which moves the node itself rather than copying it.", "Use the existing entity (read it with op=get_chunk on that id), or ask an operator to run the subject-homing migration."), nil
		}
	}
	rootID := newDocID()
	// type/status are set on the root chunk (the authoritative kind/state) AND
	// mirrored to the documents row (the denormalized copy query_documents /
	// get_document read). They start in sync here; update_chunk keeps them so.
	if err := d.exec(ctx, key, `INSERT INTO documents (id, title, root_chunk_id, type, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		docID, in.Title, rootID, nullIfEmpty(in.Type), nullIfEmpty(in.Status), now, now); err != nil {
		return errFrom("create_document: "+err.Error(), err), nil
	}
	// The root chunk anchors the hierarchy (parent_id NULL).
	if err := d.exec(ctx, key, `INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, NULL, 0, ?, ?, ?, ?, ?, 1)`,
		rootID, docID, nullIfEmpty(in.Type), nullIfEmpty(in.Status), in.Title, now, now); err != nil {
		return errFrom("create_document: root chunk: "+err.Error(), err), nil
	}
	// bodyOrigin is set only by a server path (import_md stamps an import);
	// every tool call leaves it empty.
	if err := d.writeBodyAs(ctx, mscope, key, rootID, "", "", nil, in.bodyOrigin); err != nil {
		return errFrom("create_document: root body: "+err.Error(), err), nil
	}
	// A DOCUMENT'S ROOT CAN BE THE ENTITY ITSELF (RFC CV decision 1). When the
	// caller names the entity pair AND a natural key, the root chunk gets the
	// sidecar that makes it an entity node — so the subject node and the document
	// root are ONE object rather than a container holding a node that repeats it.
	//
	// That collapse is the whole point of subject-homing: `/facts/<subject>` is the
	// subject, its facts are its children, and "what do we know about X" is one
	// get_document rather than a search or a traversal. Two objects would mean two
	// titles, two types and two places to correct either.
	//
	// Absent the pair this is a no-op, so every existing create_document is
	// unchanged — an ordinary document's root is not an entity and must not
	// acquire a sidecar row that the fact surfaces would then list.
	if strings.TrimSpace(in.Subject) != "" && strings.TrimSpace(in.NaturalKey) != "" {
		if err := d.writeChunkMeta(ctx, key, rootID, in); err != nil {
			// UNWOUND, not left behind. A document that "is not the subject it claims to
			// be" is precisely what must not survive the call: it is invisible (the
			// dirent below never runs), it is empty, and the caller retries — so leaving
			// it turns one failure into an unbounded leak. The guard above catches the
			// cause that is knowable in advance; this catches the race and anything else,
			// because the leak is what does the damage, not its reason.
			//
			// An unwind rather than a transaction because the body row lives in the
			// Memory k/v plane, a DIFFERENT database, which cannot join a SQL txn — the
			// same constraint delete_chunk works under.
			_ = d.exec(ctx, key, `DELETE FROM chunks WHERE id = ?`, rootID)
			_ = d.exec(ctx, key, `DELETE FROM documents WHERE id = ?`, docID)
			_, _ = d.Store.MemoryDelete(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), chunkBodyKey(rootID))
			return errFrom("create_document: the root's entity metadata could not be written, "+
				"so nothing was created: "+err.Error(), err), nil
		}
	}
	// A document's tags are its own (independent of the root chunk's tags).
	if len(in.Tags) > 0 {
		if err := d.replaceDocumentTags(ctx, key, docID, in.Tags); err != nil {
			return errFrom("create_document: tags: "+err.Error(), err), nil
		}
	}
	resp := map[string]any{"document_id": docID, "root_chunk_id": rootID, "title": in.Title}
	// Path-tree name (RFC AK). An explicit `path` wins; otherwise default to
	// /documents/<title> so a document is NEVER orphaned from the Path tree — the
	// dirent is what the Library / Path browser lists, so a path-less document was
	// reachable only by id (invisible to every human login). DirentCreate upserts
	// by the full (tenant, scope, scope_id, parent, name) key, so two same-titled
	// documents in ONE scope would share that slot; the default segment falls back
	// to the (unique) doc id when the title slugifies to empty, and a caller that
	// needs collision-proof naming should pass an explicit `path`.
	if p, perr := d.registerDocDirent(ctx, key, docID, docPath); perr != nil {
		resp["path_warning"] = "document created but path registration failed: " + perr.Error()
	} else {
		resp["path"] = p
	}
	// Announced only for a document a caller created directly: import_md and the
	// placeholder path create through here too, and announce their own.
	if in.Op == "create_document" {
		if why := d.publishIngested(ctx, key, mscope, docID, "created", nil); why != "" {
			resp["not_announced"] = why
		}
	}
	return jsonResult(resp)
}

// suggestDocPath is a valid path close to one that failed validation: each
// segment slugified the way a default title segment is, so
// "/documents/zz-eval Trip plan" becomes "/documents/zz-eval-Trip-plan". A
// segment that slugs to nothing is dropped.
func suggestDocPath(raw string) string {
	var segs []string
	for _, s := range strings.Split(raw, "/") {
		if seg := docDefaultPathSegment(s, ""); seg != "" {
			segs = append(segs, seg)
		}
	}
	return "/" + strings.Join(segs, "/")
}

// docDefaultPathSegment slugifies a document title into a single Path-tree name
// segment (pathSegmentRe charset: letters, digits, . _ -) for the default
// /documents/<title> dirent of a path-less create_document. Runs of other
// characters collapse to a single '-'; a title that slugs to empty falls back to
// the document id (always a valid segment).
func docDefaultPathSegment(title, docID string) string {
	var b strings.Builder
	dash := false
	for _, r := range title {
		switch {
		case r == '.' || r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	seg := strings.Trim(b.String(), "-._")
	if len(seg) > maxSegmentLen {
		seg = strings.Trim(seg[:maxSegmentLen], "-._")
	}
	if seg == "" {
		return docID
	}
	return seg
}

// setPath (op=set_path) registers a Path-tree name for an EXISTING document at
// `path` — the re-home / "give this document a path" operation. It's the cure
// for a path-less document (created without a path before the auto-default, or
// via a client that omitted one): the document is reachable only by id until it
// has a dirent, and the Library/Path browser lists dirents. Idempotent
// (DirentCreate upserts by the full coordinate). It ADDS the name at `path`; it
// does not remove any other name the document may already have elsewhere (a
// document may be named at more than one path), so it is an attach, not a move.
// Runs in the document's own scope (resolveScope), so the dirent and the
// document body align for the viewer.
func (d *Document) setPath(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	docID := in.ID
	if docID == "" {
		docID = in.DocumentID
	}
	if docID == "" {
		return errValidation("set_path: missing required field: id (the document_id)", "Pass `id`, a document_id from create_document or query_documents."), nil
	}
	if in.Path == "" {
		return errValidation("set_path: missing required field: path", "Pass `path`, e.g. /documents/<name>."), nil
	}
	// Verify the document exists in THIS scope — never name a phantom (and so a
	// cross-scope id opaquely 404s rather than creating a dangling dirent).
	res, err := d.query(ctx, key, `SELECT id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom("set_path: "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound("set_path: no such document: "+docID, "Pass a document_id from op=query_documents in the same scope."), nil
	}
	p, perr := d.registerDocDirent(ctx, key, docID, in.Path)
	if perr != nil {
		return docFail("set_path: ", perr), nil
	}
	return jsonResult(map[string]any{"document_id": docID, "path": p})
}

// direntScopeID maps a SQL-Memory scope key onto the DIRENT plane's scope id.
//
// They agree for agent and user scope (the agent name / the user id) and diverge
// for TENANT, because the two planes key a tenant differently and each is right in
// its own plane:
//
//   - SQL Memory REQUIRES a non-empty scope id — it becomes half of a schema name
//     and a database login role, so tenant scope carries the tenant there.
//   - The dirent plane leaves scope_id EMPTY for tenant and lets the tenant_id
//     column carry the identity. That is what the Path tool resolves, what Memory
//     does, and what the store's schema defaults to.
//
// Writing the SQL-Memory id into a dirent leaks a storage-engine constraint into a
// plane that does not share it, and the symptom is silent: the Document tool's own
// path lookups keep working (it reads back at the same wrong coordinate it wrote),
// while the Path tool — and therefore the whole browser — lists nothing. A
// document present in one tool's view and absent from the other's, with no error
// on either side.
func direntScopeID(key sqlmem.ScopeKey) string {
	if key.Scope == "tenant" {
		return ""
	}
	return key.ScopeID
}

// docMemScopeID is the Memory-plane scope id of a document row — a chunk body, a
// derived unit, their embeddings — in the scope (mscope, key).
//
// The same split as direntScopeID, for the k/v plane: SQL Memory keys a tenant
// scope by the tenant, the Memory plane by "" with tenant_id carrying the
// identity. A tenant-scope Memory read looks at "", so a document row keyed by the
// SQL Memory id is invisible to it — which is how tenant documents were stored
// until this mapping existed. Rows still at that id are moved by the store's
// MigrateTenantDocumentRows (postgres migration 0114, the SQLite open path, and
// a sweep on every boot), never read where they lie. Every Memory-plane call
// for a document row takes its scope id from here;
// TestDocumentRows_MemoryScopeIDGoesThroughDocMemScopeID holds the package to it.
func docMemScopeID(mscope store.MemoryScope, key sqlmem.ScopeKey) string {
	if mscope == store.MemoryScopeTenant {
		return ""
	}
	return key.ScopeID
}

// registerDocDirent names a document in the Path tree (a `document` dirent).
func (d *Document) registerDocDirent(ctx context.Context, key sqlmem.ScopeKey, docID, rawPath string) (string, error) {
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return "", docClassify(err, errValidation, docPathFix)
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return "", docClassify(fmt.Errorf("path may not be the root"), errValidation, docPathFix)
	}
	ref, _ := json.Marshal(map[string]any{"document_id": docID})
	if _, err := d.Store.DirentCreate(ctx, store.DirentRow{
		TenantID: direntTenant(ctx), Scope: key.Scope, ScopeID: direntScopeID(key),
		ParentPath: parent, Name: name, Kind: "document", ResourceRef: ref,
	}); err != nil {
		return "", err
	}
	return canonical, nil
}

// docIDFromInput resolves a document id from in.ID or in.Path (Path-tree lookup).
func (d *Document) docIDFromInput(ctx context.Context, key sqlmem.ScopeKey, in docInput) (string, error) {
	// A document is named by `id` on the ops that act on a document, and by
	// `document_id` on the ops that act on something inside one — and a model
	// that has just been handed a document_id passes document_id. Accepting
	// both costs nothing; accepting both when they DISAGREE would mean silently
	// acting on one of two documents the caller named, so that is refused.
	if in.ID != "" && in.DocumentID != "" && in.ID != in.DocumentID {
		return "", docClassify(fmt.Errorf("id %q and document_id %q name different documents; pass one", in.ID, in.DocumentID),
			errValidation, "")
	}
	if in.ID != "" {
		return in.ID, nil
	}
	if in.DocumentID != "" {
		return in.DocumentID, nil
	}
	if in.Path == "" {
		return "", docClassify(fmt.Errorf("missing required field: document_id (or id, or path)"),
			errValidation, "Pass `document_id` from create_document or query_documents, or the document's `path`.")
	}
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return "", docClassify(err, errValidation, docPathFix)
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return "", docClassify(fmt.Errorf("path may not be the root"), errValidation, docPathFix)
	}
	// A path of another scope is simply absent from this scope's dirents, so it
	// reads exactly like a path that does not exist.
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return "", docClassify(fmt.Errorf("no such path: %s", canonical),
				errNotFound, "List the scope's documents with op=query_documents, or browse with the Path tool's ls.")
		}
		return "", err
	}
	if row.Kind != "document" {
		return "", docClassify(fmt.Errorf("path %s is a %s, not a document", canonical, row.Kind),
			errValidation, "Pass the path of a document, or its document_id.")
	}
	var ref struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal(row.ResourceRef, &ref)
	return ref.DocumentID, nil
}

func (d *Document) getDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail("get_document: ", err), nil
	}
	res, err := d.query(ctx, key, `SELECT id, title, root_chunk_id, type, status, created_at, updated_at FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom("get_document: "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound("get_document: no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	m := map[string]any{}
	for i, c := range res.Columns {
		m[c] = res.Rows[0][i]
	}
	rootID := asStr(m["root_chunk_id"])
	resp := map[string]any{
		"document_id": asStr(m["id"]), "title": asStr(m["title"]),
		"root_chunk_id": rootID,
	}
	// type/status come from the documents row (the denormalized mirror of the root
	// chunk, kept in sync by create_document + the update_chunk/upsert root mirror)
	// — one read instead of a second get_chunk.
	if t := asStr(m["type"]); t != "" {
		resp["type"] = t
	}
	if s := asStr(m["status"]); s != "" {
		resp["status"] = s
	}
	// The document's own tags (independent of the root chunk's tags).
	tags, terr := d.listDocumentTags(ctx, key, docID)
	if terr != nil {
		return errFrom("get_document: tags: "+terr.Error(), terr), nil
	}
	if len(tags) > 0 {
		resp["tags"] = tags
	}
	if rootID != "" {
		// Colour is decoration, so a read fault degrades to "no scheme" rather
		// than failing the whole get — matching the getChunkRow probe above.
		// Content reads (get_chunk, export_md) propagate instead.
		rootBody, _ := d.readBody(ctx, mscope, key, rootID)
		enabled, scheme := docColorMeta(rootBody)
		resp["color_enabled"] = enabled
		if len(scheme) > 0 {
			resp["color_scheme"] = scheme
		}
		// Where an imported document came from: its root's provenance field.
		if p := d.importedProvenance(ctx, mscope, key, rootID, rootBody); p != nil {
			resp["provenance"] = p
		}
		// WHAT THE TREE CANNOT REACH. When this document's root is a subject, a fact
		// about it AND about something else is filed under the other one — the tree
		// holds each chunk once — so walking this document answers only half of "what
		// do we know about X". These are the other half, and they are reported HERE
		// rather than in export_md because that output round-trips through import_md:
		// an edge whose far end is in another document would either be dropped on
		// import or re-created pointing at nothing.
		//
		// Not gated on "is this an entity". An ordinary document's root is the target
		// of no `about` edge, so the indexed probe returns nothing and the block is
		// omitted — a gate would cost a query to save one.
		refs, refsTruncated, rerr := d.inboundSubjectRefs(ctx, key, rootID, docID)
		if rerr != nil {
			return errFrom("get_document: references: "+rerr.Error(), rerr), nil
		}
		if len(refs) > 0 {
			resp["references"] = refs
			if refsTruncated {
				resp["references_truncated"] = true
				resp["references_note"] = subjectRefsNote(rootID)
			}
		}
	}
	// WHO ELSE KNOWS THIS SUBJECT. Counts rather than rows: a dossier read is a
	// signpost, and a caller who wants the facts asks list_facts for them. What this
	// answers is the question an empty tenant dossier otherwise leaves hanging — "is
	// nothing known about Dave, or is it just not known HERE".
	if in.Across {
		if elsewhere := d.subjectAcrossScopes(ctx, key.Scope,
			d.naturalKeyOf(ctx, key, rootID), subjectRefsCap, false); len(elsewhere) > 0 {
			resp["across_scopes"] = elsewhere
		}
	}
	return jsonResult(resp)
}

// docColorMeta extracts the RFC BN per-document color settings from a root
// chunk's Memory-stored fields. color_enabled toggles tinting for the whole
// document; color_scheme is an opaque frontend map (doc.<type>.<status> and
// chunk.<status> → CSS color) the backend stores and returns verbatim — it does
// not interpret the palette. Both are optional; absent → (false, nil).
func docColorMeta(cb chunkBody) (enabled bool, scheme json.RawMessage) {
	if len(cb.Fields) == 0 {
		return false, nil
	}
	var f struct {
		ColorEnabled *bool           `json:"color_enabled"`
		ColorScheme  json.RawMessage `json:"color_scheme"`
	}
	if err := json.Unmarshal(cb.Fields, &f); err != nil {
		return false, nil
	}
	if f.ColorEnabled != nil {
		enabled = *f.ColorEnabled
	}
	return enabled, f.ColorScheme
}

// inPlaceholders builds a `?,?,…` list and the matching args for a SQL IN clause
// (Rebind converts `?`→`$N` on the postgres tier). Caller guarantees len>0.
func inPlaceholders(ids []string) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('?')
		args = append(args, id)
	}
	return sb.String(), args
}

// documentsSummary returns per-document display metadata — title, the ROOT
// chunk's type/status, and the RFC BN color settings — for a set of document ids
// and/or every document under a Path-tree path. It powers the Path-tree coloring
// in ONE call (dirents carry only a document_id, not type/status), avoiding an
// N+1 of get_document per row. Unknown/out-of-scope ids are silently skipped
// (opaque — a caller can't probe another scope's documents).
func (d *Document) documentsSummary(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	// A caller whose access-control set hides some document of the scope gets the
	// answer it would get if those documents did not exist: one found under_path
	// is dropped before the bound below is counted, and one it names in
	// document_ids is skipped as an unknown id is. Dropped after the bound, they
	// would spend it and set truncated on documents the caller cannot see.
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return errFrom("documents_summary: "+err.Error(), err), nil
	}
	ids := in.DocumentIDs
	if in.UnderPath != "" {
		under, err := d.documentsUnderPath(ctx, key, in.UnderPath)
		if err != nil {
			return docFail("documents_summary: ", err), nil
		}
		if limited {
			kept := under[:0]
			for _, id := range under {
				ok, err := acl.readable(ctx, id)
				if err != nil {
					return errFrom("documents_summary: "+err.Error(), err), nil
				}
				if ok {
					kept = append(kept, id)
				}
			}
			under = kept
		}
		ids = append(ids, under...)
	}
	// Dedup + drop empties (a caller may mix document_ids with under_path, and
	// the two sets can overlap).
	seen := map[string]bool{}
	var uniq []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		// Asked about nothing, the answer used to be an empty list, which reads
		// as "no documents exist": measured live, a model that had just created
		// a document then doubted it and made two more. Say what to pass.
		if len(in.DocumentIDs) == 0 && in.UnderPath == "" {
			return errValidation("documents_summary: pass document_ids (the documents to summarize) or under_path "+
				"(a Path-tree subtree, e.g. /documents). To list the documents in a scope, use query_documents.", ""), nil
		}
		return jsonResult(map[string]any{"documents": []any{}})
	}
	// BOUND THE RESPONSE (RFC CV OQ2). under_path resolves to EVERY document under
	// a subtree, and subject-homed facts make `/facts` exactly as large as the
	// tenant's entity count — so this built a ten-thousand-element IN list and
	// returned ten thousand rows. Postgres serves that; an agent's context window
	// does not, which is the same reason `path op=ls` is paged.
	//
	// The bound is a DEFAULT rather than a refusal, which is a deliberate deviation
	// from "requires under_path or a limit": refusing an unbounded call would break
	// the explorer, which legitimately passes a page of ids it already holds, and
	// buys nothing — the response is bounded either way. Truncation is REPORTED, so
	// it is a bound rather than a lie.
	//
	// No cursor, and that is not an omission. A caller pages the DIRECTORY through
	// `path op=ls`, which has one, and asks for metadata a page at a time; a second
	// cursor here would let it page metadata for a set it never enumerated.
	limit := in.Limit
	if limit <= 0 {
		limit = lsDefaultLimit
	}
	if limit > lsMaxLimit {
		limit = lsMaxLimit
	}
	truncated := false
	if len(uniq) > limit {
		uniq = uniq[:limit]
		truncated = true
	}
	// Batch: the documents rows.
	ph, args := inPlaceholders(uniq)
	dres, err := d.query(ctx, key, `SELECT id, title, root_chunk_id FROM documents WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return errFrom("documents_summary: "+err.Error(), err), nil
	}
	type docMeta struct{ title, root string }
	docs := map[string]docMeta{}
	var rootIDs []string
	for _, r := range dres.Rows {
		id, title, root := asStr(r[0]), asStr(r[1]), asStr(r[2])
		docs[id] = docMeta{title: title, root: root}
		if root != "" {
			rootIDs = append(rootIDs, root)
		}
	}
	// Batch: the root chunk rows (type/status).
	rootRows := map[string]chunkRow{}
	if len(rootIDs) > 0 {
		rph, rargs := inPlaceholders(rootIDs)
		rres, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE id IN (`+rph+`)`, rargs...)
		if err != nil {
			return errFrom("documents_summary: "+err.Error(), err), nil
		}
		for _, r := range rres.Rows {
			row := scanChunkRow(rres.Columns, r)
			rootRows[row.ID] = row
		}
	}
	out := make([]map[string]any, 0, len(uniq))
	for _, id := range uniq {
		dm, ok := docs[id]
		if !ok {
			continue // no such document in this scope → skip (opaque)
		}
		if limited {
			readable, err := acl.readable(ctx, id)
			if err != nil {
				return errFrom("documents_summary: "+err.Error(), err), nil
			}
			if !readable {
				continue // as if there were no such document
			}
		}
		entry := map[string]any{"document_id": id, "title": dm.title, "color_enabled": false}
		if dm.root != "" {
			entry["root_chunk_id"] = dm.root
			if row, ok := rootRows[dm.root]; ok {
				if row.Type != "" {
					entry["type"] = row.Type
				}
				if row.Status != "" {
					entry["status"] = row.Status
				}
			}
			// Decoration inside a list loop: one unreadable root must not fail
			// the whole summary. See the colour note in getDocument.
			rootBody, _ := d.readBody(ctx, mscope, key, dm.root)
			enabled, scheme := docColorMeta(rootBody)
			entry["color_enabled"] = enabled
			if len(scheme) > 0 {
				entry["color_scheme"] = scheme
			}
		}
		out = append(out, entry)
	}
	// truncated is emitted only when it fired: a caller that never hit the bound
	// should not have to reason about a field that is always false.
	res := map[string]any{"documents": out}
	if truncated {
		res["truncated"] = true
		res["note"] = "more documents matched than were returned; narrow under_path, " +
			"or page the directory with `path op=ls` and pass each page's ids as document_ids"
	}
	return jsonResult(res)
}

func (d *Document) deleteDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail("delete_document: ", err), nil
	}
	// Whether this document is a subject is read now: once deleted, nothing records
	// which key it held, and other scopes' evidence still names that key.
	subject, isSubject, err := d.subjectRootOf(ctx, key, docID, true)
	if err != nil {
		return errFrom("delete_document: "+err.Error(), err), nil
	}
	// The SQL side runs in ONE transaction: enumerate the chunk ids (for the
	// Memory-body cleanup below), then delete edges (BOTH directions — so an
	// INCOMING cross-document edge from another doc no longer dangles), then
	// the chunk rows, then the document row. Any failure rolls the whole thing
	// back — no half-deleted document.
	var ids []string
	txErr := d.withSqlTxn(ctx, key, func(txnID string) error {
		res, err := d.queryTxn(ctx, txnID, `SELECT id FROM chunks WHERE document_id = ?`, docID)
		if err != nil {
			return err
		}
		for _, r := range res.Rows {
			ids = append(ids, asStr(r[0]))
		}
		// Facts derived from this document's text keep their quotes (RFC FN P3a).
		if err := d.snapshotDerivedQuotes(ctx, txnID, `{col} IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		if err := d.expireUnsupportedRelations(ctx, key, txnID, "", `{col} IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		if err := d.deleteEdgesWhere(ctx, key, txnID, `from_id IN (SELECT id FROM chunks WHERE document_id = ?) OR to_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID, docID); err != nil {
			return err
		}
		// Drop image assets BEFORE the chunk rows (the subquery resolves against
		// chunks). RFC BO.
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_assets WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		// Tag rows: chunk tags (resolved via the chunks subquery, so before the chunk
		// rows go) + the document's own tags. RFC BS.
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_tags WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		if err := d.execTxn(ctx, txnID, `DELETE FROM document_tags WHERE document_id = ?`, docID); err != nil {
			return err
		}
		// Same ordering constraint for the entity-tier sidecar, and the same reason
		// it is done here rather than by a foreign key: an orphaned temporal row is
		// invisible. No read filters it and no sweeper reaps it, so it would sit in
		// the scope forever pointing at a chunk that no longer exists — the dead-link
		// class this schema's explicit-cascade discipline exists to prevent.
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_memory_meta WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		// The body-change log for every chunk in the document (RFC BS Phase 3a),
		// resolved via the chunks subquery so it runs before the chunk rows go.
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_revisions WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		// Canvas layout rows for every chunk in the document — resolved via the
		// chunks subquery so they go before the chunk rows. An orphaned layout row
		// is invisible dead data nothing reaps, the same class the explicit-cascade
		// discipline exists to prevent.
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_layout WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID); err != nil {
			return err
		}
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunks WHERE document_id = ?`, docID); err != nil {
			return err
		}
		return d.execTxn(ctx, txnID, `DELETE FROM documents WHERE id = ?`, docID)
	})
	if txErr != nil {
		return errFrom("delete_document: "+txErr.Error(), txErr), nil
	}
	// Bodies AFTER commit (separate store; best-effort — an orphaned body is
	// invisible dead k/v, never reachable once its chunk row is gone).
	for _, id := range ids {
		_, _ = d.Store.MemoryDelete(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), chunkBodyKey(id))
		d.deleteUnitsOf(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), id)
	}
	n := len(ids)
	// Drop any Path-tree dirent(s) pointing at this document — best-effort, by
	// document_id (works whether the caller addressed by id or path, so a
	// delete-by-id never leaves a dangling name). Scans the scope's document
	// dirents (bounded; a scope's name count is small).
	if rows, lerr := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), "/"); lerr == nil {
		for _, r := range rows {
			if r.Kind != "document" {
				continue
			}
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(r.ResourceRef, &ref)
			if ref.DocumentID == docID {
				_, _ = d.Store.DirentDelete(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), r.ParentPath, r.Name)
			}
		}
	}
	// The document's change feed goes with it: nothing can follow a document
	// that no longer exists. Best-effort, like the dirent cleanup above; the
	// idle-feed sweep (SweepDocumentFeeds) catches a purge that failed.
	if _, perr := d.Store.ChannelPurge(ctx, tools.RunIdentity(ctx).TenantID, documentFeedPrefix+docID+documentFeedSuffix, ""); perr != nil {
		slog.WarnContext(ctx, "delete_document: purge change feed", "document_id", docID, "err", perr)
	}
	out := map[string]any{"deleted": true, "document_id": docID, "n_chunks_deleted": n}
	if isSubject {
		if why := d.publishSubjectEvent(ctx, key, mscope, subjectEvent{
			Op: "removed", NaturalKey: subject.NaturalKey, DocumentID: subject.DocumentID, RootChunkID: subject.RootChunkID,
		}); why != "" {
			out["not_announced"] = why
		}
	}
	return jsonResult(out)
}

// --- ops: chunk lifecycle ---

func (d *Document) createChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.DocumentID == "" {
		return errValidation("create_chunk: missing required field: document_id", "Pass `document_id` from create_document or query_documents."), nil
	}
	if in.Title == "" {
		return errValidation("create_chunk: missing required field: title", "Pass `title`, the chunk's heading."), nil
	}
	if r := emptyIntervalRefusal("create_chunk", in.ValidAt, in.InvalidAt); r != nil {
		return *r, nil
	}
	// The `principal` field is the operator's; a new chunk never carries one.
	if refused, err := d.holdPrincipalField(ctx, key, mscope, "create_chunk", "", in.Fields); err != nil || refused != nil {
		if err != nil {
			return errFrom("create_chunk: "+err.Error(), err), nil
		}
		return *refused, nil
	}
	// RFC BP — after_id inserts the new chunk immediately after an existing
	// sibling: adopt that sibling's parent, and insert-and-shift the later
	// siblings by +1 (in one txn) so the new chunk lands at afterPos+1 with no
	// tie. Overrides parent_id/position.
	parentID := in.ParentID
	// A directly-supplied parent_id gets the SAME validation after_id already had
	// below — it must exist, and it must be in this document.
	//
	// Without it, create_chunk returned success (with a fresh id and revision) for
	// a chunk nothing could ever reach: no walk descends to it from the root, so it
	// was absent from get_document and export_md the instant it was written.
	// Writing content, being told it worked, and never seeing it again is a worse
	// outcome than an error, and it is invisible to the dead-link sweeper, which
	// looks for a missing DOCUMENT rather than a missing PARENT.
	//
	// An empty parent_id means "child of the root", and now it is: the chunk is
	// parented to the document's root chunk. It used to be stored with NO
	// parent, which put it BESIDE the root — export_md rendered it as a second
	// top-level heading next to the document's title — while every caller,
	// model or human, meant "a section of this document". The root lookup also
	// makes a mistyped document_id an error instead of an orphan chunk that no
	// document shows.
	//
	// The document is resolved FIRST, whatever else was passed. It used to be
	// looked up only when neither parent_id nor after_id was given, so a call in
	// the wrong scope that named the document's real root as parent_id was
	// refused "no such parent_id … pass the document's root_chunk_id" — which is
	// exactly what it had passed. A model reading that retries the same id or
	// drops it, never the scope (measured: qwen3.6 made two more failing calls).
	root, rerr := d.documentRootChunk(ctx, key, in.DocumentID)
	if rerr != nil {
		return errNotFound("create_chunk: "+rerr.Error()+
			" (pass the document_id create_document returned, in the same scope)", ""), nil
	}
	if in.ParentID == "" && in.AfterID == "" {
		parentID = root
	}
	if in.ParentID != "" {
		par, ok, perr := d.getChunkRow(ctx, key, in.ParentID)
		if perr != nil {
			return errFrom("create_chunk: parent lookup: "+perr.Error(), perr), nil
		}
		if !ok {
			return errNotFound("create_chunk: no such parent_id: "+in.ParentID+
				" (the chunk would be unreachable from the document root; pass the document's "+
				"root_chunk_id to put it under the document's title)", ""), nil
		}
		if par.DocumentID != in.DocumentID {
			return errValidation(fmt.Sprintf(
				"create_chunk: parent_id %q belongs to document %q, not %q — a chunk cannot be "+
					"parented across documents", in.ParentID, par.DocumentID, in.DocumentID), "Pass a parent_id from the same document, or omit it to put the chunk under the document's root."), nil
		}
	}
	pos := 0
	if in.AfterID != "" {
		sib, ok, err := d.getChunkRow(ctx, key, in.AfterID)
		if err != nil {
			return errFrom("create_chunk: "+err.Error(), err), nil
		}
		if !ok {
			return errNotFound("create_chunk: no such after_id chunk: "+in.AfterID, "Pass the id of an existing chunk in this document (list them with op=query_chunks and document_id)."), nil
		}
		if sib.DocumentID != in.DocumentID {
			return errValidation("create_chunk: after_id belongs to a different document", "Pass an after_id from the same document as document_id."), nil
		}
		parentID = sib.ParentID
		pos = sib.Position + 1
	} else if in.Position != nil {
		pos = *in.Position
	} else {
		// Append: max(position)+1 among siblings. Branch on root-level vs
		// parented so the NULL comparison is portable (postgres rejects
		// `parent_id IS ?` with a non-null bind).
		var res *sqlmem.QueryResult
		var err error
		if parentID == "" {
			res, err = d.query(ctx, key, `SELECT position FROM chunks WHERE document_id = ? AND parent_id IS NULL ORDER BY position DESC LIMIT 1`, in.DocumentID)
		} else {
			res, err = d.query(ctx, key, `SELECT position FROM chunks WHERE document_id = ? AND parent_id = ? ORDER BY position DESC LIMIT 1`, in.DocumentID, parentID)
		}
		if err == nil && len(res.Rows) > 0 {
			pos = asInt(res.Rows[0][0]) + 1
		}
	}
	now := time.Now().UnixNano()
	id := newDocID()
	// The shift (after_id) + insert must be atomic — a txn so a concurrent read
	// never sees the gap between "shifted" and "inserted".
	insert := func(exec func(stmt string, args ...any) error) error {
		if in.AfterID != "" {
			// Shift later siblings up by one to open the slot at `pos`.
			if parentID == "" {
				if err := exec(`UPDATE chunks SET position = position + 1 WHERE document_id = ? AND parent_id IS NULL AND position >= ?`, in.DocumentID, pos); err != nil {
					return err
				}
			} else {
				if err := exec(`UPDATE chunks SET position = position + 1 WHERE document_id = ? AND parent_id = ? AND position >= ?`, in.DocumentID, parentID, pos); err != nil {
					return err
				}
			}
		}
		return exec(`INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision, valid_at, invalid_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			id, in.DocumentID, nullIfEmpty(parentID), pos, nullIfEmpty(in.Type), nullIfEmpty(in.Status), in.Title, now, now,
			int64Arg(in.ValidAt), int64Arg(in.InvalidAt))
	}
	var insErr error
	if in.AfterID != "" {
		insErr = d.withSqlTxn(ctx, key, func(txnID string) error {
			return insert(func(stmt string, args ...any) error { return d.execTxn(ctx, txnID, stmt, args...) })
		})
	} else {
		insErr = insert(func(stmt string, args ...any) error { return d.exec(ctx, key, stmt, args...) })
	}
	if insErr != nil {
		return errFrom("create_chunk: "+insErr.Error(), insErr), nil
	}
	if err := d.writeBodyAs(ctx, mscope, key, id, in.Type, in.Body, in.Fields, in.bodyOrigin); err != nil {
		return errFrom("create_chunk: body: "+err.Error(), err), nil
	}
	// Seed the body-change log at revision 1 (RFC BS Phase 3a) — the chunk's body
	// exists as of now, so it is the first snapshot even when in.Body is empty.
	if err := d.recordRevision(ctx, key, id, 1, in.Body); err != nil {
		return errFrom("create_chunk: history: "+err.Error(), err), nil
	}
	// Derive this chunk's inline [[name]] link edges from its body (RFC BS Phase
	// 2a). A body with no resolvable name-links materializes no edges.
	if err := d.reconcileNameLinks(ctx, key, id, in.Body); err != nil {
		return errFrom("create_chunk: name links: "+err.Error(), err), nil
	}
	// Tags on a fresh chunk (also serves upsert_chunk's create path, which
	// delegates here). replaceChunkTags is delete-then-insert; on a new id the
	// delete matches nothing, so len>0 keeps an untagged create from a needless
	// write.
	if len(in.Tags) > 0 {
		if err := d.replaceChunkTags(ctx, key, id, in.Tags); err != nil {
			return errFrom("create_chunk: tags: "+err.Error(), err), nil
		}
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), in.DocumentID, "create_chunk", id)
	return d.getChunk(ctx, key, mscope, docInput{ID: id})
}

func (d *Document) getChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("get_chunk: missing required field: id", "Pass `id`, a chunk id from query_chunks, search or create_chunk."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("get_chunk: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("get_chunk: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	// Surface a body-read fault instead of returning the chunk with an empty
	// body: a silent empty read is indistinguishable from a deleted body, which
	// is exactly how the v1.33.0 tenant regression went unnoticed.
	cb, err := d.readBody(ctx, mscope, key, in.ID)
	if err != nil {
		return errFrom("get_chunk: body: "+err.Error(), err), nil
	}
	resp := chunkResponse(row, cb)
	// The chunk's tags (RFC BS) — the primary read surfaces them so a reader (and
	// the editor's load) sees a chunk's tags without a separate list_tags call.
	if tags, terr := d.listChunkTags(ctx, key, in.ID); terr != nil {
		return errFrom("get_chunk: tags: "+terr.Error(), terr), nil
	} else if len(tags) > 0 {
		resp["tags"] = tags
	}
	// RFC BO: surface an image chunk's stored asset (metadata only — the bytes
	// come from GET /v1/_document/asset/{id}) so a viewer knows to render an img.
	if mt, sz, ok := d.assetMeta(ctx, key, in.ID); ok {
		resp["asset"] = map[string]any{"media_type": mt, "size": sz}
	}
	// RFC BV: when the chunk is a fact (has a chunk_memory_meta sidecar), surface
	// the bi-temporal / provenance block so the memory view reads it typed —
	// without it, a fact and a plain chunk are indistinguishable on read.
	isFact := false
	if meta, ok, merr := d.readChunkMeta(ctx, key, in.ID); merr != nil {
		return errFrom("get_chunk: entity: "+merr.Error(), merr), nil
	} else if ok {
		isFact = true
		resp["entity"] = chunkMetaToJSON(meta)
		// A KEYED chunk is what sync carries, so it always names both ends of its
		// own interval — null when unset. That lets a syncing peer tell "undated
		// here" (null: clear) from "this server does not know the field" (absent:
		// keep), and an older peer is never mistaken for one that cleared a date.
		if meta.NaturalKey != "" {
			for _, k := range []string{"valid_at", "invalid_at"} {
				if _, set := resp[k]; !set {
					resp[k] = nil
				}
			}
		}
	}
	// RFC FP R8: a fact is never a definitions passage, so its read skips the lookup.
	// Only an explicit get_chunk computes the flag: create_chunk, update_chunk and
	// set_asset answer through this function too, and the lookup would put the
	// ontology read — and, right after an edit to the ontology itself, a full rebuild
	// of it — on every chunk write.
	if !isFact && in.Op == "get_chunk" {
		defs, derr := d.definitionalChunks(ctx, key, []chunkRow{row})
		if derr != nil {
			return errFrom("get_chunk: definitional: "+derr.Error(), derr), nil
		}
		if defs[row.ID] {
			resp["definitional"] = true
		}
		spans, berr := d.boilerplateSpans(ctx, key, row, cb.Body)
		if berr != nil {
			return errFrom("get_chunk: boilerplate: "+berr.Error(), berr), nil
		}
		if len(spans) > 0 {
			resp["boilerplate"] = spans
		}
	}
	return jsonResult(resp)
}

func chunkResponse(row chunkRow, cb chunkBody) map[string]any {
	out := map[string]any{
		"id": row.ID, "document_id": row.DocumentID, "position": row.Position,
		"title": row.Title, "revision": row.Revision, "body": cb.Body,
	}
	if row.ParentID != "" {
		out["parent_id"] = row.ParentID
	}
	if row.Type != "" {
		out["type"] = row.Type
	}
	if row.Status != "" {
		out["status"] = row.Status
	}
	if len(cb.Fields) > 0 {
		out["fields"] = cb.Fields
	}
	putValidity(out, row.ValidAt, row.InvalidAt)
	return out
}

// emptyIntervalRefusal refuses a validity interval that contains no instant:
// invalid_at at or before valid_at. Half-open [valid_at, invalid_at), so such a
// chunk would match no as_of at all — always a mistake, never a way to hide one.
func emptyIntervalRefusal(op string, validAt, invalidAt *int64) *tools.Result {
	if validAt == nil || invalidAt == nil || *invalidAt > *validAt {
		return nil
	}
	r := errValidation(fmt.Sprintf("%s: invalid_at (%d) must be after valid_at (%d) — the interval is [valid_at, invalid_at)", op, *invalidAt, *validAt),
		"Pass an invalid_at later than valid_at, or omit the end that is unknown.")
	return &r
}

// --- asset ops (RFC BO — image chunks) ---

// validImageMediaTypes is the RFC AT vision whitelist (the common denominator
// across the four vision providers), reused here as the set an image chunk may
// store — defined locally to avoid importing internal/loop. SVG is deliberately
// excluded (script-in-SVG is an XSS surface on the serving endpoint).
var validImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ValidImageMediaType reports whether mt is an allowed image chunk media type
// (RFC BO) — exported for the serving handler's defense-in-depth Content-Type
// check (never serve a non-whitelisted type even if one somehow got stored).
func ValidImageMediaType(mt string) bool { return validImageMediaTypes[mt] }

// defaultMaxAssetBytes bounds a decoded image when the tool has no configured cap.
const defaultMaxAssetBytes = 8 << 20 // 8 MiB

func (d *Document) maxAssetBytes() int {
	if d.MaxAssetBytes > 0 {
		return d.MaxAssetBytes
	}
	return defaultMaxAssetBytes
}

// setAsset attaches (or replaces) an image chunk's binary asset: decode the
// base64 payload to raw bytes, upsert the chunk_assets row, and mark the chunk
// type=image with its media_type/size in fields. The chunk must already exist
// (create_chunk first) — mirrors document create + set_path.
func (d *Document) setAsset(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("set_asset: missing required field: id (the chunk id)", "Create the chunk with op=create_chunk first, then pass its id."), nil
	}
	if in.MediaType == "" {
		return errValidation("set_asset: missing required field: media_type", "Pass media_type: image/png, image/jpeg, image/gif or image/webp."), nil
	}
	if !validImageMediaTypes[in.MediaType] {
		return errValidation(fmt.Sprintf("set_asset: unsupported media_type %q (allowed: image/png, image/jpeg, image/gif, image/webp)", in.MediaType), "Convert the image to one of the allowed types and resend."), nil
	}
	if in.Data == "" {
		return errValidation("set_asset: missing required field: data (base64 image bytes)", "Pass `data`, the image bytes as standard base64."), nil
	}
	// Bound the ENCODED length before decoding. base64 expands 3 bytes into 4, so
	// len(data)*3/4 is an exact upper bound on what the decode will allocate —
	// checking after the fact means materialising an oversize image in order to
	// reject it.
	if maxB := d.maxAssetBytes(); len(in.Data)/4*3 > maxB {
		return errValidation(fmt.Sprintf("set_asset: image is at least %d bytes, exceeds the %d-byte cap",
			len(in.Data)/4*3, maxB), "Send a smaller image (resize or recompress it)."), nil
	}
	raw, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil {
		return errValidation("set_asset: data must be valid standard base64 (no data: prefix): "+err.Error(), ""), nil
	}
	if len(raw) == 0 {
		return errValidation("set_asset: empty image data", "Pass the image bytes, base64-encoded, in `data`."), nil
	}
	if len(raw) > d.maxAssetBytes() {
		return errValidation(fmt.Sprintf("set_asset: image is %d bytes, exceeds the %d-byte cap", len(raw), d.maxAssetBytes()), "Send a smaller image (resize or recompress it)."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("set_asset: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("set_asset: no such chunk: "+in.ID, "Create the chunk with op=create_chunk first, then pass its id."), nil
	}
	now := time.Now().UnixNano()
	// Upsert the asset row + mark the chunk type=image in ONE transaction
	// (portable upsert = delete-then-insert — no ON CONFLICT dialect split).
	txErr := d.withSqlTxn(ctx, key, func(txnID string) error {
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_assets WHERE chunk_id = ?`, in.ID); err != nil {
			return err
		}
		if err := d.execTxn(ctx, txnID, `INSERT INTO chunk_assets (chunk_id, media_type, bytes, size, created_at) VALUES (?, ?, ?, ?, ?)`,
			in.ID, in.MediaType, raw, int64(len(raw)), now); err != nil {
			return err
		}
		return d.execTxn(ctx, txnID, `UPDATE chunks SET type = 'image', updated_at = ?, revision = revision + 1 WHERE id = ?`, now, in.ID)
	})
	if txErr != nil {
		return errFrom("set_asset: "+txErr.Error(), txErr), nil
	}
	// Record the asset facts in the chunk's fields (merged — keep any existing
	// keys + the caption body). Fields live in Memory alongside the body.
	// This is a fields-only write, but writeBody rewrites the body too — so a
	// failed read here would blank the caption. Refuse instead of persisting a
	// body we could not read.
	cb, rerr := d.readBody(ctx, mscope, key, in.ID)
	if rerr != nil {
		return errFrom("set_asset: body: "+rerr.Error(), rerr), nil
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		_ = json.Unmarshal(cb.Fields, &fields)
	}
	fields["kind"] = "image"
	fields["media_type"] = in.MediaType
	fields["size"] = len(raw)
	if in.Filename != "" {
		fields["filename"] = in.Filename
	}
	nf, _ := json.Marshal(fields)
	if err := d.writeBody(ctx, mscope, key, in.ID, "image", cb.Body, nf); err != nil {
		return errFrom("set_asset: fields: "+err.Error(), err), nil
	}
	// set_asset sets type='image' on the chunk; if that chunk is a document's root,
	// mirror it onto the documents row so get_document/query_documents stay in sync
	// (a no-op for a non-root chunk). RFC BS.
	if err := d.mirrorRootFacets(ctx, key, in.ID); err != nil {
		return errFrom("set_asset: "+err.Error(), err), nil
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), row.DocumentID, "set_asset", in.ID)
	return d.getChunk(ctx, key, mscope, docInput{ID: in.ID})
}

// getAsset returns an image chunk's asset METADATA (media_type, size) — never
// the bytes (those come from GET /v1/_document/asset/{id}).
func (d *Document) getAsset(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("get_asset: missing required field: id (the chunk id)", "Pass `id`, the image chunk's id."), nil
	}
	mt, sz, ok := d.assetMeta(ctx, key, in.ID)
	if !ok {
		return errNotFound("get_asset: no asset on chunk: "+in.ID, "Attach an image with op=set_asset, or check the id with op=get_chunk."), nil
	}
	out := map[string]any{"chunk_id": in.ID, "media_type": mt, "size": sz}
	// RFC BU phase 4 — report the description state, because "unsearchable" must be
	// VISIBLE rather than silent. Three distinguishable states, which is why
	// described_at is a column of its own: never examined (no described_at), examined
	// and described, examined and produced nothing useful.
	desc, at := d.assetDescriptionState(ctx, key, in.ID)
	out["described"] = at > 0
	if desc != "" {
		out["description"] = desc
	}
	if at > 0 {
		out["described_at"] = at
		if desc == "" {
			out["note"] = "a describe pass ran and produced no usable description; " +
				"this image is searchable only by its caption"
		}
	} else {
		out["note"] = "no describe pass has run for this image yet; it is searchable " +
			"only by its caption until one does"
	}
	return jsonResult(out)
}

// assetMeta reads an asset's media_type + size (no bytes). ok=false when the
// chunk has no asset in this scope.
func (d *Document) assetMeta(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (mediaType string, size int, ok bool) {
	res, err := d.query(ctx, key, `SELECT media_type, size FROM chunk_assets WHERE chunk_id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return "", 0, false
	}
	return asStr(res.Rows[0][0]), asInt(res.Rows[0][1]), true
}

// readAssetRow reads an asset's media_type + raw bytes using the CURRENT resolved
// key (no scope re-resolution — for export_md, which already holds the key).
func (d *Document) readAssetRow(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (mediaType string, data []byte, ok bool) {
	res, err := d.query(ctx, key, `SELECT media_type, bytes FROM chunk_assets WHERE chunk_id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return "", nil, false
	}
	return asStr(res.Rows[0][0]), asBytes(res.Rows[0][1]), true
}

// ReadAsset reads an image chunk's raw bytes for the HTTP serving endpoint. scope
// is "agent"|"user" (default user); the ScopeKey + tenant come from ctx exactly
// as the tool's own ops resolve them, so a caller can only read an asset in its
// OWN scope (cross-scope simply misses → opaque 404 at the handler). ok=false
// when the chunk has no asset in this scope.
func (d *Document) ReadAsset(ctx context.Context, scope, chunkID string) (mediaType string, data []byte, ok bool, err error) {
	if d.Store == nil || d.SqlMem == nil {
		return "", nil, false, fmt.Errorf("Document asset: requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return "", nil, false, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return "", nil, false, err
	}
	res, err := d.query(ctx, key, `SELECT media_type, bytes FROM chunk_assets WHERE chunk_id = ? LIMIT 1`, chunkID)
	if err != nil {
		return "", nil, false, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return "", nil, false, nil
	}
	return asStr(res.Rows[0][0]), asBytes(res.Rows[0][1]), true, nil
}

func (d *Document) updateChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, raw json.RawMessage) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("update_chunk: missing required field: id", "Pass `id`, the chunk to update."), nil
	}
	if in.Revision == nil {
		return errValidation("update_chunk: missing required field: revision (optimistic concurrency — pass the chunk's current revision)", "Read the chunk with op=get_chunk and pass its `revision`."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("update_chunk: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("update_chunk: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	if row.Revision != *in.Revision {
		return errBusiness(fmt.Sprintf("update_chunk: revision conflict (you passed %d, current is %d) — re-read the chunk and retry", *in.Revision, row.Revision), "Call op=get_chunk for the current revision and body, then resend with that revision."), nil
	}
	// Detect which fields the caller actually provided (presence-based; lets a
	// field be set to empty, unlike a zero-value check). For valid_at/invalid_at
	// presence is what tells an explicit null (clear) from an absent key (keep).
	var present map[string]json.RawMessage
	_ = json.Unmarshal(raw, &present)
	_, hasValidAt := present["valid_at"]
	_, hasInvalidAt := present["invalid_at"]
	// The interval in force AFTER this update is checked BEFORE the revision
	// moves, so a refused update changes nothing.
	effValid, effInvalid := row.ValidAt, row.InvalidAt
	if hasValidAt {
		effValid = in.ValidAt
	}
	if hasInvalidAt {
		effInvalid = in.InvalidAt
	}
	if r := emptyIntervalRefusal("update_chunk", effValid, effInvalid); r != nil {
		return *r, nil
	}
	// A subject's new type is checked here too, before the revision moves; it is
	// announced once the update has been written.
	var retype *subjectRetype
	if _, has := present["type"]; has {
		rt, refused, rerr := d.checkSubjectRetype(ctx, key, "update_chunk", in.ID, in.Type, false)
		if rerr != nil {
			return errFrom("update_chunk: "+rerr.Error(), rerr), nil
		}
		if refused != nil {
			return *refused, nil
		}
		retype = rt
	}
	// The `principal` field is the operator's (it binds a subject to a user account):
	// a change to it is refused here, before the revision moves.
	if _, has := present["fields"]; has {
		refused, herr := d.holdPrincipalField(ctx, key, mscope, "update_chunk", in.ID, in.Fields)
		if herr != nil {
			return errFrom("update_chunk: "+herr.Error(), herr), nil
		}
		if refused != nil {
			return *refused, nil
		}
	}
	// Claim the update ATOMICALLY first: the guarded bump only matches if the
	// revision is still what we read. If a concurrent writer raced us, it
	// matches 0 rows → conflict, and we bail BEFORE clobbering the body (the
	// fix for the silent lost-update: the read-check above is advisory; THIS is
	// the real gate).
	now := time.Now().UnixNano()
	bumped, err := d.docSQL().Exec(ctx, key, d.docSQL().Rebind(`UPDATE chunks SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`), []any{now, in.ID, *in.Revision}, 0)
	if err != nil {
		return errFrom("update_chunk: "+err.Error(), err), nil
	}
	if bumped.RowsAffected == 0 {
		return errBusiness(fmt.Sprintf("update_chunk: revision conflict (revision %d was changed by a concurrent write) — re-read the chunk and retry", *in.Revision), "Call op=get_chunk for the current revision and body, then resend with that revision."), nil
	}
	if hasValidAt || hasInvalidAt {
		// Metadata: the revision bump above covers it, and no chunk_revisions row is
		// written — that log records body changes only.
		if err := d.exec(ctx, key, `UPDATE chunks SET valid_at = ?, invalid_at = ? WHERE id = ?`,
			int64Arg(effValid), int64Arg(effInvalid), in.ID); err != nil {
			return errFrom("update_chunk: "+err.Error(), err), nil
		}
	}
	if _, has := present["title"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET title = ? WHERE id = ?`, in.Title, in.ID); err != nil {
			return errFrom("update_chunk: "+err.Error(), err), nil
		}
	}
	if _, has := present["type"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET type = ? WHERE id = ?`, nullIfEmpty(in.Type), in.ID); err != nil {
			return errFrom("update_chunk: "+err.Error(), err), nil
		}
	}
	if _, has := present["status"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET status = ? WHERE id = ?`, nullIfEmpty(in.Status), in.ID); err != nil {
			return errFrom("update_chunk: "+err.Error(), err), nil
		}
	}
	_, hasBody := present["body"]
	_, hasFields := present["fields"]
	if hasBody || hasFields {
		// The type that will be in force AFTER this update decides the embedding
		// policy — a chunk being turned into a mermaid diagram in the same call
		// that sets its body must embed as a diagram, not as prose.
		effType := row.Type
		if _, has := present["type"]; has {
			effType = in.Type
		}
		var cb chunkBody
		if !hasBody || !hasFields {
			// Only one half was supplied, so the other must be preserved from
			// the stored row. writeBody rewrites both, so a failed read here
			// would erase the half the caller did not send — refuse instead of
			// persisting the loss. This is the exact path that blanked a
			// document root under the v1.33.0 tenant regression: a fields-only
			// colour-scheme write, on a chunk whose body had become unreadable.
			stored, rerr := d.readBody(ctx, mscope, key, in.ID)
			if rerr != nil {
				return errFrom("update_chunk: body: "+rerr.Error(), rerr), nil
			}
			cb = stored
		}
		// When the caller supplies BOTH halves there is nothing to preserve, so
		// no read happens and a store fault cannot block a complete write.
		if hasBody {
			cb.Body = in.Body
		}
		if hasFields {
			cb.Fields = in.Fields
		}
		if err := d.writeBody(ctx, mscope, key, in.ID, effType, cb.Body, cb.Fields); err != nil {
			return errFrom("update_chunk: body: "+err.Error(), err), nil
		}
		// Re-derive [[name]] link edges only when the BODY actually changed (RFC BS
		// Phase 2a). A fields-only update preserves the body verbatim, so its links
		// are unchanged — reconciling then would needlessly churn (and, if a linked
		// target's dirent had since moved, wrongly drop a still-valid edge).
		if hasBody {
			// Snapshot the new body at the POST-bump revision (RFC BS Phase 3a).
			// The guarded UPDATE above advanced revision from *in.Revision to
			// *in.Revision+1, so that is the revision this body now carries. A
			// fields-only update skips this branch (its body did not change).
			if err := d.recordRevision(ctx, key, in.ID, *in.Revision+1, cb.Body); err != nil {
				return errFrom("update_chunk: history: "+err.Error(), err), nil
			}
			if err := d.reconcileNameLinks(ctx, key, in.ID, cb.Body); err != nil {
				return errFrom("update_chunk: name links: "+err.Error(), err), nil
			}
		}
	}
	// Replace-set the chunk's tags only when `tags` was supplied — a present `[]`
	// clears, an absent key leaves them untouched (same presence rule as above).
	if _, has := present["tags"]; has {
		if err := d.replaceChunkTags(ctx, key, in.ID, in.Tags); err != nil {
			return errFrom("update_chunk: tags: "+err.Error(), err), nil
		}
	}
	// If this chunk is a document's root and its title/type/status just changed,
	// mirror them onto the documents row (a no-op for a non-root chunk). Renaming
	// the root chunk is how a document is renamed; its Path-tree name stays put.
	_, hasTitle := present["title"]
	_, hasType := present["type"]
	_, hasStatus := present["status"]
	if hasTitle || hasType || hasStatus {
		if err := d.mirrorRootFacets(ctx, key, in.ID); err != nil {
			return errFrom("update_chunk: "+err.Error(), err), nil
		}
	}
	if hasType && in.Type != row.Type && !hasBody && !hasFields && d.Embedder != nil {
		// The type decides the content half of the chunk's index text (a mermaid chunk is
		// indexed under its labels, an image under its description), so a type change
		// with no body write re-indexes the chunk. Only the chunk: descendants' headers
		// carry titles, never a type. A body write above already embedded under the new
		// type.
		d.reindexChunk(ctx, direntTenant(ctx), key, mscope, in.ID, in.Type)
	}
	if _, has := present["title"]; has && in.Title != row.Title {
		// A title is part of the index text of this chunk and of every chunk beneath it
		// (their headers), so a rename re-indexes the subtree. A root rename is a
		// document rename, and re-indexes every chunk in the document.
		//
		// Only a CHANGED title: models echo every field back, and an unchanged title on
		// a root re-embedded the whole document on a status-only update.
		d.reindexSubtree(ctx, key, mscope, row.DocumentID, in.ID)
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), row.DocumentID, "update_chunk", in.ID)
	res, err := d.getChunk(ctx, key, mscope, docInput{ID: in.ID})
	if err != nil {
		return res, err
	}
	return withNotAnnounced(res, d.announceRetype(ctx, key, mscope, retype)), nil
}

func (d *Document) deleteChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("delete_chunk: missing required field: id", "Pass `id`, the chunk to delete."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("delete_chunk: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("delete_chunk: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	// Refuse to delete a document's ROOT chunk — that would orphan the
	// documents row (root_chunk_id dangling, zero chunks). Use delete_document.
	// Fail CLOSED if the lookup errors: a guard that silently skips on a query
	// fault would let exactly the orphan it protects against slip through.
	if rr, rerr := d.query(ctx, key, `SELECT 1 FROM documents WHERE root_chunk_id = ? LIMIT 1`, in.ID); rerr != nil {
		return errFrom("delete_chunk: "+rerr.Error(), rerr), nil
	} else if len(rr.Rows) > 0 {
		return errBusiness("delete_chunk: refusing to delete a document's root chunk — use delete_document", ""), nil
	}
	// The cascade runs in ONE transaction: enumerate the chunk + all
	// descendants (iterative BFS — portable, no recursive CTE; a visited set
	// guarantees termination even on a corrupt parent cycle), then delete each
	// node's edges (both directions) + row. Any failure rolls back the whole
	// subtree — never a half-deleted graph.
	var ids []string
	txErr := d.withSqlTxn(ctx, key, func(txnID string) error {
		ids = []string{in.ID}
		visited := map[string]bool{in.ID: true}
		frontier := []string{in.ID}
		for len(frontier) > 0 {
			next := []string{}
			for _, pid := range frontier {
				res, qerr := d.queryTxn(ctx, txnID, `SELECT id FROM chunks WHERE parent_id = ?`, pid)
				if qerr != nil {
					return qerr
				}
				// Fail CLOSED on truncation: if one frontier level has more
				// children than the row cap, the unseen rows would survive a
				// parent delete as orphans. Refuse rather than half-delete the
				// subtree (the txn rolls back). Pathological (cap default 10k
				// siblings) but exactly the orphan-mess we're hardening against.
				if res.Truncated {
					return fmt.Errorf("subtree too wide to cascade safely (a level exceeds the row cap) — delete children in smaller batches first")
				}
				for _, r := range res.Rows {
					cid := asStr(r[0])
					if visited[cid] {
						continue
					}
					visited[cid] = true
					ids = append(ids, cid)
					next = append(next, cid)
				}
			}
			frontier = next
		}
		// BATCHED, and it matters most here of all the cascades. This ran SEVEN
		// statements per chunk over the whole descendant set, inside one transaction
		// — at the cascade's own 10k-per-level cap that is ~70k statements holding a
		// transaction open, and on postgres each is a round trip. Batched it is seven
		// per 500 chunks.
		//
		// The ORDER is unchanged and load-bearing: every dependent row goes before
		// the chunk row it hangs off, so an interrupted cascade cannot leave a
		// sidecar/revision/layout row pointing at a chunk that is gone. The
		// transaction makes that moot on commit, but the order is what keeps the
		// intent readable.
		for _, batch := range batchIDs(ids, graphIDBatch) {
			if len(batch) == 0 {
				continue
			}
			marks := placeholders(len(batch))
			args := make([]any, 0, len(batch))
			for _, cid := range batch {
				args = append(args, cid)
			}
			// chunk_edges is the one that matches on EITHER endpoint, so it takes the
			// id list twice.
			edgeArgs := append(append([]any(nil), args...), args...)
			if err := d.snapshotDerivedQuotes(ctx, txnID, `{col} IN (`+marks+`)`, args...); err != nil {
				return err
			}
			if err := d.expireUnsupportedRelations(ctx, key, txnID, "", `{col} IN (`+marks+`)`, args...); err != nil {
				return err
			}
			if err := d.deleteEdgesWhere(ctx, key, txnID,
				`from_id IN (`+marks+`) OR to_id IN (`+marks+`)`,
				edgeArgs...); err != nil {
				return err
			}
			// The sidecar's SECOND cascade site. Both are required: delete_document
			// walks by document, this walks the descendant set of one chunk, and a
			// sidecar row reachable only through the path that was missed is an orphan
			// nothing can see. chunk_revisions is the body-change log (RFC BS Phase
			// 3a) and chunk_layout the canvas row — both orphan the same way.
			for _, tbl := range []string{"chunk_assets", "chunk_tags", "chunk_memory_meta",
				"chunk_revisions", "chunk_layout"} {
				if err := d.execTxn(ctx, txnID,
					`DELETE FROM `+tbl+` WHERE chunk_id IN (`+marks+`)`, args...); err != nil {
					return err
				}
			}
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunks WHERE id IN (`+marks+`)`, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return errFrom("delete_chunk: "+txErr.Error(), txErr), nil
	}
	// Bodies after commit (best-effort; see delete_document).
	for _, cid := range ids {
		_, _ = d.Store.MemoryDelete(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), chunkBodyKey(cid))
		d.deleteUnitsOf(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), cid)
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), row.DocumentID, "delete_chunk", in.ID)
	return jsonResult(map[string]any{"deleted": true, "cascade_deleted_descendants": len(ids) - 1})
}

func (d *Document) moveChunk(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("move_chunk: missing required field: id", "Pass `id`, the chunk to move."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("move_chunk: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("move_chunk: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	// An empty new_parent_id moves the chunk under the document's root, for the
	// same reason create_chunk parents to it: a parentless chunk sits BESIDE the
	// root, not in the document. The root itself stays parentless — it is the
	// top of the tree.
	if in.NewParentID == "" {
		root, rerr := d.documentRootChunk(ctx, key, row.DocumentID)
		if rerr != nil {
			return errFrom("move_chunk: "+rerr.Error(), rerr), nil
		}
		if root != in.ID {
			in.NewParentID = root
		}
	}
	// Reject moving a chunk under itself or one of its own descendants — that
	// would create a parent_id cycle (and a cycle makes delete_chunk's
	// descendant walk non-terminating). Walk UP from the new parent to the
	// root; if we reach the chunk being moved, it's a cycle.
	if in.NewParentID != "" {
		if in.NewParentID == in.ID {
			return errValidation("move_chunk: cannot move a chunk under itself", "Pass a different new_parent_id, or omit it to move the chunk under the document's root."), nil
		}
		// The new parent must EXIST, and must live in the same document.
		//
		// Neither was checked, and both failures reported ok:true. A missing parent
		// silently orphaned the chunk: the row survives but nothing walks to it from
		// the root, so it vanishes from get_document and export_md while still
		// occupying the scope — invisible loss, which is worse than an error. And the
		// dead-link sweeper does not catch this class: it reports a chunk whose
		// DOCUMENT is gone, not one whose PARENT is.
		//
		// A cross-document parent is the same corruption from the other side: the
		// chunk keeps its own document_id while its parent_id points into another
		// tree, so the two documents disagree about who owns it.
		//
		// An empty new_parent_id (moving under the document's root) is resolved to
		// the root above, so it is checked here like any other parent.
		newParent, found, perr := d.getChunkRow(ctx, key, in.NewParentID)
		if perr != nil {
			return errFrom("move_chunk: new parent lookup: "+perr.Error(), perr), nil
		}
		if !found {
			return errNotFound("move_chunk: no such new_parent_id: "+in.NewParentID+
				" (moving under a parent that does not exist would orphan the chunk; "+
				"pass the document's root_chunk_id to move it under the document's title)", ""), nil
		}
		if newParent.DocumentID != row.DocumentID {
			return errValidation(fmt.Sprintf(
				"move_chunk: new_parent_id %q belongs to document %q, but this chunk is in %q — "+
					"a chunk cannot be parented across documents",
				in.NewParentID, newParent.DocumentID, row.DocumentID), "Pass a new_parent_id from this chunk's own document."), nil
		}
		cur := in.NewParentID
		for i := 0; cur != "" && i <= maxChunkDepth; i++ {
			anc, found, aerr := d.getChunkRow(ctx, key, cur)
			if aerr != nil {
				return errFrom("move_chunk: "+aerr.Error(), aerr), nil
			}
			if !found {
				break
			}
			if anc.ID == in.ID {
				return errBusiness("move_chunk: cannot move a chunk into its own subtree (would create a cycle)", "Pass a new_parent_id outside this chunk's own subtree."), nil
			}
			cur = anc.ParentID
		}
	}
	pos := 0
	if in.Position != nil {
		pos = *in.Position
	}
	now := time.Now().UnixNano()
	if err := d.exec(ctx, key, `UPDATE chunks SET parent_id = ?, position = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(in.NewParentID), pos, now, in.ID); err != nil {
		return errFrom("move_chunk: "+err.Error(), err), nil
	}
	// Moving to a new parent changes the heading path of the chunk and everything under
	// it. A reorder under the same parent changes only the position, which no index text
	// carries, so it re-indexes nothing.
	if in.NewParentID != row.ParentID {
		d.reindexSubtree(ctx, key, store.MemoryScope(key.Scope), row.DocumentID, in.ID)
	}
	d.publishChange(ctx, store.MemoryScope(key.Scope), docMemScopeID(store.MemoryScope(key.Scope), key), row.DocumentID, "move_chunk", in.ID)
	return jsonResult(map[string]any{"ok": true, "id": in.ID, "new_parent_id": in.NewParentID, "position": pos})
}

// reorderChunk moves a chunk one step up or down among its same-parent siblings
// (RFC BP). It loads the siblings in canonical (position, id) order, swaps the
// target with its neighbor in `direction`, and RENUMBERS the whole sibling list
// to contiguous 0..n-1 in one transaction — which also self-heals any
// pre-existing position ties/gaps. A boundary (already first/last) or a sole
// child is a no-op success. Never changes parentage; safe on the root (a root is
// just a sibling among any other root-level chunks).
func (d *Document) reorderChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("reorder_chunk: missing required field: id", "Pass `id`, the chunk to reorder."), nil
	}
	if in.Direction != "up" && in.Direction != "down" {
		return errValidation(`reorder_chunk: direction must be "up" or "down"`, ""), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom("reorder_chunk: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound("reorder_chunk: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	// Load same-parent siblings in canonical order (the id tiebreaker makes the
	// order deterministic even if positions currently tie). NULL-branch for the
	// portable parent comparison.
	var sres *sqlmem.QueryResult
	if row.ParentID == "" {
		sres, err = d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND parent_id IS NULL ORDER BY position, id`, row.DocumentID)
	} else {
		sres, err = d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND parent_id = ? ORDER BY position, id`, row.DocumentID, row.ParentID)
	}
	if err != nil {
		return errFrom("reorder_chunk: "+err.Error(), err), nil
	}
	ids := make([]string, 0, len(sres.Rows))
	for _, r := range sres.Rows {
		ids = append(ids, asStr(r[0]))
	}
	idx := -1
	for i, id := range ids {
		if id == in.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errTransient("reorder_chunk: chunk not found among its siblings", "The chunk moved or was deleted while this ran; read it with op=get_chunk, then try again."), nil
	}
	swap := idx - 1
	if in.Direction == "down" {
		swap = idx + 1
	}
	if swap < 0 || swap >= len(ids) {
		// Already at the boundary — nothing to do.
		return jsonResult(map[string]any{"reordered": false, "id": in.ID})
	}
	ids[idx], ids[swap] = ids[swap], ids[idx]
	// Renumber ALL siblings to contiguous 0..n-1 in the new order (heals ties/
	// gaps) — atomically.
	now := time.Now().UnixNano()
	if err := d.withSqlTxn(ctx, key, func(txnID string) error {
		for i, id := range ids {
			if err := d.execTxn(ctx, txnID, `UPDATE chunks SET position = ?, updated_at = ? WHERE id = ?`, i, now, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return errFrom("reorder_chunk: "+err.Error(), err), nil
	}
	d.publishChange(ctx, mscope, docMemScopeID(mscope, key), row.DocumentID, "reorder_chunk", in.ID)
	return jsonResult(map[string]any{"reordered": true, "id": in.ID, "direction": in.Direction})
}

// --- ops: edges ---

// edgeEndpointFields are the per-endpoint columns get_edges surfaces (present
// only when non-empty) — the endpoint's title/type/status/document_id, so the UI
// can render a References list + a relationship graph without a follow-up
// get_chunk per endpoint.
var edgeEndpointFields = []string{
	"from_title", "from_type", "from_status", "from_document_id",
	"to_title", "to_type", "to_status", "to_document_id",
}

// getEdges returns the statements with an endpoint in the document (outgoing,
// incoming or both), each with its envelope (RFC FN), both endpoints'
// title/type/status/document_id, and up to three evidence rows. Powers the
// viewer's Connections panel and the relationship graph in one call. LEFT JOINs
// so a (defensively) dangling endpoint still lists with empty fields. `auto` is
// kept (true = a parser-generated [[name]] edge).
//
// Proposed statements are included by default — the panel is where the review
// queue shows — while rejected, refuted and retired ones are withheld unless asked
// for. Paged: limit (default 500, max 5000) and after = next_cursor.
func (d *Document) getEdges(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.DocumentID == "" {
		return errValidation("get_edges: missing required field: document_id", "Pass `document_id`, from query_documents or create_document."), nil
	}
	var dirCond string
	var args []any
	switch in.Direction {
	case "", "both":
		dirCond = "(e.from_id IN (SELECT id FROM chunks WHERE document_id = ?) OR e.to_id IN (SELECT id FROM chunks WHERE document_id = ?))"
		args = []any{in.DocumentID, in.DocumentID}
	case "out":
		dirCond = "e.from_id IN (SELECT id FROM chunks WHERE document_id = ?)"
		args = []any{in.DocumentID}
	case "in":
		dirCond = "e.to_id IN (SELECT id FROM chunks WHERE document_id = ?)"
		args = []any{in.DocumentID}
	default:
		return errValidation("get_edges: direction must be out, in or both", ""), nil
	}
	includeProposed := in.IncludeProposed == nil || *in.IncludeProposed
	return d.listEdges(ctx, key, in, "get_edges", "edges", dirCond, args, includeProposed)
}

// listEdges is get_edges and backlinks: the statements matching base, filtered by
// kind, visibility and validity, paged by (kind, created_at, id).
//
// A caller whose access-control set hides some document of the scope has the
// rows touching it withheld here, before the page is cut, refilling from past
// them until the page is full: dropped after LIMIT they would shorten the page
// and leave truncated / next_cursor standing on rows the caller cannot see,
// telling it hidden statements exist. The page then answers exactly as if
// those rows did not. A caller the set limits nothing keeps the one query.
func (d *Document) listEdges(ctx context.Context, key sqlmem.ScopeKey, in docInput, op, field, base string, baseArgs []any, includeProposed bool) (tools.Result, error) {
	limit := edgeGetEdgesDefault
	if in.Limit > 0 {
		limit = min(in.Limit, edgeGetEdgesMax)
	}
	where := []string{base}
	args := append([]any{}, baseArgs...)
	kindCond, kindArgs, expandedTo := d.kindFilter(ctx, in)
	if kindCond != "" {
		where = append(where, kindCond)
		args = append(args, kindArgs...)
	}
	vis, visArgs := statementVisibility{
		includeProposed: includeProposed, includeRefuted: in.IncludeRefuted,
		includeRetired: in.IncludeRetired, asOf: in.AsOf,
	}.clause()
	where = append(where, vis)
	args = append(args, visArgs...)
	var after *edgeCursor
	if in.After != "" {
		c, ok := decodeEdgeCursor(in.After)
		if !ok {
			return errValidation(op+": after is not a cursor this op returned", "Pass next_cursor from the previous page."), nil
		}
		after = &c
	}
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	batch := limit + 1
	if limited {
		// A floor, so a small page among many hidden rows is not a query per row.
		batch = max(limit, edgeGetEdgesDefault) + 1
	}
	readable := map[string]bool{}
	visible := func(r []any) (bool, error) {
		// from_document_id and to_document_id; an end whose chunk is gone has no
		// document and stays visible, as the listing filter has it.
		for _, docID := range []string{asStr(r[statementColCount+3]), asStr(r[statementColCount+7])} {
			if docID == "" {
				continue
			}
			ok, seen := readable[docID]
			if !seen {
				var rerr error
				if ok, rerr = acl.readable(ctx, docID); rerr != nil {
					return false, rerr
				}
				readable[docID] = ok
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	var rows [][]any
	for {
		w := append([]string{}, where...)
		a := append([]any{}, args...)
		if after != nil {
			w = append(w, "(e.kind > ? OR (e.kind = ? AND (e.created_at > ? OR (e.created_at = ? AND e.id > ?))))")
			a = append(a, after.Kind, after.Kind, after.CreatedAt, after.CreatedAt, after.ID)
		}
		a = append(a, batch)
		res, err := d.query(ctx, key, `SELECT `+statementCols+`,
       cf.title, cf.type, cf.status, cf.document_id,
       ct.title, ct.type, ct.status, ct.document_id
FROM chunk_edges e
LEFT JOIN chunks cf ON cf.id = e.from_id
LEFT JOIN chunks ct ON ct.id = e.to_id
WHERE `+strings.Join(w, " AND ")+`
ORDER BY e.kind, e.created_at, e.id
LIMIT ?`, a...)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		if !limited {
			rows = res.Rows
			break
		}
		for _, r := range res.Rows {
			ok, err := visible(r)
			if err != nil {
				return errFrom(op+": "+err.Error(), err), nil
			}
			if !ok {
				continue
			}
			rows = append(rows, r)
			if len(rows) > limit {
				break
			}
		}
		// The store's row cap can cut a batch short; only an uncut short batch
		// is the end.
		if len(rows) > limit || len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < batch) {
			break
		}
		last := scanStatement(res.Rows[len(res.Rows)-1])
		after = &edgeCursor{Kind: last.Kind, CreatedAt: last.CreatedAt, ID: last.ID}
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	stmts := make([]statementRow, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		stmts[i] = scanStatement(r)
		ids[i] = stmts[i].ID
	}
	ev, counts, err := d.readableEvidenceFor(ctx, key, ids, edgeEvidencePreview)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	edges := make([]map[string]any, 0, len(rows))
	for i, r := range rows {
		ends := map[string]string{}
		for j, f := range edgeEndpointFields {
			ends[f] = asStr(r[statementColCount+j])
		}
		if field == "backlinks" {
			for _, f := range []string{"to_title", "to_type", "to_status", "to_document_id"} {
				delete(ends, f)
			}
		}
		edges = append(edges, d.statementJSON(ctx, stmts[i], ends, ev[stmts[i].ID], counts[stmts[i].ID], field == "backlinks"))
	}
	out := map[string]any{field: edges}
	if len(expandedTo) > 0 {
		out["kind_expanded_to"] = expandedTo
	}
	if truncated && len(stmts) > 0 {
		last := stmts[len(stmts)-1]
		out["truncated"] = true
		out["next_cursor"] = encodeEdgeCursor(edgeCursor{Kind: last.Kind, CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return jsonResult(out)
}

// withholdUnreadableEvidence drops evidence rows citing a chunk of a document the
// caller's access-control set may not read: a statement may be visible while one
// of its quotes comes from somewhere the caller cannot go.
func (d *Document) withholdUnreadableEvidence(ctx context.Context, key sqlmem.ScopeKey, ev map[string][]edgeEvidenceRow) (map[string][]edgeEvidenceRow, error) {
	acl := d.newDocACL(ctx, key)
	if limits, err := acl.limitsReading(ctx); err != nil || !limits {
		return ev, err
	}
	readable := map[string]bool{}
	for edgeID, rows := range ev {
		kept := rows[:0]
		for _, r := range rows {
			if r.ChunkID == "" {
				kept = append(kept, r)
				continue
			}
			row, ok, err := d.getChunkRow(ctx, key, r.ChunkID)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			ok, seen := readable[row.DocumentID]
			if !seen {
				var rerr error
				if ok, rerr = acl.readable(ctx, row.DocumentID); rerr != nil {
					return nil, rerr
				}
				readable[row.DocumentID] = ok
			}
			if ok {
				kept = append(kept, r)
			}
		}
		ev[edgeID] = kept
	}
	return ev, nil
}

// --- chunk history + backlinks (RFC BS Phase 3a) ---

// chunkHistory lists the revisions at which a chunk's BODY changed, newest
// first — metadata only (revision / created_at / actor), not the bodies. The
// body of any listed revision is fetched with get_version.
func (d *Document) chunkHistory(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("history: missing required field: id", "Pass `id`, the chunk whose revisions to list."), nil
	}
	limit := 100
	if in.Limit > 0 {
		limit = in.Limit
	}
	res, err := d.query(ctx, key,
		`SELECT revision, created_at, actor FROM chunk_revisions WHERE chunk_id = ? ORDER BY revision DESC LIMIT ?`,
		in.ID, limit)
	if err != nil {
		return errFrom("history: "+err.Error(), err), nil
	}
	// Positional read: the SELECT fixes the column order (revision, created_at,
	// actor). created_at is a NOT-NULL unix-nanos BIGINT → asInt64 keeps its full
	// width; the presence bool is irrelevant (the column is never NULL here).
	revs := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		createdAt, _ := asInt64(r[1])
		revs = append(revs, map[string]any{
			"revision":   asInt(r[0]),
			"created_at": createdAt,
			"actor":      asStr(r[2]),
		})
	}
	return jsonResult(map[string]any{"chunk_id": in.ID, "revisions": revs})
}

// revisionBody reads one revision's stored body. ok=false means that
// (chunk_id, revision) pair was never recorded.
func (d *Document) revisionBody(ctx context.Context, key sqlmem.ScopeKey, chunkID string, revision int) (string, bool, error) {
	res, err := d.query(ctx, key, `SELECT body FROM chunk_revisions WHERE chunk_id = ? AND revision = ?`, chunkID, revision)
	if err != nil {
		return "", false, err
	}
	if len(res.Rows) == 0 {
		return "", false, nil
	}
	return asStr(res.Rows[0][0]), true, nil
}

// getVersion returns a single revision's exact body.
func (d *Document) getVersion(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("get_version: missing required field: id", "Pass `id`, the chunk id."), nil
	}
	if in.Revision == nil {
		return errValidation("get_version: missing required field: revision", "Pass `revision`; list a chunk's revisions with op=history."), nil
	}
	body, ok, err := d.revisionBody(ctx, key, in.ID, *in.Revision)
	if err != nil {
		return errFrom("get_version: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound(fmt.Sprintf("get_version: no such revision %d for chunk %s", *in.Revision, in.ID), "List the chunk's revisions with op=history."), nil
	}
	return jsonResult(map[string]any{"chunk_id": in.ID, "revision": *in.Revision, "body": body})
}

// diffVersions returns a unified diff between two revisions' bodies. Either
// missing revision is a clear error rather than a diff against "".
func (d *Document) diffVersions(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("diff: missing required field: id", "Pass `id`, the chunk id."), nil
	}
	if in.FromRevision == nil || in.ToRevision == nil {
		return errValidation("diff: from_revision and to_revision are required", "Pass both; list the chunk's revisions with op=history."), nil
	}
	fromBody, ok, err := d.revisionBody(ctx, key, in.ID, *in.FromRevision)
	if err != nil {
		return errFrom("diff: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound(fmt.Sprintf("diff: no such revision %d for chunk %s", *in.FromRevision, in.ID), "List the chunk's revisions with op=history."), nil
	}
	toBody, ok, err := d.revisionBody(ctx, key, in.ID, *in.ToRevision)
	if err != nil {
		return errFrom("diff: "+err.Error(), err), nil
	}
	if !ok {
		return errNotFound(fmt.Sprintf("diff: no such revision %d for chunk %s", *in.ToRevision, in.ID), "List the chunk's revisions with op=history."), nil
	}
	// go-difflib produces a correct, standard unified diff (LCS-aligned hunks with
	// context). It is preferred over a hand-rolled line differ because a correct
	// unified diff needs LCS alignment + hunk grouping — more code and more bug
	// surface than a battle-tested library that is already in the build (a
	// transitive dep of testify, promoted here to a direct require).
	text, derr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(fromBody),
		B:        difflib.SplitLines(toBody),
		FromFile: fmt.Sprintf("rev %d", *in.FromRevision),
		ToFile:   fmt.Sprintf("rev %d", *in.ToRevision),
		Context:  3,
	})
	if derr != nil {
		return errFrom("diff: "+derr.Error(), derr), nil
	}
	return jsonResult(map[string]any{
		"chunk_id":      in.ID,
		"from_revision": *in.FromRevision,
		"to_revision":   *in.ToRevision,
		"diff":          text,
	})
}

// backlinks returns "what links here": the statements pointing TO the given chunk,
// each with its envelope (RFC FN), its FROM endpoint's title/type/status/document_id,
// the inverse name as seen from this end when the vocabulary declares one, and an
// evidence preview. The reverse direction of get_edges, keyed on a single chunk,
// served by the chunk_edges_to_kind reverse index. Proposed statements are left
// out unless include_proposed is set.
//
// BOUNDED, and it takes `limit` like its siblings. Inbound degree is the one
// direction that concentrates: on a 304-chunk benchmark store the maximum
// in-degree was 162, because a hub entity collects every fact written about it.
func (d *Document) backlinks(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("backlinks: missing required field: id", "Pass `id`, the chunk to find incoming links for."), nil
	}
	if in.Limit <= 0 {
		in.Limit = graphDefaultLimit
	}
	if in.Limit > graphMaxLimit {
		in.Limit = graphMaxLimit
	}
	includeProposed := in.IncludeProposed != nil && *in.IncludeProposed
	return d.listEdges(ctx, key, in, "backlinks", "backlinks", "e.to_id = ?", []any{in.ID}, includeProposed)
}

// --- discovery: related (semantic) + unlinked_mentions (textual) ---

// chunkMeta is the small display tuple related/unlinked_mentions enrich a bare
// chunk id with, so a caller renders a hit without a per-id get_chunk.
type chunkMeta struct {
	title      string
	typ        string
	documentID string
	validAt    *int64
	invalidAt  *int64
}

// chunkMetaByIDs fetches title/type/document_id for a set of chunk ids in ONE
// IN(...) query, keyed by id. Ids the query does not return are simply absent
// from the map (a hit whose structure row vanished still lists its id). An empty
// input is a no-op. Portable `?` placeholders → the postgres tier Rebinds them.
func (d *Document) chunkMetaByIDs(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]chunkMeta, error) {
	out := map[string]chunkMeta{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := d.query(ctx, key,
		"SELECT id, title, type, document_id, valid_at, invalid_at FROM chunks WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	for _, r := range res.Rows {
		out[asStr(r[0])] = chunkMeta{title: asStr(r[1]), typ: asStr(r[2]), documentID: asStr(r[3]),
			validAt: asInt64Ptr(r[4]), invalidAt: asInt64Ptr(r[5])}
	}
	return out, nil
}

// searchChunks finds the chunks whose bodies best match a free-text query — the
// convenience wrapper RFC BU §6 deferred.
//
// WHY IT EARNS ITS PLACE rather than being sugar. Without it the entry point into a
// document is `memory op=search prefix="doc.chunk:"`, which asks a caller to know three
// things that are not its business: that chunk bodies live in the memory keyspace, that
// the reserved prefix is spelled exactly "doc.chunk:", and that the chunk id must be cut
// back off the returned key. A live transcript showed an agent failing at precisely that
// class of thing — it did not know a reserved string, so it guessed, and concluded
// nothing was remembered. This op takes a query and returns chunks.
//
// It also RETURNS MORE than the raw search can: title, type and document_id per hit, so
// a result is navigable without a second round trip. The raw form stays available for a
// caller that wants the memory row itself.
//
// Scope-confined exactly like `related`, whose machinery this reuses — same embedder,
// same prefixed vector search, same enrichment. One mechanism, two entry points.
func (d *Document) searchChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return errValidation("search: missing required field: query (the text to match against chunk bodies)", ""), nil
	}
	if err := validateExpand(in.Expand); err != nil {
		return errValidation("search: "+err.Error(), ""), nil
	}
	if d.Embedder == nil {
		return errBusiness("search: requires a configured embedder / vector memory", "Semantic search is not available on this server; filter with op=query_chunks instead."), nil
	}
	topK := 10
	if in.Limit > 0 {
		topK = in.Limit
	}
	if topK > 50 {
		topK = 50
	}
	// A structural filter is resolved to chunk ids BEFORE the search, so every
	// leg's LIMIT sees only the wanted corpus (document_search_filter.go). Only
	// when one was named: a filter-free call stays on exactly the old path.
	var filter *searchFilterSet
	if hasSearchFilter(in) {
		set, refusal, err := d.resolveSearchFilter(ctx, key, "search", in)
		if err != nil {
			return docFail("search: filter: ", err), nil
		}
		if refusal != nil {
			return *refusal, nil
		}
		// Nothing matched: answer without embedding the query or touching the
		// store, so an unresolvable path costs nothing and bumps no access count.
		if set.echo.MatchedChunks == 0 {
			return jsonResult(map[string]any{"chunks": []map[string]any{}, "filter": set.echo})
		}
		filter = set
	}
	// With no filter, the chunks a confined reader may not read are withheld from
	// the search itself, before it cuts at topK (resolveSearchACL); a filter's
	// own allowlist already holds only readable chunks.
	var aclList *searchACLList
	if filter == nil {
		list, err := d.resolveSearchACL(ctx, key)
		if err != nil {
			return docFail("search: access control: ", err), nil
		}
		// Nothing readable: answer as a filter that matched nothing does, without
		// embedding the query.
		if list != nil && list.none {
			return jsonResult(map[string]any{"chunks": []map[string]any{}})
		}
		aclList = list
	}
	// THE MEMORY SEARCH PIPELINE, NOT A SECOND ONE. This op used to run a vector
	// query of its own, so a chunk whose words matched the question but whose
	// embedding did not — a name, a code, an error string — was unreachable here
	// while `Memory op=search` over the same chunks found it through the full-text
	// leg. Running the same backend is what makes the two agree.
	//
	// A key prefix, not `sources=[documents]`: this op has always returned every
	// chunk body, fact chunks included, and the documents selector excludes the
	// chunks memory's facts are homed in.
	//
	// The agent's memory_rerank applies here as it does to Memory op=search: it is
	// the same search, and an agent whose document searches are reranked should not
	// get a different order depending on which tool it reached them through.
	rerank := rerankOptions(ctx)
	sq := memrank.SearchQuery{
		QueryText: q,
		Prefix:    chunkBodyKeyPrefix,
		TopK:      topK,
		Rerank:    rerank,
		NoUnits:   noUnits(ctx),
	}
	if filter != nil {
		sq.IDs, sq.ExcludeIDs = filter.ids, filter.excludeIDs
	}
	if aclList != nil {
		sq.IDs, sq.ExcludeIDs = aclList.ids, aclList.excludeIDs
	}
	res, err := d.searchBackend().Search(ctx, mscope, docMemScopeID(mscope, key), sq, memrank.DefaultRankConfig(), memrank.DedupConfig{})
	if err != nil {
		return errFrom("search: "+err.Error(), err), nil
	}
	ids := make([]string, 0, len(res.Entries))
	scores := make(map[string]float64, len(res.Entries))
	rankScores := make(map[string]float64, len(res.Entries))
	matched := make(map[string]*memrank.MatchedUnit)
	for i, e := range res.Entries {
		cid := ChunkIDFromBodyKey(e.Key)
		if cid == "" {
			continue
		}
		ids = append(ids, cid)
		scores[cid] = e.Score
		if i < len(res.RankScores) {
			rankScores[cid] = res.RankScores[i]
		}
		if i < len(res.MatchedUnits) && res.MatchedUnits[i] != nil {
			matched[cid] = res.MatchedUnits[i]
		}
	}
	meta, err := d.chunkMetaByIDs(ctx, key, ids)
	if err != nil {
		return errFrom("search: enrich: "+err.Error(), err), nil
	}
	out := make([]map[string]any, 0, len(ids))
	for _, cid := range ids {
		// score is the raw cosine and rank_score what the order used — they differ
		// once the full-text leg contributes, and a chunk found by its words alone
		// has a low score and a high rank_score.
		m := map[string]any{"chunk_id": cid, "score": scores[cid], "rank_score": rankScores[cid]}
		if md, ok := meta[cid]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.typ != "" {
				m["type"] = md.typ
			}
			if md.documentID != "" {
				m["document_id"] = md.documentID
			}
			putValidity(m, md.validAt, md.invalidAt)
		}
		// Found through one of its derived search units: which one, and so why.
		if mu, ok := matched[cid]; ok {
			m["matched_unit"] = mu
		}
		out = append(out, m)
	}
	// In the backend's order, and the enrichment loop preserves it — a caller
	// reading the first element must get the best match, not whatever order the
	// metadata query returned. Expanded rows follow the ranked ones, unranked and
	// marked, in the same list so the access-control filter reads them too.
	expanded, expandTruncated, err := d.expandHits(ctx, key, in, out, filter)
	if err != nil {
		return errFrom("search: expand: "+err.Error(), err), nil
	}
	out = append(out, expanded...)
	result := map[string]any{"chunks": out}
	if in.Expand != nil {
		result["expanded"] = len(expanded)
		if expandTruncated {
			result["expand_truncated"] = true
		}
	}
	renderRerank(result, rerank, res.Rerank)
	if filter != nil {
		result["filter"] = filter.echo
	}
	return jsonResult(result)
}

// searchBackend is the backend op=search runs on: the wired one, else an
// in-process backend over this tool's store and embedder.
func (d *Document) searchBackend() memrank.Backend {
	if d.Backend != nil {
		return d.Backend
	}
	return inprocess.New(d.Store, d.Embedder)
}

// related returns the chunks whose bodies embed CLOSEST to a chunk's — its
// semantic neighbours, ranked by cosine score (RFC BS Phase 3b). It reuses the
// per-chunk-body embeddings writeBody already indexes on write (keyed
// "doc.chunk:<id>" in the k/v Memory plane, under the chunk's scope + the raw
// direntTenant), so nothing about the write path changes. Scope-confined: the
// vector search is bounded to the caller's own tenant/scope/scope_id.
func (d *Document) related(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" && in.RelatedTo == "" {
		return errValidation("related: missing required field: id", "Pass `id`, the chunk to find neighbours of (by meaning), or `related_to` (by relation)."), nil
	}
	// Semantic neighbours need both a vector index and something to turn the query
	// body into a vector. No embedder → there is no vector plane to search, and
	// without related_to nothing else to answer with.
	const noEmbedderHint = "Semantic neighbours are not available on this server; use op=backlinks or op=query_chunks instead."
	if d.Embedder == nil && in.RelatedTo == "" {
		return errBusiness("related: requires a configured embedder / vector memory", noEmbedderHint), nil
	}
	// The same structural filter as search, resolved once and applied to both
	// halves: the vector leg and the neighbours by relation. withFilter adds the
	// echo, and the relation half, to every answer.
	var filter *searchFilterSet
	if hasSearchFilter(in) {
		set, refusal, err := d.resolveSearchFilter(ctx, key, "related", in)
		if err != nil {
			return docFail("related: filter: ", err), nil
		}
		if refusal != nil {
			return *refusal, nil
		}
		filter = set
	}
	var linked []map[string]any
	withFilter := func(m map[string]any) map[string]any {
		if filter != nil {
			m["filter"] = filter.echo
		}
		if linked != nil {
			m["linked"] = linked
		}
		return m
	}
	// related_to: the neighbours by relation, beside (or instead of) the ones by
	// meaning. Needs no embedder.
	if in.RelatedTo != "" {
		limit := 10
		if in.Limit > 0 {
			limit = min(in.Limit, 50)
		}
		var err error
		if linked, err = d.relatedByRelation(ctx, key, in, limit, filter); err != nil {
			return errFrom("related: related_to: "+err.Error(), err), nil
		}
		if in.ID == "" {
			return jsonResult(withFilter(map[string]any{}))
		}
		// Both were asked for and only the relation half can be answered: give it,
		// and say why the other is missing, rather than throw it away.
		if d.Embedder == nil {
			return jsonResult(withFilter(map[string]any{"note": "related: no embedder is configured, so only the neighbours by relation (linked) are returned. " + noEmbedderHint}))
		}
	}
	topK := 10
	if in.Limit > 0 {
		topK = in.Limit
	}
	if topK > 50 {
		topK = 50
	}
	sf := store.MemorySearchFilter{KeyPrefix: chunkBodyKeyPrefix}
	if filter != nil {
		if filter.echo.MatchedChunks == 0 {
			return jsonResult(withFilter(map[string]any{"related": []map[string]any{}}))
		}
		sf.IDs, sf.ExcludeIDs = filter.ids, filter.excludeIDs
	}
	// The query text is the chunk's own body. An empty or media body has nothing
	// to embed — the same predicate embedBody skips on write — so it has no
	// neighbours; return an empty set rather than an error (a section/parent chunk
	// legitimately has no body).
	cb, err := d.readBody(ctx, mscope, key, in.ID)
	if err != nil {
		return errFrom("related: body: "+err.Error(), err), nil
	}
	body := strings.TrimSpace(cb.Body)
	if body == "" {
		return jsonResult(withFilter(map[string]any{"related": []map[string]any{}}))
	}
	if typ, _, _, _ := classifyMediaBody(cb.Body); typ != "" {
		return jsonResult(withFilter(map[string]any{"related": []map[string]any{}}))
	}
	vec, err := d.Embedder.Embed(ctx, []string{body})
	if err != nil {
		return errFrom("related: embed: "+err.Error(), err), nil
	}
	if len(vec) == 0 {
		return errTransient("related: embed: embedder returned no vector", "Try again; if it keeps failing, use op=backlinks or op=query_chunks instead."), nil
	}
	// topK+1: a chunk is its own nearest neighbour, so the search ranks self first;
	// ask for one extra to leave room to drop it and still return topK others.
	entries, err := d.Store.MemoryEmbedSearch(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), sf, vec[0], topK+1)
	if err != nil {
		return errFrom("related: search: "+err.Error(), err), nil
	}
	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(entries))
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		cid := strings.TrimPrefix(e.Key, "doc.chunk:")
		if cid == in.ID {
			continue // the query chunk itself is never its own neighbour
		}
		hits = append(hits, hit{id: cid, score: e.Score})
		ids = append(ids, cid)
		if len(hits) >= topK {
			break
		}
	}
	meta, err := d.chunkMetaByIDs(ctx, key, ids)
	if err != nil {
		return errFrom("related: enrich: "+err.Error(), err), nil
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		m := map[string]any{"chunk_id": h.id, "score": h.score}
		if md, ok := meta[h.id]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.typ != "" {
				m["type"] = md.typ
			}
			if md.documentID != "" {
				m["document_id"] = md.documentID
			}
			putValidity(m, md.validAt, md.invalidAt)
		}
		out = append(out, m)
	}
	return jsonResult(withFilter(map[string]any{"related": out}))
}

// unlinkedMentionScanCap bounds the body scan unlinked_mentions performs. The op
// is an O(scope-chunk-count) scan of every chunk body in the scope (it reads text
// SQL cannot reach), so the cap keeps one very large scope from turning a single
// call into an unbounded read. A document- or path-bounded variant is a follow-up.
const unlinkedMentionScanCap = 5000

// unlinkedMentions finds the chunks whose body text MENTIONS a target chunk's
// title but do NOT already link to it (RFC BS Phase 3b) — the "you wrote the name
// but never made it a link" surface. Both a manual link_chunks edge and an inline
// [[name]] parser edge count as "already linked" (they both land in chunk_edges),
// and the target's own body mentioning its own title is excluded. Scope-confined.
func (d *Document) unlinkedMentions(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errValidation("unlinked_mentions: missing required field: id", "Pass `id`, the chunk whose title to look for."), nil
	}
	limit := 50
	if in.Limit > 0 {
		limit = in.Limit
	}
	if limit > 200 {
		limit = 200
	}
	// Resolve the target's title — the text to look for in other bodies.
	tres, err := d.query(ctx, key, `SELECT title FROM chunks WHERE id = ? LIMIT 1`, in.ID)
	if err != nil {
		return errFrom("unlinked_mentions: "+err.Error(), err), nil
	}
	if len(tres.Rows) == 0 {
		return errNotFound("unlinked_mentions: no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
	}
	title := strings.TrimSpace(asStr(tres.Rows[0][0]))
	if title == "" {
		return errBusiness("unlinked_mentions: target chunk has no title to match on", "Give the chunk a title with op=update_chunk, then try again."), nil
	}
	// The already-linked set: every chunk that already references the target
	// (manual OR [[name]] edge — both live in chunk_edges as from→to). A mention
	// from a chunk that already links is not "unlinked". Every status counts on
	// purpose: a proposal is already awaiting review, and a rejected or refuted
	// relation was considered and refused — suggesting it again invites a link
	// that would be refused, or, from an operator, would undo the ruling.
	linked := map[string]bool{}
	lres, err := d.query(ctx, key, `SELECT from_id FROM chunk_edges WHERE to_id = ?`, in.ID)
	if err != nil {
		return errFrom("unlinked_mentions: edges: "+err.Error(), err), nil
	}
	for _, r := range lres.Rows {
		linked[asStr(r[0])] = true
	}
	// Scan chunk bodies from the k/v Memory plane, keyed on the SAME
	// tenant/scope/scope_id writeBody wrote them under (direntTenant + mscope +
	// docMemScopeID) — reading a different plane would silently return zero hits.
	entries, listTruncated, err := d.Store.MemoryList(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), "doc.chunk:", unlinkedMentionScanCap)
	if err != nil {
		return errFrom("unlinked_mentions: scan: "+err.Error(), err), nil
	}
	needle := strings.ToLower(title)
	matchIDs := make([]string, 0, limit)
	hitCap := false
	for _, e := range entries {
		cid := strings.TrimPrefix(e.Key, "doc.chunk:")
		if cid == in.ID || linked[cid] {
			continue // self / already-linked
		}
		var cb chunkBody
		if json.Unmarshal(e.Value, &cb) != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(cb.Body), needle) {
			continue
		}
		matchIDs = append(matchIDs, cid)
		if len(matchIDs) >= limit {
			hitCap = true
			break
		}
	}
	meta, err := d.chunkMetaByIDs(ctx, key, matchIDs)
	if err != nil {
		return errFrom("unlinked_mentions: enrich: "+err.Error(), err), nil
	}
	out := make([]map[string]any, 0, len(matchIDs))
	for _, cid := range matchIDs {
		m := map[string]any{"chunk_id": cid}
		if md, ok := meta[cid]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.documentID != "" {
				m["document_id"] = md.documentID
			}
		}
		out = append(out, m)
	}
	// Never silently under-report the scan's completeness: it is bounded three
	// ways — the store had more bodies than the cap (listTruncated), we filled the
	// result limit and stopped early (hitCap), or the scan itself hit its own cap.
	truncated := listTruncated || hitCap || len(entries) >= unlinkedMentionScanCap
	return jsonResult(map[string]any{"unlinked_mentions": out, "truncated": truncated})
}

// --- inline [[name]] links → typed edges (RFC BS Phase 2a) ---

// nameLinkRe matches a wiki-style name-link `[[target]]`; the inner target holds
// no brackets. Compiled once at package scope. Go's regexp has no lookbehind, so
// an `![[…]]` EMBED (a transclusion — a SEPARATE later step, NOT handled here) is
// rejected by inspecting the byte before the match, not in the pattern.
var nameLinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// parseNameLinks returns the distinct inner targets of every `[[target]]` in the
// body, skipping `![[…]]` embeds. A `|display` alias suffix is dropped
// (`[[target|shown text]]` links to `target`). Order is first-seen; deduped.
func parseNameLinks(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range nameLinkRe.FindAllStringSubmatchIndex(body, -1) {
		// m = [matchStart, matchEnd, groupStart, groupEnd].
		if m[0] > 0 && body[m[0]-1] == '!' {
			continue // `![[…]]` is an embed, not a link
		}
		target := body[m[2]:m[3]]
		if i := strings.IndexByte(target, '|'); i >= 0 {
			target = target[:i] // drop the |display alias
		}
		target = strings.TrimSpace(target)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

// resolveLinkTarget maps a `[[target]]` inner string to a chunk id, CONFINED to
// the writer's own scope/tenant (a name-link never resolves across scopes).
//
//   - A leading '/' is a Path: resolve the dirent to a document and return its
//     ROOT chunk. A `#anchor` chunk-anchor suffix is out of scope for this step —
//     the path part resolves to the document root; any `#…` is ignored.
//   - Otherwise it is a title: an exact-match document title (→ its root chunk),
//     else an exact-match chunk title. Oldest wins on a tie (ORDER BY created_at)
//     so resolution is stable.
//
// ok=false (nothing matched) → the caller writes NO edge and the `[[…]]` stays
// literal text. NOTE: a link to a not-yet-created target is not re-resolved when
// that target is later created — a documented follow-up for a later step.
func (d *Document) resolveLinkTarget(ctx context.Context, key sqlmem.ScopeKey, target string) (string, bool) {
	if strings.HasPrefix(target, "/") {
		pathPart := target
		if i := strings.IndexByte(pathPart, '#'); i >= 0 {
			pathPart = pathPart[:i] // a chunk anchor is out of scope; resolve the path
		}
		// docIDFromInput does the normalize + dirent lookup in this scope; any
		// error (bad path, no such dirent, not a document) means unresolved.
		docID, err := d.docIDFromInput(ctx, key, docInput{Path: pathPart})
		if err != nil || docID == "" {
			return "", false
		}
		res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
		if err != nil || len(res.Rows) == 0 {
			return "", false
		}
		if rc := asStr(res.Rows[0][0]); rc != "" {
			return rc, true
		}
		return "", false
	}
	// Title: prefer a document with this exact title (→ its root chunk).
	if res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE title = ? ORDER BY created_at LIMIT 1`, target); err == nil && len(res.Rows) > 0 {
		if rc := asStr(res.Rows[0][0]); rc != "" {
			return rc, true
		}
	}
	// Else a chunk with this exact title.
	if res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE title = ? ORDER BY created_at LIMIT 1`, target); err == nil && len(res.Rows) > 0 {
		if id := asStr(res.Rows[0][0]); id != "" {
			return id, true
		}
	}
	return "", false
}

// resolveEmbedTarget maps a `![[target]]` embed's inner string to the chunk id
// whose body is transcluded, CONFINED to the writer's own scope/tenant. It is a
// superset of resolveLinkTarget (the Phase-2a link resolver): an embed also
// accepts an exact chunk id and a `/path#Section` anchor, so a document can
// transclude ONE named chunk — not only a whole document's root. RFC BS Phase 2b.
//
// Resolution order:
//   - an exact chunk id (`![[<chunk-uuid>]]` embeds that chunk directly);
//   - a `/path#Section` anchor → resolve /path to a document, then its chunk
//     titled `Section` (lowest position wins on a title tie). An anchor that
//     names no such chunk stays UNRESOLVED — it does not fall back to the
//     document root, because the author asked for a specific section;
//   - otherwise resolveLinkTarget (path→root chunk, or an exact title).
//
// ok=false → the caller leaves the `![[…]]` literal.
func (d *Document) resolveEmbedTarget(ctx context.Context, key sqlmem.ScopeKey, target string) (string, bool) {
	// An exact chunk id embeds that one chunk directly. Checked first so a
	// uuid-shaped target is never mistaken for a title.
	if res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE id = ? LIMIT 1`, target); err == nil && len(res.Rows) > 0 {
		if id := asStr(res.Rows[0][0]); id != "" {
			return id, true
		}
	}
	// A `/path#Section` anchor → the named chunk within the path's document.
	if strings.HasPrefix(target, "/") {
		if i := strings.IndexByte(target, '#'); i >= 0 {
			pathPart, anchor := target[:i], strings.TrimSpace(target[i+1:])
			if anchor != "" {
				docID, err := d.docIDFromInput(ctx, key, docInput{Path: pathPart})
				if err == nil && docID != "" {
					if res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND title = ? ORDER BY position LIMIT 1`, docID, anchor); err == nil && len(res.Rows) > 0 {
						if id := asStr(res.Rows[0][0]); id != "" {
							return id, true
						}
					}
				}
				// A `#Section` that matched nothing stays literal (no root fallback).
				return "", false
			}
		}
	}
	return d.resolveLinkTarget(ctx, key, target)
}

// reconcileNameLinks re-derives fromChunkID's inline `[[name]]` link edges from
// its body on every body write (create_chunk / a body-bearing update_chunk):
// each resolvable target becomes a `references` edge tagged auto=1. This is the
// bridge from prose to graph — the Path dirent tree supplies the names, the
// existing chunk_edges table holds the edges.
//
// Idempotent by construction: it DELETEs this chunk's prior auto=1 edges then
// re-inserts the current set, so removing a `[[…]]` from the body drops its edge
// on the next write. A MANUAL edge (auto=0, from link_chunks) is never touched —
// not deleted here, and the guarded insert will not duplicate or overwrite a
// pre-existing manual `references` edge. Unresolved links and a self-link are
// dropped. Edges bind to the RESOLVED chunk id (stable), so moving/renaming the
// target's Path dirent later does not break an already-materialized edge.
//
// Non-transactional delete-then-insert, mirroring replaceChunkTags: name-links
// are an advisory derived facet, not a correctness gate.
func (d *Document) reconcileNameLinks(ctx context.Context, key sqlmem.ScopeKey, fromChunkID, body string) error {
	targets := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, name := range parseNameLinks(body) {
		to, ok := d.resolveLinkTarget(ctx, key, name)
		if !ok || to == fromChunkID || seen[to] {
			continue // unresolved, self-link, or already collected
		}
		seen[to] = true
		targets = append(targets, to)
	}
	// Clear this chunk's prior parser edges (auto=1 only — manual edges survive).
	if err := d.deleteEdgesWhere(ctx, key, "", `from_id = ? AND auto = 1`, fromChunkID); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	// A link to a SUBJECT (an entity node) is a mention of it, not a citation of a
	// passage (RFC FN P3b): it is what "where is this subject mentioned" reads, and
	// it is never walked by default. It carries the bracketed text and its span.
	subjects := d.subjectNodes(ctx, key, targets)
	for _, to := range targets {
		kind := "references"
		if subjects[to] {
			kind = "mentions"
		}
		// Guarded so a pre-existing MANUAL edge of the kind (auto=0) is neither
		// duplicated nor overwritten (the portable existence-check from addTagRows).
		edgeID := newDocID()
		stmt := `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, auto, method)
			SELECT ?, ?, ?, ?, ?, 1, '` + edgeMethodWikilink + `'
			WHERE NOT EXISTS (SELECT 1 FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = ?)`
		if err := d.exec(ctx, key, stmt, edgeID, fromChunkID, to, kind, now, fromChunkID, to, kind); err != nil {
			return err
		}
		if kind == "mentions" {
			d.mentionEvidence(ctx, key, edgeID, fromChunkID, body, to)
		}
	}
	return nil
}

// --- ops: query ---

func (d *Document) queryChunks(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	// Raw escape hatch: route the model's SELECT straight to SQL Memory (the
	// validator gates it — read-only, no ATTACH/etc.).
	if in.SQL != "" {
		// Raw escape hatch: pass the model's SQL straight to the Manager (NO
		// Rebind — the model uses the tier's native placeholders; rebinding
		// could corrupt a `?` inside a string literal). Validator-gated.
		res, err := d.docSQL().Query(ctx, key, in.SQL, nil)
		if err != nil {
			return docRawSQLFail("query_chunks: "+err.Error(), err), nil
		}
		return jsonResult(map[string]any{"columns": res.Columns, "rows": res.Rows, "truncated": res.Truncated})
	}
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	where := "1=1"
	args := []any{}
	// under_path: resolve documents at/under a Path-tree path → restrict to them.
	if in.UnderPath != "" {
		docIDs, err := d.documentsUnderPath(ctx, key, in.UnderPath)
		if err != nil {
			return docFail("query_chunks: ", err), nil
		}
		if len(docIDs) == 0 {
			return jsonResult(map[string]any{"chunks": []any{}})
		}
		ph := ""
		for i, id := range docIDs {
			if i > 0 {
				ph += ","
			}
			ph += "?"
			args = append(args, id)
		}
		where += " AND document_id IN (" + ph + ")"
	}
	if in.DocumentID != "" {
		where += " AND document_id = ?"
		args = append(args, in.DocumentID)
	}
	// RFC BZ subtype expansion: `type=event` also matches its subclasses.
	typeExpansion := d.expandTypeFilter(ctx, in.Type)
	if frag, targs := typeFilterSQL("type", typeExpansion); frag != "" {
		where += " AND " + frag
		args = append(args, targs...)
	}
	if in.Status != "" {
		where += " AND status = ?"
		args = append(args, in.Status)
	}
	if in.ParentID != "" {
		where += " AND parent_id = ?"
		args = append(args, in.ParentID)
	}
	// tag: exact membership. tag_prefix: the tag itself OR anything nested under it
	// (tag_prefix + '/…'). NOTE the LIKE metacharacters '%' and '_' in a prefix are
	// not escaped (matching the RFC's own SQL) — a false-positive for a prefix that
	// literally contains one is tolerated for a Phase-1 query filter (no security
	// impact; tags are normally plain slash-nested identifiers).
	if in.Tag != "" {
		where += " AND id IN (SELECT chunk_id FROM chunk_tags WHERE tag = ?)"
		args = append(args, in.Tag)
	}
	if in.TagPrefix != "" {
		where += " AND id IN (SELECT chunk_id FROM chunk_tags WHERE tag = ? OR tag LIKE ?)"
		args = append(args, in.TagPrefix, in.TagPrefix+"/%")
	}
	// as_of: the chunk's own interval AND, for a fact-tier chunk, its sidecar's —
	// the predicate search and related apply. The sidecar half is a NOT EXISTS
	// rather than search's LEFT JOIN because this statement selects unqualified
	// columns that chunk_memory_meta shares (valid_at, invalid_at); chunk_id is the
	// sidecar's key, so "no row contradicts the instant" is the same test.
	if in.AsOf != nil {
		if *in.AsOf <= 0 {
			return errValidation("query_chunks: as_of must be a positive instant in unix nanoseconds",
				"Pass as_of as unix nanoseconds, e.g. 1747180800000000000, or omit it."), nil
		}
		frag, targs := chunkIntervalClause("chunks", *in.AsOf)
		where += " AND " + frag + ` AND NOT EXISTS (SELECT 1 FROM chunk_memory_meta mv WHERE mv.chunk_id = chunks.id
			AND ((mv.valid_at IS NOT NULL AND mv.valid_at > ?) OR (mv.invalid_at IS NOT NULL AND mv.invalid_at <= ?)))`
		args = append(args, targs...)
		args = append(args, *in.AsOf, *in.AsOf)
	}
	args = append(args, limit)
	res, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE `+where+` ORDER BY document_id, parent_id, position LIMIT ?`, args...)
	if err != nil {
		return errFrom("query_chunks: "+err.Error(), err), nil
	}
	rows := make([]chunkRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, scanChunkRow(res.Columns, r))
	}
	// RFC FP R8, computed for the whole page at once: at most two queries, not one
	// per chunk.
	defs, derr := d.definitionalChunks(ctx, key, rows)
	if derr != nil {
		return errFrom("query_chunks: definitional: "+derr.Error(), derr), nil
	}
	chunks := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Structured query returns the SQL row (no bodies — keeps it light;
		// fetch a body with get_chunk).
		m := map[string]any{"id": row.ID, "document_id": row.DocumentID, "title": row.Title, "position": row.Position, "revision": row.Revision}
		if row.ParentID != "" {
			m["parent_id"] = row.ParentID
		}
		if row.Type != "" {
			m["type"] = row.Type
		}
		if row.Status != "" {
			m["status"] = row.Status
		}
		putValidity(m, row.ValidAt, row.InvalidAt)
		if defs[row.ID] {
			m["definitional"] = true
		}
		chunks = append(chunks, m)
	}
	out := map[string]any{"chunks": chunks}
	// REPORTED, not silent. The filter the caller asked for is not the filter that
	// ran, and a reader comparing counts across two deployments needs to know which
	// subclasses were folded in — a wider answer with no explanation reads as a bug.
	if len(typeExpansion) > 1 {
		out["type_expanded_to"] = typeExpansion
	}
	return jsonResult(out)
}

// documentsUnderPath returns the document ids named at/under a Path-tree path
// (the under_path query filter). Reuses the dirent recursive listing.
func (d *Document) documentsUnderPath(ctx context.Context, key sqlmem.ScopeKey, rawPath string) ([]string, error) {
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return nil, docClassify(err, errValidation, docPathFix)
	}
	rows, err := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), dirPrefix(canonical))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range rows {
		if r.Kind != "document" {
			continue
		}
		var ref struct {
			DocumentID string `json:"document_id"`
		}
		_ = json.Unmarshal(r.ResourceRef, &ref)
		if ref.DocumentID != "" {
			ids = append(ids, ref.DocumentID)
		}
	}
	return ids, nil
}

// --- ops: Markdown export ---

// headingReplacer collapses newlines so a chunk title stays on one heading line.
var headingReplacer = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// embedRe matches a transclusion `![[target]]` — the leading `!` is what
// distinguishes an EMBED (expanded inline at clean-export time) from a
// `[[link]]` (Phase-2a: a graph edge, never expanded). The inner target holds
// no brackets. RFC BS Phase 2b.
var embedRe = regexp.MustCompile(`!\[\[([^\[\]]+)\]\]`)

// maxEmbedDepth bounds how deep transclusion recurses (a chain of embeds each
// pulling in another). A cycle is already caught by the chain guard; this cap
// additionally bounds a very long ACYCLIC chain so export can't run away.
const maxEmbedDepth = 4

// expandTransclusions returns body with each resolvable `![[target]]` replaced
// by the target chunk's own (recursively expanded) body. RFC BS Phase 2b.
//
// chain holds the chunk ids on the current expansion path (seeded with the
// chunk being rendered) so a self- or mutual-embed leaves a literal instead of
// looping; depth caps a long acyclic chain. An unresolved target, a cycle, the
// depth cap, or a body-read fault all degrade to the LITERAL `![[target]]` —
// transclusion is a rendering nicety, never a correctness gate, so it must not
// abort the export walk.
//
// Iterates FindAllStringSubmatchIndex rather than ReplaceAllStringFunc because
// each match needs its own resolve + cycle/depth decision, and the gaps between
// matches are copied verbatim.
func (d *Document) expandTransclusions(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, body string, chain []string, depth int) string {
	matches := embedRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return body
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		// m = [matchStart, matchEnd, groupStart, groupEnd].
		b.WriteString(body[last:m[0]]) // the gap before this embed, verbatim
		last = m[1]
		token := body[m[0]:m[1]] // the literal `![[target]]`
		target := strings.TrimSpace(body[m[2]:m[3]])
		targetID, ok := d.resolveEmbedTarget(ctx, key, target)
		switch {
		case !ok, inChain(chain, targetID), depth >= maxEmbedDepth:
			// Unresolved, would loop, or too deep → leave the embed literal.
			b.WriteString(token)
		default:
			cb, err := d.readBody(ctx, mscope, key, targetID)
			if err != nil {
				b.WriteString(token) // a store fault degrades gracefully
				continue
			}
			b.WriteString(d.expandTransclusions(ctx, key, mscope, cb.Body, append(chain, targetID), depth+1))
		}
	}
	b.WriteString(body[last:]) // the tail after the last embed
	return b.String()
}

// inChain reports whether id is already on the expansion path (the cycle guard).
func inChain(chain []string, id string) bool {
	for _, c := range chain {
		if c == id {
			return true
		}
	}
	return false
}

// exportMD renders a document to Markdown (RFC AK §4.5 / RFC AM Phase 2).
// Walks the chunk hierarchy from the root(s) depth-first in position order:
// each chunk is a heading (level = depth+1, capped at 6) followed by its
// Markdown body. With include_metadata (default true) each chunk carries a
// `<!-- loom: {...} -->` comment (id/type/status/fields) and a trailing
// `<!-- loom-edges: ... -->` block, so the output round-trips through import_md
// (Phase 3); with include_metadata=false it is clean human-facing Markdown.
func (d *Document) exportMD(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	docID := in.DocumentID
	if docID == "" {
		var err error
		docID, err = d.docIDFromInput(ctx, key, in)
		if err != nil {
			return docFail("export_md: ", err), nil
		}
	}
	dres, err := d.query(ctx, key, `SELECT title, root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom("export_md: "+err.Error(), err), nil
	}
	if len(dres.Rows) == 0 {
		return errNotFound("export_md: no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	title := asStr(dres.Rows[0][0])

	roots, byParent, err := d.loadChunkTree(ctx, key, docID)
	if err != nil {
		return errFrom("export_md: "+err.Error(), err), nil
	}
	includeMeta := in.IncludeMetadata == nil || *in.IncludeMetadata

	// An imported document's provenance (its root's fields) is in the result
	// either way. A metadata export carries it in the root's loom comment
	// already; a clean one says it in a line under the title.
	var prov map[string]any
	if len(roots) > 0 {
		rootBody, rerr := d.readBody(ctx, mscope, key, roots[0].ID)
		if rerr != nil {
			return errFrom("export_md: body: "+rerr.Error(), rerr), nil
		}
		prov = d.importedProvenance(ctx, mscope, key, roots[0].ID, rootBody)
	}

	var b strings.Builder
	if err := d.renderChunkTree(ctx, key, mscope, &b, roots, byParent, 0, includeMeta); err != nil {
		return errFrom("export_md: body: "+err.Error(), err), nil
	}
	if line := provenanceLine(prov); line != "" && !includeMeta {
		md := b.String()
		head, rest, _ := strings.Cut(md, "\n")
		b.Reset()
		b.WriteString(head + "\n\n" + line + "\n" + rest)
	}

	// Edges trailer — the free-form graph edges originating from this document's
	// chunks (parent-child is the hierarchy above, not an edge). Metadata-only:
	// a clean export (include_metadata=false) omits it.
	if includeMeta {
		lines, err := d.exportEdgeLines(ctx, key, docID)
		if err != nil {
			return errFrom("export_md: edges: "+err.Error(), err), nil
		}
		if len(lines) > 0 {
			b.WriteString("<!-- loom-edges:\n" + strings.Join(lines, "\n") + "\n-->\n")
		}
	}

	out := map[string]any{"markdown": b.String(), "document_id": docID, "title": title}
	if prov != nil {
		out["provenance"] = prov
	}
	return jsonResult(out)
}

// loadChunkTree reads a document's chunks grouped for a depth-first walk:
// the top-level rows (parent_id "") and every other row under its parent, each
// slice in position order.
func (d *Document) loadChunkTree(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]chunkRow, map[string][]chunkRow, error) {
	// ORDER BY parent_id first so each parent's rows are contiguous, then
	// position, then id as a stable tiebreaker — makes the byParent grouping
	// below deterministic even if two siblings somehow share a position
	// (reachable via an explicit `position` on create_chunk/move_chunk).
	cres, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ? ORDER BY parent_id, position, id`, docID)
	if err != nil {
		return nil, nil, err
	}
	// Group children by parent (the global position ORDER BY keeps each parent's
	// slice in ascending position). parent_id "" = a top-level (root) chunk.
	byParent := map[string][]chunkRow{}
	var roots []chunkRow
	for _, r := range cres.Rows {
		row := scanChunkRow(cres.Columns, r)
		if row.ParentID == "" {
			roots = append(roots, row)
		} else {
			byParent[row.ParentID] = append(byParent[row.ParentID], row)
		}
	}
	return roots, byParent, nil
}

// renderChunkTree writes rows and their descendants to b depth-first in
// position order: each chunk is a heading (level = depth+1, capped at 6)
// followed by its Markdown body — the rendering export_md emits. depth is the
// depth of rows themselves, so a subtree keeps the heading levels it has in the
// whole document.
func (d *Document) renderChunkTree(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, b *strings.Builder, rows []chunkRow, byParent map[string][]chunkRow, depth int, includeMeta bool) error {
	// An export that renders headings with silently-empty bodies is
	// indistinguishable from a document that genuinely has none — precisely the
	// failure mode that let the v1.33.0 tenant regression pass for a full
	// table-of-contents export. Abort the walk on the first fault instead.
	var walkErr error
	var walk func(row chunkRow, depth int)
	walk = func(row chunkRow, depth int) {
		if walkErr != nil {
			return
		}
		level := depth + 1
		if level > 6 {
			level = 6
		}
		cb, rerr := d.readBody(ctx, mscope, key, row.ID)
		if rerr != nil {
			walkErr = rerr
			return
		}
		// A heading is one line — collapse any newline in the title to a space
		// so a multi-line title can't split the heading and corrupt the doc.
		title := headingReplacer.Replace(row.Title)
		b.WriteString(strings.Repeat("#", level) + " " + title + "\n")
		if includeMeta {
			meta := map[string]any{"id": row.ID}
			if row.Type != "" {
				meta["type"] = row.Type
			}
			if row.Status != "" {
				meta["status"] = row.Status
			}
			if len(cb.Fields) > 0 {
				meta["fields"] = cb.Fields
			}
			putValidity(meta, row.ValidAt, row.InvalidAt)
			mj, _ := json.Marshal(meta)
			b.WriteString("<!-- loom: " + string(mj) + " -->\n")
		}
		// RFC BO — render media chunks self-contained + human-readable (and
		// re-importable): a mermaid chunk's source as a ```mermaid fence, an image
		// chunk's bytes as a data-URL. import_md re-detects both (see
		// classifyMediaBody). A plain chunk emits its body verbatim.
		switch {
		case row.Type == "mermaid" && cb.Body != "":
			b.WriteString("\n```mermaid\n" + cb.Body + "\n```\n")
		case row.Type == "image":
			if mt, raw, ok := d.readAssetRow(ctx, key, row.ID); ok {
				b.WriteString("\n![" + imgAltForExport(cb.Body) + "](data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(raw) + ")\n")
			} else if cb.Body != "" {
				b.WriteString("\n" + cb.Body + "\n")
			}
		default:
			if cb.Body != "" {
				body := cb.Body
				// RFC BS Phase 2b — expand `![[…]]` embeds ONLY in clean mode.
				// In metadata/round-trip mode the body stays verbatim, or an
				// export(include_metadata=true)→import_md would bake the expanded
				// copy in and lose the embed. The chain seeds with THIS chunk's id
				// so a chunk embedding itself is caught as a cycle.
				if !includeMeta {
					body = d.expandTransclusions(ctx, key, mscope, cb.Body, []string{row.ID}, 0)
				}
				b.WriteString("\n" + body + "\n")
			}
		}
		b.WriteString("\n")
		for _, c := range byParent[row.ID] {
			walk(c, depth+1)
		}
	}
	for _, r := range rows {
		walk(r, depth)
	}
	return walkErr
}

// --- ops: Markdown import ---

// imgAltForExport makes a chunk caption safe as Markdown image alt text: no
// newlines (one line) and no ']' (which would close the alt early). RFC BO.
var imgAltReplacer = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "]", "")

func imgAltForExport(caption string) string { return imgAltReplacer.Replace(caption) }

// mdChunk is one parsed chunk from an export_md-shaped document.
type mdChunk struct {
	level  int
	title  string
	oldID  string // the id in the `<!-- loom: ... -->` comment (for edge remap)
	typ    string
	status string
	fields json.RawMessage
	// validAt / invalidAt are the chunk's own interval from the metadata comment;
	// nil when absent (a clean export imports timeless chunks).
	validAt   *int64
	invalidAt *int64
	body      string
	// RFC BO — set when the body was a self-contained media form: an image
	// chunk's decoded bytes to re-attach via set_asset (assetData base64 +
	// assetMediaType), unwrapped from a data-URL. A mermaid chunk carries its
	// source in body (fence stripped) with typ="mermaid".
	assetMediaType string
	assetData      string
	// assetRef is a volume image reference (RFC FF) the runtime reads into
	// assetData before the chunk is created; body holds its alt text.
	assetRef string
}

type mdEdge struct {
	from, to, kind string
	env            *mdEdgeEnvelope
}

var (
	mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	// A trailer line is `from -> to [kind]`, optionally followed by the statement's
	// envelope as JSON (RFC FN). `from` may be `@<natural_key>` for an inbound
	// statement whose source is outside the document.
	mdEdgeRe = regexp.MustCompile(`^\s*(\S+)\s*->\s*(\S+)\s*\[([^\]]*)\](?:\s*(\{.*\}))?\s*$`)
	// RFC BO — detect a WHOLE-body media form (anchored, so a text chunk that
	// merely contains a fence/image among prose is NOT reclassified). A mermaid
	// fence: ```mermaid\n<source>\n```. A data-URL image: ![alt](data:<mt>;base64,<b64>).
	mdMermaidFenceRe = regexp.MustCompile("(?s)^```mermaid\\s*\\n(.*?)\\n?```$")
	mdDataImageRe    = regexp.MustCompile(`^!\[([^\]]*)\]\(data:([a-zA-Z0-9.+/-]+);base64,([A-Za-z0-9+/=\s]+)\)$`)
)

// classifyMediaBody detects a chunk body that is ENTIRELY a media form (RFC BO)
// and returns the reconstructed chunk type + unwrapped content. typ="" means the
// body is ordinary Markdown (left untouched). For an image, mediaType+assetData
// (base64) are the bytes to re-attach via set_asset and newBody is the alt/caption;
// for mermaid, newBody is the raw source (fence stripped).
func classifyMediaBody(body string) (typ, mediaType, assetData, newBody string) {
	trimmed := strings.TrimSpace(body)
	if m := mdMermaidFenceRe.FindStringSubmatch(trimmed); m != nil {
		return "mermaid", "", "", m[1]
	}
	if m := mdDataImageRe.FindStringSubmatch(trimmed); m != nil {
		// Strip any whitespace the base64 payload may carry across lines.
		data := strings.Join(strings.Fields(m[3]), "")
		return "image", m[2], data, m[1]
	}
	return "", "", "", body
}

// parseLoomMarkdown parses an export_md output: heading lines define the chunk
// hierarchy (level = heading depth), an optional `<!-- loom: {…} -->` line
// right after a heading carries id/type/status/fields, and a trailing
// `<!-- loom-edges: … -->` block carries the graph edges. Lines between
// headings (minus the loom comment) are the chunk body.
// mdFence describes an open fenced code block: which character opened it and how
// long the run was. CommonMark closes a fence only with the SAME character and a
// run at least as long as the opener, which is what lets a ```-fenced block
// contain a shorter ``` run as literal text.
type mdFence struct {
	char byte
	run  int
}

// mdFenceDelim reads a line as a fence delimiter, returning the character and run
// length, or run 0 if the line is not one.
//
// Up to three leading spaces are allowed (four would make it an indented code
// block, which is a different construct and needs no tracking here because it
// cannot contain an ATX heading either).
func mdFenceDelim(line string) (byte, int) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0
	}
	c := line[i]
	run := 0
	for i+run < len(line) && line[i+run] == c {
		run++
	}
	if run < 3 {
		return 0, 0
	}
	// A ``` opener may carry an info string, but a closer may not contain a
	// backtick. Not enforced here: treating an info-bearing line as a delimiter is
	// correct for openers, and for closers the run/char match below is what decides.
	return c, run
}

func parseLoomMarkdown(md string) ([]mdChunk, []mdEdge) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var chunks []mdChunk
	var edges []mdEdge
	var cur *mdChunk
	var bodyLines []string
	flush := func() {
		if cur != nil {
			cur.body = strings.Trim(strings.Join(bodyLines, "\n"), "\n")
			// RFC BO — reconstruct a media chunk from a whole-body media form. The
			// meta comment's type (if any) and the rendered form agree on a loom
			// round-trip; a clean export (no meta) is inferred purely from the form.
			if typ, mt, ad, nb := classifyMediaBody(cur.body); typ != "" {
				if cur.typ == "" {
					cur.typ = typ
				}
				cur.body, cur.assetMediaType, cur.assetData = nb, mt, ad
			} else if alt, ref, ok := classifyVolumeImage(cur.body); ok {
				if cur.typ == "" {
					cur.typ = "image"
				}
				cur.body, cur.assetRef = alt, ref
			}
			chunks = append(chunks, *cur)
		}
		cur = nil
		bodyLines = nil
	}
	// FENCED CODE BLOCKS ARE LITERAL, and tracking that is the whole reason this
	// state exists. Without it a body containing a Markdown sample — which is most
	// technical documentation, including this project's own RFCs — was re-parsed on
	// import: its "## Example" lines became real chunks, the surrounding body was
	// truncated at the fence, and the document came back with more chunks than it
	// had. export_md was already correct (it emits the body verbatim); the asymmetry
	// meant export -> import silently restructured the tree.
	var fence mdFence

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if c, run := mdFenceDelim(line); run > 0 {
			switch {
			case fence.run == 0:
				fence = mdFence{char: c, run: run}
			case c == fence.char && run >= fence.run:
				fence = mdFence{}
			}
			if cur != nil {
				bodyLines = append(bodyLines, line)
			}
			continue
		}
		// Inside a fence every line is content — headings, loom comments and the
		// edges trailer alike. An unterminated fence therefore runs to the end of
		// the document, which is also what CommonMark specifies.
		if fence.run > 0 {
			if cur != nil {
				bodyLines = append(bodyLines, line)
			}
			continue
		}

		// Edges trailer — consume to its closing "-->".
		if strings.HasPrefix(trimmed, "<!-- loom-edges:") {
			for i++; i < len(lines) && !strings.Contains(lines[i], "-->"); i++ {
				if m := mdEdgeRe.FindStringSubmatch(lines[i]); m != nil {
					e := mdEdge{from: m[1], to: m[2], kind: m[3]}
					if m[4] != "" {
						var env mdEdgeEnvelope
						if json.Unmarshal([]byte(m[4]), &env) == nil {
							e.env = &env
						}
					}
					edges = append(edges, e)
				}
			}
			continue
		}
		if m := mdHeadingRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &mdChunk{level: len(m[1]), title: strings.TrimSpace(m[2])}
			continue
		}
		// Per-chunk metadata comment (single line directly under a heading).
		if cur != nil && strings.HasPrefix(trimmed, "<!-- loom:") && strings.HasSuffix(trimmed, "-->") {
			payload := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "<!-- loom:"), "-->"))
			var meta struct {
				ID        string          `json:"id"`
				Type      string          `json:"type"`
				Status    string          `json:"status"`
				Fields    json.RawMessage `json:"fields"`
				ValidAt   *int64          `json:"valid_at"`
				InvalidAt *int64          `json:"invalid_at"`
			}
			if json.Unmarshal([]byte(payload), &meta) == nil {
				cur.oldID, cur.typ, cur.status, cur.fields = meta.ID, meta.Type, meta.Status, meta.Fields
				cur.validAt, cur.invalidAt = meta.ValidAt, meta.InvalidAt
			}
			continue
		}
		if cur != nil {
			bodyLines = append(bodyLines, line)
		}
	}
	flush()
	return chunks, edges
}

// resultField pulls a string field out of a sub-op's JSON result (used to chain
// importMD onto the existing create_document / create_chunk ops).
func resultField(r tools.Result, field string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(r.Text), &m)
	return asStr(m[field])
}

// importAsset re-attaches an image chunk's bytes during import_md (RFC BO) by
// reusing setAsset — which validates the media_type + size and marks the chunk
// type=image. Best-effort: a bad/oversized data-URL is skipped (the chunk stays,
// just without its image) rather than aborting the whole import. It reports
// whether the image was attached.
func (d *Document) importAsset(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID string, c mdChunk) bool {
	res, err := d.setAsset(ctx, key, mscope, docInput{
		ID: chunkID, MediaType: c.assetMediaType, Data: c.assetData,
	})
	return err == nil && !res.IsError
}

// importMD builds a document from export_md-shaped Markdown (RFC AK §4.5 / RFC
// AM Phase 3). With no document_id it creates a NEW document (the first heading
// becomes the root); with a document_id (+ optional parent_id) it imports the
// parsed subtree under an existing chunk. Chunks get fresh ids; the
// `<!-- loom-edges -->` graph is recreated by remapping the old ids → new ones
// (an edge whose endpoint wasn't imported is skipped). Not atomic — it composes
// the existing create_document / create_chunk / link_chunks ops, so a mid-import
// failure leaves a partial document the caller can delete and retry.
func (d *Document) importMD(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if strings.TrimSpace(in.Markdown) == "" {
		return errValidation("import_md: missing required field: markdown", "Pass `markdown`, the document text with at least one '# Heading'."), nil
	}
	chunks, edges := parseLoomMarkdown(in.Markdown)
	if len(chunks) == 0 {
		return errValidation("import_md: no headings found — a document needs at least one '# Heading'", ""), nil
	}
	if in.AmendingDocumentID != "" && (!in.Reimport || strings.TrimSpace(in.WorkKey) == "") {
		return errValidation("import_md: amending_document_id needs reimport with work_key — it names what made a new edition of a work. Nothing was imported.",
			"Pass reimport: true and the work's work_key, or omit amending_document_id."), nil
	}
	if in.Reimport {
		return d.reimportMD(ctx, key, mscope, in, chunks, edges)
	}
	// Checked before anything is written: import_md is not atomic, and a refusal
	// half way would leave a partial document behind.
	for _, c := range chunks {
		if r := emptyIntervalRefusal("import_md: chunk "+strconv.Quote(c.title), c.validAt, c.invalidAt); r != nil {
			return *r, nil
		}
	}
	// An IMPORT: the File tool's (provenance on the context) or one that names
	// its source. Its chunk bodies carry the import origin, so the text is
	// searched as a document and never served as a fact, and its root carries
	// where it came from.
	prov, imported := importProvenanceFrom(ctx)
	src, err := ImportSource{URL: in.SourceURL, Kind: in.SourceKind, Title: in.SourceTitle}.normalize()
	if err != nil {
		return errValidation("import_md: "+err.Error()+". Nothing was imported.",
			"Pass source_url as an http(s) link without a user name or password, and source_kind as one of "+strings.Join(ImportSourceKinds, ", ")+"."), nil
	}
	if !src.empty() {
		if in.DocumentID != "" {
			return errValidation("import_md: a source describes a whole document, so it is taken only when the import makes a new one. Nothing was imported.",
				"Omit document_id to import as a new document, or omit the source fields."), nil
		}
		if !imported {
			prov = newImportProvenance()
			imported = true
		}
		prov = prov.withSource(src)
	}
	bodyOrigin := ""
	if imported {
		bodyOrigin = store.OriginImport
	}
	oldToNew := map[string]string{}
	type frame struct {
		level int
		id    string
	}
	var stack []frame
	var docID, rootID string
	created := 0
	start := 0
	// RFC FF: image chunks attached, and images that stayed links (a volume
	// reference that could not be read, or a data-URL setAsset refused).
	imageChunks, skipped := 0, 0
	var skippedWhy []string
	skip := func(why string) {
		skipped++
		if len(skippedWhy) < 20 {
			skippedWhy = append(skippedWhy, why)
		}
	}
	attach := func(id string, c mdChunk) {
		if d.importAsset(ctx, key, mscope, id, c) {
			imageChunks++
		} else {
			skip("an image the store refused (type or size)")
		}
	}
	// Every chunk below is new, so none may carry the operator's `principal` field.
	// Refused before anything is written, rather than part-way through.
	for _, c := range chunks {
		if refused, err := d.holdPrincipalField(ctx, key, mscope, "import_md", "", c.fields); err != nil || refused != nil {
			if err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			return *refused, nil
		}
	}
	// A volume image is read just before its chunk is created and attached,
	// so the import holds one image's bytes at a time (a file named again
	// later is kept until its last use); see newVolumeImageLoader for the caps.
	images := newVolumeImageLoader(chunks)
	load := func(c *mdChunk) {
		if c.assetRef == "" {
			return
		}
		if why, ok := d.loadVolumeImage(ctx, c, images); !ok {
			skip(why)
		}
	}

	if in.DocumentID == "" {
		// New document: the first heading is the root chunk.
		first := chunks[0]
		load(&first)
		var stamp map[string]any
		var tags []string
		if imported {
			stamp = prov.fields
			tags = prov.tags()
		}
		cd, _ := d.createDocument(ctx, key, mscope, docInput{Title: titleOrUntitled(first.title), Path: in.Path, Tags: tags, bodyOrigin: bodyOrigin})
		if cd.IsError {
			return cd, nil
		}
		docID = resultField(cd, "document_id")
		rootID = resultField(cd, "root_chunk_id")
		if err := d.putBody(ctx, mscope, key, rootID, first.typ, first.body, first.fields, bodyOrigin, stamp); err != nil {
			return errFrom("import_md: root body: "+err.Error(), err), nil
		}
		if first.typ != "" || first.status != "" {
			if err := d.exec(ctx, key, `UPDATE chunks SET type = ?, status = ? WHERE id = ?`, nullIfEmpty(first.typ), nullIfEmpty(first.status), rootID); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			// Keep the documents row's mirrored facets in step with the root chunk
			// (createDocument set them from an empty type/status above).
			if err := d.mirrorRootFacets(ctx, key, rootID); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
		}
		if first.validAt != nil || first.invalidAt != nil {
			if err := d.exec(ctx, key, `UPDATE chunks SET valid_at = ?, invalid_at = ? WHERE id = ?`,
				int64Arg(first.validAt), int64Arg(first.invalidAt), rootID); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
		}
		if first.assetData != "" {
			attach(rootID, first)
		}
		if first.oldID != "" {
			oldToNew[first.oldID] = rootID
		}
		stack = []frame{{level: first.level, id: rootID}}
		created, start = 1, 1
	} else {
		docID = in.DocumentID
		base := in.ParentID
		if base == "" {
			dres, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
			if err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			if len(dres.Rows) == 0 {
				return errNotFound("import_md: no such document: "+docID, "Find the document with op=query_documents in the same scope, or omit document_id to import as a new document."), nil
			}
			base = asStr(dres.Rows[0][0])
		} else if _, ok, err := d.getChunkRow(ctx, key, base); err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		} else if !ok {
			return errNotFound("import_md: no such parent chunk: "+base, "Pass a parent_id from this document, or omit it to import under the document's root."), nil
		}
		rootID = base
		stack = []frame{{level: 0, id: base}}
	}

	for _, c := range chunks[start:] {
		load(&c)
		// Pop to the nearest ancestor whose heading level is shallower.
		for len(stack) > 1 && stack[len(stack)-1].level >= c.level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].id
		cc, _ := d.createChunk(ctx, key, mscope, docInput{
			DocumentID: docID, ParentID: parent, Title: titleOrUntitled(c.title),
			Type: c.typ, Status: c.status, Body: c.body, Fields: c.fields,
			ValidAt: c.validAt, InvalidAt: c.invalidAt, bodyOrigin: bodyOrigin,
		})
		if cc.IsError {
			return cc, nil
		}
		newID := resultField(cc, "id")
		if c.assetData != "" {
			attach(newID, c)
		}
		if c.oldID != "" {
			oldToNew[c.oldID] = newID
		}
		stack = append(stack, frame{level: c.level, id: newID})
		created++
	}

	// Recreate edges by remapping old → new ids. An inbound statement names its
	// source by natural key and is reconnected when that key exists in this scope;
	// anything else whose endpoints were not both imported is skipped.
	edgesSkipped := d.importEdges(ctx, key, mscope, edges, oldToNew)

	out := map[string]any{"document_id": docID, "root_chunk_id": rootID, "chunks_created": created,
		"image_chunks": imageChunks, "skipped": skipped}
	if edgesSkipped > 0 {
		out["edges_skipped"] = edgesSkipped
	}
	if imported && in.DocumentID == "" {
		out["provenance"] = prov.fields
	}
	if len(skippedWhy) > 0 {
		out["skipped_images"] = skippedWhy
	}
	if why := d.publishIngested(ctx, key, mscope, docID, "imported", nil); why != "" {
		out["not_announced"] = why
	}
	return jsonResult(out)
}

func titleOrUntitled(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Untitled"
	}
	return s
}

// --- ops: JSON Canvas import/export ---
//
// JSON Canvas is the open, spatial-graph interchange format Obsidian Canvas
// reads/writes (a top-level {"nodes":[...],"edges":[...]}). export_canvas renders
// a document's CONTENT chunks as canvas nodes + their cross-reference edges;
// import_canvas builds a new document from a canvas. The two are a round-trip:
// export→import→export lands the same node texts, layouts, and edges.

// canvasNode is one JSON Canvas v1.0 node. x/y/width/height are integers per the
// spec and always emitted (0,0 is a valid position, so no omitempty). The
// type-specific fields (text/file/url/label) carry omitempty so a text node
// doesn't emit an empty "file", etc. This tool only ever WRITES "text" nodes on
// export, but the full field set lets import accept a real Obsidian canvas.
type canvasNode struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Color  string `json:"color,omitempty"`
	Text   string `json:"text,omitempty"`
	File   string `json:"file,omitempty"`
	URL    string `json:"url,omitempty"`
	Label  string `json:"label,omitempty"`
}

// canvasEdge is one JSON Canvas v1.0 edge. Only fromNode/toNode are mandatory;
// the side/end/color/label fields are optional and omitted when empty. export
// sets toEnd="arrow" (the spec default, made explicit) and label=the edge kind.
type canvasEdge struct {
	ID       string `json:"id"`
	FromNode string `json:"fromNode"`
	ToNode   string `json:"toNode"`
	FromSide string `json:"fromSide,omitempty"`
	ToSide   string `json:"toSide,omitempty"`
	FromEnd  string `json:"fromEnd,omitempty"`
	ToEnd    string `json:"toEnd,omitempty"`
	Color    string `json:"color,omitempty"`
	Label    string `json:"label,omitempty"`
}

// canvasDoc is the top-level JSON Canvas object.
type canvasDoc struct {
	Nodes []canvasNode `json:"nodes"`
	Edges []canvasEdge `json:"edges"`
}

// Auto-grid defaults for a content chunk with no stored layout: a deterministic
// row-major grid so an un-placed document still exports to a readable canvas and
// re-exports to the SAME coordinates (the round-trip relies on this).
const (
	canvasGridCols = 4
	canvasNodeW    = 400
	canvasNodeH    = 200
	canvasNodeGap  = 50
	// canvasTitleMax caps a node-derived chunk title (rune-safe) so a long node
	// body doesn't become an unwieldy title.
	canvasTitleMax = 120
)

// canvasLayout is a stored spatial position for a chunk (a chunk_layout row).
type canvasLayout struct {
	x, y, w, h int
	color      string
}

// canvasNodeTitle derives a chunk title from a node's primary text: the first
// non-empty line, trimmed and rune-capped, else "node <id>" — createChunk
// requires a non-empty title, so this must never return "".
func canvasNodeTitle(text, id string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > canvasTitleMax {
			line = string(r[:canvasTitleMax])
		}
		return line
	}
	return "node " + id
}

// exportCanvas renders a document as a JSON Canvas v1.0 graph.
//
// Nodes are the document's NON-ROOT (content) chunks — the root container chunk
// holds the doc title/type, not canvas content, so skipping it keeps a
// round-trip from accreting an empty node. Each content chunk becomes a "text"
// node whose text is its body (or its title when the body is empty), positioned
// from its stored chunk_layout row or, absent one, a deterministic auto-grid by
// export order. Edges are the within-document chunk_edges whose BOTH endpoints
// are content nodes (so an edge to the root, or a cross-document endpoint, is
// dropped).
func (d *Document) exportCanvas(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	docID := in.DocumentID
	if docID == "" {
		var err error
		docID, err = d.docIDFromInput(ctx, key, in)
		if err != nil {
			return docFail("export_canvas: ", err), nil
		}
	}
	dres, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom("export_canvas: "+err.Error(), err), nil
	}
	if len(dres.Rows) == 0 {
		return errNotFound("export_canvas: no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	rootID := asStr(dres.Rows[0][0])

	// Content chunks in the SAME deterministic order export_md walks
	// (parent_id, position, id) so the auto-grid index is stable across exports.
	cres, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ? ORDER BY parent_id, position, id`, docID)
	if err != nil {
		return errFrom("export_canvas: "+err.Error(), err), nil
	}

	// Stored layouts for this document's chunks in one query.
	layouts := map[string]canvasLayout{}
	lres, err := d.query(ctx, key, `SELECT chunk_id, x, y, width, height, color FROM chunk_layout WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID)
	if err != nil {
		return errFrom("export_canvas: layout: "+err.Error(), err), nil
	}
	for _, r := range lres.Rows {
		layouts[asStr(r[0])] = canvasLayout{x: asInt(r[1]), y: asInt(r[2]), w: asInt(r[3]), h: asInt(r[4]), color: asStr(r[5])}
	}

	nodes := []canvasNode{}
	nodeSet := map[string]bool{}
	i := 0
	for _, r := range cres.Rows {
		row := scanChunkRow(cres.Columns, r)
		if row.ID == rootID {
			continue // the root container is not canvas content
		}
		cb, rerr := d.readBody(ctx, mscope, key, row.ID)
		if rerr != nil {
			return errFrom("export_canvas: body: "+rerr.Error(), rerr), nil
		}
		text := cb.Body
		if strings.TrimSpace(text) == "" {
			text = row.Title
		}
		n := canvasNode{ID: row.ID, Type: "text", Text: text}
		if lay, ok := layouts[row.ID]; ok {
			n.X, n.Y, n.Width, n.Height, n.Color = lay.x, lay.y, lay.w, lay.h, lay.color
		} else {
			n.X = (i % canvasGridCols) * (canvasNodeW + canvasNodeGap)
			n.Y = (i / canvasGridCols) * (canvasNodeH + canvasNodeGap)
			n.Width = canvasNodeW
			n.Height = canvasNodeH
		}
		nodes = append(nodes, n)
		nodeSet[row.ID] = true
		i++
	}

	// Within-document edges whose BOTH endpoints are content nodes. The SQL
	// restricts to this document; the nodeSet check then drops any edge touching
	// the root (the one in-document chunk that is not a node). Live statements
	// only: a canvas edge has no status, and import_canvas asserts every edge, so a
	// proposal or a rejection drawn here would come back asserted.
	eres, err := d.query(ctx, key, `SELECT e.from_id, e.to_id, e.kind FROM chunk_edges e WHERE e.from_id IN (SELECT id FROM chunks WHERE document_id = ?) AND e.to_id IN (SELECT id FROM chunks WHERE document_id = ?) AND `+edgeLiveClause+` ORDER BY e.from_id, e.to_id, e.kind`, docID, docID)
	if err != nil {
		return errFrom("export_canvas: edges: "+err.Error(), err), nil
	}
	edges := []canvasEdge{}
	for _, r := range eres.Rows {
		from, to, kind := asStr(r[0]), asStr(r[1]), asStr(r[2])
		if !nodeSet[from] || !nodeSet[to] {
			continue
		}
		edges = append(edges, canvasEdge{
			ID: from + "-" + to + "-" + kind, FromNode: from, ToNode: to,
			Label: kind, ToEnd: "arrow",
		})
	}

	return jsonResult(map[string]any{"canvas": canvasDoc{Nodes: nodes, Edges: edges}, "document_id": docID})
}

// importCanvas builds a NEW document from a JSON Canvas graph.
//
// Each node becomes a child chunk of the new document's root, its coordinates
// recorded in chunk_layout, and each edge becomes a manual chunk_edge (auto=0 —
// an imported explicit edge is authored, not parser-derived). Node→chunk id
// mapping remaps the edges; an edge whose endpoint node is absent is skipped
// (defensive). Node bodies are ordinary data — the normal createChunk path runs,
// so an imported [[name]] in a body reconciles into edges exactly as a typed one
// would.
func (d *Document) importCanvas(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if len(in.Canvas) == 0 {
		return errValidation("import_canvas: missing required field: canvas", "Pass `canvas`, a JSON Canvas object with nodes and edges."), nil
	}
	var cv canvasDoc
	if err := json.Unmarshal(in.Canvas, &cv); err != nil {
		return errValidation("import_canvas: invalid canvas JSON: "+err.Error(), "Pass `canvas` as a valid JSON Canvas object with \"nodes\" and \"edges\" arrays."), nil
	}
	title := in.Title
	if strings.TrimSpace(title) == "" {
		title = "Imported canvas"
	}
	cd, _ := d.createDocument(ctx, key, mscope, docInput{Title: title, Path: in.Path})
	if cd.IsError {
		return cd, nil
	}
	docID := resultField(cd, "document_id")
	rootID := resultField(cd, "root_chunk_id")

	nodeToChunk := map[string]string{}
	created := 0
	for _, n := range cv.Nodes {
		var body, chunkTitle string
		switch n.Type {
		case "file":
			body = "[file: " + n.File + "]"
			chunkTitle = canvasNodeTitle(n.File, n.ID)
		case "link":
			body = n.URL
			chunkTitle = canvasNodeTitle(n.URL, n.ID)
		case "group":
			chunkTitle = strings.TrimSpace(n.Label)
			if chunkTitle == "" {
				chunkTitle = "group"
			}
		default:
			// "text" and any forward-compat/unknown node type: keep the node (so its
			// edges still resolve) with its text as the body.
			body = n.Text
			chunkTitle = canvasNodeTitle(n.Text, n.ID)
		}
		cc, _ := d.createChunk(ctx, key, mscope, docInput{
			DocumentID: docID, ParentID: rootID, Title: chunkTitle, Body: body,
		})
		if cc.IsError {
			return cc, nil
		}
		newID := resultField(cc, "id")
		nodeToChunk[n.ID] = newID
		if err := d.exec(ctx, key, `INSERT INTO chunk_layout (chunk_id, x, y, width, height, color) VALUES (?, ?, ?, ?, ?, ?)`,
			newID, n.X, n.Y, n.Width, n.Height, nullIfEmpty(n.Color)); err != nil {
			return errFrom("import_canvas: layout: "+err.Error(), err), nil
		}
		created++
	}

	edgesCreated := 0
	for _, e := range cv.Edges {
		from, ok1 := nodeToChunk[e.FromNode]
		to, ok2 := nodeToChunk[e.ToNode]
		if !ok1 || !ok2 {
			continue
		}
		// An unlabeled canvas edge is a plain relation. It used to become
		// `references`, which is the [[name]] parser's reserved kind (RFC FN).
		kind := strings.TrimSpace(e.Label)
		if kind == "" {
			kind = "related"
		}
		lr, _ := d.linkChunks(ctx, key, mscope, docInput{FromID: from, ToID: to, Kind: kind, edgeMethod: edgeMethodCanvas})
		if lr.IsError {
			return lr, nil
		}
		edgesCreated++
	}

	return jsonResult(map[string]any{
		"document_id": docID, "root_chunk_id": rootID,
		"chunks_created": created, "edges_created": edgesCreated,
	})
}

// --- ops: type definitions ---

func (d *Document) defineType(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.Name == "" {
		return errValidation("define_type: missing required field: name", "Pass `name`, the type's name."), nil
	}
	fields := in.Fields
	if len(fields) == 0 {
		fields = json.RawMessage(`[]`)
	}
	now := time.Now().UnixNano()
	// document_id "" = a cross-document (scope-wide) type.
	docID := in.DocumentID
	// Upsert via delete+insert for portability (no ON CONFLICT dialect dance).
	if err := d.exec(ctx, key, `DELETE FROM chunk_types WHERE document_id = ? AND name = ?`, docID, in.Name); err != nil {
		return errFrom("define_type: "+err.Error(), err), nil
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_types (document_id, name, fields, created_at) VALUES (?, ?, ?, ?)`, docID, in.Name, string(fields), now); err != nil {
		return errFrom("define_type: "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{"ok": true, "name": in.Name, "document_id": docID})
}

func (d *Document) listTypes(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	res, err := d.query(ctx, key, `SELECT document_id, name, fields FROM chunk_types WHERE document_id = ? ORDER BY name`, in.DocumentID)
	if err != nil {
		return errFrom("list_types: "+err.Error(), err), nil
	}
	types := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		m := map[string]any{}
		for i, c := range res.Columns {
			m[c] = r[i]
		}
		var fields json.RawMessage
		_ = json.Unmarshal([]byte(asStr(m["fields"])), &fields)
		types = append(types, map[string]any{"name": asStr(m["name"]), "document_id": asStr(m["document_id"]), "fields": fields})
	}
	// The structural vocabulary the primitive itself writes into chunks.type, listed
	// apart from the document's own types (RFC FO §3.2) so a `section` chunk is not
	// mistaken for an undeclared class.
	return jsonResult(map[string]any{"types": types, "reserved": memrank.ReservedStructuralTypes()})
}

// --- ops: tags + document facets (RFC BS Phase 1) ---

// upsertChunkOp runs upsert_chunk (whose create/update internals live in the
// entity-tier file) and then applies the Phase-1 facets this change owns. On the
// CREATE path there is nothing to add here: upsert delegates the insert to
// createChunk, which already applies `tags`, and never makes a root chunk. On the
// UPDATE path (updateChunkForUpsert, which does not touch tags) this replace-sets
// the tags when supplied. Mirroring a root chunk's title/type/status onto the
// documents row happens inside upsertChunk, so its direct callers get it too.
func (d *Document) upsertChunkOp(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, raw json.RawMessage) (tools.Result, error) {
	res, err := d.upsertChunk(ctx, key, mscope, in)
	if err != nil || res.IsError {
		return res, err
	}
	var meta struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	_ = json.Unmarshal([]byte(res.Text), &meta)
	if meta.ID == "" || meta.Created {
		return res, nil
	}
	if jsonHasField(raw, "tags") {
		if terr := d.replaceChunkTags(ctx, key, meta.ID, in.Tags); terr != nil {
			return errFrom("upsert_chunk: tags: "+terr.Error(), terr), nil
		}
	}
	return res, nil
}

// mirrorRootFacets keeps the documents row's denormalized title/type/status in step
// with its root chunk. It is a no-op for a non-root chunk (the WHERE matches
// nothing), so callers can invoke it unconditionally after a chunk's
// title/type/status changes.
//
// The root chunk's title IS the document's name, but get_document,
// documents_summary, query_documents, export_md and title link resolution all
// read documents.title — so a rename that skipped this row changed nothing an
// agent could see. A blank root title keeps the row's last name instead:
// documents.title is NOT NULL, every listing shows it, and create_document never
// accepts an empty one either.
func (d *Document) mirrorRootFacets(ctx context.Context, key sqlmem.ScopeKey, chunkID string) error {
	return d.exec(ctx, key,
		`UPDATE documents SET type = (SELECT type FROM chunks WHERE id = ?),
		                      status = (SELECT status FROM chunks WHERE id = ?),
		                      title = COALESCE((SELECT title FROM chunks WHERE id = ? AND TRIM(title) <> ''), title),
		                      updated_at = ?
		 WHERE root_chunk_id = ?`,
		chunkID, chunkID, chunkID, time.Now().UnixNano(), chunkID)
}

// sanitizeTags trims, drops empties, and de-duplicates a tag list (order
// preserved). The composite PRIMARY KEY(<owner>, tag) rejects a duplicate insert,
// so the dedup is what lets a replace-set carry the caller's raw list safely. Tags
// are stored verbatim otherwise — including any '/', which is the nesting
// convention a prefix query walks.
func sanitizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// replaceChunkTags makes a chunk's tag set exactly `tags` (delete-all then
// insert). Not wrapped in a txn: chunk_tags rows are independent of the chunk row
// and the common chunk-write path is itself non-transactional, so this matches the
// surrounding write's atomicity.
func (d *Document) replaceChunkTags(ctx context.Context, key sqlmem.ScopeKey, chunkID string, tags []string) error {
	if err := d.exec(ctx, key, `DELETE FROM chunk_tags WHERE chunk_id = ?`, chunkID); err != nil {
		return err
	}
	for _, t := range sanitizeTags(tags) {
		if err := d.exec(ctx, key, `INSERT INTO chunk_tags (chunk_id, tag) VALUES (?, ?)`, chunkID, t); err != nil {
			return err
		}
	}
	return nil
}

// replaceDocumentTags makes a document's tag set exactly `tags` (delete-all then
// insert), the document-level twin of replaceChunkTags.
func (d *Document) replaceDocumentTags(ctx context.Context, key sqlmem.ScopeKey, docID string, tags []string) error {
	if err := d.exec(ctx, key, `DELETE FROM document_tags WHERE document_id = ?`, docID); err != nil {
		return err
	}
	for _, t := range sanitizeTags(tags) {
		if err := d.exec(ctx, key, `INSERT INTO document_tags (document_id, tag) VALUES (?, ?)`, docID, t); err != nil {
			return err
		}
	}
	return nil
}

// addTagRows inserts tags for a target idempotently (INSERT … WHERE NOT EXISTS —
// portable, no ON CONFLICT dialect split, mirroring linkChunks' existence-check
// style). table/keyCol are hardcoded literals ("chunk_tags"/"chunk_id" or
// "document_tags"/"document_id"), never caller input.
func (d *Document) addTagRows(ctx context.Context, key sqlmem.ScopeKey, table, keyCol, id string, tags []string) error {
	want := sanitizeTags(tags)
	if len(want) == 0 {
		return nil
	}
	// READ THEN WRITE, rather than one guarded INSERT per tag. The guard
	// (`WHERE NOT EXISTS`) is portable idempotence, not a correctness barrier — the
	// table's PRIMARY KEY (id, tag) is what actually enforces uniqueness — so the
	// same idempotence comes from filtering in Go against one read. Set-a-tag-set is
	// a routine call and this turns N round trips into 2.
	//
	// The race is UNCHANGED, not introduced: two concurrent adders could both pass
	// the old per-row NOT EXISTS and collide on the primary key just the same.
	res, err := d.query(ctx, key, `SELECT tag FROM `+table+` WHERE `+keyCol+` = ?`, id)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) > 0 {
			have[asStr(r[0])] = true
		}
	}
	missing := make([]string, 0, len(want))
	for _, t := range want {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	// Chunked so a caller handing in a very large tag set cannot build a statement
	// with more placeholders than the driver accepts.
	for _, batch := range batchIDs(missing, graphIDBatch) {
		if len(batch) == 0 {
			continue
		}
		vals := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch))
		for _, t := range batch {
			vals = append(vals, "(?, ?)")
			args = append(args, id, t)
		}
		stmt := `INSERT INTO ` + table + ` (` + keyCol + `, tag) VALUES ` + strings.Join(vals, ", ")
		if err := d.exec(ctx, key, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// removeTagRows deletes the given tags from a target (see addTagRows for the
// hardcoded-identifier note).
func (d *Document) removeTagRows(ctx context.Context, key sqlmem.ScopeKey, table, keyCol, id string, tags []string) error {
	drop := sanitizeTags(tags)
	if len(drop) == 0 {
		return nil
	}
	for _, batch := range batchIDs(drop, graphIDBatch) {
		if len(batch) == 0 {
			continue
		}
		args := make([]any, 0, len(batch))
		args = append(args, id)
		for _, t := range batch {
			args = append(args, t)
		}
		stmt := `DELETE FROM ` + table + ` WHERE ` + keyCol + ` = ? AND tag IN (` + placeholders(len(batch)) + `)`
		if err := d.exec(ctx, key, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

// listChunkTags / listDocumentTags return a target's tags, sorted.
func (d *Document) listChunkTags(ctx context.Context, key sqlmem.ScopeKey, chunkID string) ([]string, error) {
	return d.tagList(ctx, key, `SELECT tag FROM chunk_tags WHERE chunk_id = ? ORDER BY tag`, chunkID)
}

func (d *Document) listDocumentTags(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]string, error) {
	return d.tagList(ctx, key, `SELECT tag FROM document_tags WHERE document_id = ? ORDER BY tag`, docID)
}

func (d *Document) tagList(ctx context.Context, key sqlmem.ScopeKey, stmt, id string) ([]string, error) {
	res, err := d.query(ctx, key, stmt, id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, asStr(r[0]))
	}
	return out, nil
}

// documentExists reports whether a document row is present in this scope.
func (d *Document) documentExists(ctx context.Context, key sqlmem.ScopeKey, docID string) (bool, error) {
	res, err := d.query(ctx, key, `SELECT 1 FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0, nil
}

// jsonHasField reports whether a raw JSON object literally contains key — the
// presence test that distinguishes "field omitted" (leave as-is) from "field
// present but empty" (e.g. tags:[] clears). Mirrors the `present` map update_chunk
// builds, for a single-key check.
func jsonHasField(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func (d *Document) addTags(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	return d.mutateTags(ctx, key, in, true)
}

func (d *Document) removeTags(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	return d.mutateTags(ctx, key, in, false)
}

// mutateTags is the shared body of add_tags / remove_tags: it targets a chunk
// (id) or a document (document_id) — verifying the target exists in this scope
// first so no phantom tag rows are ever written — and returns the target's
// resulting tag set.
func (d *Document) mutateTags(ctx context.Context, key sqlmem.ScopeKey, in docInput, add bool) (tools.Result, error) {
	op := "remove_tags"
	if add {
		op = "add_tags"
	}
	if len(in.Tags) == 0 {
		return errValidation(op+": missing required field: tags", "Pass `tags`, a list of tag strings."), nil
	}
	apply := func(table, keyCol, id string) error {
		if add {
			return d.addTagRows(ctx, key, table, keyCol, id, in.Tags)
		}
		return d.removeTagRows(ctx, key, table, keyCol, id, in.Tags)
	}
	switch {
	case in.ID != "":
		if _, ok, err := d.getChunkRow(ctx, key, in.ID); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		} else if !ok {
			return errNotFound(op+": no such chunk: "+in.ID, "Find the chunk with op=query_chunks or op=search in the same scope."), nil
		}
		if err := apply("chunk_tags", "chunk_id", in.ID); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		tags, err := d.listChunkTags(ctx, key, in.ID)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		return jsonResult(map[string]any{"chunk_id": in.ID, "tags": tags})
	case in.DocumentID != "":
		if ok, err := d.documentExists(ctx, key, in.DocumentID); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		} else if !ok {
			return errNotFound(op+": no such document: "+in.DocumentID, "Find the document with op=query_documents in the same scope."), nil
		}
		if err := apply("document_tags", "document_id", in.DocumentID); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		tags, err := d.listDocumentTags(ctx, key, in.DocumentID)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		return jsonResult(map[string]any{"document_id": in.DocumentID, "tags": tags})
	default:
		return errValidation(op+": target a chunk (id) or a document (document_id)", ""), nil
	}
}

// listTags returns distinct tags with counts, sorted. Target is a chunk (id), a
// document (document_id), or — with neither — the whole scope, where the count is
// a tag's total usage across every chunk AND document in the scope.
func (d *Document) listTags(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	var stmt string
	var args []any
	switch {
	case in.ID != "":
		stmt = `SELECT tag, COUNT(*) AS count FROM chunk_tags WHERE chunk_id = ? GROUP BY tag ORDER BY tag`
		args = []any{in.ID}
	case in.DocumentID != "":
		stmt = `SELECT tag, COUNT(*) AS count FROM document_tags WHERE document_id = ? GROUP BY tag ORDER BY tag`
		args = []any{in.DocumentID}
	default:
		stmt = `SELECT tag, COUNT(*) AS count FROM (SELECT tag FROM chunk_tags UNION ALL SELECT tag FROM document_tags) t GROUP BY tag ORDER BY tag`
	}
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return errFrom("list_tags: "+err.Error(), err), nil
	}
	tags := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		tags = append(tags, map[string]any{"tag": asStr(r[0]), "count": asInt(r[1])})
	}
	return jsonResult(map[string]any{"tags": tags})
}

// queryDocuments filters the documents table by type / status / tag (via
// document_tags) / under_path (the Path tree, exactly like query_chunks), and
// returns the matching document rows.
//
// A caller whose access-control set hides some document of the scope has those
// rows withheld before the answer is cut at limit, reading on past them until it
// holds limit readable rows or the documents run out: dropped after LIMIT they
// would shorten the answer, telling the caller hidden documents exist. A caller
// the set limits nothing keeps the one query.
func (d *Document) queryDocuments(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	where := "1=1"
	args := []any{}
	if in.UnderPath != "" {
		docIDs, err := d.documentsUnderPath(ctx, key, in.UnderPath)
		if err != nil {
			return docFail("query_documents: ", err), nil
		}
		if len(docIDs) == 0 {
			return jsonResult(map[string]any{"documents": []any{}})
		}
		ph, pargs := inPlaceholders(docIDs)
		where += " AND id IN (" + ph + ")"
		args = append(args, pargs...)
	}
	if in.Type != "" {
		where += " AND type = ?"
		args = append(args, in.Type)
	}
	if in.Status != "" {
		where += " AND status = ?"
		args = append(args, in.Status)
	}
	if in.Tag != "" {
		where += " AND id IN (SELECT document_id FROM document_tags WHERE tag = ?)"
		args = append(args, in.Tag)
	}
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return errFrom("query_documents: "+err.Error(), err), nil
	}
	const cols = `SELECT id, title, type, status, root_chunk_id, created_at, updated_at FROM documents WHERE `
	if !limited {
		args = append(args, limit)
		res, err := d.query(ctx, key, cols+where+` ORDER BY title, id LIMIT ?`, args...)
		if err != nil {
			return errFrom("query_documents: "+err.Error(), err), nil
		}
		return queryDocumentsAnswer(res.Columns, res.Rows)
	}
	// A floor, so a small limit among many hidden documents is not a query per row.
	batch := max(limit, 100)
	var columns []string
	var rows [][]any
	var after []any // the last row scanned: its title and id
	for len(rows) < limit {
		w, a := where, append([]any{}, args...)
		if after != nil {
			w += " AND (title > ? OR (title = ? AND id > ?))"
			a = append(a, after[1], after[1], after[0])
		}
		a = append(a, batch)
		res, err := d.query(ctx, key, cols+w+` ORDER BY title, id LIMIT ?`, a...)
		if err != nil {
			return errFrom("query_documents: "+err.Error(), err), nil
		}
		columns = res.Columns
		for _, r := range res.Rows {
			ok, err := acl.readable(ctx, asStr(r[0]))
			if err != nil {
				return errFrom("query_documents: "+err.Error(), err), nil
			}
			if ok {
				rows = append(rows, r)
				if len(rows) == limit {
					break
				}
			}
		}
		// The store's row cap can cut a batch short; only an uncut short batch
		// is the end.
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < batch) {
			break
		}
		after = res.Rows[len(res.Rows)-1]
	}
	return queryDocumentsAnswer(columns, rows)
}

// queryDocumentsAnswer renders query_documents' rows.
func queryDocumentsAnswer(columns []string, rows [][]any) (tools.Result, error) {
	docs := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{}
		for i, c := range columns {
			if i < len(r) {
				m[c] = r[i]
			}
		}
		entry := map[string]any{
			"document_id":   asStr(m["id"]),
			"title":         asStr(m["title"]),
			"root_chunk_id": asStr(m["root_chunk_id"]),
			"created_at":    asInt(m["created_at"]),
			"updated_at":    asInt(m["updated_at"]),
		}
		if t := asStr(m["type"]); t != "" {
			entry["type"] = t
		}
		if s := asStr(m["status"]); s != "" {
			entry["status"] = s
		}
		docs = append(docs, entry)
	}
	return jsonResult(map[string]any{"documents": docs})
}

// --- change events ---

func (d *Document) publishChange(ctx context.Context, mscope store.MemoryScope, scopeID, documentID, op, chunkID string) {
	d.publishEdgeEvent(ctx, mscope, scopeID, documentID, map[string]any{"op": op, "chunk_id": chunkID, "actor": tools.RunIdentity(ctx).UserID})
}

// documentFeedPrefix and documentFeedSuffix frame a document's change feed,
// documents/<document_id>/chunks.
const (
	documentFeedPrefix = "documents/"
	documentFeedSuffix = "/chunks"
)

// IsDocumentFeedChannel reports whether name is a document's change feed. One
// exists for every document ever edited, none is declared, and only a viewer
// that knows the document reads it, so channel listings tag them apart from
// orphans rather than show hundreds of undeclared rows. documents/ingested is
// not one.
func IsDocumentFeedChannel(name string) bool {
	if !strings.HasPrefix(name, documentFeedPrefix) || !strings.HasSuffix(name, documentFeedSuffix) {
		return false
	}
	id := name[len(documentFeedPrefix) : len(name)-len(documentFeedSuffix)]
	return id != "" && !strings.Contains(id, "/")
}

// DocumentFeedIdleTTL is how long a document's change feed is kept after its
// last event. The feed is for a viewer following edits live, and nothing reads
// its history, so a day of quiet means nobody is following; a deleted
// document's feed (one whose delete did not purge it) also goes this way.
const DocumentFeedIdleTTL = 24 * time.Hour

// SweepDocumentFeeds purges every document change feed whose newest event is
// older than idle, across all tenants, and returns how many it purged. It
// reads only channel stats, never the documents, so it cannot tell a deleted
// document from a quiet one, and need not: a quiet feed's events have no
// reader. A feed whose purge fails is left for the next sweep.
func SweepDocumentFeeds(ctx context.Context, st store.Store, idle time.Duration, now time.Time) (int, error) {
	stats, err := st.ChannelStats(ctx)
	if err != nil {
		return 0, fmt.Errorf("document feed sweep: %w", err)
	}
	cutoff := now.Add(-idle)
	purged := 0
	for _, s := range stats {
		if !IsDocumentFeedChannel(s.Channel) || s.NewestVisibleAt.IsZero() || s.NewestVisibleAt.After(cutoff) {
			continue
		}
		if _, err := st.ChannelPurge(ctx, s.TenantID, s.Channel, ""); err != nil {
			slog.WarnContext(ctx, "document feed sweep: purge", "channel", s.Channel, "tenant", s.TenantID, "err", err)
			continue
		}
		purged++
	}
	return purged, nil
}

// publishEdgeEvent writes one event to a document's change channel; the
// timestamp is stamped here.
func (d *Document) publishEdgeEvent(ctx context.Context, mscope store.MemoryScope, scopeID, documentID string, event map[string]any) {
	if d.Bus == nil {
		return
	}
	event["timestamp"] = time.Now().UnixNano()
	payload, _ := json.Marshal(event)
	w := d.Writer
	if w == nil {
		w = &channels.StorePublisher{Store: d.Store, Bus: d.Bus}
	}
	// Best-effort change-event ring (cap 256). Errors are ignored — a missing
	// subscriber/declared channel must never fail a chunk mutation. The Web UI
	// subscriber arrives in a later phase.
	_, _ = w.Write(ctx, channels.WriteRequest{
		Channel:     documentFeedPrefix + documentID + documentFeedSuffix,
		TenantID:    tools.RunIdentity(ctx).TenantID, // RFC N: authoritative run tenant
		Scope:       mscope,
		ScopeID:     scopeID,
		Payload:     payload,
		PublishedBy: tools.RunIdentity(ctx).UserID,
		MaxMessages: 256,
	})
}

// nullIfEmpty returns nil for an empty string so it stores SQL NULL (rather
// than the empty string), keeping IS NULL / nullable-column semantics clean.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
