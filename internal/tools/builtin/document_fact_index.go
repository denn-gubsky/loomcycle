package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryFactKeyPrefix is the namespace a distilled fact's k/v row lives in —
// `memory/<class>/<slug>`. It is also, verbatim, the fact chunk's natural_key:
// "one key space for both stores is what stops them drifting", which is what makes
// collapsing the two planes a JOIN on a column that already matches rather than a
// reconciliation.
//
// Stated once, here, because the collapse deletes rows by this prefix and the
// document bodies it must never touch share the same keyspace at
// DocumentChunkKeyPrefix.
const MemoryFactKeyPrefix = "memory/"

// FactChunksByNaturalKey indexes a scope's FACT chunks by the natural key they
// share with the k/v plane, mapping it to the `doc.chunk:<hex>` row that holds the
// body — the row a collapsed fact lives in.
//
// It is the "verify by listing BOTH planes" half of the collapse. The question it
// answers is not "did the mirror write succeed" but "does this fact exist over
// there right now", which is the only form of the question a migration may trust:
// a sweep that reports N moved and leaves rows behind reads as a clean run and
// silently keeps the duplication.
//
// Only chunks of type `fact` are indexed. An entity/subject node also carries a
// natural key, and it is NOT a fact — treating one as the home of a k/v row would
// let the collapse delete a fact because its SUBJECT happened to be mirrored.
func (d *Document) FactChunksByNaturalKey(ctx context.Context, tenantID string,
	scope store.MemoryScope, scopeID string) (map[string]string, error) {
	if d.SqlMem == nil {
		return nil, nil
	}
	key, ok := docScopeKeyFor(tenantID, scope, scopeID)
	if !ok {
		return nil, nil
	}
	// A scope that has never had a Document op has no chunk tables at all. That is
	// a legitimate state meaning "nothing is mirrored here", NOT a fault — and the
	// difference matters, because the caller refuses on unmirrored facts and would
	// otherwise report a read error for a scope that is simply empty. Probed with
	// the leading-SELECT the rest of this package uses: validator-safe (a leading
	// PRAGMA is denied) and tier-portable.
	if _, perr := d.SqlMem.Query(ctx, key, `SELECT natural_key FROM chunk_memory_meta WHERE 1=0`, nil); perr != nil {
		return map[string]string{}, nil
	}
	res, err := d.SqlMem.Query(ctx, key,
		`SELECT m.natural_key, c.id FROM chunk_memory_meta m `+
			`JOIN chunks c ON c.id = m.chunk_id `+
			`WHERE c.type = 'fact' AND m.natural_key IS NOT NULL`, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		nk, id := asStr(row[0]), asStr(row[1])
		if nk == "" || id == "" || !strings.HasPrefix(nk, MemoryFactKeyPrefix) {
			continue
		}
		out[nk] = chunkBodyKey(id)
	}
	return out, nil
}

// ScopeKeyForMemory exposes the Memory-plane → SQL-Memory scope mapping to
// callers outside this package, so the collapse does not restate a rule whose
// whole point is living in one place (the two planes disagree for scope=tenant).
func ScopeKeyForMemory(tenantID string, scope store.MemoryScope, scopeID string) (sqlmem.ScopeKey, bool) {
	return docScopeKeyFor(tenantID, scope, scopeID)
}
