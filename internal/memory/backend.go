package memory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Backend is the RFC I (MR-2) pluggability seam between the Memory tool
// and the storage substrate. The Memory tool routes its data operations
// through a Backend instead of calling store.Store directly, so MR-3's
// MemoryBackendDef and any future server-side backend can plug in
// behind the same six methods.
//
// The default implementation is the in-process backend
// (internal/memory/backends/inprocess), which delegates to a store.Store
// + loomcycle's Embedder. It is the unconditional fallback and is
// behaviorally identical to the pre-MR-2 direct-store path.
//
// Scope of MR-2 — what is and is NOT on the interface:
//
//   - Get / Set / Delete / List / Search / Stats are here.
//   - Incr is NOT: it is an in-process-only atomic op the tool keeps
//     calling on store.Store directly (MemoryIncrement). A server-side
//     backend has its own atomicity story; routing it through here would
//     leak that detail into MR-2.
//   - BulkInsert is NOT: deferred to MR-5 eval-seeding.
//   - The reducer ops (merge / append_dedupe / bounded_list) stay on the
//     tool via store.MemoryAtomicUpdate — they are read-modify-write
//     primitives, not part of the six-op data surface MR-3/MR-4 plug into.
//
// Search and Set own the embedding work: an in-process backend embeds
// query/value text via loomcycle's Embedder; a remote backend embeds
// server-side. The tool no longer touches the Embedder for these paths
// (it keeps the field only for the upfront misconfiguration pre-flight).
type Backend interface {
	// Get reads one entry. Returns *store.ErrNotFound for both a missing
	// and an expired key (the caller renders {"value": null}).
	Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error)

	// Set writes (or overwrites) one entry. When opts.Embed is set the
	// backend ALSO produces and stores an embedding. The embedding is a
	// best-effort companion to the k/v row: a transient embedder failure
	// AFTER the k/v lands does NOT roll back — it surfaces via
	// SetResult.EmbedWarning with SetResult.Embedded=false. A PERMANENT
	// misconfiguration (no embedder, no vector support) is refused via
	// the returned error BEFORE any k/v write (a typed *store.MemoryError
	// so the tool can render the exact same upfront-refusal message).
	Set(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, opts SetOptions) (SetResult, error)

	// Delete removes an entry. Returns whether a row existed.
	Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error)

	// List enumerates entries under (scope, scopeID) with an optional key
	// prefix, capped at limit. truncated reports whether more rows matched
	// than were returned.
	List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) (entries []store.MemoryEntry, truncated bool, err error)

	// Search embeds q.QueryText, runs the Top-K cosine search (with the
	// over-fetch pool a hybrid rank needs), applies the MR-1 RankConfig,
	// runs the MR-5 search-time dedup, trims to q.TopK, and returns the
	// ranked entries. The backend owns the embed step. Dedup runs AFTER
	// ranking and BEFORE the trim so the highest-ranked member of a
	// duplicate cluster survives (RFC I Decision 2); a zero-value
	// DedupConfig (Enabled=false) is the zero-regression no-op. Refuses
	// with a typed *store.MemoryError when vectors are unsupported, no
	// embedder is configured, or a dimension mismatch is detected — the
	// tool renders these unchanged.
	Search(ctx context.Context, scope store.MemoryScope, scopeID string, q SearchQuery, rank RankConfig, dedup DedupConfig) (SearchResult, error)

	// Stats returns per-(provider, model) embedding row counts for the
	// scope. Backs the admin embed-stats view in later slices.
	Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error)
}

// SetOptions carries the write-side knobs for Backend.Set.
type SetOptions struct {
	// TTL > 0 sets an expiry; <= 0 means no expiry (or keep existing on
	// update), matching store.MemorySet's contract.
	TTL time.Duration
	// Embed requests an embedding alongside the k/v row.
	Embed bool
	// EmbedText is the text to embed when Embed is set. Empty falls back
	// to the JSON-stringified value (the pre-MR-2 persistEmbedding rule).
	EmbedText string
	// Provenance (RFC BL) records where this fact came from — set by a
	// consolidation write. A zero value takes the plain k/v write path, so
	// every pre-existing caller is byte-identical.
	Provenance store.MemoryProvenance
}

// SetResult reports the embedding outcome of a Set. For a non-embed write
// both fields are zero and the caller omits the embedded/embed_warning
// keys entirely (preserving the pre-MR-2 response shape). For an embed
// write, Embedded reflects success; on a transient failure Embedded is
// false and EmbedWarning carries the (non-fatal) reason.
type SetResult struct {
	Embedded     bool
	EmbedWarning string
}

// SearchQuery is the input to Backend.Search. The backend embeds
// QueryText internally (in-process via the Embedder; server-side for
// a remote service), so the tool passes text, never a vector.
type SearchQuery struct {
	QueryText string
	Prefix    string
	TopK      int

	// Sources names WHICH KINDS of remembered thing may be returned (RFC BW). Empty
	// means "no restriction" — every existing caller therefore behaves exactly as
	// before.
	//
	// It exists because the memory keyspace is shared: Document chunk bodies live in
	// the same (tenant, scope, scope_id) partition under a reserved prefix, and once
	// RFC BU embedded them they became indistinguishable from an agent's own facts in
	// a vector search. Measured on the reference deployment, a medication question
	// returned seven document chunks in ten hits, with a horizontal rule outranking
	// the fact that held the answer.
	//
	// A NAMED SELECTOR rather than a prefix the caller supplies: the failure this
	// answers is an agent not knowing a reserved string, so requiring it to know one
	// would answer the symptom. Prefix remains for callers that genuinely want to
	// narrow by key.
	Sources []Source

	// When bounds the rows' OBSERVED time (RFC CL). The zero value constrains
	// nothing, so a caller that never mentions time is unaffected.
	When ObservedWindow
}

