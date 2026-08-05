package builtin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/denn-gubsky/loomcycle/internal/channels"
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
}

func (d *Document) Name() string { return "Document" }

func (d *Document) Description() string {
	return "A chunked-graph document: each chunk is a first-class unit (UUID, hierarchy, type, fields, graph edges, Markdown body) that agents and humans co-author and query. Ops: create_document/get_document/documents_summary (per-document type/status + display metadata for a set of ids or a Path subtree)/query_documents (filter documents by type/status/tag/under_path)/delete_document/set_path, create_chunk (optional after_id inserts right after a sibling)/get_chunk/update_chunk/delete_chunk/move_chunk/reorder_chunk (move up|down within a level), link_chunks/unlink_chunks/get_edges (the cross-reference edges touching a document, each enriched with its endpoints' titles/types/statuses)/backlinks (the chunks that link TO a chunk — both manual links and inline [[name]] links)/related (the chunks whose bodies are semantically closest to a chunk's — ranked by score; needs a configured embedder)/unlinked_mentions (the chunks whose body text mentions a chunk's title but do NOT already link to it), history/get_version/diff (a chunk's body-change log: list the revisions its body changed at, read one revision's exact body, or unified-diff two of them), query_chunks (structured filters incl. tag/tag_prefix + a raw sql escape hatch), add_tags/remove_tags (incremental tags on a chunk (id) or document (document_id))/list_tags (distinct tags + counts for a chunk, a document, or the whole scope), define_type/list_types, set_asset (attach an image's bytes to a chunk → type=image, served by GET /v1/_document/asset/{id})/get_asset (asset metadata), export_md (render the document to Markdown), import_md (build a document from export_md-shaped Markdown), export_canvas (render the document as a JSON Canvas v1.0 spatial graph — content chunks as nodes + their cross-reference edges, for Obsidian Canvas / spatial views)/import_canvas (build a new document from a JSON Canvas). Scope is agent or user; documents are named in the Path tree (path:) — create_document defaults to /documents/<title> if you omit one, and set_path attaches/re-homes a path for an existing document."
}

