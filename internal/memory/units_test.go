package memory

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestUnitKey_RoundTripsItsChunk(t *testing.T) {
	k := UnitKey("abc123", UnitClaim, 2)
	if k != "doc.unit:abc123:claim:2" {
		t.Errorf("UnitKey = %q", k)
	}
	if id, ok := UnitChunkID(k); !ok || id != "abc123" {
		t.Errorf("UnitChunkID(%q) = %q, %v", k, id, ok)
	}
	for _, bad := range []string{"doc.chunk:abc123", "doc.unit:", "doc.unit::claim:0", "notes/x"} {
		if _, ok := UnitChunkID(bad); ok {
			t.Errorf("UnitChunkID(%q) claimed a unit key", bad)
		}
	}
	if !strings.HasPrefix(k, UnitKeyPrefixFor("abc123")) || strings.HasPrefix(UnitKey("abc1234", UnitClaim, 0), UnitKeyPrefixFor("abc123")) {
		t.Error("UnitKeyPrefixFor must match exactly one chunk's units, not a chunk whose id extends it")
	}
}

// TestFilter_UnitsReachOnlyDocumentSearches — units are Document material. A
// notes or facts search never sees them (a unit has no provenance, so a notes
// selector would take it for a note), an agent that opted out never sees them, and
// a search whose prefix is elsewhere needs no exclusion at all.
func TestFilter_UnitsReachOnlyDocumentSearches(t *testing.T) {
	for _, c := range []struct {
		name string
		q    SearchQuery
		excl bool
	}{
		{"default search admits units", SearchQuery{}, false},
		{"an opted-out agent does not", SearchQuery{NoUnits: true}, true},
		{"notes do not", SearchQuery{Sources: []Source{SourceNotes}}, true},
		{"facts do not", SearchQuery{Sources: []Source{SourceFacts}}, true},
		{"everything admits them", SearchQuery{Sources: []Source{SourceFacts, SourceNotes, SourceDocuments}}, false},
		{"a prefix elsewhere needs no exclusion", SearchQuery{Prefix: "notes/", NoUnits: true}, false},
	} {
		f, err := c.q.Filter()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := f.ExcludeUnitPrefix == DocumentUnitKeyPrefix; got != c.excl {
			t.Errorf("%s: excludes units = %v, want %v", c.name, got, c.excl)
		}
	}
}

// TestUnitLegFilter_OnlyForChunkTargetedSearches — the extra leg runs where the
// main legs cannot reach units (a documents selector, a doc.chunk: prefix), and
// is narrowed the same way the main prefix is.
func TestUnitLegFilter_OnlyForChunkTargetedSearches(t *testing.T) {
	for _, c := range []struct {
		name   string
		q      SearchQuery
		run    bool
		prefix string
	}{
		{"default: units ride the main legs", SearchQuery{}, false, ""},
		{"documents selector", SearchQuery{Sources: []Source{SourceDocuments}}, true, DocumentUnitKeyPrefix},
		{"chunk prefix", SearchQuery{Prefix: DocumentChunkKeyPrefix}, true, DocumentUnitKeyPrefix},
		{"one chunk", SearchQuery{Prefix: DocumentChunkKeyPrefix + "abc"}, true, DocumentUnitKeyPrefix + "abc"},
		{"opted out", SearchQuery{Sources: []Source{SourceDocuments}, NoUnits: true}, false, ""},
		{"not a document search", SearchQuery{Sources: []Source{SourceNotes}}, false, ""},
	} {
		main, err := c.q.Filter()
		if err != nil {
			t.Fatal(err)
		}
		f, run := c.q.UnitLegFilter(main)
		if run != c.run || f.KeyPrefix != c.prefix {
			t.Errorf("%s: leg = %v prefix %q, want %v %q", c.name, run, f.KeyPrefix, c.run, c.prefix)
		}
	}
}

func unitRow(chunk, kind string, n int, text string, score float64) store.MemorySearchEntry {
	v, _ := json.Marshal(UnitValue{ChunkID: chunk, Kind: kind, Text: text})
	return store.MemorySearchEntry{MemoryEntry: store.MemoryEntry{Key: UnitKey(chunk, kind, n), Value: v}, Score: score, SemanticScore: score}
}

func bodyRow(chunk string, score float64) store.MemorySearchEntry {
	return store.MemorySearchEntry{MemoryEntry: store.MemoryEntry{Key: DocumentChunkKeyPrefix + chunk, Value: json.RawMessage(`{"body":"b"}`)}, Score: score, SemanticScore: score}
}

func keysOf(es []store.MemorySearchEntry) string {
	var ks []string
	for _, e := range es {
		ks = append(ks, e.Key)
	}
	return strings.Join(ks, ",")
}

