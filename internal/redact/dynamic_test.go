package redact

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeClock is a settable clock for the rebuild interval.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newBounded(capacity int) (*Redactor, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := New(nil, false)
	r.dyn.cap = capacity
	r.dyn.now = clk.now
	return r, clk
}

func held(r *Redactor, v string) bool {
	r.dyn.mu.RLock()
	defer r.dyn.mu.RUnlock()
	_, ok := r.dyn.entries[v]
	return ok
}

func ownedBy(r *Redactor, tenant string) int {
	r.dyn.mu.RLock()
	defer r.dyn.mu.RUnlock()
	if l := r.dyn.owners[tenant]; l != nil {
		return l.Len()
	}
	return 0
}

// TestRedact_DynamicSetIsBounded: the registered set never grows past its cap;
// a tenant past its share evicts its own oldest value, not another tenant's;
// tenant registrations never evict an operator value; a value registered again
// is fresh again; and the compiled replacer is bounded with the set.
func TestRedact_DynamicSetIsBounded(t *testing.T) {
	r, clk := newBounded(16) // share: 4
	for i := 0; i < 3; i++ {
		r.RegisterOperator(fmt.Sprintf("operator-secret-%d", i))
	}
	for i := 0; i < 4; i++ {
		r.RegisterTenant("tenant-a", fmt.Sprintf("a-secret-%d", i))
	}
	// Tenant b floods: a model-chosen value per call.
	for i := 0; i < 1000; i++ {
		clk.t = clk.t.Add(10 * time.Millisecond)
		r.RegisterTenant("tenant-b", fmt.Sprintf("b-flood-%04d", i))
	}
	if n := ownedBy(r, "tenant-b"); n != 4 {
		t.Errorf("tenant-b holds %d values, want its share of 4", n)
	}
	if held(r, "b-flood-0000") || !held(r, "b-flood-0999") {
		t.Error("tenant-b's flood did not evict its own oldest value first")
	}
	for i := 0; i < 4; i++ {
		v := fmt.Sprintf("a-secret-%d", i)
		if !held(r, v) {
			t.Errorf("tenant-b's flood evicted tenant-a's %s", v)
		}
		if got := r.String("echo " + v); strings.Contains(got, v) {
			t.Errorf("tenant-a's %s no longer masked after the flood: %q", v, got)
		}
	}

	// Many tenants: the global cap holds, the least recently used tenant
	// values go, and the operator values stay.
	r.RegisterTenant("tenant-a", "a-secret-0") // used again: now the freshest of a's
	for tn := 0; tn < 40; tn++ {
		for i := 0; i < 4; i++ {
			clk.t = clk.t.Add(10 * time.Millisecond)
			r.RegisterTenant(fmt.Sprintf("t%02d", tn), fmt.Sprintf("t%02d-secret-%d", tn, i))
		}
		if n := len(r.dyn.entries); n > 16 {
			t.Fatalf("after tenant %d the set holds %d values, want at most 16", tn, n)
		}
	}
	for i := 0; i < 3; i++ {
		v := fmt.Sprintf("operator-secret-%d", i)
		if !held(r, v) {
			t.Errorf("tenant registrations evicted operator value %s", v)
		}
		if got := r.String("echo " + v); strings.Contains(got, v) {
			t.Errorf("operator value %s not masked: %q", v, got)
		}
	}
	if !held(r, "t39-secret-3") || held(r, "t00-secret-0") {
		t.Error("the global cap did not evict the least recently used tenant value")
	}

	// The operator plane is bounded by its own share, evicting its own oldest.
	for i := 3; i < 10; i++ {
		r.RegisterOperator(fmt.Sprintf("operator-secret-%d", i))
	}
	if held(r, "operator-secret-0") || !held(r, "operator-secret-9") {
		t.Error("the operator plane did not keep its most recent share")
	}
	if n := r.dyn.operator.Len(); n != 4 {
		t.Errorf("operator plane holds %d values, want its share of 4", n)
	}

	// The compiled form is bounded with the set: everything compiled is
	// held now or was evicted since the last rebuild, never a growing log.
	clk.t = clk.t.Add(time.Hour)
	r.RegisterTenant("t39", "fresh-after-an-hour")
	if n := len(r.dyn.settled) + len(r.dyn.pending); n > 16 {
		t.Errorf("the compiled form holds %d values, want at most the cap of 16", n)
	}
}