// documentInputSchema is a package const so the LoomCycle MCP server can
// source the wrapper's advertised inputSchema verbatim (via
// MCPWrapperInputSchema) rather than restating it — the same pattern as
// memoryInputSchema.
const documentInputSchema = `{
	"type": "object",
	"properties": {
		"op":          {"type": "string", "enum": ["create_document","get_document","documents_summary","query_documents","delete_document","set_path","create_chunk","upsert_chunk","get_chunk","update_chunk","delete_chunk","supersede_chunk","graph_recall","list_facts","move_chunk","reorder_chunk","link_chunks","unlink_chunks","get_edges","query_chunks","add_tags","remove_tags","list_tags","define_type","list_types","set_asset","get_asset","export_md","import_md","export_canvas","import_canvas","history","get_version","diff","backlinks","related","unlinked_mentions"]},
		"scope":       {"type": "string", "enum": ["agent","user","tenant"], "description": "Which store (default user). agent = this agent; user = this end-user (needs a user_id on the run); tenant = shared by every user and agent in the tenant — anything written here is read by all of them, so use it for curated reference material, not for anything derived from untrusted text. tenant requires the operator to grant BOTH memory_scopes and sql_scopes with the tenant value."},
		"id":          {"type": "string", "description": "Document id (get/delete_document, set_path) or chunk id (get/update/delete/move_chunk)."},
		"path":        {"type": "string", "description": "create_document: name the doc in the Path tree (default /documents/<title> if omitted). set_path: the path to attach to an existing document (by id). get/delete_document: address by path instead of id."},
		"title":       {"type": "string"},
		"document_id": {"type": "string"},
		"document_ids": {"type": "array", "items": {"type": "string"}, "description": "documents_summary: the document ids to summarize (combine with or instead of under_path)."},
		"parent_id":   {"type": "string", "description": "create_chunk: parent chunk (omit for a child of the root)."},
		"new_parent_id": {"type": "string", "description": "move_chunk: the new parent."},
		"after_id":    {"type": "string", "description": "create_chunk: insert the new chunk immediately AFTER this sibling (same parent; shifts later siblings). Overrides parent_id/position."},
		"direction":   {"type": "string", "enum": ["up","down"], "description": "reorder_chunk: move the chunk up or down within its current level."},
		"type":        {"type": "string", "description": "Optional supertag-like chunk type. list_facts (browse the scope's facts — chunks that carry entity metadata — newest first, metadata only, no bodies): return only facts of this type."},
		"body":        {"type": "string", "description": "Markdown body."},
		"seed_ids":  {"type": "array", "items": {"type": "string"}, "description": "graph_recall: chunk ids to start from. Use this to hand in results you already found some other way (a Memory search, a previous recall) and follow the graph out from them."},
		"query":     {"type": "string", "description": "graph_recall: find starting chunks whose title matches this text. Use seed_ids instead when you already know where to start."},
		"hops":      {"type": "integer", "description": "graph_recall: how far to follow relations from each starting chunk. 0 = the starting chunks only, 1 = their neighbours (default), 2 = the maximum."},
		"as_of":     {"type": "integer", "description": "graph_recall / list_facts: answer as of this moment (unix nanos) instead of now — returns what was true then, including facts since corrected."},
		"include_retired": {"type": "boolean", "description": "graph_recall / list_facts: also return facts that have been superseded. Off by default, so you get only what is currently true."},
		"limit":     {"type": "integer", "description": "graph_recall: maximum chunks returned (default 50)."},
		"natural_key": {"type": "string", "description": "upsert_chunk: the stable identity of this entity or fact. Upserting twice with the same key updates ONE chunk instead of adding a second — use a derived form such as person:ada-lovelace, or subject|predicate|object for a fact. Unique within the scope."},
		"supersedes_id": {"type": "string", "description": "supersede_chunk: the id of the chunk being RETIRED by this one. The retired chunk is not deleted — it stays queryable so that questions about an earlier point in time still have an answer."},
		"valid_at":   {"type": "integer", "description": "When the fact became true IN THE WORLD (unix nanos). Defaults to now. Distinct from when it was recorded."},
		"invalid_at": {"type": "integer", "description": "When the fact STOPPED being true in the world (unix nanos). Leave unset for something still true."},
		"class":      {"type": "string", "enum": ["derived","evidential"], "description": "derived = distilled from something else (the default). evidential = source material, exempt from age-based pruning. list_facts: filter to facts of this class."},
		"confidence": {"type": "number", "description": "0..1, how sure you are of this fact."},
		"fields":      {"type": "object", "description": "Type-specific structured fields."},
		"status":      {"type": "string"},
		"position":    {"type": "integer"},
		"revision":    {"type": "integer", "description": "update_chunk: the chunk's current revision (optimistic concurrency). get_version: which body-change revision to read."},
		"from_revision": {"type": "integer", "description": "diff: the earlier revision to compare (from history)."},
		"to_revision":   {"type": "integer", "description": "diff: the later revision to compare (from history)."},
		"from_id":     {"type": "string"},
		"to_id":       {"type": "string"},
		"kind":        {"type": "string", "description": "link/unlink_chunks: edge kind (promotes/targets/...)."},
		"media_type":  {"type": "string", "description": "set_asset: the image MIME type (image/png, image/jpeg, image/gif, image/webp)."},
		"data":        {"type": "string", "description": "set_asset: the image bytes as standard base64 (no data: prefix)."},
		"filename":    {"type": "string", "description": "set_asset: an optional original filename (metadata only)."},
		"under_path":  {"type": "string", "description": "query_chunks / query_documents / documents_summary: restrict to documents at/under this Path-tree path."},
		"tags":        {"type": "array", "items": {"type": "string"}, "description": "The chunk's or document's tags. create_chunk/update_chunk/upsert_chunk/create_document replace-set the whole tag set (omit to leave unchanged; [] to clear). add_tags/remove_tags take the tags to add/remove incrementally. Nested tags use a slash: area/sub/topic."},
		"tag":         {"type": "string", "description": "query_chunks / query_documents: return only chunks/documents carrying exactly this tag."},
		"tag_prefix":  {"type": "string", "description": "query_chunks: return chunks whose tag equals this OR is nested under it (prefix + '/'), e.g. tag_prefix=area matches area and area/sub."},
		"sql":         {"type": "string", "description": "query_chunks: raw read-only SELECT against the chunk tables (escape hatch; validator-gated)."},
		"limit":       {"type": "integer"},
		"name":        {"type": "string", "description": "define/list_types: the type name."},
		"include_metadata": {"type": "boolean", "description": "export_md: embed round-trippable chunk metadata + edges as HTML comments (default true). false = clean human-facing Markdown."},
		"markdown":    {"type": "string", "description": "import_md: an export_md-shaped Markdown document (headings = hierarchy; <!-- loom: ... --> metadata; <!-- loom-edges: ... --> trailer). Omit document_id to create a new document; pass document_id (+ optional parent_id) to import under an existing chunk."},
		"canvas":      {"type": "object", "description": "import_canvas: a JSON Canvas v1.0 object ({\"nodes\":[...],\"edges\":[...]}) — e.g. the contents of an Obsidian .canvas file. Each node becomes a chunk (positioned via its x/y/width/height); each edge becomes a link. Pass title/path to name the new document."}
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
	Position  *int            `json:"position"`
	Revision  *int            `json:"revision"`
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
	// SupersedesID names the chunk being retired by supersede_chunk. It is an
	// explicitly-named field rather than a reuse of from_id/to_id on purpose: a
	// caller who transposed those would invalidate the NEW fact and leave the stale
	// one current — a silent inversion of history, which is worse than one more
	// field to document.
	SupersedesID string `json:"supersedes_id"`
	// Pointers so "unset" is distinguishable from zero — valid_at=0 is a real
	// instant (the unix epoch), not a missing value.
	ValidAt    *int64   `json:"valid_at"`
	InvalidAt  *int64   `json:"invalid_at"`
	Confidence *float64 `json:"confidence"`
	// Class is 'derived' | 'evidential' — the retention-exemption signal.
	Class string `json:"class"`

	// graph_recall inputs.
	SeedIDs        []string `json:"seed_ids"`
	Query          string   `json:"query"`
	Hops           *int     `json:"hops"`
	AsOf           *int64   `json:"as_of"`
	IncludeRetired bool     `json:"include_retired"`
	// MediaType/Data/Filename carry an image asset for set_asset (RFC BO). Data
	// is standard base64 (no data: prefix); it is decoded to raw bytes and stored
	// in the chunk_assets BYTEA/BLOB table.
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	Filename  string `json:"filename"`
	UnderPath string `json:"under_path"`
	SQL       string `json:"sql"`
	Limit     int    `json:"limit"`
	Name      string `json:"name"`
	// IncludeMetadata gates export_md's round-trip comments (default true when
	// omitted; a pointer so an explicit `false` is distinguishable from unset).
	IncludeMetadata *bool `json:"include_metadata"`
	// Markdown is the import_md source (an export_md-shaped document).
	Markdown string `json:"markdown"`
	// Canvas is the import_canvas source: a JSON Canvas v1.0 object
	// ({"nodes":[...],"edges":[...]}). Kept as RawMessage so a caller can hand in
	// an object verbatim (e.g. the contents of an Obsidian .canvas file) and it is
	// decoded into the canvas struct in import_canvas.
	Canvas json.RawMessage `json:"canvas"`
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
		created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, revision INTEGER NOT NULL DEFAULT 1)`,
	`CREATE TABLE IF NOT EXISTS chunk_edges (
		from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL,
		created_at BIGINT NOT NULL, PRIMARY KEY (from_id, to_id, kind))`,
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
		natural_key TEXT)`,
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
		return errResult("Document tool: not configured — requires the Store backend and SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)"), nil
	}
	var in docInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input JSON: " + err.Error()), nil
	}
	key, mscope, err := d.resolveScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return errResult("document: schema init: " + err.Error()), nil
	}

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
	case "list_facts":
		return d.listFacts(ctx, key, in)
	case "link_chunks":
		return d.linkChunks(ctx, key, in)
	case "unlink_chunks":
		return d.unlinkChunks(ctx, key, in)
	case "get_edges":
		return d.getEdges(ctx, key, in)
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
	case "related":
		return d.related(ctx, key, mscope, in)
	case "unlinked_mentions":
		return d.unlinkedMentions(ctx, key, mscope, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q", in.Op)), nil
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
	switch requested {
	case "agent":
		name := tools.AgentName(ctx)
		if name == "" {
			return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: scope=agent requires a yaml-declared agent")
		}
		return sqlmem.ScopeKey{Tenant: sqlTenant, Scope: "agent", ScopeID: name}, store.MemoryScopeAgent, nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: scope=user requires a user_id on the run")
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
		if !contains(tools.MemoryPolicy(ctx).AllowedScopes, "tenant") {
			missing = append(missing, "memory_scopes: [tenant]")
		}
		if !contains(tools.SqlMemPolicy(ctx).AllowedScopes, "tenant") {
			missing = append(missing, "sql_scopes: [tenant]")
		}
		if len(missing) > 0 {
			return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: scope=tenant is not granted to this agent — "+
				"a tenant document is readable and writable by every user and agent in the tenant, so the "+
				"operator must opt in. Missing on the agent: %s (a document needs both — structure lives in "+
				"SQL Memory, chunk bodies in Memory)", strings.Join(missing, " and "))
		}
		return sqlmem.ScopeKey{Tenant: sqlTenant, Scope: "tenant", ScopeID: sqlTenant}, store.MemoryScopeTenant, nil
	default:
		return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: unknown scope %q (agent | user | tenant)", requested)
	}
}

// direntTenant is the tenant used for Path-tree dirents — the RAW
// RunIdentity tenant (NOT the SQL-canonicalized one), so Document's document
// dirents share the same namespace as the Path/Memory/Volume dirents (which
// all key on the raw tenant). In open mode this is "" for dirents while SQL
// uses "default"; both consistently represent the single/default tenant.
func direntTenant(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID }

func (d *Document) ensureSchema(ctx context.Context, key sqlmem.ScopeKey) error {
	for _, ddl := range docSchemaDDL {
		if _, err := d.SqlMem.Exec(ctx, key, ddl, nil, 0); err != nil {
			return err
		}
	}
	// chunk_assets (RFC BO) holds image chunk bytes as TRUE binary. The binary
	// column type is the ONE non-portable part of the doc schema — sqlite BLOB vs
	// postgres BYTEA — so it is built per-tier here rather than in the static
	// docSchemaDDL. Bytes are bound/scanned as []byte; base64 exists only on the
	// wire (set_asset payload / export_md data-URL), decoded before storage.
	binType := "BLOB"
	if d.SqlMem.Tier() == "postgres" {
		binType = "BYTEA"
	}
	assetDDL := `CREATE TABLE IF NOT EXISTS chunk_assets (
		chunk_id TEXT PRIMARY KEY, media_type TEXT NOT NULL, bytes ` + binType + ` NOT NULL,
		size BIGINT NOT NULL, created_at BIGINT NOT NULL)`
	if _, err := d.SqlMem.Exec(ctx, key, assetDDL, nil, 0); err != nil {
		return err
	}
	if err := d.migrateConfidencePrecision(ctx, key); err != nil {
		return err
	}
	if err := d.migrateDocumentFacets(ctx, key); err != nil {
		return err
	}
	if err := d.migrateAssetDescription(ctx, key); err != nil {
		return err
	}
	return d.migrateEdgeAuto(ctx, key)
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
	if d.SqlMem.Tier() != "postgres" {
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
	_, err = d.SqlMem.Exec(ctx, key,
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
func (d *Document) tableHasColumn(ctx context.Context, key sqlmem.ScopeKey, table, col string) bool {
	_, err := d.query(ctx, key, `SELECT `+col+` FROM `+table+` WHERE 1=0`)
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
	scopeID := key.ScopeID
	v, _ := json.Marshal(chunkBody{Body: body, Fields: fields})
	// RFC BL: key chunk bodies on the same tenant the document's dirent + SQL
	// structure use (direntTenant = the raw RunIdentity tenant), so bodies and
	// structure never drift across the tenant axis.
	tenant := direntTenant(ctx)
	bodyKey := chunkBodyKey(chunkID)
	if err := d.Store.MemorySet(ctx, tenant, mscope, scopeID, bodyKey, v, 0); err != nil {
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

// embedBody indexes a chunk body for semantic search, best-effort.
//
// WHAT text gets embedded is per chunk type:
//
//   - prose   → the body verbatim
//   - mermaid → extracted labels + diagram kind (see document_mermaid.go).
//     Diagram SOURCE tokenises to `graph TD`, `-->`, `[`, which carry no meaning.
//   - image   → SKIPPED until phase 4. The body is a caption or a rendered media
//     form; the searchable text is a generated description that does not exist yet.
//
// THE TYPE IS PASSED IN, NOT SNIFFED FROM THE BODY. An earlier version classified
// with classifyMediaBody, which recognises only the ```mermaid FENCED form — but a
// mermaid chunk STORES its bare source (export re-adds the fence), so a natively
// created diagram was embedded as raw source while an imported one was skipped.
// Sniffing cannot be repaired either: `mermaidKindRe` on a first line would match a
// prose chunk that happens to open with the word "pie". The chunk type is the only
// authoritative answer, so callers supply it.
func (d *Document) embedBody(ctx context.Context, tenant string, mscope store.MemoryScope, key sqlmem.ScopeKey, bodyKey, chunkID, chunkType, body string) {
	scopeID := key.ScopeID
	if d.Embedder == nil {
		return
	}
	// DERIVE THE TEXT FIRST, THEN CHECK IT — never guard on the raw body. An earlier
	// version returned early on an empty body before this switch ran, which made an
	// UNCAPTIONED image permanently unsearchable: its body is empty by definition, so
	// the generated description was never consulted no matter how many times a
	// describe pass wrote one. The body is only one of the sources here; for an image
	// it may not be a source at all.
	var text string
	switch chunkType {
	case "image":
		// The body is the CAPTION (export renders it as the alt text) and the
		// description comes from the asset row, so an image with neither is what
		// yields "" — not an image with no caption.
		text = imageEmbedText(body, d.assetDescription(ctx, key, chunkID))
	case "mermaid":
		text = mermaidEmbedText(body)
	default:
		// A body that is ENTIRELY a fenced diagram or a data-URL image, on a chunk
		// whose type was not set: the same predicate export/import uses, so the two
		// paths cannot disagree about what a media body is.
		if typ, _, _, src := classifyMediaBody(body); typ != "" {
			if typ != "mermaid" {
				return
			}
			text = mermaidEmbedText(src)
		} else {
			// indexableText drops a body that is nothing but Markdown scaffolding
			// ("```sh", "---", "#"), which a heading-split import turns into its own
			// chunk. Such a body ranks mid-high for EVERY query, so it does not just
			// waste a row — it outranks real answers.
			text = indexableText(body)
		}
	}
	// FALL BACK TO THE TITLE. A chunk whose body yields no text is usually a heading
	// that organises the document — "RFC BE — History Tool (browse / search / rename
	// / annotate past chats)", "Phase 2 — name-links + transclusion". That is real
	// language and a real answer to a search; excluding it means the most navigable
	// part of a document is the one part retrieval cannot see.
	//
	// Measured on the reference deployment before adding this: of 20 sampled bodyless
	// chunks, 18 had meaningful titles. The two that did not were fragments used as
	// headings (a JSON line). No content heuristic beyond requiring a letter — see
	// indexableText — because a filter guessing at "meaningful" would drop real
	// headings to avoid an occasional weak vector, and 18-for-2 is the wrong trade to
	// optimise against.
	//
	// The title is read HERE rather than passed in: it costs a query only on this
	// path (the body was empty), and embedBody already reads SQL for an image's
	// description, so the seam exists.
	if text == "" {
		text = indexableText(d.chunkTitle(ctx, key, chunkID))
	}
	// Still nothing: no body text AND no usable title. Embedding punctuation — or a
	// placeholder — is worse than embedding nothing, because a row that exists ranks
	// against every query.
	if text == "" {
		return
	}
	vec, err := d.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vec) == 0 {
		// One log line, not a returned error — see writeBody. The admin re-embed
		// surface is how an operator recovers a scope that was written while the
		// embedder was down.
		log.Printf("document: embed body failed for chunk %s: %v", chunkID, err)
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
		log.Printf("document: store embedding failed for chunk %s: %v", chunkID, err)
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
func (d *Document) readBody(ctx context.Context, mscope store.MemoryScope, scopeID, chunkID string) (chunkBody, error) {
	entry, err := d.Store.MemoryGet(ctx, direntTenant(ctx), mscope, scopeID, chunkBodyKey(chunkID))
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

// TitleFallbackForBodyKey resolves the title-derived embed text for a bodyless
// document chunk, given only its k/v key.
//
// Exported for the admin embedding backfill, which sees memory rows rather than
// chunks and therefore cannot reach a title on its own. It lives here so the
// ""→"default" SQL-tenant rule and the title-quality judgement stay in one package —
// duplicating either at the call site is how the tenant axis drifts (chunk bodies
// key on the RAW tenant while SQL Memory canonicalises it, a seam that has produced
// silent cross-tenant bugs before).
//
// Returns "" for a non-chunk key, a missing chunk, an unusable title, or no SQL
// Memory — every case the caller should treat as "nothing to embed".
func TitleFallbackForBodyKey(ctx context.Context, mgr *sqlmem.Manager, tenant string,
	mscope store.MemoryScope, scopeID, memKey string) string {

	chunkID := ChunkIDFromBodyKey(memKey)
	if chunkID == "" || mgr == nil {
		return ""
	}
	d := &Document{SqlMem: mgr}
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenantValue(tenant), Scope: string(mscope), ScopeID: scopeID}
	return indexableText(d.chunkTitle(ctx, key, chunkID))
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

// exec/query run the tool's OWN statements (written with portable `?`
// placeholders) — Rebind converts `?`→`$N` on the postgres tier. The raw `sql:`
// escape hatch does NOT go through these (it calls the Manager directly with
// the model's dialect-native SQL).
func (d *Document) exec(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) error {
	_, err := d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(stmt), args, 0)
	return err
}

func (d *Document) query(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) (*sqlmem.QueryResult, error) {
	return d.SqlMem.Query(ctx, key, d.SqlMem.Rebind(stmt), args)
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
	if _, err := d.SqlMem.BeginTxn(ctx, txnID, tools.RunIdentity(ctx).RootRunID, key); err != nil {
		return err
	}
	if err := fn(txnID); err != nil {
		_, _ = d.SqlMem.RollbackTxn(txnID)
		return err
	}
	if _, err := d.SqlMem.CommitTxn(txnID); err != nil {
		_, _ = d.SqlMem.RollbackTxn(txnID)
		return err
	}
	return nil
}

func (d *Document) execTxn(ctx context.Context, txnID, stmt string, args ...any) error {
	_, err := d.SqlMem.ExecTxn(ctx, txnID, d.SqlMem.Rebind(stmt), args, 0)
	return err
}

func (d *Document) queryTxn(ctx context.Context, txnID, stmt string, args ...any) (*sqlmem.QueryResult, error) {
	return d.SqlMem.QueryTxn(ctx, txnID, d.SqlMem.Rebind(stmt), args)
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
}

const chunkSelectCols = `id, document_id, parent_id, position, type, status, title, revision`

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
	res, err := d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(`UPDATE chunks SET status = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`), []any{nullIfEmpty(status), now, chunkID, row.Revision}, 0)
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
	d.publishChange(ctx, mscope, key.ScopeID, row.DocumentID, "update_chunk", chunkID)
	return nil
}

