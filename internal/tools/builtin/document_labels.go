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
	// NaturalKey is the fact's STABLE IDENTITY — `memory/<class>/<slug>`, the same
	// string its k/v row is keyed on, by deliberate design ("one key space for both
	// stores is what stops them drifting").
	//
	// It is carried here so a chunk-homed fact can be addressed by the name it has
	// always had. A chunk's row key is `doc.chunk:<hex>`, which is an opaque
	// server-assigned address; handing that back as a fact's id would change every
	// fact's identity the moment its home moved, and the consolidator's merge path
	// writes a neighbour back UNDER ITS OWN KEY. Empty for an ordinary prose chunk,
	// which has no sidecar row.
	NaturalKey string
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
	// The sidecar joins the same way and for the same reason — an ordinary prose
	// chunk has no row there, and must still yield its label.
	stmt := `SELECT c.id, c.title, d.title, coalesce(m.natural_key, '') FROM chunks c ` +
		`LEFT JOIN documents d ON d.id = c.document_id ` +
		`LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id WHERE c.id IN (` +
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
		lb := ChunkLabel{Document: asStr(row[2]), Title: asStr(row[1])}
		if len(row) > 3 {
			lb.NaturalKey = asStr(row[3])
		}
		out[id] = lb
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