// TestRedact_JustRegisteredValueIsMaskedImmediately: a registration inside the
// rebuild interval is masked by the very next String, whole, including a value
// that contains, or is contained in, one compiled before it.
func TestRedact_JustRegisteredValueIsMaskedImmediately(t *testing.T) {
	r, clk := newBounded(0)
	r.RegisterTenant("t", "first-credential-value") // compiled now
	rebuilds := r.dyn.rebuildCount

	clk.t = clk.t.Add(10 * time.Millisecond)
	r.RegisterTenant("t", "second-credential-value")
	if r.dyn.rebuildCount != rebuilds {
		t.Fatalf("a registration inside the interval rebuilt the set (%d -> %d); the deferral is not exercised", rebuilds, r.dyn.rebuildCount)
	}
	if got := r.String("x second-credential-value y"); got != "x "+dynamicMarker+" y" {
		t.Errorf("deferred value not masked at once: %q", got)
	}

	// A new value that contains a compiled one is masked whole, no fragment
	// of it left beside a marker.
	const outer = "wrap-first-credential-value-wrap"
	r.RegisterTenant("t", outer)
	if got := r.String("x " + outer + " y"); got != "x "+dynamicMarker+" y" {
		t.Errorf("value containing a compiled one leaked a fragment: %q", got)
	}
	// And one contained in a compiled value leaves that value masked whole.
	r.RegisterTenant("t", "credential")
	if got := r.String("x " + outer + " y"); got != "x "+dynamicMarker+" y" {
		t.Errorf("a shorter value split a compiled one: %q", got)
	}
	// Byte-identical to a single longest-first replacer over the same set.
	text := "a second-credential-value b first-credential-value c " + outer + " d credential e"
	want := longestFirstReplacer([]string{"first-credential-value", "second-credential-value", outer, "credential"}).Replace(text)
	if got := r.String(text); got != want {
		t.Errorf("overlay and settled set disagree with one replacer:\n got  %q\n want %q", got, want)
	}
}

// TestRedact_RebuildIsRateLimited: registrations inside one interval cost one
// rebuild per maxDynamicPending values, not one each; past the interval the
// next registration rebuilds.
func TestRedact_RebuildIsRateLimited(t *testing.T) {
	r, clk := newBounded(0)
	r.RegisterTenant("t", "seed-credential-0000")
	base := r.dyn.rebuildCount
	const n = 10 * maxDynamicPending
	for i := 0; i < n; i++ {
		r.RegisterTenant("t", fmt.Sprintf("value-%04d-xyz", i))
	}
	if got, limit := r.dyn.rebuildCount-base, n/maxDynamicPending; got > limit {
		t.Errorf("%d registrations in one interval rebuilt %d times, want at most %d", n, got, limit)
	}
	for i := 0; i < n; i++ {
		v := fmt.Sprintf("value-%04d-xyz", i)
		if got := r.String(v); got != dynamicMarker {
			t.Fatalf("%s not masked: %q", v, got)
		}
	}
	clk.t = clk.t.Add(dynamicRebuildInterval)
	before := r.dyn.rebuildCount
	r.RegisterTenant("t", "after-the-interval")
	if r.dyn.rebuildCount != before+1 || len(r.dyn.pending) != 0 {
		t.Errorf("a registration past the interval did not rebuild (rebuilds %d -> %d, pending %d)", before, r.dyn.rebuildCount, len(r.dyn.pending))
	}
}