// --- ops: document lifecycle ---

func (d *Document) createDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.Title == "" {
		return errResult("create_document: missing required field: title"), nil
	}
	now := time.Now().UnixNano()
	docID := newDocID()
	rootID := newDocID()
	// type/status are set on the root chunk (the authoritative kind/state) AND
	// mirrored to the documents row (the denormalized copy query_documents /
	// get_document read). They start in sync here; update_chunk keeps them so.
	if err := d.exec(ctx, key, `INSERT INTO documents (id, title, root_chunk_id, type, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		docID, in.Title, rootID, nullIfEmpty(in.Type), nullIfEmpty(in.Status), now, now); err != nil {
		return errResult("create_document: " + err.Error()), nil
	}
	// The root chunk anchors the hierarchy (parent_id NULL).
	if err := d.exec(ctx, key, `INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, NULL, 0, ?, ?, ?, ?, ?, 1)`,
		rootID, docID, nullIfEmpty(in.Type), nullIfEmpty(in.Status), in.Title, now, now); err != nil {
		return errResult("create_document: root chunk: " + err.Error()), nil
	}
	if err := d.writeBody(ctx, mscope, key, rootID, "", "", nil); err != nil {
		return errResult("create_document: root body: " + err.Error()), nil
	}
	// A document's tags are its own (independent of the root chunk's tags).
	if len(in.Tags) > 0 {
		if err := d.replaceDocumentTags(ctx, key, docID, in.Tags); err != nil {
			return errResult("create_document: tags: " + err.Error()), nil
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
	docPath := in.Path
	if docPath == "" {
		docPath = "/documents/" + docDefaultPathSegment(in.Title, docID)
	}
	if p, perr := d.registerDocDirent(ctx, key, docID, docPath); perr != nil {
		resp["path_warning"] = "document created but path registration failed: " + perr.Error()
	} else {
		resp["path"] = p
	}
	return jsonResult(resp)
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
		return errResult("set_path: missing required field: id (the document_id)"), nil
	}
	if in.Path == "" {
		return errResult("set_path: missing required field: path"), nil
	}
	// Verify the document exists in THIS scope — never name a phantom (and so a
	// cross-scope id opaquely 404s rather than creating a dangling dirent).
	res, err := d.query(ctx, key, `SELECT id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errResult("set_path: " + err.Error()), nil
	}
	if len(res.Rows) == 0 {
		return errResult("set_path: no such document: " + docID), nil
	}
	p, perr := d.registerDocDirent(ctx, key, docID, in.Path)
	if perr != nil {
		return errResult("set_path: " + perr.Error()), nil
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

// registerDocDirent names a document in the Path tree (a `document` dirent).
func (d *Document) registerDocDirent(ctx context.Context, key sqlmem.ScopeKey, docID, rawPath string) (string, error) {
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return "", err
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return "", fmt.Errorf("path may not be the root")
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
	if in.ID != "" {
		return in.ID, nil
	}
	if in.Path == "" {
		return "", fmt.Errorf("missing required field: id (or path)")
	}
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return "", err
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return "", fmt.Errorf("path may not be the root")
	}
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return "", fmt.Errorf("no such path: %s", canonical)
		}
		return "", err
	}
	if row.Kind != "document" {
		return "", fmt.Errorf("path %s is a %s, not a document", canonical, row.Kind)
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
		return errResult("get_document: " + err.Error()), nil
	}
	res, err := d.query(ctx, key, `SELECT id, title, root_chunk_id, type, status, created_at, updated_at FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errResult("get_document: " + err.Error()), nil
	}
	if len(res.Rows) == 0 {
		return errResult("get_document: no such document: " + docID), nil
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
		return errResult("get_document: tags: " + terr.Error()), nil
	}
	if len(tags) > 0 {
		resp["tags"] = tags
	}
	if rootID != "" {
		// Colour is decoration, so a read fault degrades to "no scheme" rather
		// than failing the whole get — matching the getChunkRow probe above.
		// Content reads (get_chunk, export_md) propagate instead.
		rootBody, _ := d.readBody(ctx, mscope, key.ScopeID, rootID)
		enabled, scheme := docColorMeta(rootBody)
		resp["color_enabled"] = enabled
		if len(scheme) > 0 {
			resp["color_scheme"] = scheme
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
	ids := in.DocumentIDs
	if in.UnderPath != "" {
		under, err := d.documentsUnderPath(ctx, key, in.UnderPath)
		if err != nil {
			return errResult("documents_summary: " + err.Error()), nil
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
		return jsonResult(map[string]any{"documents": []any{}})
	}
	// Batch: the documents rows.
	ph, args := inPlaceholders(uniq)
	dres, err := d.query(ctx, key, `SELECT id, title, root_chunk_id FROM documents WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return errResult("documents_summary: " + err.Error()), nil
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
			return errResult("documents_summary: " + err.Error()), nil
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
			rootBody, _ := d.readBody(ctx, mscope, key.ScopeID, dm.root)
			enabled, scheme := docColorMeta(rootBody)
			entry["color_enabled"] = enabled
			if len(scheme) > 0 {
				entry["color_scheme"] = scheme
			}
		}
		out = append(out, entry)
	}
	return jsonResult(map[string]any{"documents": out})
}

