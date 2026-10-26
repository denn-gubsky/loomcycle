package builtin

import (
	"context"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Access control on the chunk hits of a Memory search or recall.
//
// Chunk bodies share the memory keyspace, so a Memory search returns rows keyed
// doc.chunk: beside facts and notes. Document op=search leaves out every chunk
// whose document the caller's access-control set denies it reading
// (document_acl.go); without the same rule here, a Memory search is a second door
// to those documents. So a chunk hit is kept only when Document op=search would
// have kept it: the chunk belongs to a document, and the caller may read that
// document at every name it is filed under. A chunk with no document row is left
// out, as Document op=search leaves it out.
//
// WHICH HITS ARE CHECKED IS DECIDED BY THE KEY, NEVER BY THE ROW'S CLASS. A fact
// a consolidator homed in a chunk is a doc.chunk: row with provenance — it
// classifies as a fact, yet it sits in a document, and Document op=search hides
// it with the rest of a denied document. Choosing by class let it through.
//
// Only for a caller whose set CAN deny reading some document of the scope. A run
// with no set, or one whose set leaves the scope's documents alone, gets exactly
// the answer it got before: no lookup is made and nothing is dropped.

// documentHitReader is the Document tool's side: which of these chunks may the
// caller read. It owns the chunk → document mapping and the Path-tree names the
// rules are written against, so the Memory tool asks it.
type documentHitReader interface {
	// ReadableDocumentChunks returns the chunk ids among chunkIDs the caller on
	// ctx may read, in the memory scope (scope, scopeID). A chunk whose document
	// cannot be established is not readable.
	ReadableDocumentChunks(ctx context.Context, scope store.MemoryScope, scopeID string, chunkIDs []string) (map[string]bool, error)
}

// documentReadLimited reports whether the caller's set can deny reading some
// document of (scope, scopeID) — the question newDocACL and limitsReading ask on
// the Document tool, asked without it so a server with no Document tool still
// answers it. Nil set: nothing is limited.
func documentReadLimited(ctx context.Context, scope store.MemoryScope, scopeID string) (bool, error) {
	eff := tools.CallerACL(ctx)
	if eff == nil {
		return false, nil
	}
	// The resource a rule is matched against needs the scope and, for a user
	// scope, its owner — the memory scope id is the user id there. limitsReading
	// never reaches the Document tool on this path (only a path-sensitive set
	// reads names, and that one answers "limited" before it would).
	a := &docACL{eff: eff, key: sqlmem.ScopeKey{Scope: string(scope), ScopeID: scopeID},
		pathSensitive: eff.HasInstanceRules("document", string(scope))}
	return a.limitsReading(ctx)
}

// readableDocumentHits returns a predicate over chunk ids: false for a chunk hit
// of (scope, scopeID) the caller may not read. A nil predicate means nothing
// is left out — the caller's set cannot deny anything here, or there are no
// document hits to ask about.
func (m *Memory) readableDocumentHits(ctx context.Context, scope store.MemoryScope, scopeID string, chunkIDs []string) (func(string) bool, error) {
	if len(chunkIDs) == 0 {
		return nil, nil
	}
	limited, err := documentReadLimited(ctx, scope, scopeID)
	if err != nil || !limited {
		return nil, err
	}
	// Read HERE, at use: boot assigns the resolver before it is fully bound.
	reader, ok := m.DocumentFilters.(documentHitReader)
	if !ok || reader == nil {
		// Fail closed: with no Document tool there is no way to tell which
		// document a chunk belongs to, so none can be shown to be readable.
		return func(string) bool { return false }, nil
	}
	readable, err := reader.ReadableDocumentChunks(ctx, scope, scopeID, chunkIDs)
	if err != nil {
		return nil, err
	}
	return func(id string) bool { return readable[id] }, nil
}

// documentHitChunkID is the chunk a hit's key lies in — a chunk body's own id, or
// the chunk of a derived unit — or "" for a key outside the document namespace
// (a fact or note under its own key, a trace). Whatever the row's class.
func documentHitChunkID(key string) string {
	if id, ok := strings.CutPrefix(key, memrank.DocumentChunkKeyPrefix); ok {
		return id
	}
	id, _ := memrank.UnitChunkID(key)
	return id
}

// dropUnreadableSearchHits removes from res the chunk hits of (scope, scopeID)
// the caller may not read, keeping every index-aligned slice aligned. inScope
// reports whether entry i is from that scope (nil: all are).
func (m *Memory) dropUnreadableSearchHits(ctx context.Context, scope store.MemoryScope, scopeID string,
	res *memrank.SearchResult, inScope func(int) bool) error {
	var ids []string
	for i, e := range res.Entries {
		if inScope != nil && !inScope(i) {
			continue
		}
		if id := documentHitChunkID(e.Key); id != "" {
			ids = append(ids, id)
		}
	}
	keep, err := m.readableDocumentHits(ctx, scope, scopeID, ids)
	if err != nil || keep == nil {
		return err
	}
	n := 0
	for i, e := range res.Entries {
		if inScope == nil || inScope(i) {
			if id := documentHitChunkID(e.Key); id != "" && !keep(id) {
				continue
			}
		}
		res.Entries[n] = e
		if i < len(res.RankScores) {
			res.RankScores[n] = res.RankScores[i]
		}
		if i < len(res.MatchedUnits) {
			res.MatchedUnits[n] = res.MatchedUnits[i]
		}
		n++
	}
	res.Entries = res.Entries[:n]
	if len(res.RankScores) > n {
		res.RankScores = res.RankScores[:n]
	}
	if len(res.MatchedUnits) > n {
		res.MatchedUnits = res.MatchedUnits[:n]
	}
	return nil
}

// dropUnreadableRecallHits is dropUnreadableSearchHits for a recall's facts.
func (m *Memory) dropUnreadableRecallHits(ctx context.Context, scope store.MemoryScope, scopeID string,
	res *memrank.RecallResult, inScope func(int) bool) error {
	var ids []string
	for i, f := range res.Facts {
		if inScope != nil && !inScope(i) {
			continue
		}
		if id := documentHitChunkID(f.ID); id != "" {
			ids = append(ids, id)
		}
	}
	keep, err := m.readableDocumentHits(ctx, scope, scopeID, ids)
	if err != nil || keep == nil {
		return err
	}
	kept := res.Facts[:0:0]
	for i, f := range res.Facts {
		if inScope == nil || inScope(i) {
			if id := documentHitChunkID(f.ID); id != "" && !keep(id) {
				continue
			}
		}
		kept = append(kept, f)
	}
	res.Facts = kept
	return nil
}
