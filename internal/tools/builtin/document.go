package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
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
// Gated by allowed_tools:[Document]. Requires SQL Memory enabled
// (LOOMCYCLE_SQLMEM_ENABLED=1) — the structure tables live there.
type Document struct {
	Store  store.Store
	SqlMem *sqlmem.Manager
	Bus    *channels.Bus
}

func (d *Document) Name() string { return "Document" }

func (d *Document) Description() string {
	return "A chunked-graph document: each chunk is a first-class unit (UUID, hierarchy, type, fields, graph edges, Markdown body) that agents and humans co-author and query. Ops: create_document/get_document/delete_document, create_chunk/get_chunk/update_chunk/delete_chunk/move_chunk, link_chunks/unlink_chunks, query_chunks (structured filters + a raw sql escape hatch), define_type/list_types, export_md (render the document to Markdown). Scope is agent or user; documents can be named in the Path tree (path:)."
}

// documentInputSchema is a package const so the LoomCycle MCP server can
// source the wrapper's advertised inputSchema verbatim (via
// MCPWrapperInputSchema) rather than restating it — the same pattern as
// memoryInputSchema.
const documentInputSchema = `{
	"type": "object",
	"properties": {
		"op":          {"type": "string", "enum": ["create_document","get_document","delete_document","create_chunk","get_chunk","update_chunk","delete_chunk","move_chunk","link_chunks","unlink_chunks","query_chunks","define_type","list_types","export_md"]},
		"scope":       {"type": "string", "enum": ["agent","user"], "description": "Which store (default user). agent = this agent; user = this end-user (needs a user_id on the run). tenant scope is not yet supported."},
		"id":          {"type": "string", "description": "Document id (get/delete_document) or chunk id (get/update/delete/move_chunk)."},
		"path":        {"type": "string", "description": "create_document: name the doc in the Path tree (e.g. /docs/launch). get/delete_document: address by path instead of id."},
		"title":       {"type": "string"},
		"document_id": {"type": "string"},
		"parent_id":   {"type": "string", "description": "create_chunk: parent chunk (omit for a child of the root)."},
		"new_parent_id": {"type": "string", "description": "move_chunk: the new parent."},
		"type":        {"type": "string", "description": "Optional supertag-like chunk type."},
		"body":        {"type": "string", "description": "Markdown body."},
		"fields":      {"type": "object", "description": "Type-specific structured fields."},
		"status":      {"type": "string"},
		"position":    {"type": "integer"},
		"revision":    {"type": "integer", "description": "update_chunk: the chunk's current revision (optimistic concurrency)."},
		"from_id":     {"type": "string"},
		"to_id":       {"type": "string"},
		"kind":        {"type": "string", "description": "link/unlink_chunks: edge kind (promotes/targets/...)."},
		"under_path":  {"type": "string", "description": "query_chunks: restrict to documents at/under this Path-tree path."},
		"sql":         {"type": "string", "description": "query_chunks: raw read-only SELECT against the chunk tables (escape hatch; validator-gated)."},
		"limit":       {"type": "integer"},
		"name":        {"type": "string", "description": "define/list_types: the type name."},
		"include_metadata": {"type": "boolean", "description": "export_md: embed round-trippable chunk metadata + edges as HTML comments (default true). false = clean human-facing Markdown."}
	},
	"required": ["op"]
}`

func (d *Document) InputSchema() json.RawMessage { return json.RawMessage(documentInputSchema) }

