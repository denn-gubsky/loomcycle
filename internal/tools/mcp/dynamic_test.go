package mcp

import (
	"sync"
	"testing"
)

func TestDynamicRegistry_GetReturnsZeroValueForUnknown(t *testing.T) {
	r := NewDynamicRegistry()
	_, ok := r.Get("", "nope")
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
	got, ok := r.Get("", "n8n-mailgun")
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
	got, _ := r.Get("", "x")
	if got.URL != "https://v2.example/mcp" {
		t.Errorf("second Set didn't replace: %s", got.URL)
	}
}

func TestDynamicRegistry_RemoveExisting(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{Name: "x"})
	if !r.Remove("", "x") {
		t.Error("Remove of existing entry returned false")
	}
	if _, ok := r.Get("", "x"); ok {
		t.Error("entry still present after Remove")
	}
}

func TestDynamicRegistry_RemoveNonexistent(t *testing.T) {
	r := NewDynamicRegistry()
	if r.Remove("", "nope") {
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

// TestDynamicRegistry_TenantKeying pins the RFC N boundary: two tenants
// register the SAME name with DIFFERENT specs and each reads back its OWN
// — no cross-tenant collision (the leak the pool-key change closes).
func TestDynamicRegistry_TenantKeying(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{TenantID: "a", Name: "n8n", URL: "https://a.example/mcp"})
	r.Set(DynamicMCPServerSpec{TenantID: "b", Name: "n8n", URL: "https://b.example/mcp"})

	gotA, ok := r.Get("a", "n8n")
	if !ok || gotA.URL != "https://a.example/mcp" {
		t.Errorf("tenant a: got %+v ok=%v, want a's URL", gotA, ok)
	}
	gotB, ok := r.Get("b", "n8n")
	if !ok || gotB.URL != "https://b.example/mcp" {
		t.Errorf("tenant b: got %+v ok=%v, want b's URL", gotB, ok)
	}
	if _, ok := r.Get("c", "n8n"); ok {
		t.Error("tenant c unexpectedly resolved a's/b's n8n")
	}
	if !r.Remove("a", "n8n") {
		t.Error("Remove(a, n8n) returned false")
	}
	if _, ok := r.Get("a", "n8n"); ok {
		t.Error("a's entry still present after Remove")
	}
	if _, ok := r.Get("b", "n8n"); !ok {
		t.Error("b's entry was clobbered by Remove(a, n8n)")
	}
}

// TestDynamicRegistry_NamesForTenant pins the RFC N §3 candidate set: a
// tenant sees its own names PLUS shared ("" tenant) names, a tenant name
// shadows a shared one of the same name (appears once), and a different
// tenant's names are never visible.
func TestDynamicRegistry_NamesForTenant(t *testing.T) {
	r := NewDynamicRegistry()
	r.Set(DynamicMCPServerSpec{TenantID: "", Name: "shared-srv"})
	r.Set(DynamicMCPServerSpec{TenantID: "", Name: "shadowed"})
	r.Set(DynamicMCPServerSpec{TenantID: "a", Name: "a-only"})
	r.Set(DynamicMCPServerSpec{TenantID: "a", Name: "shadowed"}) // shadows the shared one
	r.Set(DynamicMCPServerSpec{TenantID: "b", Name: "b-only"})

	gotA := r.NamesForTenant("a")
	wantA := []string{"a-only", "shadowed", "shared-srv"}
	if !equalStrs(gotA, wantA) {
		t.Errorf("NamesForTenant(a) = %v, want %v (no b-only, shadowed dedup'd)", gotA, wantA)
	}
	gotShared := r.NamesForTenant("")
	wantShared := []string{"shadowed", "shared-srv"}
	if !equalStrs(gotShared, wantShared) {
		t.Errorf("NamesForTenant(\"\") = %v, want %v", gotShared, wantShared)
	}
	for _, n := range r.NamesForTenant("b") {
		if n == "a-only" {
			t.Error("tenant b's candidate set leaked a-only")
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
			_, _ = r.Get("", "x")
		}()
		go func() {
			defer wg.Done()
			r.Remove("", "x")
		}()
	}
	wg.Wait()
	// Just must not race-detector-trip; final state doesn't matter.
}
