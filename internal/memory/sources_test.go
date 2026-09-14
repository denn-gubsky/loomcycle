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
			name: "documents restricts to prose — the namespace AND no provenance",
			q:    SearchQuery{Sources: []Source{SourceDocuments}},
			want: store.MemorySearchFilter{KeyPrefix: DocumentChunkKeyPrefix, Provenance: store.ProvenanceAbsent},
			comment: "the prefix alone selects every chunk body, facts included, so " +
				"asking for documents used to return distilled facts; a fact's body " +
				"row carries an origin, so excluding provenance is what makes this " +
				"selector mean prose",
		},
		{
			name: "all three is the same as neither",
			q:    SearchQuery{Sources: []Source{SourceFacts, SourceNotes, SourceDocuments}},
			want: store.MemorySearchFilter{},
			comment: "asking for everything must not produce a contradictory predicate " +
				"(exclude AND require the same prefix), which would return nothing",
		},
		{
			name: "facts are wherever provenance is, now that the chunk is their home",
			q:    SearchQuery{Sources: []Source{SourceFacts}},
			want: store.MemorySearchFilter{Provenance: store.ProvenanceRequired},
			comment: "origin is server-stamped, so it is the unforgeable discriminator " +
				"(RFC BW §9 Q1) — class is model-supplied and would let an agent promote " +
				"its own note to a fact. No namespace constraint: excluding the chunk " +
				"prefix was right only while a fact's home was its own k/v row and the " +
				"chunk body was a duplicate, and it would now hide a chunk-homed fact",
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
			name: "facts+notes excludes the document CLASS, not the namespace",
			q:    SearchQuery{Sources: []Source{SourceFacts, SourceNotes}},
			want: store.MemorySearchFilter{ExcludeDocumentPrefix: DocumentChunkKeyPrefix},
			comment: "the recall default: everything the agent remembers, prose " +
				"excluded. Excluding the namespace no longer expresses that, because " +
				"the facts half now lives inside it — so what is ruled out is the " +
				"class (a chunk body with no provenance) rather than the prefix",
		},
		{
			name: "an explicit prefix survives a source selector",
			q:    SearchQuery{Prefix: "proj/", Sources: []Source{SourceFacts, SourceNotes}},
			want: store.MemorySearchFilter{KeyPrefix: "proj/", ExcludeDocumentPrefix: DocumentChunkKeyPrefix},
		},
		{
			name: "an explicit prefix WINS over documents-only",
			q:    SearchQuery{Prefix: "doc.chunk:abc", Sources: []Source{SourceDocuments}},
			want: store.MemorySearchFilter{KeyPrefix: "doc.chunk:abc", Provenance: store.ProvenanceAbsent},
			comment: "narrowing to one chunk must not be widened back to the whole " +
				"namespace by the selector",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.q.Filter()
			if err != nil {
				t.Fatalf("Filter() error = %v", err)
			}
			// EVERY selector that does not ask for traces excludes them, so the
			// invariant is asserted here once rather than restated on each `want`
			// below — restating it would make the table about the exclusion instead
			// of about what each selector is FOR, and a row that forgot it would read
			// as a deliberate exception.
			if got.ExcludeTracePrefix != store.TraceTurnKeyPrefix {
				t.Errorf("selector did not exclude raw turns: %+v", got)
			}
			got.ExcludeTracePrefix = ""
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
	// Everything the three KNOWN classes can be, and nothing else — except the trace
	// exclusion, which is on every selector that does not ask for traces INCLUDING
	// this one. "All three sources" was written when three was all there was; traces
	// are opt-in by decision, so the widest request over the derived layers still does
	// not reach into the raw one.
	if f.ExcludeTracePrefix != store.TraceTurnKeyPrefix {
		t.Errorf("the widest derived-layer request did not exclude raw turns, got %+v", f)
	}
	f.ExcludeTracePrefix = ""
	if !f.IsZero() {
		t.Errorf("both sources should constrain nothing beyond the trace exclusion, got %+v", f)
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
		// PROVENANCE wins, not the namespace. The prefix says "this row is a chunk
		// body"; it does not say what kind of thing the chunk holds, and facts and
		// prose are both chunk bodies. Namespace-first therefore labelled every
		// chunk-homed fact a document — which is why `sources=documents` returned
		// facts — and the filter agreed with the label only because both were wrong
		// in the same way. A body row carries an origin iff a distiller wrote it.
		{"doc.chunk:abc", "consolidator", store.MemoryRowFact},
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

// TestSearchQueryFilter_DualWriteNeverSelectsAFactTwice is the invariant the
// FactsAreChunkHomed constant exists for.
//
// While a fact is written to both planes, its k/v row and its chunk body BOTH
// carry an origin, so a provenance-only predicate matches both and one fact comes
// back twice — measured against real Postgres as 2 hits for a single fact, on
// `sources=facts` and on the `facts+notes` recall default. Duplicates halve the
// effective top_k, which is an accuracy effect, so this is a correctness invariant
// rather than a retrieval-quality preference.
//
// The assertion is written against the CONSTANT rather than against today's value,
// so it keeps meaning after the collapse flips it: before, exactly one plane is
// selected; after, the namespace is no longer excluded because there is no twin
// left to exclude.
func TestSearchQueryFilter_DualWriteNeverSelectsAFactTwice(t *testing.T) {
	for _, srcs := range [][]Source{
		{SourceFacts},
		{SourceFacts, SourceNotes},
	} {
		f, err := (SearchQuery{Sources: srcs}).Filter()
		if err != nil {
			t.Fatalf("Filter(%v): %v", srcs, err)
		}
		if FactsAreChunkHomed {
			if f.ExcludeKeyPrefix == DocumentChunkKeyPrefix {
				t.Errorf("%v still excludes the chunk namespace after the collapse — that is "+
					"where the facts live now, so this selects nothing", srcs)
			}
			continue
		}
		if f.ExcludeKeyPrefix != DocumentChunkKeyPrefix {
			t.Errorf("%v selects BOTH planes while a fact is dual-written, so every fact "+
				"comes back twice and the effective top_k halves; got %+v", srcs, f)
		}
	}
}

// RFC CI §4 — traces are the one source whose DEFAULT is exclusion.

// TestSearchQueryFilter_TracesAloneScopeToTheNamespace.
func TestSearchQueryFilter_TracesAloneScopeToTheNamespace(t *testing.T) {
	f, err := SearchQuery{Sources: []Source{SourceTraces}}.Filter()
	if err != nil {
		t.Fatalf("traces alone must be expressible: %v", err)
	}
	if f.KeyPrefix != store.TraceTurnKeyPrefix {
		t.Errorf("KeyPrefix = %q, want the trace namespace", f.KeyPrefix)
	}
	if f.ExcludeTracePrefix != "" {
		t.Errorf("asking for traces also excluded them — require-and-exclude on one " +
			"prefix matches nothing, so the only request for this class would return empty")
	}
}

// TestSearchQueryFilter_TracesRefuseToCombine. Raw turns and the layers derived from
// them are independent dimensions, and a fused result ranks a turn against the fact
// extracted from it. Refused by name rather than silently returning one of the two.
func TestSearchQueryFilter_TracesRefuseToCombine(t *testing.T) {
	for _, with := range [][]Source{
		{SourceTraces, SourceFacts},
		{SourceTraces, SourceNotes},
		{SourceTraces, SourceDocuments},
		{SourceTraces, SourceFacts, SourceNotes, SourceDocuments},
	} {
		if _, err := (SearchQuery{Sources: with}).Filter(); !errors.Is(err, ErrTracesNotCombinable) {
			t.Errorf("Filter(%v) error = %v, want ErrTracesNotCombinable", with, err)
		}
	}
}

// TestSearchQueryFilter_AnUnfilteredSearchStillExcludesTraces is the compatibility
// property RFC CI §4 turns on: the off-run search has no default sources, so an empty
// selector means "every plane". Admitting a new class there would change every
// existing unfiltered search silently — and traces would dominate what it changed to,
// being far more numerous than facts and lexically overlapping them by construction.
func TestSearchQueryFilter_AnUnfilteredSearchStillExcludesTraces(t *testing.T) {
	f, err := SearchQuery{}.Filter()
	if err != nil {
		t.Fatalf("an empty selector must be expressible: %v", err)
	}
	if f.ExcludeTracePrefix != store.TraceTurnKeyPrefix {
		t.Fatal("an unfiltered search would now return raw turns — every existing " +
			"caller's results change the day the index is enabled")
	}
	f.ExcludeTracePrefix = ""
	if !f.IsZero() {
		t.Errorf("an empty selector constrains something other than traces: %+v", f)
	}
}

// TestClassifyMemoryRow_TraceBeatsProvenance. The origin-first order exists because a
// chunk body can be a fact or prose. A trace is neither, so no provenance should be
// able to relabel one as a fact and pull it into an unfiltered search.
func TestClassifyMemoryRow_TraceBeatsProvenance(t *testing.T) {
	key := store.TraceTurnKeyPrefix + "sess-a:7"
	if got := store.ClassifyMemoryRow(key, "consolidator", DocumentChunkKeyPrefix); got != store.MemoryRowTrace {
		t.Errorf("a trace row carrying provenance classified as %q — it would be returned "+
			"by sources=facts and by every unfiltered search", got)
	}
	if got := store.ClassifyMemoryRow(key, "", DocumentChunkKeyPrefix); got != store.MemoryRowTrace {
		t.Errorf("plain trace row classified as %q", got)
	}
	// And the existing classes are untouched.
	if got := store.ClassifyMemoryRow("memory/fact/x", "consolidator", DocumentChunkKeyPrefix); got != store.MemoryRowFact {
		t.Errorf("fact classification regressed: %q", got)
	}
	if got := store.ClassifyMemoryRow(DocumentChunkKeyPrefix+"abc", "", DocumentChunkKeyPrefix); got != store.MemoryRowDocument {
		t.Errorf("document classification regressed: %q", got)
	}
}