func (d *Document) deleteDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return errResult("delete_document: " + err.Error()), nil
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
		if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_edges WHERE from_id IN (SELECT id FROM chunks WHERE document_id = ?) OR to_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID, docID); err != nil {
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
		return errResult("delete_document: " + txErr.Error()), nil
	}
	// Bodies AFTER commit (separate store; best-effort — an orphaned body is
	// invisible dead k/v, never reachable once its chunk row is gone).
	for _, id := range ids {
		_, _ = d.Store.MemoryDelete(ctx, direntTenant(ctx), mscope, key.ScopeID, chunkBodyKey(id))
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
	return jsonResult(map[string]any{"deleted": true, "document_id": docID, "n_chunks_deleted": n})
}

// --- ops: chunk lifecycle ---

func (d *Document) createChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.DocumentID == "" {
		return errResult("create_chunk: missing required field: document_id"), nil
	}
	if in.Title == "" {
		return errResult("create_chunk: missing required field: title"), nil
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
	// An empty parent_id still means "child of the root", so nothing legitimate is
	// narrowed.
	if in.ParentID != "" {
		par, ok, perr := d.getChunkRow(ctx, key, in.ParentID)
		if perr != nil {
			return errResult("create_chunk: parent lookup: " + perr.Error()), nil
		}
		if !ok {
			return errResult("create_chunk: no such parent_id: " + in.ParentID +
				" (the chunk would be unreachable from the document root; omit parent_id " +
				"to attach it to the root)"), nil
		}
		if par.DocumentID != in.DocumentID {
			return errResult(fmt.Sprintf(
				"create_chunk: parent_id %q belongs to document %q, not %q — a chunk cannot be "+
					"parented across documents", in.ParentID, par.DocumentID, in.DocumentID)), nil
		}
	}
	pos := 0
	if in.AfterID != "" {
		sib, ok, err := d.getChunkRow(ctx, key, in.AfterID)
		if err != nil {
			return errResult("create_chunk: " + err.Error()), nil
		}
		if !ok {
			return errResult("create_chunk: no such after_id chunk: " + in.AfterID), nil
		}
		if sib.DocumentID != in.DocumentID {
			return errResult("create_chunk: after_id belongs to a different document"), nil
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
		return exec(`INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			id, in.DocumentID, nullIfEmpty(parentID), pos, nullIfEmpty(in.Type), nullIfEmpty(in.Status), in.Title, now, now)
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
		return errResult("create_chunk: " + insErr.Error()), nil
	}
	if err := d.writeBody(ctx, mscope, key, id, in.Type, in.Body, in.Fields); err != nil {
		return errResult("create_chunk: body: " + err.Error()), nil
	}
	// Seed the body-change log at revision 1 (RFC BS Phase 3a) — the chunk's body
	// exists as of now, so it is the first snapshot even when in.Body is empty.
	if err := d.recordRevision(ctx, key, id, 1, in.Body); err != nil {
		return errResult("create_chunk: history: " + err.Error()), nil
	}
	// Derive this chunk's inline [[name]] link edges from its body (RFC BS Phase
	// 2a). A body with no resolvable name-links materializes no edges.
	if err := d.reconcileNameLinks(ctx, key, id, in.Body); err != nil {
		return errResult("create_chunk: name links: " + err.Error()), nil
	}
	// Tags on a fresh chunk (also serves upsert_chunk's create path, which
	// delegates here). replaceChunkTags is delete-then-insert; on a new id the
	// delete matches nothing, so len>0 keeps an untagged create from a needless
	// write.
	if len(in.Tags) > 0 {
		if err := d.replaceChunkTags(ctx, key, id, in.Tags); err != nil {
			return errResult("create_chunk: tags: " + err.Error()), nil
		}
	}
	d.publishChange(ctx, mscope, key.ScopeID, in.DocumentID, "create_chunk", id)
	return d.getChunk(ctx, key, mscope, docInput{ID: id})
}

func (d *Document) getChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("get_chunk: missing required field: id"), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("get_chunk: " + err.Error()), nil
	}
	if !ok {
		return errResult("get_chunk: no such chunk: " + in.ID), nil
	}
	// Surface a body-read fault instead of returning the chunk with an empty
	// body: a silent empty read is indistinguishable from a deleted body, which
	// is exactly how the v1.33.0 tenant regression went unnoticed.
	cb, err := d.readBody(ctx, mscope, key.ScopeID, in.ID)
	if err != nil {
		return errResult("get_chunk: body: " + err.Error()), nil
	}
	resp := chunkResponse(row, cb)
	// The chunk's tags (RFC BS) — the primary read surfaces them so a reader (and
	// the editor's load) sees a chunk's tags without a separate list_tags call.
	if tags, terr := d.listChunkTags(ctx, key, in.ID); terr != nil {
		return errResult("get_chunk: tags: " + terr.Error()), nil
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
	if meta, ok, merr := d.readChunkMeta(ctx, key, in.ID); merr != nil {
		return errResult("get_chunk: entity: " + merr.Error()), nil
	} else if ok {
		resp["entity"] = chunkMetaToJSON(meta)
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
	return out
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
		return errResult("set_asset: missing required field: id (the chunk id)"), nil
	}
	if in.MediaType == "" {
		return errResult("set_asset: missing required field: media_type"), nil
	}
	if !validImageMediaTypes[in.MediaType] {
		return errResult(fmt.Sprintf("set_asset: unsupported media_type %q (allowed: image/png, image/jpeg, image/gif, image/webp)", in.MediaType)), nil
	}
	if in.Data == "" {
		return errResult("set_asset: missing required field: data (base64 image bytes)"), nil
	}
	raw, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil {
		return errResult("set_asset: data must be valid standard base64 (no data: prefix): " + err.Error()), nil
	}
	if len(raw) == 0 {
		return errResult("set_asset: empty image data"), nil
	}
	if len(raw) > d.maxAssetBytes() {
		return errResult(fmt.Sprintf("set_asset: image is %d bytes, exceeds the %d-byte cap", len(raw), d.maxAssetBytes())), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("set_asset: " + err.Error()), nil
	}
	if !ok {
		return errResult("set_asset: no such chunk: " + in.ID), nil
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
		return errResult("set_asset: " + txErr.Error()), nil
	}
	// Record the asset facts in the chunk's fields (merged — keep any existing
	// keys + the caption body). Fields live in Memory alongside the body.
	// This is a fields-only write, but writeBody rewrites the body too — so a
	// failed read here would blank the caption. Refuse instead of persisting a
	// body we could not read.
	cb, rerr := d.readBody(ctx, mscope, key.ScopeID, in.ID)
	if rerr != nil {
		return errResult("set_asset: body: " + rerr.Error()), nil
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
		return errResult("set_asset: fields: " + err.Error()), nil
	}
	// set_asset sets type='image' on the chunk; if that chunk is a document's root,
	// mirror it onto the documents row so get_document/query_documents stay in sync
	// (a no-op for a non-root chunk). RFC BS.
	if err := d.mirrorRootFacets(ctx, key, in.ID); err != nil {
		return errResult("set_asset: " + err.Error()), nil
	}
	d.publishChange(ctx, mscope, key.ScopeID, row.DocumentID, "set_asset", in.ID)
	return d.getChunk(ctx, key, mscope, docInput{ID: in.ID})
}

// getAsset returns an image chunk's asset METADATA (media_type, size) — never
// the bytes (those come from GET /v1/_document/asset/{id}).
func (d *Document) getAsset(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("get_asset: missing required field: id (the chunk id)"), nil
	}
	mt, sz, ok := d.assetMeta(ctx, key, in.ID)
	if !ok {
		return errResult("get_asset: no asset on chunk: " + in.ID), nil
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
		return errResult("update_chunk: missing required field: id"), nil
	}
	if in.Revision == nil {
		return errResult("update_chunk: missing required field: revision (optimistic concurrency — pass the chunk's current revision)"), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("update_chunk: " + err.Error()), nil
	}
	if !ok {
		return errResult("update_chunk: no such chunk: " + in.ID), nil
	}
	if row.Revision != *in.Revision {
		return errResult(fmt.Sprintf("update_chunk: revision conflict (you passed %d, current is %d) — re-read the chunk and retry", *in.Revision, row.Revision)), nil
	}
	// Claim the update ATOMICALLY first: the guarded bump only matches if the
	// revision is still what we read. If a concurrent writer raced us, it
	// matches 0 rows → conflict, and we bail BEFORE clobbering the body (the
	// fix for the silent lost-update: the read-check above is advisory; THIS is
	// the real gate).
	now := time.Now().UnixNano()
	bumped, err := d.SqlMem.Exec(ctx, key, d.SqlMem.Rebind(`UPDATE chunks SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`), []any{now, in.ID, *in.Revision}, 0)
	if err != nil {
		return errResult("update_chunk: " + err.Error()), nil
	}
	if bumped.RowsAffected == 0 {
		return errResult(fmt.Sprintf("update_chunk: revision conflict (revision %d was changed by a concurrent write) — re-read the chunk and retry", *in.Revision)), nil
	}
	// Detect which fields the caller actually provided (presence-based; lets a
	// field be set to empty, unlike a zero-value check).
	var present map[string]json.RawMessage
	_ = json.Unmarshal(raw, &present)
	if _, has := present["title"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET title = ? WHERE id = ?`, in.Title, in.ID); err != nil {
			return errResult("update_chunk: " + err.Error()), nil
		}
	}
	if _, has := present["type"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET type = ? WHERE id = ?`, nullIfEmpty(in.Type), in.ID); err != nil {
			return errResult("update_chunk: " + err.Error()), nil
		}
	}
	if _, has := present["status"]; has {
		if err := d.exec(ctx, key, `UPDATE chunks SET status = ? WHERE id = ?`, nullIfEmpty(in.Status), in.ID); err != nil {
			return errResult("update_chunk: " + err.Error()), nil
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
			stored, rerr := d.readBody(ctx, mscope, key.ScopeID, in.ID)
			if rerr != nil {
				return errResult("update_chunk: body: " + rerr.Error()), nil
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
			return errResult("update_chunk: body: " + err.Error()), nil
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
				return errResult("update_chunk: history: " + err.Error()), nil
			}
			if err := d.reconcileNameLinks(ctx, key, in.ID, cb.Body); err != nil {
				return errResult("update_chunk: name links: " + err.Error()), nil
			}
		}
	}
	// Replace-set the chunk's tags only when `tags` was supplied — a present `[]`
	// clears, an absent key leaves them untouched (same presence rule as above).
	if _, has := present["tags"]; has {
		if err := d.replaceChunkTags(ctx, key, in.ID, in.Tags); err != nil {
			return errResult("update_chunk: tags: " + err.Error()), nil
		}
	}
	// If this chunk is a document's root and its type/status just changed, mirror
	// the new facets onto the documents row (a no-op for a non-root chunk).
	_, hasType := present["type"]
	_, hasStatus := present["status"]
	if hasType || hasStatus {
		if err := d.mirrorRootFacets(ctx, key, in.ID); err != nil {
			return errResult("update_chunk: " + err.Error()), nil
		}
	}
	d.publishChange(ctx, mscope, key.ScopeID, row.DocumentID, "update_chunk", in.ID)
	return d.getChunk(ctx, key, mscope, docInput{ID: in.ID})
}

func (d *Document) deleteChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("delete_chunk: missing required field: id"), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("delete_chunk: " + err.Error()), nil
	}
	if !ok {
		return errResult("delete_chunk: no such chunk: " + in.ID), nil
	}
	// Refuse to delete a document's ROOT chunk — that would orphan the
	// documents row (root_chunk_id dangling, zero chunks). Use delete_document.
	// Fail CLOSED if the lookup errors: a guard that silently skips on a query
	// fault would let exactly the orphan it protects against slip through.
	if rr, rerr := d.query(ctx, key, `SELECT 1 FROM documents WHERE root_chunk_id = ? LIMIT 1`, in.ID); rerr != nil {
		return errResult("delete_chunk: " + rerr.Error()), nil
	} else if len(rr.Rows) > 0 {
		return errResult("delete_chunk: refusing to delete a document's root chunk — use delete_document"), nil
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
		for _, cid := range ids {
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_edges WHERE from_id = ? OR to_id = ?`, cid, cid); err != nil {
				return err
			}
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_assets WHERE chunk_id = ?`, cid); err != nil {
				return err
			}
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_tags WHERE chunk_id = ?`, cid); err != nil {
				return err
			}
			// The sidecar's SECOND cascade site. Both are required: delete_document
			// walks by document, this walks the descendant set of one chunk, and a
			// sidecar row reachable only through the path that was missed is an orphan
			// nothing can see.
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_memory_meta WHERE chunk_id = ?`, cid); err != nil {
				return err
			}
			// The body-change log for this chunk (RFC BS Phase 3a). Before the chunk
			// row goes, like the other per-chunk cascades — an orphaned revision row
			// is invisible dead data nothing reaps.
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_revisions WHERE chunk_id = ?`, cid); err != nil {
				return err
			}
			// The canvas layout row for this chunk — the descendant-walk cascade site
			// (delete_document walks by document; this walks one chunk's subtree).
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunk_layout WHERE chunk_id = ?`, cid); err != nil {
				return err
			}
			if err := d.execTxn(ctx, txnID, `DELETE FROM chunks WHERE id = ?`, cid); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return errResult("delete_chunk: " + txErr.Error()), nil
	}
	// Bodies after commit (best-effort; see delete_document).
	for _, cid := range ids {
		_, _ = d.Store.MemoryDelete(ctx, direntTenant(ctx), mscope, key.ScopeID, chunkBodyKey(cid))
	}
	d.publishChange(ctx, mscope, key.ScopeID, row.DocumentID, "delete_chunk", in.ID)
	return jsonResult(map[string]any{"deleted": true, "cascade_deleted_descendants": len(ids) - 1})
}

func (d *Document) moveChunk(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("move_chunk: missing required field: id"), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("move_chunk: " + err.Error()), nil
	}
	if !ok {
		return errResult("move_chunk: no such chunk: " + in.ID), nil
	}
	// Reject moving a chunk under itself or one of its own descendants — that
	// would create a parent_id cycle (and a cycle makes delete_chunk's
	// descendant walk non-terminating). Walk UP from the new parent to the
	// root; if we reach the chunk being moved, it's a cycle.
	if in.NewParentID != "" {
		if in.NewParentID == in.ID {
			return errResult("move_chunk: cannot move a chunk under itself"), nil
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
		// Moving to the ROOT level is still expressed by an empty new_parent_id, so
		// this narrows nothing a caller could legitimately want.
		newParent, found, perr := d.getChunkRow(ctx, key, in.NewParentID)
		if perr != nil {
			return errResult("move_chunk: new parent lookup: " + perr.Error()), nil
		}
		if !found {
			return errResult("move_chunk: no such new_parent_id: " + in.NewParentID +
				" (moving under a parent that does not exist would orphan the chunk; " +
				"pass an empty new_parent_id to move it to the root level)"), nil
		}
		if newParent.DocumentID != row.DocumentID {
			return errResult(fmt.Sprintf(
				"move_chunk: new_parent_id %q belongs to document %q, but this chunk is in %q — "+
					"a chunk cannot be parented across documents",
				in.NewParentID, newParent.DocumentID, row.DocumentID)), nil
		}
		cur := in.NewParentID
		for i := 0; cur != "" && i <= maxChunkDepth; i++ {
			anc, found, aerr := d.getChunkRow(ctx, key, cur)
			if aerr != nil {
				return errResult("move_chunk: " + aerr.Error()), nil
			}
			if !found {
				break
			}
			if anc.ID == in.ID {
				return errResult("move_chunk: cannot move a chunk into its own subtree (would create a cycle)"), nil
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
		return errResult("move_chunk: " + err.Error()), nil
	}
	d.publishChange(ctx, store.MemoryScope(key.Scope), key.ScopeID, row.DocumentID, "move_chunk", in.ID)
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
		return errResult("reorder_chunk: missing required field: id"), nil
	}
	if in.Direction != "up" && in.Direction != "down" {
		return errResult(`reorder_chunk: direction must be "up" or "down"`), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errResult("reorder_chunk: " + err.Error()), nil
	}
	if !ok {
		return errResult("reorder_chunk: no such chunk: " + in.ID), nil
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
		return errResult("reorder_chunk: " + err.Error()), nil
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
		return errResult("reorder_chunk: chunk not found among its siblings"), nil
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
		return errResult("reorder_chunk: " + err.Error()), nil
	}
	d.publishChange(ctx, mscope, key.ScopeID, row.DocumentID, "reorder_chunk", in.ID)
	return jsonResult(map[string]any{"reordered": true, "id": in.ID, "direction": in.Direction})
}

// --- ops: edges ---

func (d *Document) linkChunks(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.FromID == "" || in.ToID == "" || in.Kind == "" {
		return errResult("link_chunks: from_id, to_id, and kind are required"), nil
	}
	// Both endpoints MUST exist (in this scope) — otherwise the edge is born
	// dangling. Cross-document edges are allowed (both chunks just have to
	// exist; they may be in different documents of this scope), but an edge to
	// a non-existent chunk is refused.
	if _, ok, err := d.getChunkRow(ctx, key, in.FromID); err != nil {
		return errResult("link_chunks: " + err.Error()), nil
	} else if !ok {
		return errResult("link_chunks: from_id: no such chunk: " + in.FromID), nil
	}
	if _, ok, err := d.getChunkRow(ctx, key, in.ToID); err != nil {
		return errResult("link_chunks: " + err.Error()), nil
	} else if !ok {
		return errResult("link_chunks: to_id: no such chunk: " + in.ToID), nil
	}
	now := time.Now().UnixNano()
	// Idempotent (INSERT OR IGNORE-equivalent via existence check for portability).
	res, err := d.query(ctx, key, `SELECT 1 FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = ? LIMIT 1`, in.FromID, in.ToID, in.Kind)
	if err != nil {
		return errResult("link_chunks: " + err.Error()), nil
	}
	if len(res.Rows) == 0 {
		if err := d.exec(ctx, key, `INSERT INTO chunk_edges (from_id, to_id, kind, created_at) VALUES (?, ?, ?, ?)`, in.FromID, in.ToID, in.Kind, now); err != nil {
			return errResult("link_chunks: " + err.Error()), nil
		}
	}
	return jsonResult(map[string]any{"ok": true, "from_id": in.FromID, "to_id": in.ToID, "kind": in.Kind})
}

func (d *Document) unlinkChunks(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.FromID == "" || in.ToID == "" || in.Kind == "" {
		return errResult("unlink_chunks: from_id, to_id, and kind are required"), nil
	}
	if err := d.exec(ctx, key, `DELETE FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = ?`, in.FromID, in.ToID, in.Kind); err != nil {
		return errResult("unlink_chunks: " + err.Error()), nil
	}
	return jsonResult(map[string]any{"removed": true})
}

// edgeEndpointFields are the per-endpoint columns get_edges surfaces (present
// only when non-empty) — the endpoint's title/type/status/document_id, so the UI
// can render a References list + a relationship graph without a follow-up
// get_chunk per endpoint.
var edgeEndpointFields = []string{
	"from_title", "from_type", "from_status", "from_document_id",
	"to_title", "to_type", "to_status", "to_document_id",
}

// getEdges returns every cross-reference edge with an endpoint in the document
// (outgoing OR incoming), each enriched via a self-join on chunks with both
// endpoints' title/type/status/document_id. Powers the viewer's References list +
// the RFC BN relationship graph in ONE call, replacing the raw-SQL escape hatch.
// LEFT JOINs so a (defensively) dangling endpoint still lists with empty fields.
// Each edge also carries `auto` (true = a parser-generated [[name]]-link edge,
// false = a manual link_chunks edge — RFC BS Phase 2a).
func (d *Document) getEdges(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.DocumentID == "" {
		return errResult("get_edges: missing required field: document_id"), nil
	}
	res, err := d.query(ctx, key, `SELECT e.from_id AS from_id, e.to_id AS to_id, e.kind AS kind, e.auto AS auto,
       cf.title AS from_title, cf.type AS from_type, cf.status AS from_status, cf.document_id AS from_document_id,
       ct.title AS to_title, ct.type AS to_type, ct.status AS to_status, ct.document_id AS to_document_id
