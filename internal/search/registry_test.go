package search

import (
	"sort"
	"testing"
)

func TestBuildRegistry(t *testing.T) {
	reg, err := BuildRegistry([]ProviderSpec{
		{ID: "brave"}, {ID: "serper"}, {ID: "searxng", BaseURL: "http://searxng:8080"},
	})
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	for _, id := range []string{"brave", "serper", "searxng"} {
		p, ok := reg.Get(id)
		if !ok || p.ID() != id {
			t.Errorf("Get(%q) missing or wrong id", id)
		}
	}
	ids := reg.IDs()
	sort.Strings(ids)
	if !equal(ids, []string{"brave", "searxng", "serper"}) {
		t.Errorf("IDs() = %v", ids)
	}
}

func TestBuildRegistry_Errors(t *testing.T) {
	if _, err := BuildRegistry([]ProviderSpec{{ID: "searxng"}}); err == nil {
		t.Error("searxng without base_url should error")
	}
	if _, err := BuildRegistry([]ProviderSpec{{ID: "nope"}}); err == nil {
		t.Error("unknown provider id should error")
	}
}

func TestKnownProviderIDs(t *testing.T) {
	got := KnownProviderIDs()
	sort.Strings(got)
	if !equal(got, []string{"brave", "exa", "searxng", "serper", "tavily"}) {
		t.Errorf("KnownProviderIDs() = %v", got)
	}
}

func TestGet_NilRegistry(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("brave"); ok {
		t.Error("nil registry Get should be (nil,false)")
	}
}
