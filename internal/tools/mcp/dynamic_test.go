package mcp

import (
	"sync"
	"testing"
)

func TestDynamicRegistry_GetReturnsZeroValueForUnknown(t *testing.T) {
	r := NewDynamicRegistry()
	_, ok := r.Get("nope")
	if ok {
		t.Error("Get on unknown name returned ok=true")
	}
}

func TestDynamicRegistry_SetGetRoundTrip(t *testing.T) {
	r := NewDynamicRegistry()
	spec := DynamicMCPServerSpec{
		Name:      "n8n-mailgun",
		Transport: "streamable-http",
		URL:       "https://n8n.example.com/mcp/abc",
		Headers:   map[string]string{"Authorization": "Bearer ${LOOMCYCLE_N8N_TOKEN}"},
	}
	r.Set(spec)
	got, ok := r.Get("n8n-mailgun")
	if !ok {
		t.Fatal("Get after Set returned ok=false")
	}
	if got.URL != spec.URL || got.Transport != spec.Transport {
		t.Errorf("Get returned wrong spec: %+v", got)
	}
	if got.Headers["Authorization"] != spec.Headers["Authorization"] {
		t.Errorf("Headers lost in round-trip")
	}
}

func TestDynamicRegistry_SetReplacesExisting(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{Name: "x", URL: "https://v1.example/mcp"})
	r.Set(DynamicMCPServerSpec{Name: "x", URL: "https://v2.example/mcp"})
	got, _ := r.Get("x")
	if got.URL != "https://v2.example/mcp" {
		t.Errorf("second Set didn't replace: %s", got.URL)
	}
}

func TestDynamicRegistry_RemoveExisting(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{Name: "x"})
	if !r.Remove("x") {
		t.Error("Remove of existing entry returned false")
	}
	if _, ok := r.Get("x"); ok {
		t.Error("entry still present after Remove")
	}
}

func TestDynamicRegistry_RemoveNonexistent(t *testing.T) {
	r := NewDynamicRegistry()
	if r.Remove("nope") {
		t.Error("Remove of nonexistent entry returned true")
	}
}

func TestDynamicRegistry_Names(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{Name: "zeta"})
	r.Set(DynamicMCPServerSpec{Name: "alpha"})
	r.Set(DynamicMCPServerSpec{Name: "mu"})
	got := r.Names()
	want := []string{"alpha", "mu", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Names() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDynamicRegistry_ConcurrentSetGetRemove(t *testing.T) {
	r := NewDynamicRegistry()
	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		i := i
		wg.Add(3)
		go func() {
			defer wg.Done()
			r.Set(DynamicMCPServerSpec{Name: "x", URL: "https://example.com"})
			_ = i
		}()
		go func() {
			defer wg.Done()
			_, _ = r.Get("x")
		}()
		go func() {
			defer wg.Done()
			r.Remove("x")
		}()
	}
	wg.Wait()
	// Just must not race-detector-trip; final state doesn't matter.
}