// TestResolveUnits_AChunkWithFiveMatchingUnitsOccupiesOneSlot — the RFC's C1
// check, and the point of resolving: units vote for their chunk, they never take
// slots of their own.
func TestResolveUnits_AChunkWithFiveMatchingUnitsOccupiesOneSlot(t *testing.T) {
	var pool []store.MemorySearchEntry
	for i := 0; i < 5; i++ {
		pool = append(pool, unitRow("c1", UnitQuestion, i, fmt.Sprintf("question %d", i), 0.9-float64(i)/100))
	}
	pool = append(pool, bodyRow("c1", 0.5), bodyRow("c2", 0.4))
	out, matched := ResolveUnits(pool, func(string) (store.MemorySearchEntry, bool) {
		t.Fatal("the body ranked in the pool; nothing needs fetching")
		return store.MemorySearchEntry{}, false
	})
	if got := keysOf(out); got != "doc.chunk:c1,doc.chunk:c2" {
		t.Fatalf("resolved = %s, want c1 once then c2", got)
	}
	if out[0].Score != 0.9 {
		t.Errorf("c1 took score %v, want its best unit's 0.9", out[0].Score)
	}
	if m := matched["doc.chunk:c1"]; m == nil || m.Kind != UnitQuestion || m.Text != "question 0" {
		t.Errorf("matched = %+v, want the best unit", m)
	}
	if matched["doc.chunk:c2"] != nil {
		t.Error("a chunk found directly carries no matched unit")
	}
}

// TestResolveUnits_AUnitBringsItsChunkWhenTheBodyDidNotRank — the unit's body is
// fetched and takes the unit's place; a unit whose chunk is gone is dropped, never
// returned as itself.
func TestResolveUnits_AUnitBringsItsChunkWhenTheBodyDidNotRank(t *testing.T) {
	pool := []store.MemorySearchEntry{
		unitRow("gone", UnitClaim, 0, "orphan claim", 0.95),
		unitRow("c3", UnitClaim, 0, "the claim", 0.9),
		bodyRow("c1", 0.8),
	}
	var fetched []string
	out, matched := ResolveUnits(pool, func(id string) (store.MemorySearchEntry, bool) {
		fetched = append(fetched, id)
		if id == "gone" {
			return store.MemorySearchEntry{}, false
		}
		return bodyRow(id, 0), true
	})
	if got := keysOf(out); got != "doc.chunk:c3,doc.chunk:c1" {
		t.Errorf("resolved = %s", got)
	}
	if !reflect.DeepEqual(fetched, []string{"gone", "c3"}) {
		t.Errorf("fetched %v", fetched)
	}
	if matched["doc.chunk:c3"] == nil || matched["doc.chunk:c3"].Text != "the claim" {
		t.Errorf("matched = %+v", matched)
	}
}

// TestResolveUnits_ABodyThatOutranksItsUnitsKeepsItsPlace — the chunk takes the
// BEST rank of itself and its units.
func TestResolveUnits_ABodyThatOutranksItsUnitsKeepsItsPlace(t *testing.T) {
	pool := []store.MemorySearchEntry{bodyRow("c1", 0.9), unitRow("c2", UnitClaim, 0, "x", 0.8), unitRow("c1", UnitClaim, 0, "y", 0.7)}
	out, matched := ResolveUnits(pool, func(id string) (store.MemorySearchEntry, bool) { return bodyRow(id, 0), true })
	if got := keysOf(out); got != "doc.chunk:c1,doc.chunk:c2" {
		t.Errorf("resolved = %s", got)
	}
	if matched["doc.chunk:c1"] != nil {
		t.Error("c1 ranked by its own body: it was not found through a unit")
	}
}

func TestResolveUnits_APoolWithoutUnitsIsUntouched(t *testing.T) {
	pool := []store.MemorySearchEntry{bodyRow("c1", 0.9), {MemoryEntry: store.MemoryEntry{Key: "notes/a"}}}
	out, matched := ResolveUnits(pool, nil)
	if matched != nil || &out[0] != &pool[0] {
		t.Error("a pool with no unit must be returned as it was")
	}
}

func TestFuseRRFMany_TwoListsIsFuseRRF(t *testing.T) {
	a := []store.MemorySearchEntry{bodyRow("c1", 0.9), bodyRow("c2", 0.8)}
	b := []store.MemorySearchEntry{bodyRow("c2", 3), bodyRow("c3", 2)}
	if !reflect.DeepEqual(FuseRRF(a, b, 60), FuseRRFMany(60, a, b)) {
		t.Error("FuseRRFMany over two lists must equal FuseRRF")
	}
	got := keysOf(FuseRRFMany(60, a, b, []store.MemorySearchEntry{bodyRow("c3", 1)}))
	if got != "doc.chunk:c2,doc.chunk:c3,doc.chunk:c1" {
		t.Errorf("three lists fused = %s", got)
	}
}
