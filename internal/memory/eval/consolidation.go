package eval

// Harness machinery for the consolidation eval (RFC BL P2): a full-text-capable
// store wrapper and an exact-cosine embedder.
//
// WHY NOT REUSE THE RETRIEVAL-QUALITY HARNESS'S PIECES AS-IS. The existing
// vectorStore + DeterministicEmbedder are exactly right for precision/recall
// over a corpus, and are reused here for everything that only needs "a real
// backend over a real store". Two invariants need more:
//
//   - HYBRID/RRF. sqlite reports SupportsFullText() == false, so the in-process
//     backend's keyword leg comes back empty and RRF degrades to the vector
//     order. An assertion about "the hybrid path" against that store proves
//     nothing about fusion. fullTextStore supplies the second leg.
//   - DEDUP BANDS. The band assertion (>=0.95 merges, 0.85–0.95 does not) needs
//     two pairs at KNOWN cosines. DeterministicEmbedder is a bag of hashed
//     tokens, so a pair's cosine depends on how its tokens collide mod dim —
//     controllable in principle, fragile in practice, and it would silently
//     drift the band if dim ever changed. FixedVectorEmbedder pins the cosine
//     exactly, so the fixture's premise cannot rot.
//
// Both are additive: the existing harness's numbers are untouched (a new type
// each, no change to vectorStore or DeterministicEmbedder).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fullTextKeywordCap bounds the naive keyword scan. The harness corpus is a
// handful of rows; the cap only stops a future large fixture from turning the
// keyword leg into a full-table walk.
const fullTextKeywordCap = 500

// fullTextStore is vectorStore plus a keyword retrieval leg, so the in-process
// backend takes its HYBRID path with BOTH legs populated and RRF actually fuses
// two rankings.
//
// The keyword scorer is deliberately naive (count of distinct query tokens
// present in the row's key or value, ties broken by key). It is not Postgres
// ts_rank and does not pretend to be: the invariant under test is that FUSION
// promotes a lexical-only match the vector leg ranks deep, and any monotone
// keyword ranking exercises that. Substituting a fancier scorer would add
// nothing and could hide a fusion bug behind its own ordering.
type fullTextStore struct {
	*vectorStore
}

// newConsolidationStore opens a fresh in-memory SQLite store wrapped with both
// the in-memory vector index and the keyword leg. The returned closer disposes
// the underlying store.
func newConsolidationStore() (*fullTextStore, func(), error) {
	vs, closeStore, err := newVectorStore()
	if err != nil {
		return nil, nil, err
	}
	return &fullTextStore{vectorStore: vs}, closeStore, nil
}

// SupportsFullText reports true so the in-process backend takes the hybrid path
// on its own capability check rather than incidentally (via a non-pure-semantic
// rank config) — the same reason the real gate is SupportsFullText and not
// SupportsVectors.
func (f *fullTextStore) SupportsFullText() bool { return true }

// MemoryFullTextSearch runs the keyword leg. Rows with no query-token overlap
// are omitted entirely: an empty result must mean "no lexical match", because
// FuseRRF would otherwise hand every row a spurious rank contribution and the
// fusion assertion would pass for the wrong reason.
func (f *fullTextStore) MemoryFullTextSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, filter store.MemorySearchFilter, queryText string, topK int) ([]store.MemorySearchEntry, error) {
	if strings.TrimSpace(queryText) == "" || topK <= 0 {
		return nil, nil
	}
	entries, _, err := f.Store.MemoryList(ctx, tenantID, scope, scopeID, filter.KeyPrefix, fullTextKeywordCap)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, tok := range tokenize(queryText) {
		want[tok] = true
	}

	type scored struct {
		entry store.MemoryEntry
		hits  int
	}
	var rows []scored
	for _, e := range entries {
		seen := map[string]bool{}
		hits := 0
		for _, tok := range tokenize(e.Key + " " + string(e.Value)) {
			if want[tok] && !seen[tok] {
				seen[tok] = true
				hits++
			}
		}
		if hits > 0 {
			rows = append(rows, scored{entry: e, hits: hits})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].hits != rows[j].hits {
			return rows[i].hits > rows[j].hits
		}
		return rows[i].entry.Key < rows[j].entry.Key
	})
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, r := range rows {
		// Score carries the leg's own relevance value. FuseRRF deliberately does
		// NOT fabricate a cosine for a full-text-only hit, so this is what the
		// tool would render as `score` for such a row — and leaving Vector empty
		// is correct: this leg has no embedding, and a full-text-only hit must
		// never be treated as a dedup anchor on a vector it does not have.
		se := store.MemorySearchEntry{MemoryEntry: r.entry, Score: float64(r.hits)}
		out = append(out, se)
	}
	return out, nil
}

