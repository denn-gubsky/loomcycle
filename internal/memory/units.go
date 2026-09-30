package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Document derived search units (RFC DM Design C).
//
// A unit is a short text a model wrote ABOUT a chunk — a description, one atomic
// claim, or one question the chunk answers — indexed on its own so a question can
// match a unit's phrasing when it does not match the chunk's own words. It is the
// memory pattern applied to documents: find the small unit, hand the reader its
// source.
//
// A UNIT IS AN INDEX ROW, NEVER A RESULT. It lives in the memory keyspace beside
// the chunk bodies, never in the document tree, and when a search finds one it
// RESOLVES to its chunk: the chunk takes the best rank of itself and its units,
// appears once, and says which unit found it. A chunk's units never take slots of
// their own — that crowding is what cost the probe its answers when units were
// stored as extra chunks.

// DocumentUnitKeyPrefix is the reserved namespace for units:
// doc.unit:<chunk_id>:<kind>:<n>. Separate from DocumentChunkKeyPrefix on purpose:
// everything that means "chunk bodies only" — related, the graph walk, verbatim
// answers, the chunk-body export — keeps meaning exactly that without a change.
const DocumentUnitKeyPrefix = "doc.unit:"

// Unit kinds, as written in the key and the value.
const (
	UnitDescription = "description"
	UnitClaim       = "claim"
	UnitQuestion    = "question"
)

// UnitKey is the key of the n-th unit of a kind for a chunk.
func UnitKey(chunkID, kind string, n int) string {
	return fmt.Sprintf("%s%s:%s:%d", DocumentUnitKeyPrefix, chunkID, kind, n)
}

// UnitKeyPrefixFor is the prefix every unit of one chunk shares.
func UnitKeyPrefixFor(chunkID string) string { return DocumentUnitKeyPrefix + chunkID + ":" }

// UnitChunkID returns the chunk a unit key belongs to. ok is false for a key that
// is not a unit key.
func UnitChunkID(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, DocumentUnitKeyPrefix)
	if !ok {
		return "", false
	}
	id, _, found := strings.Cut(rest, ":")
	if !found || id == "" {
		return "", false
	}
	return id, true
}