FROM chunk_edges e
LEFT JOIN chunks cf ON cf.id = e.from_id
LEFT JOIN chunks ct ON ct.id = e.to_id
WHERE e.from_id IN (SELECT id FROM chunks WHERE document_id = ?)
   OR e.to_id IN (SELECT id FROM chunks WHERE document_id = ?)
ORDER BY e.kind, e.created_at`, in.DocumentID, in.DocumentID)
	if err != nil {
		return errResult("get_edges: " + err.Error()), nil
	}
	edges := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		m := map[string]any{}
		for i, c := range res.Columns {
			if i < len(r) {
				m[c] = r[i]
			}
		}
		// auto=1 distinguishes a parser-generated [[name]]-link edge (RFC BS Phase
		// 2a) from a manual link_chunks edge, so a consumer can tell them apart.
		e := map[string]any{"from_id": asStr(m["from_id"]), "to_id": asStr(m["to_id"]), "kind": asStr(m["kind"]), "auto": asInt(m["auto"]) == 1}
		for _, f := range edgeEndpointFields {
			if v := asStr(m[f]); v != "" {
				e[f] = v
			}
		}
		edges = append(edges, e)
	}
	return jsonResult(map[string]any{"edges": edges})
}

// --- chunk history + backlinks (RFC BS Phase 3a) ---

// backlinkEndpointFields are the FROM-endpoint columns backlinks surfaces
// (present only when non-empty) — the linking chunk's title/type/status/document_id,
// so "what links here" renders without a follow-up get_chunk per source. The
// mirror of getEdges' edgeEndpointFields, restricted to the from side (backlinks
// fixes the to side to the queried chunk).
var backlinkEndpointFields = []string{"from_title", "from_type", "from_status", "from_document_id"}

// chunkHistory lists the revisions at which a chunk's BODY changed, newest
// first — metadata only (revision / created_at / actor), not the bodies. The
// body of any listed revision is fetched with get_version.
func (d *Document) chunkHistory(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("history: missing required field: id"), nil
	}
	limit := 100
	if in.Limit > 0 {
		limit = in.Limit
	}
	res, err := d.query(ctx, key,
		`SELECT revision, created_at, actor FROM chunk_revisions WHERE chunk_id = ? ORDER BY revision DESC LIMIT ?`,
		in.ID, limit)
	if err != nil {
		return errResult("history: " + err.Error()), nil
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
		return errResult("get_version: missing required field: id"), nil
	}
	if in.Revision == nil {
		return errResult("get_version: missing required field: revision"), nil
	}
	body, ok, err := d.revisionBody(ctx, key, in.ID, *in.Revision)
	if err != nil {
		return errResult("get_version: " + err.Error()), nil
	}
	if !ok {
		return errResult(fmt.Sprintf("get_version: no such revision %d for chunk %s", *in.Revision, in.ID)), nil
	}
	return jsonResult(map[string]any{"chunk_id": in.ID, "revision": *in.Revision, "body": body})
}

// diffVersions returns a unified diff between two revisions' bodies. Either
// missing revision is a clear error rather than a diff against "".
func (d *Document) diffVersions(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("diff: missing required field: id"), nil
	}
	if in.FromRevision == nil || in.ToRevision == nil {
		return errResult("diff: from_revision and to_revision are required"), nil
	}
	fromBody, ok, err := d.revisionBody(ctx, key, in.ID, *in.FromRevision)
	if err != nil {
		return errResult("diff: " + err.Error()), nil
	}
	if !ok {
		return errResult(fmt.Sprintf("diff: no such revision %d for chunk %s", *in.FromRevision, in.ID)), nil
	}
	toBody, ok, err := d.revisionBody(ctx, key, in.ID, *in.ToRevision)
	if err != nil {
		return errResult("diff: " + err.Error()), nil
	}
	if !ok {
		return errResult(fmt.Sprintf("diff: no such revision %d for chunk %s", *in.ToRevision, in.ID)), nil
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
		return errResult("diff: " + derr.Error()), nil
	}
	return jsonResult(map[string]any{
		"chunk_id":      in.ID,
		"from_revision": *in.FromRevision,
		"to_revision":   *in.ToRevision,
		"diff":          text,
	})
}

// backlinks returns "what links here": every edge pointing TO the given chunk,
// each enriched with its FROM endpoint's title/type/status/document_id and the
// auto flag (true = a parser-generated [[name]]-link edge, false = a manual
// link_chunks edge). The reverse direction of get_edges, keyed on a single chunk
// (get_edges is per-document). Served by the chunk_edges_to_kind reverse index.
func (d *Document) backlinks(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("backlinks: missing required field: id"), nil
	}
	res, err := d.query(ctx, key, `SELECT e.from_id AS from_id, e.kind AS kind, e.auto AS auto,
       cf.title AS from_title, cf.type AS from_type, cf.status AS from_status, cf.document_id AS from_document_id