// FixedVectorEmbedder returns a caller-pinned vector for a registered text and
// falls back to the deterministic hash embedder for anything else. It exists so
// a fixture can assert a threshold BAND: the cosine between two registered
// texts is exactly what the fixture computed, independent of tokenization,
// dimension, or hash collisions.
//
// It is NOT semantic and is not a substitute for DeterministicEmbedder in the
// retrieval-quality harness — it has no notion of shared meaning at all, only
// of vectors the fixture handed it.
type FixedVectorEmbedder struct {
	dim      int
	vecs     map[string][]float32
	fallback *DeterministicEmbedder
}

// NewFixedVectorEmbedder builds the embedder at the given dimension. dim <= 0
// falls back to 64, mirroring DeterministicEmbedder.
func NewFixedVectorEmbedder(dim int) *FixedVectorEmbedder {
	if dim <= 0 {
		dim = 64
	}
	return &FixedVectorEmbedder{dim: dim, vecs: map[string][]float32{}, fallback: NewDeterministicEmbedder(dim)}
}

// Register pins the vector returned for `text`. A wrong-length vector is a
// fixture bug, not a runtime condition, so it is reported as an error rather
// than silently padded — a padded vector would change the cosine and quietly
// invalidate a band assertion.
func (e *FixedVectorEmbedder) Register(text string, vec []float32) error {
	if len(vec) != e.dim {
		return fmt.Errorf("register %q: vector has %d dims, embedder has %d", text, len(vec), e.dim)
	}
	cp := make([]float32, len(vec))
	copy(cp, vec)
	e.vecs[text] = cp
	return nil
}

// Embed returns the registered vector for each text, or the deterministic
// hash vector when unregistered.
func (e *FixedVectorEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := e.vecs[t]; ok {
			cp := make([]float32, len(v))
			copy(cp, v)
			out[i] = cp
			continue
		}
		vs, err := e.fallback.Embed(ctx, []string{t})
		if err != nil {
			return nil, err
		}
		out[i] = vs[0]
	}
	return out, nil
}

func (e *FixedVectorEmbedder) Model() string    { return "fixed-vector-eval-stub" }
func (e *FixedVectorEmbedder) Provider() string { return "eval" }
func (e *FixedVectorEmbedder) Dimension() int   { return e.dim }

var _ providers.Embedder = (*FixedVectorEmbedder)(nil)

// UnitAxis returns the unit vector along `axis`.
func UnitAxis(dim, axis int) []float32 {
	v := make([]float32, dim)
	if axis >= 0 && axis < dim {
		v[axis] = 1
	}
	return v
}

// UnitTilt returns a unit vector whose cosine with UnitAxis(dim, axis) is
// exactly `cos`, tilted into `into`. Building the fixture vectors this way is
// what makes the dedup-band assertion exact: cos is the fixture's input, not a
// property of some tokenizer.
//
// A cos outside [0,1] or a degenerate axis pair yields the plain axis vector, so
// a fixture typo produces an obviously-wrong cosine of 1 rather than a NaN that
// would silently make every comparison false.
func UnitTilt(dim, axis, into int, cos float64) []float32 {
	v := UnitAxis(dim, axis)
	if cos < 0 || cos > 1 || into == axis || into < 0 || into >= dim || axis < 0 || axis >= dim {
		return v
	}
	v[axis] = float32(cos)
	v[into] = float32(math.Sqrt(1 - cos*cos))
	return v
}
