package memory

import (
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestSearchQueryFilter_SourcesMapToPredicate pins the mapping RFC BW rests on.
//
// The selector exists so a caller never names the reserved doc.chunk: prefix — that
// knowledge belongs to the runtime, and an agent not knowing a reserved string is the
// failure this answers. So these cases are the contract: what a caller asks for, and
// what the store is told.
func TestSearchQueryFilter_SourcesMapToPredicate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		q       SearchQuery
		want    store.MemorySearchFilter
		comment string
	}{
		{
			name: "no sources constrains nothing",
			q:    SearchQuery{},
			want: store.MemorySearchFilter{},
			comment: "every pre-RFC-BW caller lands here, so behaviour is unchanged " +
				"unless a selector is passed",
		},

		{
			name: "documents restricts to it",
			q:    SearchQuery{Sources: []Source{SourceDocuments}},
			want: store.MemorySearchFilter{KeyPrefix: DocumentChunkKeyPrefix},
		},
		{
			name: "all three is the same as neither",
			q:    SearchQuery{Sources: []Source{SourceFacts, SourceNotes, SourceDocuments}},
			want: store.MemorySearchFilter{},
			comment: "asking for everything must not produce a contradictory predicate " +
				"(exclude AND require the same prefix), which would return nothing",
		},
		{
			name: "facts only requires provenance",
			q:    SearchQuery{Sources: []Source{SourceFacts}},
			want: store.MemorySearchFilter{
				ExcludeKeyPrefix: DocumentChunkKeyPrefix,
				Provenance:       store.ProvenanceRequired,
			},
			comment: "origin is server-stamped, so it is the unforgeable discriminator " +
				"(RFC BW §9 Q1) — class is model-supplied and would let an agent promote " +
				"its own note to a fact",
		},
		{
			name: "notes only excludes provenance",
			q:    SearchQuery{Sources: []Source{SourceNotes}},
			want: store.MemorySearchFilter{
				ExcludeKeyPrefix: DocumentChunkKeyPrefix,
				Provenance:       store.ProvenanceAbsent,
			},
		},
		{
			name: "facts+notes is memory with no provenance constraint",
			q:    SearchQuery{Sources: []Source{SourceFacts, SourceNotes}},
			want: store.MemorySearchFilter{ExcludeKeyPrefix: DocumentChunkKeyPrefix},
			comment: "the recall default: everything the agent remembers, documents " +
				"excluded",
		},
		{
			name: "an explicit prefix survives a source selector",
			q:    SearchQuery{Prefix: "proj/", Sources: []Source{SourceFacts, SourceNotes}},
			want: store.MemorySearchFilter{KeyPrefix: "proj/", ExcludeKeyPrefix: DocumentChunkKeyPrefix},
		},
		{
			name: "an explicit prefix WINS over documents-only",
			q:    SearchQuery{Prefix: "doc.chunk:abc", Sources: []Source{SourceDocuments}},
			want: store.MemorySearchFilter{KeyPrefix: "doc.chunk:abc"},
			comment: "narrowing to one chunk must not be widened back to the whole " +
				"namespace by the selector",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.q.Filter()
			if err != nil {
				t.Fatalf("Filter() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Filter() = %+v, want %+v\n%s", got, tc.want, tc.comment)
			}
		})
	}
}

// TestSearchQueryFilter_BothSourcesIsNotContradictory guards the case that would fail
// closed rather than open: requiring AND excluding the same prefix matches no row, so
// a caller asking for facts AND documents would get an empty result.
func TestSearchQueryFilter_BothSourcesIsNotContradictory(t *testing.T) {
	f, err := SearchQuery{Sources: []Source{SourceDocuments, SourceFacts, SourceNotes}}.Filter()
	if err != nil {
		t.Fatalf("all three sources should be expressible: %v", err)
	}
	if f.KeyPrefix == DocumentChunkKeyPrefix && f.ExcludeKeyPrefix == DocumentChunkKeyPrefix {
		t.Fatal("asking for both sources produced require-and-exclude on the same prefix, " +
			"which matches nothing — the widest request would return the narrowest result")
	}
	if !f.IsZero() {
		t.Errorf("both sources should constrain nothing, got %+v", f)
	}
}

// TestSearchQueryFilter_RefusesInexpressibleCombinations pins the deliberate limit.
//
// Documents-plus-only-one-of-facts/notes needs a DISJUNCTION across two independent
// dimensions — `(key LIKE doc) OR (key NOT LIKE doc AND provenance…)`. It could be
// built and is deliberately not; the alternative that matters is what happens instead.
// Silently widening to "everything" would hand back rows the caller excluded while it
// believed the filter applied, which is the exact failure RFC BW §6 exists to prevent:
// a result trusted for a label it did not earn.
func TestSearchQueryFilter_RefusesInexpressibleCombinations(t *testing.T) {
	for _, srcs := range [][]Source{
		{SourceFacts, SourceDocuments},
		{SourceNotes, SourceDocuments},
	} {
		got, err := (SearchQuery{Sources: srcs}).Filter()
		if !errors.Is(err, ErrSourcesNotExpressible) {
			t.Errorf("Filter(%v) err = %v, want ErrSourcesNotExpressible — silently widening "+
				"would return rows the caller excluded", srcs, err)
		}
		if !got.IsZero() {
			t.Errorf("Filter(%v) returned a usable filter %+v alongside the error; a caller "+
				"ignoring the error would then run an unintended query", srcs, got)
		}
	}
	// And the supported sets must NOT refuse.
	for _, srcs := range [][]Source{
		nil,
		{SourceFacts},
		{SourceNotes},
		{SourceFacts, SourceNotes},
		{SourceDocuments},
		{SourceFacts, SourceNotes, SourceDocuments},
	} {
		if _, err := (SearchQuery{Sources: srcs}).Filter(); err != nil {
			t.Errorf("Filter(%v) refused a supported set: %v", srcs, err)
		}
	}
}

// TestClass_LabelsRowsFromTheirOwnColumns — the label a result carries must be derived
// from the same inputs the filter selects on, or `kind` becomes a claim the filter does
// not back.
func TestClass_LabelsRowsFromTheirOwnColumns(t *testing.T) {
	for _, tc := range []struct {
		key, origin string
		want        store.MemoryRowClass
	}{
		{"doc.chunk:abc", "", store.MemoryRowDocument},
		{"doc.chunk:abc", "consolidator", store.MemoryRowDocument}, // namespace wins
		{"memory/fact/x", "consolidator", store.MemoryRowFact},
		{"memory/fact/x", "", store.MemoryRowNote}, // no writer stamped
		{"scratch/todo", "", store.MemoryRowNote},
		{"scratch/todo", "  ", store.MemoryRowNote}, // whitespace is not an origin
	} {
		got := Class(store.MemorySearchEntry{
			MemoryEntry: store.MemoryEntry{Key: tc.key},
			Origin:      tc.origin,
		})
		if got != tc.want {
			t.Errorf("Class(key=%q origin=%q) = %q, want %q", tc.key, tc.origin, got, tc.want)
		}
	}
}