FROM chunk_edges e
LEFT JOIN chunks cf ON cf.id = e.from_id
WHERE e.to_id = ?
ORDER BY e.kind, e.created_at`, in.ID)
	if err != nil {
		return errResult("backlinks: " + err.Error()), nil
	}
	links := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		m := map[string]any{}
		for i, c := range res.Columns {
			if i < len(r) {
				m[c] = r[i]
			}
		}
		bl := map[string]any{"from_id": asStr(m["from_id"]), "kind": asStr(m["kind"]), "auto": asInt(m["auto"]) == 1}
		for _, f := range backlinkEndpointFields {
			if v := asStr(m[f]); v != "" {
				bl[f] = v
			}
		}
		links = append(links, bl)
	}
	return jsonResult(map[string]any{"backlinks": links})
}

// --- discovery: related (semantic) + unlinked_mentions (textual) ---

// chunkMeta is the small display tuple related/unlinked_mentions enrich a bare
// chunk id with, so a caller renders a hit without a per-id get_chunk.
type chunkMeta struct {
	title      string
	typ        string
	documentID string
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
		"SELECT id, title, type, document_id FROM chunks WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	for _, r := range res.Rows {
		out[asStr(r[0])] = chunkMeta{title: asStr(r[1]), typ: asStr(r[2]), documentID: asStr(r[3])}
	}
	return out, nil
}

// related returns the chunks whose bodies embed CLOSEST to a chunk's — its
// semantic neighbours, ranked by cosine score (RFC BS Phase 3b). It reuses the
// per-chunk-body embeddings writeBody already indexes on write (keyed
// "doc.chunk:<id>" in the k/v Memory plane, under the chunk's scope + the raw
// direntTenant), so nothing about the write path changes. Scope-confined: the
// vector search is bounded to the caller's own tenant/scope/scope_id.
func (d *Document) related(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("related: missing required field: id"), nil
	}
	// Semantic neighbours need both a vector index and something to turn the query
	// body into a vector. No embedder → there is no vector plane to search.
	if d.Embedder == nil {
		return errResult("related: requires a configured embedder / vector memory"), nil
	}
	topK := 10
	if in.Limit > 0 {
		topK = in.Limit
	}
	if topK > 50 {
		topK = 50
	}
	// The query text is the chunk's own body. An empty or media body has nothing
	// to embed — the same predicate embedBody skips on write — so it has no
	// neighbours; return an empty set rather than an error (a section/parent chunk
	// legitimately has no body).
	cb, err := d.readBody(ctx, mscope, key.ScopeID, in.ID)
	if err != nil {
		return errResult("related: body: " + err.Error()), nil
	}
	body := strings.TrimSpace(cb.Body)
	if body == "" {
		return jsonResult(map[string]any{"related": []map[string]any{}})
	}
	if typ, _, _, _ := classifyMediaBody(cb.Body); typ != "" {
		return jsonResult(map[string]any{"related": []map[string]any{}})
	}
	vec, err := d.Embedder.Embed(ctx, []string{body})
	if err != nil {
		return errResult("related: embed: " + err.Error()), nil
	}
	if len(vec) == 0 {
		return errResult("related: embed: embedder returned no vector"), nil
	}
	// topK+1: a chunk is its own nearest neighbour, so the search ranks self first;
	// ask for one extra to leave room to drop it and still return topK others.
	entries, err := d.Store.MemoryEmbedSearch(ctx, direntTenant(ctx), mscope, key.ScopeID,
		store.MemorySearchFilter{KeyPrefix: chunkBodyKeyPrefix}, vec[0], topK+1)
	if err != nil {
		return errResult("related: search: " + err.Error()), nil
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
		return errResult("related: enrich: " + err.Error()), nil
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
		}
		out = append(out, m)
	}
	return jsonResult(map[string]any{"related": out})
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
		return errResult("unlinked_mentions: missing required field: id"), nil
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
		return errResult("unlinked_mentions: " + err.Error()), nil
	}
	if len(tres.Rows) == 0 {
		return errResult("unlinked_mentions: no such chunk: " + in.ID), nil
	}
	title := strings.TrimSpace(asStr(tres.Rows[0][0]))
	if title == "" {
		return errResult("unlinked_mentions: target chunk has no title to match on"), nil
	}
	// The already-linked set: every chunk that already references the target
	// (manual OR [[name]] edge — both live in chunk_edges as from→to). A mention
	// from a chunk that already links is not "unlinked".
	linked := map[string]bool{}
	lres, err := d.query(ctx, key, `SELECT from_id FROM chunk_edges WHERE to_id = ?`, in.ID)
	if err != nil {
		return errResult("unlinked_mentions: edges: " + err.Error()), nil
	}
	for _, r := range lres.Rows {
		linked[asStr(r[0])] = true
	}
	// Scan chunk bodies from the k/v Memory plane, keyed on the SAME
	// tenant/scope/scope_id writeBody wrote them under (direntTenant + mscope +
	// key.ScopeID) — reading a different plane would silently return zero hits.
	entries, listTruncated, err := d.Store.MemoryList(ctx, direntTenant(ctx), mscope, key.ScopeID, "doc.chunk:", unlinkedMentionScanCap)
	if err != nil {
		return errResult("unlinked_mentions: scan: " + err.Error()), nil
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
		return errResult("unlinked_mentions: enrich: " + err.Error()), nil
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
	if err := d.exec(ctx, key, `DELETE FROM chunk_edges WHERE from_id = ? AND auto = 1`, fromChunkID); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	for _, to := range targets {
		// Guarded so a pre-existing MANUAL references edge (auto=0) is neither
		// duplicated nor overwritten (the portable existence-check from addTagRows).
		stmt := `INSERT INTO chunk_edges (from_id, to_id, kind, created_at, auto)
			SELECT ?, ?, 'references', ?, 1
			WHERE NOT EXISTS (SELECT 1 FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = 'references')`
		if err := d.exec(ctx, key, stmt, fromChunkID, to, now, fromChunkID, to); err != nil {
			return err
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
		res, err := d.SqlMem.Query(ctx, key, in.SQL, nil)
		if err != nil {
			return errResult("query_chunks: " + err.Error()), nil
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
			return errResult("query_chunks: " + err.Error()), nil
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
	if in.Type != "" {
		where += " AND type = ?"
		args = append(args, in.Type)
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
	args = append(args, limit)
	res, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE `+where+` ORDER BY document_id, parent_id, position LIMIT ?`, args...)
	if err != nil {
		return errResult("query_chunks: " + err.Error()), nil
	}
	chunks := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := scanChunkRow(res.Columns, r)
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
		chunks = append(chunks, m)
	}
	return jsonResult(map[string]any{"chunks": chunks})
}

