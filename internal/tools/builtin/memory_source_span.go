package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// maxSourceSpanLookup bounds the IN list. recall itself caps at 50 hits, so this
// is headroom rather than a policy — it exists so a future caller raising the cap
// cannot turn one recall into an unbounded query.
const maxSourceSpanLookup = 64

// SourceSpansFor returns the verbatim source span recorded for each recalled
// fact, keyed by the fact's id.
//
// WHY THIS EXISTS (RFC CV P1). Every system that scores well on long-conversation
// QA keeps the episode reachable from the fact: Graphiti's facts point at their
// source episode, MemGPT's recall memory IS the transcript. loomcycle's own
// numbers say the same thing louder than any of them — the raw-turns arm answers
// temporal questions at 0.873 while the distilled-facts arm answers them at 0.18.
// The information exists; distillation discards it.
//
// ⚠️ RFC CV P1 SAYS THE POINTER IS `session_id` / `event_seq` AND THAT IS NOT WHAT
// IS POPULATED. Measured on a live rig, 87 rows of chunk_memory_meta: session_id 0,
// event_seq 0, run_id 87 — and `source_quote` 76 (87%). So the plan's stated
// reach-through (resolve a session event through History) has no data behind it,
// while the span it wanted to reach is ALREADY STORED on the fact. This reads what
// exists instead of resolving what does not, which makes P1's first slice a
// projection rather than a new lookup path.
//
// The span is what the temporal case needs, because it is stored with its own
// leading timestamp — e.g. `user: [1:56 pm on 8 May, 2023] Caroline: …`. A
// distilled sentence that dropped "last week" is undatable; its span is not.
//
// A fact's id from recall IS the k/v key, and a fact chunk's `natural_key` is that
// key verbatim — the bundle keeps one key space for both stores precisely so they
// cannot drift. That is the join, and it needs no new column.
//
// Returns nil on any failure. A missing span must degrade to "no span" rather than
// failing the recall: the distilled fact is still a valid answer, and an error here
// would trade a working retrieval for a provenance nicety.
func SourceSpansFor(ctx context.Context, sm *sqlmem.Manager, tenantID string,
	scope store.MemoryScope, scopeID string, factIDs []string) map[string]string {
	if sm == nil || len(factIDs) == 0 {
		return nil
	}
	key, ok := docScopeKeyFor(tenantID, scope, scopeID)
	if !ok {
		return nil
	}
	keys := make([]any, 0, len(factIDs))
	seen := make(map[string]bool, len(factIDs))
	for _, id := range factIDs {
		// Only k/v-keyed facts can join: a document-chunk hit's id is a chunk id,
		// which is not a natural_key and would match nothing.
		if id == "" || seen[id] || !strings.HasPrefix(id, "memory/") {
			continue
		}
		seen[id] = true
		keys = append(keys, id)
		if len(keys) >= maxSourceSpanLookup {
			break
		}
	}
	if len(keys) == 0 {
		return nil
	}
	stmt := `SELECT natural_key, source_quote FROM chunk_memory_meta ` +
		`WHERE source_quote IS NOT NULL AND source_quote <> '' AND natural_key IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
	res, err := sm.Query(ctx, key, sm.Rebind(stmt), keys)
	if err != nil || res == nil {
		return nil
	}
	out := make(map[string]string, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		nk, span := asStr(row[0]), asStr(row[1])
		if nk == "" || span == "" {
			continue
		}
		out[nk] = span
	}
	return out
}
