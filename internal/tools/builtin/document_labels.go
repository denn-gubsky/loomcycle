package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// document_labels.go resolves the READABLE identity of a document chunk, for
// surfaces that can otherwise only show its id.
//
// A semantic search over the Memory plane returns document bodies keyed
// `doc.chunk:<32 hex>`. The id is the correct address — it is stable, unique, and
// what get_chunk takes — but a page of six of them tells a reader nothing about
// WHERE the prose came from, so a caller who wants to cite a hit has to fetch each
// one just to learn its heading. These labels are the annotation that closes that
// gap; the key stays the address.
//
// Everything here is best-effort. The labels live in SQL Memory, a different plane
// from the bodies, so a scope with no document tables, a store fault, or a chunk
// deleted between the search and the lookup must all cost a cosmetic string and
// never a result.

// ChunkLabel is one chunk's readable identity: the heading it sits under, and the
// document it belongs to.
type ChunkLabel struct {
	Document string // the document's title
	Title    string // the chunk's own title (its heading)
}

// maxChunkLabelLookup bounds the IN list. Both search surfaces cap top_k at 50
// today; the bound is stated here so a future caller with a larger page cannot
// turn one cosmetic lookup into an unbounded query.
const maxChunkLabelLookup = 100

// ChunkLabelsFor resolves labels for document-chunk hits in ONE batched query,
// returned keyed by chunk id. A missing entry means "no label available" — callers
// omit the fields rather than rendering an empty string.
//
// scope/scopeID are the MEMORY plane's coordinates (the ones the search ran on).
// Mapping them onto SQL Memory's differently-keyed scope happens here so that
// neither caller restates it: the two planes disagree for scope=tenant, and a
// restated rule is how the tenant axis drifts.
func ChunkLabelsFor(ctx context.Context, sm *sqlmem.Manager, tenantID string,
	scope store.MemoryScope, scopeID string, chunkIDs []string) map[string]ChunkLabel {
	if sm == nil || len(chunkIDs) == 0 {
		return nil
	}
	key, ok := docScopeKeyFor(tenantID, scope, scopeID)
	if !ok {
		return nil
	}
	// Distinct ids only: several hits from one document is the common case, and a
	// repeated id would widen the IN list for nothing.
	ids := make([]any, 0, len(chunkIDs))
	seen := make(map[string]bool, len(chunkIDs))
	for _, id := range chunkIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) >= maxChunkLabelLookup {
			break
		}
	}
	if len(ids) == 0 {
		return nil
	}

	// LEFT JOIN: a chunk whose document row is missing still yields its own title.
	stmt := `SELECT c.id, c.title, d.title FROM chunks c ` +
		`LEFT JOIN documents d ON d.id = c.document_id WHERE c.id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	res, err := sm.Query(ctx, key, sm.Rebind(stmt), ids)
	if err != nil || res == nil {
		return nil
	}
	out := make(map[string]ChunkLabel, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 3 {
			continue
		}
		id := asStr(row[0])
		if id == "" {
			continue
		}
		out[id] = ChunkLabel{Document: asStr(row[2]), Title: asStr(row[1])}
	}
	return out
}

// docScopeKeyFor maps the Memory plane's (tenant, scope, scope_id) onto SQL
// Memory's ScopeKey, mirroring Document.resolveScope's mapping without its
// ctx-derived identity or its authorization gates — this is a read of titles the
// caller has ALREADY been served bodies for, so it adds no reach.
//
// ok=false for a scope SQL Memory cannot key, which keeps the miss cosmetic.
func docScopeKeyFor(tenantID string, scope store.MemoryScope, scopeID string) (sqlmem.ScopeKey, bool) {
	tenant := sqlScopeTenantValue(tenantID)
	switch scope {
	case store.MemoryScopeAgent, store.MemoryScopeUser:
		if scopeID == "" {
			return sqlmem.ScopeKey{}, false
		}
		return sqlmem.ScopeKey{Tenant: tenant, Scope: string(scope), ScopeID: scopeID}, true
	case store.MemoryScopeTenant:
		// The two planes take DIFFERENT ids for this ONE logical scope: Memory keys
		// it "" (the tenant_id column already carries the identity) while SQL Memory
		// hashes the tenant into a schema + role name and rejects an empty
		// component. Documented at Document.resolveScope.
		return sqlmem.ScopeKey{Tenant: tenant, Scope: string(scope), ScopeID: tenant}, true
	}
	return sqlmem.ScopeKey{}, false
}