type docInput struct {
	Op          string          `json:"op"`
	Scope       string          `json:"scope"`
	ID          string          `json:"id"`
	Path        string          `json:"path"`
	Title       string          `json:"title"`
	DocumentID  string          `json:"document_id"`
	ParentID    string          `json:"parent_id"`
	NewParentID string          `json:"new_parent_id"`
	Type        string          `json:"type"`
	Body        string          `json:"body"`
	Fields      json.RawMessage `json:"fields"`
	Status      string          `json:"status"`
	Position    *int            `json:"position"`
	Revision    *int            `json:"revision"`
	FromID      string          `json:"from_id"`
	ToID        string          `json:"to_id"`
	Kind        string          `json:"kind"`
	UnderPath   string          `json:"under_path"`
	SQL         string          `json:"sql"`
	Limit       int             `json:"limit"`
	Name        string          `json:"name"`
	// IncludeMetadata gates export_md's round-trip comments (default true when
	// omitted; a pointer so an explicit `false` is distinguishable from unset).
	IncludeMetadata *bool `json:"include_metadata"`
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
	`CREATE INDEX IF NOT EXISTS chunks_doc_parent_pos ON chunks(document_id, parent_id, position)`,
	`CREATE INDEX IF NOT EXISTS chunks_doc_type_status ON chunks(document_id, type, status)`,
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
		return d.getDocument(ctx, key, in)
	case "delete_document":
		return d.deleteDocument(ctx, key, mscope, in)
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
	case "link_chunks":
		return d.linkChunks(ctx, key, in)
	case "unlink_chunks":
		return d.unlinkChunks(ctx, key, in)
	case "query_chunks":
		return d.queryChunks(ctx, key, in)
	case "define_type":
		return d.defineType(ctx, key, in)
	case "list_types":
		return d.listTypes(ctx, key, in)
	case "export_md":
		return d.exportMD(ctx, key, mscope, in)
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
		return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: scope=tenant is not yet supported (SQL Memory has no tenant scope); use agent or user")
	default:
		return sqlmem.ScopeKey{}, "", fmt.Errorf("Document: unknown scope %q (agent | user)", requested)
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
	return nil
}

// --- chunk body (Memory) helpers ---

type chunkBody struct {
	Body   string          `json:"body"`
	Fields json.RawMessage `json:"fields,omitempty"`
}

func (d *Document) writeBody(ctx context.Context, mscope store.MemoryScope, scopeID, chunkID, body string, fields json.RawMessage) error {
	v, _ := json.Marshal(chunkBody{Body: body, Fields: fields})
	return d.Store.MemorySet(ctx, mscope, scopeID, chunkBodyKey(chunkID), v, 0)
}

func (d *Document) readBody(ctx context.Context, mscope store.MemoryScope, scopeID, chunkID string) chunkBody {
	var cb chunkBody
	entry, err := d.Store.MemoryGet(ctx, mscope, scopeID, chunkBodyKey(chunkID))
	if err == nil {
		_ = json.Unmarshal(entry.Value, &cb)
	}
	return cb
}

// chunkBodyKey namespaces chunk bodies in the Memory keyspace so they don't
// collide with an agent's own k/v keys.
func chunkBodyKey(chunkID string) string { return "doc.chunk:" + chunkID }

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

// --- ops: document lifecycle ---

