package inprocess_test

import (
	"context"
	"encoding/json"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// unitsFixture: chunk c1's body says "alice", but five of its units are phrased
// the way the question is ("go rust"); chunk c2's body is "bob python". A question
// in the units' words should find c1 THROUGH its units — once.
func unitsFixture(t *testing.T) (*inprocess.Backend, func()) {
	t.Helper()
	b, _, _, cleanup := vectorFixture(t)
	ctx := context.Background()
	set := func(key, value, embed string) {
		if _, err := b.Set(ctx, store.MemoryScopeUser, "u1", key, json.RawMessage(value),
			memory.SetOptions{Embed: true, EmbedText: embed}); err != nil {
			t.Fatal(err)
		}
	}
	set("doc.chunk:c1", `{"body":"alice"}`, "Guide — One\nalice")
	set("doc.chunk:c2", `{"body":"bob python"}`, "Guide — Two\nbob python")
	for i := 0; i < 5; i++ {
		v, _ := json.Marshal(memory.UnitValue{ChunkID: "c1", Kind: memory.UnitQuestion, Text: "how do go and rust compare?"})
		set(memory.UnitKey("c1", memory.UnitQuestion, i), string(v), "Guide — One\ngo rust")
	}
	return b, cleanup
}

func searchUnits(t *testing.T, b *inprocess.Backend, q memory.SearchQuery) memory.SearchResult {
	t.Helper()
	res, err := b.Search(context.Background(), store.MemoryScopeUser, "u1", q, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Entries {
		if _, isUnit := memory.UnitChunkID(e.Key); isUnit {
			t.Fatalf("a unit reached the caller as itself: %s", e.Key)
		}
	}
	return res
}

// TestSearchUnits_AUnitFindsItsChunkOnce — on the default search (units ride the
// main legs) and on a documents search (units come through their own leg), c1 is
// found through its units, appears once, and says which unit found it.
func TestSearchUnits_AUnitFindsItsChunkOnce(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	for name, q := range map[string]memory.SearchQuery{
		"default":   {QueryText: "go rust", TopK: 5},
		"documents": {QueryText: "go rust", TopK: 5, Sources: []memory.Source{memory.SourceDocuments}},
		"prefix":    {QueryText: "go rust", TopK: 5, Prefix: memory.DocumentChunkKeyPrefix},
	} {
		res := searchUnits(t, b, q)
		if len(res.Entries) == 0 || res.Entries[0].Key != "doc.chunk:c1" {
			t.Errorf("%s: top = %v, want doc.chunk:c1 found through its units", name, keys(res))
			continue
		}
		n := 0
		for _, e := range res.Entries {
			if e.Key == "doc.chunk:c1" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: c1 occupies %d slots, want 1 — five matching units must not take five", name, n)
		}
		if len(res.MatchedUnits) != len(res.Entries) || res.MatchedUnits[0] == nil ||
			res.MatchedUnits[0].Kind != memory.UnitQuestion || res.MatchedUnits[0].Text != "how do go and rust compare?" {
			t.Errorf("%s: matched = %+v", name, res.MatchedUnits)
		}
		if res.Truncated {
			t.Errorf("%s: truncated — a chunk's units are not extra results", name)
		}
	}
}

// TestSearchUnits_AnOptedOutAgentAndANotesSearchNeverSeeUnits.
func TestSearchUnits_AnOptedOutAgentAndANotesSearchNeverSeeUnits(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	for name, q := range map[string]memory.SearchQuery{
		"memory_units: false":        {QueryText: "go rust", TopK: 5, NoUnits: true},
		"memory_units: false (docs)": {QueryText: "go rust", TopK: 5, NoUnits: true, Sources: []memory.Source{memory.SourceDocuments}},
		"notes":                      {QueryText: "go rust", TopK: 5, Sources: []memory.Source{memory.SourceNotes}},
	} {
		res := searchUnits(t, b, q)
		if res.MatchedUnits != nil {
			t.Errorf("%s: matched units %+v", name, res.MatchedUnits)
		}
	}
}

// TestSearchUnits_AUnitWhoseChunkIsGoneIsDropped — an orphan unit (the sweeper
// has not reaped it yet) must not surface as itself or as a phantom chunk.
func TestSearchUnits_AUnitWhoseChunkIsGoneIsDropped(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	if _, err := b.Delete(context.Background(), store.MemoryScopeUser, "u1", "doc.chunk:c1"); err != nil {
		t.Fatal(err)
	}
	res := searchUnits(t, b, memory.SearchQuery{QueryText: "go rust", TopK: 5})
	for _, e := range res.Entries {
		if e.Key == "doc.chunk:c1" {
			t.Error("a deleted chunk came back through its orphaned units")
		}
	}
}

// TestSearchUnits_TheRerankRanksChunksNotUnits — resolution precedes the rerank,
// so the model is shown each chunk once, as the chunk.
func TestSearchUnits_TheRerankRanksChunksNotUnits(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	r := &scriptedReranker{reply: "[2, 1]"}
	b.SetReranker(r)
	res := searchUnits(t, b, memory.SearchQuery{QueryText: "go rust", TopK: 5, Rerank: rerankOn})
	if res.Rerank == nil || res.Rerank.Candidates != 2 {
		t.Fatalf("the rerank saw %+v candidates, want the 2 chunks", res.Rerank)
	}
	if res.Entries[1].Key != "doc.chunk:c1" || res.MatchedUnits[1] == nil {
		t.Errorf("after the rerank the matched unit must travel with its chunk: %v %+v", keys(res), res.MatchedUnits)
	}
}

// TestRecallUnits_RecallCarriesTheMatchedUnit — recall over documents resolves
// the same way and reports the unit.
func TestRecallUnits_RecallCarriesTheMatchedUnit(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	res, err := b.Recall(context.Background(), store.MemoryScopeUser, "u1", memory.RecallQuery{
		Query: "go rust", TopK: 5, Sources: []memory.Source{memory.SourceDocuments}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facts) == 0 || res.Facts[0].ID != "doc.chunk:c1" || res.Facts[0].MatchedUnit == nil {
		t.Errorf("recall = %+v", res.Facts)
	}
}

// TestScopeUsage_UnitsDoNotCountAgainstTheQuota.
func TestScopeUsage_UnitsDoNotCountAgainstTheQuota(t *testing.T) {
	b, cleanup := unitsFixture(t)
	defer cleanup()
	keys, _, err := b.ScopeUsage(context.Background(), store.MemoryScopeUser, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if keys != 0 {
		t.Errorf("quota counts %d keys, want 0 — two chunk bodies and five units are all Document rows", keys)
	}
}
