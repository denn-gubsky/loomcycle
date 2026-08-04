package memory

import (
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
			name: "facts excludes the document namespace",
			q:    SearchQuery{Sources: []Source{SourceFacts}},
			want: store.MemorySearchFilter{ExcludeKeyPrefix: DocumentChunkKeyPrefix},
		},
		{
			name: "documents restricts to it",
			q:    SearchQuery{Sources: []Source{SourceDocuments}},
			want: store.MemorySearchFilter{KeyPrefix: DocumentChunkKeyPrefix},
		},
		{
			name: "both is the same as neither",
			q:    SearchQuery{Sources: []Source{SourceFacts, SourceDocuments}},
			want: store.MemorySearchFilter{},
			comment: "asking for everything must not produce a contradictory predicate " +
				"(exclude AND require the same prefix), which would return nothing",
		},
		{
			name: "an explicit prefix survives facts-only",
			q:    SearchQuery{Prefix: "proj/", Sources: []Source{SourceFacts}},
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
			got := tc.q.Filter()
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
	f := SearchQuery{Sources: []Source{SourceDocuments, SourceFacts}}.Filter()
	if f.KeyPrefix == DocumentChunkKeyPrefix && f.ExcludeKeyPrefix == DocumentChunkKeyPrefix {
		t.Fatal("asking for both sources produced require-and-exclude on the same prefix, " +
			"which matches nothing — the widest request would return the narrowest result")
	}
	if !f.IsZero() {
		t.Errorf("both sources should constrain nothing, got %+v", f)
	}
}