func (d *Document) createDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if in.Title == "" {
		return errResult("create_document: missing required field: title"), nil
	}
	now := time.Now().UnixNano()
	docID := newDocID()
	rootID := newDocID()
	if err := d.exec(ctx, key, `INSERT INTO documents (id, title, root_chunk_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		docID, in.Title, rootID, now, now); err != nil {
		return errResult("create_document: " + err.Error()), nil
	}
	// The root chunk anchors the hierarchy (parent_id NULL).
	if err := d.exec(ctx, key, `INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, NULL, 0, NULL, NULL, ?, ?, ?, 1)`,
		rootID, docID, in.Title, now, now); err != nil {
		return errResult("create_document: root chunk: " + err.Error()), nil
	}
	if err := d.writeBody(ctx, mscope, key.ScopeID, rootID, "", nil); err != nil {
		return errResult("create_document: root body: " + err.Error()), nil
	}
	resp := map[string]any{"document_id": docID, "root_chunk_id": rootID, "title": in.Title}
	// Optional Path-tree name.
	if in.Path != "" {
		if p, perr := d.registerDocDirent(ctx, key, docID, in.Path); perr != nil {
			resp["path_warning"] = "document created but path registration failed: " + perr.Error()
		} else {
			resp["path"] = p
		}
	}
	return jsonResult(resp)
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
		TenantID: direntTenant(ctx), Scope: key.Scope, ScopeID: key.ScopeID,
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
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, key.ScopeID, parent, name)
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

func (d *Document) getDocument(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return errResult("get_document: " + err.Error()), nil
	}
	res, err := d.query(ctx, key, `SELECT id, title, root_chunk_id, created_at, updated_at FROM documents WHERE id = ? LIMIT 1`, docID)
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
	return jsonResult(map[string]any{
		"document_id": asStr(m["id"]), "title": asStr(m["title"]),
		"root_chunk_id": asStr(m["root_chunk_id"]),
	})
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
		_, _ = d.Store.MemoryDelete(ctx, mscope, key.ScopeID, chunkBodyKey(id))
	}
	n := len(ids)
	// Drop any Path-tree dirent(s) pointing at this document — best-effort, by
	// document_id (works whether the caller addressed by id or path, so a
	// delete-by-id never leaves a dangling name). Scans the scope's document
	// dirents (bounded; a scope's name count is small).
	if rows, lerr := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, key.ScopeID, "/"); lerr == nil {
		for _, r := range rows {
			if r.Kind != "document" {
				continue
			}
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(r.ResourceRef, &ref)
			if ref.DocumentID == docID {
				_, _ = d.Store.DirentDelete(ctx, direntTenant(ctx), key.Scope, key.ScopeID, r.ParentPath, r.Name)
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
	pos := 0
	if in.Position != nil {
		pos = *in.Position
	} else {
		// Append: max(position)+1 among siblings. Branch on root-level vs
		// parented so the NULL comparison is portable (postgres rejects
		// `parent_id IS ?` with a non-null bind).
		var res *sqlmem.QueryResult
		var err error
		if in.ParentID == "" {
			res, err = d.query(ctx, key, `SELECT position FROM chunks WHERE document_id = ? AND parent_id IS NULL ORDER BY position DESC LIMIT 1`, in.DocumentID)
		} else {
			res, err = d.query(ctx, key, `SELECT position FROM chunks WHERE document_id = ? AND parent_id = ? ORDER BY position DESC LIMIT 1`, in.DocumentID, in.ParentID)
		}
		if err == nil && len(res.Rows) > 0 {
			pos = asInt(res.Rows[0][0]) + 1
		}
	}
	now := time.Now().UnixNano()
	id := newDocID()
	if err := d.exec(ctx, key,
		`INSERT INTO chunks (id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		id, in.DocumentID, nullIfEmpty(in.ParentID), pos, nullIfEmpty(in.Type), nullIfEmpty(in.Status), in.Title, now, now); err != nil {
		return errResult("create_chunk: " + err.Error()), nil
	}
	if err := d.writeBody(ctx, mscope, key.ScopeID, id, in.Body, in.Fields); err != nil {
		return errResult("create_chunk: body: " + err.Error()), nil
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
	cb := d.readBody(ctx, mscope, key.ScopeID, in.ID)
	return jsonResult(chunkResponse(row, cb))
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
		cb := d.readBody(ctx, mscope, key.ScopeID, in.ID)
		if hasBody {
			cb.Body = in.Body
		}
		if hasFields {
			cb.Fields = in.Fields
		}
		if err := d.writeBody(ctx, mscope, key.ScopeID, in.ID, cb.Body, cb.Fields); err != nil {
			return errResult("update_chunk: body: " + err.Error()), nil
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
		_, _ = d.Store.MemoryDelete(ctx, mscope, key.ScopeID, chunkBodyKey(cid))
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
	rows, err := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, key.ScopeID, dirPrefix(canonical))
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
	var walk func(row chunkRow, depth int)
	walk = func(row chunkRow, depth int) {
		level := depth + 1
		if level > 6 {
			level = 6
		}
		cb := d.readBody(ctx, mscope, key.ScopeID, row.ID)
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
		if cb.Body != "" {
			b.WriteString("\n" + cb.Body + "\n")
		}
		b.WriteString("\n")
		for _, c := range byParent[row.ID] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
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