// Source is one kind of remembered thing a search may return.
type Source string

const (
	// SourceFacts is memory a consolidator DISTILLED — it has provenance, so a
	// server-known writer stands behind it. It does NOT include notes; that
	// predates the facts/notes split and reading it that way is what let recall's
	// default drop every directly-written row.
	SourceFacts Source = "facts"
	// SourceNotes is memory an agent wrote directly with `set` — no provenance, so no
	// server-known writer distilled it.
	SourceNotes Source = "notes"
	// SourceDocuments is Document chunk bodies — prose written down in a document,
	// which is NOT the same claim as something the user said.
	SourceDocuments Source = "documents"
)

// ErrSourcesNotExpressible is returned for a source set that cannot be rendered as a
// single conjunctive predicate — see SearchQuery.Filter.
var ErrSourcesNotExpressible = errors.New(
	"memory: this combination of sources cannot be filtered in one query. " +
		"The document namespace and the provenance split are independent dimensions, so " +
		"mixing documents with only ONE of facts/notes would need a disjunction the store " +
		"does not build. Use [facts], [notes], [facts,notes], [documents], or all three")

// Filter renders the requested sources as the store-level predicate.
//
// The two dimensions are INDEPENDENT: whether a row is in the document namespace (a
// key prefix) and whether it carries provenance (a column). Every combination this
// returns is therefore a conjunction, which is what a single indexed query can apply
// with the LIMIT after the predicate.
//
// TWO COMBINATIONS ARE REFUSED rather than approximated: documents together with only
// ONE of facts/notes needs `(key LIKE doc) OR (key NOT LIKE doc AND provenance…)` — a
// disjunction across both dimensions. It could be built, and it is deliberately not,
// because the honest alternatives were worse: silently widening to "everything" would
// hand back rows the caller excluded while it believed the filter applied, which is
// the failure RFC BW §6 exists to prevent. Refusing names the supported sets instead.
// Both refused combinations are also odd requests ("my facts and my documents but not
// my notes"); if one turns out to be real, the disjunction is the fix.
func (q SearchQuery) Filter() (store.MemorySearchFilter, error) {
	// The RFC CL window is applied FIRST so it survives every return path below.
	// Filter has several early exits (no selector, documents-only, the refusal), and
	// attaching the window at each one is how a predicate ends up honoured on some
	// queries and silently dropped on others.
	f := q.When.Filter(store.MemorySearchFilter{KeyPrefix: q.Prefix})
	var facts, notes, docs bool
	for _, s := range q.Sources {
		switch s {
		case SourceFacts:
			facts = true
		case SourceNotes:
			notes = true
		case SourceDocuments:
			docs = true
		}
	}
	if !facts && !notes && !docs {
		return f, nil // no selector: constrain nothing
	}
	memory := facts || notes
	if docs && memory && !(facts && notes) {
		return store.MemorySearchFilter{}, ErrSourcesNotExpressible
	}
	switch {
	case docs && !memory:
		// An explicit prefix wins if the caller set one; otherwise scope to documents.
		if f.KeyPrefix == "" {
			f.KeyPrefix = DocumentChunkKeyPrefix
		}
	case memory && !docs:
		f.ExcludeKeyPrefix = DocumentChunkKeyPrefix
		if facts && !notes {
			f.Provenance = store.ProvenanceRequired
		} else if notes && !facts {
			f.Provenance = store.ProvenanceAbsent
		}
	}
	// docs && facts && notes → everything; the zero filter already says that.
	return f, nil
}

// Class labels a result row so a caller can tell what it got (RFC BW §4b).
func Class(e store.MemorySearchEntry) store.MemoryRowClass {
	return store.ClassifyMemoryRow(e.Key, e.Origin, DocumentChunkKeyPrefix)
}

// SearchResult is the ranked output of Backend.Search. Entries are
// already trimmed to TopK and carry their cosine Score. RankScores is
// index-aligned with Entries (the hybrid score each result was ordered
// by) — the backend computes it with the SAME `now` it ranked with, so
// the rendered rank_score always matches the ordering. QueryEmbeddingDim
// is the dimension of the embedded query (rendered as query_embedding_dim).
// Truncated reports that the cosine pool had more matches than TopK.
// RankNote is non-empty when a reserved (source/frequency) weight was set
// — carries MR-1's surfaced note so the tool renders it unchanged.
// DedupDropped is the number of near-duplicate entries the MR-5 dedup pass
// dropped (mode=drop/merge) or flagged (mode=keep) out of the post-rank,
// pre-trim candidate set. It is 0 when dedup is disabled.
type SearchResult struct {
	// TimeFilter is what the RFC CL observed-time predicate did, present only when
	// the query carried one. Nil means no window was asked for — distinct from a
	// window that matched nothing, which reports zero counts.
	TimeFilter *TimeFilterReport

	Entries           []store.MemorySearchEntry
	RankScores        []float64
	QueryEmbeddingDim int
	Truncated         bool
	RankNote          string

	// SourcesApplied reports that the backend actually honoured SearchQuery.Sources
	// (RFC BW phase 3).
	//
	// FALSE IS THE ZERO VALUE ON PURPOSE. A backend that ignores the selector — an
	// external memory-layer service with its own namespaces — must not be able to do
	// so silently, because the caller would then trust a label the result does not
	// deserve: it asked for facts and is reading document prose. Defaulting to false
	// means the honest answer is what you get for free, and honouring the selector is
	// the thing you have to declare.
	SourcesApplied bool
	DedupDropped   int
}