// documentsUnderPath returns the document ids named at/under a Path-tree path
// (the under_path query filter). Reuses the dirent recursive listing.
func (d *Document) documentsUnderPath(ctx context.Context, key sqlmem.ScopeKey, rawPath string) ([]string, error) {
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return nil, err
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
			cb, err := d.readBody(ctx, mscope, key.ScopeID, targetID)
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
			return errResult("export_md: " + err.Error()), nil
		}
	}
	dres, err := d.query(ctx, key, `SELECT title, root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errResult("export_md: " + err.Error()), nil
	}
	if len(dres.Rows) == 0 {
		return errResult("export_md: no such document: " + docID), nil
	}
	title := asStr(dres.Rows[0][0])

	// ORDER BY parent_id first so each parent's rows are contiguous, then
	// position, then id as a stable tiebreaker — makes the byParent grouping
	// below deterministic even if two siblings somehow share a position
	// (reachable via an explicit `position` on create_chunk/move_chunk).
	cres, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ? ORDER BY parent_id, position, id`, docID)
	if err != nil {
		return errResult("export_md: " + err.Error()), nil
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
	includeMeta := in.IncludeMetadata == nil || *in.IncludeMetadata

	var b strings.Builder
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
		cb, rerr := d.readBody(ctx, mscope, key.ScopeID, row.ID)
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
	for _, r := range roots {
		walk(r, 0)
	}
	if walkErr != nil {
		return errResult("export_md: body: " + walkErr.Error()), nil
	}

	// Edges trailer — the free-form graph edges originating from this document's
	// chunks (parent-child is the hierarchy above, not an edge). Metadata-only:
	// a clean export (include_metadata=false) omits it.
	if includeMeta {
		eres, err := d.query(ctx, key, `SELECT from_id, to_id, kind FROM chunk_edges WHERE from_id IN (SELECT id FROM chunks WHERE document_id = ?) ORDER BY from_id, to_id, kind`, docID)
		if err != nil {
			return errResult("export_md: edges: " + err.Error()), nil
		}
		var lines []string
		for _, r := range eres.Rows {
			lines = append(lines, asStr(r[0])+" -> "+asStr(r[1])+" ["+asStr(r[2])+"]")
		}
		if len(lines) > 0 {
			b.WriteString("<!-- loom-edges:\n" + strings.Join(lines, "\n") + "\n-->\n")
		}
	}

	return jsonResult(map[string]any{"markdown": b.String(), "document_id": docID, "title": title})
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
	body   string
	// RFC BO — set when the body was a self-contained media form: an image
	// chunk's decoded bytes to re-attach via set_asset (assetData base64 +
	// assetMediaType), unwrapped from a data-URL. A mermaid chunk carries its
	// source in body (fence stripped) with typ="mermaid".
	assetMediaType string
	assetData      string
}

type mdEdge struct{ from, to, kind string }

