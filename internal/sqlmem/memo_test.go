package sqlmem

import (
	"errors"
	"testing"
)

// TestMemo_RebuildsOnlyWhenTheFingerprintChanges.
func TestMemo_RebuildsOnlyWhenTheFingerprintChanges(t *testing.T) {
	m := &Manager{}
	key := ScopeKey{Tenant: "t", Scope: "tenant", ScopeID: "t"}
	builds := 0
	build := func() (any, error) { builds++; return builds, nil }

	v1, _ := m.Memo(key, "ontology", "fp1", build)
	v2, _ := m.Memo(key, "ontology", "fp1", build)
	if builds != 1 || v1 != v2 {
		t.Fatalf("same fingerprint rebuilt: builds=%d v1=%v v2=%v", builds, v1, v2)
	}
	v3, _ := m.Memo(key, "ontology", "fp2", build)
	if builds != 2 || v3 == v1 {
		t.Errorf("a changed fingerprint must rebuild: builds=%d", builds)
	}
	other := ScopeKey{Tenant: "u", Scope: "tenant", ScopeID: "u"}
	if _, _ = m.Memo(other, "ontology", "fp2", build); builds != 3 {
		t.Error("another scope must not share an entry")
	}
	if _, err := m.Memo(key, "x", "fp", func() (any, error) { return nil, errors.New("boom") }); err == nil {
		t.Error("a build error must be returned")
	}
	if _, _ = m.Memo(key, "x", "fp", build); builds != 4 {
		t.Error("a failed build must not be cached")
	}
	if _, err := m.Memo(ScopeKey{Tenant: "a\x1fb", Scope: "tenant", ScopeID: "x"}, "x", "fp", build); err == nil {
		t.Error("an invalid key must be refused, as EnsureOnce refuses it")
	}
}