// UnitValue is what a unit row stores.
type UnitValue struct {
	ChunkID string `json:"chunk_id"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	// Model wrote it; BodyRevision is the revision of the chunk body it was
	// written from, which is how a later pass finds the stale ones.
	Model        string `json:"model,omitempty"`
	BodyRevision int    `json:"body_revision,omitempty"`
	// BodySHA256 is the hash of the body text the unit was written from. It, not
	// the revision, decides staleness: a chunk's revision also moves when its
	// status or fields change, and regenerating units for those would spend a
	// model call on a body nobody edited.
	BodySHA256 string `json:"body_sha256,omitempty"`
	// UnitCount is how many units the write that produced this one wrote for its
	// chunk. The hash says a unit is from the current body; only the count says the
	// chunk still holds ALL of them — a write cut short part-way leaves a smaller
	// set whose every unit has the current hash. 0 on a unit written before the
	// field existed.
	UnitCount int `json:"unit_count,omitempty"`
}

// MatchedUnit is the unit that found a chunk, as a search reports it.
type MatchedUnit struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// admitsDocuments reports whether the source selector admits Document material.
func (q SearchQuery) admitsDocuments() bool {
	if len(q.Sources) == 0 {
		return true
	}
	for _, s := range q.Sources {
		if s == SourceDocuments {
			return true
		}
	}
	return false
}

// withUnitExclusion keeps units out of every search that must not see them: one
// whose selector does not admit documents (notes, facts — units carry no
// provenance, so a notes selector would otherwise take them for notes), and every
// search of an agent that opted out. Only where the key prefix could reach the
// unit namespace at all; a prefix elsewhere already excludes it.
func (q SearchQuery) withUnitExclusion(f store.MemorySearchFilter) store.MemorySearchFilter {
	if !prefixReaches(f.KeyPrefix, DocumentUnitKeyPrefix) {
		return f
	}
	if q.NoUnits || !q.admitsDocuments() {
		f.ExcludeUnitPrefix = DocumentUnitKeyPrefix
	}
	return f
}

// UnitLegFilter returns the filter for a separate derived-unit retrieval leg, and
// whether one should run. It runs only when units are admitted, the search can
// return documents, and the main legs CANNOT reach the unit namespace — which is
// exactly the chunk-targeted searches (sources=[documents], a doc.chunk: prefix,
// Document op=search). Everywhere else units, if admitted, ride the main legs.
func (q SearchQuery) UnitLegFilter(main store.MemorySearchFilter) (store.MemorySearchFilter, bool) {
	if q.NoUnits || !q.CanReturnDocuments() || prefixReaches(main.KeyPrefix, DocumentUnitKeyPrefix) {
		return store.MemorySearchFilter{}, false
	}
	prefix := DocumentUnitKeyPrefix
	// A search narrowed to one chunk (or a range of chunk ids) is narrowed to
	// their units the same way.
	if rest, ok := strings.CutPrefix(main.KeyPrefix, DocumentChunkKeyPrefix); ok {
		prefix += rest
	}
	return q.When.Filter(store.MemorySearchFilter{KeyPrefix: prefix}), true
}

// prefixReaches reports whether a key filtered by prefix could lie in ns.
func prefixReaches(prefix, ns string) bool {
	return prefix == "" || strings.HasPrefix(ns, prefix) || strings.HasPrefix(prefix, ns)
}

// ResolveUnits collapses unit hits onto their chunks, in rank order. For each chunk
// the FIRST occurrence wins — its own body or one of its units, whichever ranked
// higher — and every later occurrence is dropped, so a chunk appears once however
// many of its units matched.
//
// When a unit wins, its chunk's body row takes the unit's place (fetched with body
// when the body did not rank on its own), carrying the unit's scores: the chunk is
// ranked by the best match it had. A unit whose chunk body is gone is dropped — a
// unit never reaches a caller as itself. matched maps each such entry's key to the
// unit that found it.
//
// A pool with no unit in it is returned unchanged, so a scope with no units pays
// nothing.
func ResolveUnits(pool []store.MemorySearchEntry, body func(chunkID string) (store.MemorySearchEntry, bool)) (out []store.MemorySearchEntry, matched map[string]*MatchedUnit) {
	hasUnit := false
	for _, e := range pool {
		if strings.HasPrefix(e.Key, DocumentUnitKeyPrefix) {
			hasUnit = true
			break
		}
	}
	if !hasUnit {
		return pool, nil
	}
	bodies := make(map[string]store.MemorySearchEntry)
	for _, e := range pool {
		if id, ok := strings.CutPrefix(e.Key, DocumentChunkKeyPrefix); ok {
			if _, dup := bodies[id]; !dup {
				bodies[id] = e
			}
		}
	}
	seen := make(map[string]bool)
	out = make([]store.MemorySearchEntry, 0, len(pool))
	for _, e := range pool {
		chunkID, isUnit := UnitChunkID(e.Key)
		if !isUnit {
			if id, isBody := strings.CutPrefix(e.Key, DocumentChunkKeyPrefix); isBody {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			out = append(out, e)
			continue
		}
		if seen[chunkID] {
			continue
		}
		b, ok := bodies[chunkID]
		if !ok {
			if b, ok = body(chunkID); !ok {
				continue // the chunk is gone: its unit must not surface as itself
			}
		}
		seen[chunkID] = true
		b.Score, b.SemanticScore, b.EmbeddedWith = e.Score, e.SemanticScore, e.EmbeddedWith
		out = append(out, b)
		if matched == nil {
			matched = make(map[string]*MatchedUnit)
		}
		matched[b.Key] = unitMatch(e.Value)
	}
	return out, matched
}

// unitMatch reads what a unit row says about itself.
func unitMatch(v json.RawMessage) *MatchedUnit {
	var u UnitValue
	_ = json.Unmarshal(v, &u)
	return &MatchedUnit{Kind: u.Kind, Text: u.Text}
}

// UnitRequest is one chunk to write units for. Kinds is the opt-in vocabulary:
// description, claims, questions.
type UnitRequest struct {
	DocumentTitle string
	SectionPath   string // the chunk's heading path below the document title, " > "-joined
	Text          string
	Kinds         []string
}

// GeneratedUnit is one unit a model wrote: Kind is UnitDescription, UnitClaim or
// UnitQuestion.
type GeneratedUnit struct {
	Kind string
	Text string
}

// UnitGenerator writes a chunk's units. The generation pass depends on this, not
// on a driver, so a test can stand one in.
type UnitGenerator interface {
	ModelID() string
	Generate(ctx context.Context, r UnitRequest) ([]GeneratedUnit, error)
}