var (
	mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdEdgeRe    = regexp.MustCompile(`^\s*(\S+)\s*->\s*(\S+)\s*\[(.*)\]\s*$`)
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
					edges = append(edges, mdEdge{from: m[1], to: m[2], kind: m[3]})
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
				ID     string          `json:"id"`
				Type   string          `json:"type"`
				Status string          `json:"status"`
				Fields json.RawMessage `json:"fields"`
			}
			if json.Unmarshal([]byte(payload), &meta) == nil {
				cur.oldID, cur.typ, cur.status, cur.fields = meta.ID, meta.Type, meta.Status, meta.Fields
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
// just without its image) rather than aborting the whole import.
func (d *Document) importAsset(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID string, c mdChunk) {
	_, _ = d.setAsset(ctx, key, mscope, docInput{
		ID: chunkID, MediaType: c.assetMediaType, Data: c.assetData,
	})
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
		return errResult("import_md: missing required field: markdown"), nil
	}
	chunks, edges := parseLoomMarkdown(in.Markdown)
	if len(chunks) == 0 {
		return errResult("import_md: no headings found — a document needs at least one '# Heading'"), nil
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

	if in.DocumentID == "" {
		// New document: the first heading is the root chunk.
		first := chunks[0]
		cd, _ := d.createDocument(ctx, key, mscope, docInput{Title: titleOrUntitled(first.title), Path: in.Path})
		if cd.IsError {
			return cd, nil
		}
		docID = resultField(cd, "document_id")
		rootID = resultField(cd, "root_chunk_id")
		if err := d.writeBody(ctx, mscope, key, rootID, first.typ, first.body, first.fields); err != nil {
			return errResult("import_md: root body: " + err.Error()), nil
		}
		if first.typ != "" || first.status != "" {
			if err := d.exec(ctx, key, `UPDATE chunks SET type = ?, status = ? WHERE id = ?`, nullIfEmpty(first.typ), nullIfEmpty(first.status), rootID); err != nil {
				return errResult("import_md: " + err.Error()), nil
			}
			// Keep the documents row's mirrored facets in step with the root chunk
			// (createDocument set them from an empty type/status above).
			if err := d.mirrorRootFacets(ctx, key, rootID); err != nil {
				return errResult("import_md: " + err.Error()), nil
			}
		}
		if first.assetData != "" {
			d.importAsset(ctx, key, mscope, rootID, first)
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
				return errResult("import_md: " + err.Error()), nil
			}
			if len(dres.Rows) == 0 {
				return errResult("import_md: no such document: " + docID), nil
			}
			base = asStr(dres.Rows[0][0])
		} else if _, ok, err := d.getChunkRow(ctx, key, base); err != nil {
			return errResult("import_md: " + err.Error()), nil
		} else if !ok {
			return errResult("import_md: no such parent chunk: " + base), nil
		}
		rootID = base
		stack = []frame{{level: 0, id: base}}
	}

	for _, c := range chunks[start:] {
		// Pop to the nearest ancestor whose heading level is shallower.
		for len(stack) > 1 && stack[len(stack)-1].level >= c.level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].id
		cc, _ := d.createChunk(ctx, key, mscope, docInput{
			DocumentID: docID, ParentID: parent, Title: titleOrUntitled(c.title),
			Type: c.typ, Status: c.status, Body: c.body, Fields: c.fields,
		})
		if cc.IsError {
			return cc, nil
		}
		newID := resultField(cc, "id")
		if c.assetData != "" {
			d.importAsset(ctx, key, mscope, newID, c)
		}
		if c.oldID != "" {
			oldToNew[c.oldID] = newID
		}
		stack = append(stack, frame{level: c.level, id: newID})
		created++
	}

	// Recreate edges by remapping old → new ids; skip any whose endpoints
	// weren't both imported (e.g. a cross-document edge).
	for _, e := range edges {
		nf, ok1 := oldToNew[e.from]
		nt, ok2 := oldToNew[e.to]
		if !ok1 || !ok2 {
			continue
		}
		_, _ = d.linkChunks(ctx, key, docInput{FromID: nf, ToID: nt, Kind: e.kind})
	}

	return jsonResult(map[string]any{"document_id": docID, "root_chunk_id": rootID, "chunks_created": created})
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
			return errResult("export_canvas: " + err.Error()), nil
		}
	}
	dres, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errResult("export_canvas: " + err.Error()), nil
	}
	if len(dres.Rows) == 0 {
		return errResult("export_canvas: no such document: " + docID), nil
	}
	rootID := asStr(dres.Rows[0][0])

	// Content chunks in the SAME deterministic order export_md walks
	// (parent_id, position, id) so the auto-grid index is stable across exports.
	cres, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ? ORDER BY parent_id, position, id`, docID)
	if err != nil {
		return errResult("export_canvas: " + err.Error()), nil
	}

	// Stored layouts for this document's chunks in one query.
	layouts := map[string]canvasLayout{}
	lres, err := d.query(ctx, key, `SELECT chunk_id, x, y, width, height, color FROM chunk_layout WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID)
	if err != nil {
		return errResult("export_canvas: layout: " + err.Error()), nil
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
		cb, rerr := d.readBody(ctx, mscope, key.ScopeID, row.ID)
		if rerr != nil {
			return errResult("export_canvas: body: " + rerr.Error()), nil
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
	// the root (the one in-document chunk that is not a node).
	eres, err := d.query(ctx, key, `SELECT from_id, to_id, kind FROM chunk_edges WHERE from_id IN (SELECT id FROM chunks WHERE document_id = ?) AND to_id IN (SELECT id FROM chunks WHERE document_id = ?) ORDER BY from_id, to_id, kind`, docID, docID)
	if err != nil {
		return errResult("export_canvas: edges: " + err.Error()), nil
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
		return errResult("import_canvas: missing required field: canvas"), nil
	}
	var cv canvasDoc
	if err := json.Unmarshal(in.Canvas, &cv); err != nil {
		return errResult("import_canvas: invalid canvas JSON: " + err.Error()), nil
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
			return errResult("import_canvas: layout: " + err.Error()), nil
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
		kind := strings.TrimSpace(e.Label)
		if kind == "" {
			kind = "references"
		}
		lr, _ := d.linkChunks(ctx, key, docInput{FromID: from, ToID: to, Kind: kind})
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
		return errResult("define_type: missing required field: name"), nil
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
		return errResult("define_type: " + err.Error()), nil
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_types (document_id, name, fields, created_at) VALUES (?, ?, ?, ?)`, docID, in.Name, string(fields), now); err != nil {
		return errResult("define_type: " + err.Error()), nil
	}
	return jsonResult(map[string]any{"ok": true, "name": in.Name, "document_id": docID})
}

func (d *Document) listTypes(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	res, err := d.query(ctx, key, `SELECT document_id, name, fields FROM chunk_types WHERE document_id = ? ORDER BY name`, in.DocumentID)
	if err != nil {
		return errResult("list_types: " + err.Error()), nil
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
	return jsonResult(map[string]any{"types": types})
}

// --- ops: tags + document facets (RFC BS Phase 1) ---

// upsertChunkOp runs upsert_chunk (whose create/update internals live in the
// entity-tier file) and then applies the Phase-1 facets this change owns. On the
// CREATE path there is nothing to add here: upsert delegates the insert to
// createChunk, which already applies `tags`, and never makes a root chunk. On the
// UPDATE path (updateChunkForUpsert, which does not touch tags) this replace-sets
// the tags when supplied and mirrors the documents row if the upserted chunk is a
// root and its type/status was set.
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
			return errResult("upsert_chunk: tags: " + terr.Error()), nil
		}
	}
	// Only worth a mirror when the upsert could have moved a root chunk's facets
	// (updateChunkForUpsert sets type/status only when supplied); otherwise it is a
	// guaranteed 0-row UPDATE on the entity tier's hot path.
	if in.Type != "" || in.Status != "" {
		if terr := d.mirrorRootFacets(ctx, key, meta.ID); terr != nil {
			return errResult("upsert_chunk: " + terr.Error()), nil
		}
	}
	return res, nil
}

// mirrorRootFacets keeps the documents row's denormalized type/status in step with
// its root chunk. It is a no-op for a non-root chunk (the WHERE matches nothing),
// so callers can invoke it unconditionally after a chunk's type/status changes.
func (d *Document) mirrorRootFacets(ctx context.Context, key sqlmem.ScopeKey, chunkID string) error {
	return d.exec(ctx, key,
		`UPDATE documents SET type = (SELECT type FROM chunks WHERE id = ?),
		                      status = (SELECT status FROM chunks WHERE id = ?),
		                      updated_at = ?
		 WHERE root_chunk_id = ?`,
		chunkID, chunkID, time.Now().UnixNano(), chunkID)
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
	for _, t := range sanitizeTags(tags) {
		stmt := `INSERT INTO ` + table + ` (` + keyCol + `, tag) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM ` + table + ` WHERE ` + keyCol + ` = ? AND tag = ?)`
		if err := d.exec(ctx, key, stmt, id, t, id, t); err != nil {
			return err
		}
	}
	return nil
}

// removeTagRows deletes the given tags from a target (see addTagRows for the
// hardcoded-identifier note).
func (d *Document) removeTagRows(ctx context.Context, key sqlmem.ScopeKey, table, keyCol, id string, tags []string) error {
	for _, t := range sanitizeTags(tags) {
		if err := d.exec(ctx, key, `DELETE FROM `+table+` WHERE `+keyCol+` = ? AND tag = ?`, id, t); err != nil {
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
		return errResult(op + ": missing required field: tags"), nil
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
			return errResult(op + ": " + err.Error()), nil
		} else if !ok {
			return errResult(op + ": no such chunk: " + in.ID), nil
		}
		if err := apply("chunk_tags", "chunk_id", in.ID); err != nil {
			return errResult(op + ": " + err.Error()), nil
		}
		tags, err := d.listChunkTags(ctx, key, in.ID)
		if err != nil {
			return errResult(op + ": " + err.Error()), nil
		}
		return jsonResult(map[string]any{"chunk_id": in.ID, "tags": tags})
	case in.DocumentID != "":
		if ok, err := d.documentExists(ctx, key, in.DocumentID); err != nil {
			return errResult(op + ": " + err.Error()), nil
		} else if !ok {
			return errResult(op + ": no such document: " + in.DocumentID), nil
		}
		if err := apply("document_tags", "document_id", in.DocumentID); err != nil {
			return errResult(op + ": " + err.Error()), nil
		}
		tags, err := d.listDocumentTags(ctx, key, in.DocumentID)
		if err != nil {
			return errResult(op + ": " + err.Error()), nil
		}
		return jsonResult(map[string]any{"document_id": in.DocumentID, "tags": tags})
	default:
		return errResult(op + ": target a chunk (id) or a document (document_id)"), nil
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
		return errResult("list_tags: " + err.Error()), nil
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
			return errResult("query_documents: " + err.Error()), nil
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
	args = append(args, limit)
	res, err := d.query(ctx, key, `SELECT id, title, type, status, root_chunk_id, created_at, updated_at FROM documents WHERE `+where+` ORDER BY title, id LIMIT ?`, args...)
	if err != nil {
		return errResult("query_documents: " + err.Error()), nil
	}
	docs := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		m := map[string]any{}
		for i, c := range res.Columns {
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
	if d.Bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"op": op, "chunk_id": chunkID, "timestamp": time.Now().UnixNano(), "actor": tools.RunIdentity(ctx).UserID})
	channel := "documents/" + documentID + "/chunks"
	// Best-effort change-event ring (cap 256). Errors are ignored — a missing
	// subscriber/declared channel must never fail a chunk mutation. The Web UI
	// subscriber arrives in a later phase.
	_, _, _ = d.Store.ChannelPublish(ctx, store.ChannelMessage{
		Channel: channel, Scope: mscope, ScopeID: scopeID, Payload: payload,
		PublishedByUserID: tools.RunIdentity(ctx).UserID,
	}, 256)
	d.Bus.Notify(channel)
}

// nullIfEmpty returns nil for an empty string so it stores SQL NULL (rather
// than the empty string), keeping IS NULL / nullable-column semantics clean.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
