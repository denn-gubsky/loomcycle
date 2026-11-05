package redact

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// liveRuns is a settable set of live run ids.
type liveRuns struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (l *liveRuns) set(id string, live bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = map[string]bool{}
	}
	l.ids[id] = live
}

func (l *liveRuns) snapshot() func(string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make(map[string]bool, len(l.ids))
	for k, v := range l.ids {
		cp[k] = v
	}
	return func(id string) bool { return cp[id] }
}

func newBounded(capacity int) (*Redactor, *liveRuns) {
	live := &liveRuns{}
	r := New(nil, false)
	r.dyn.cap = capacity
	r.SetLiveness(live.snapshot)
	return r, live
}

func held(r *Redactor, v string) bool {
	r.dyn.mu.RLock()
	defer r.dyn.mu.RUnlock()
	_, ok := r.dyn.entries[v]
	return ok
}

func bucketLen(r *Redactor, kind bucketKind, h Holder) int {
	r.dyn.mu.RLock()
	defer r.dyn.mu.RUnlock()
	if b := r.dyn.buckets[bucketKey(kind, h)]; b != nil {
		return b.Len()
	}
	return 0
}

func masked(t *testing.T, r *Redactor, v string) {
	t.Helper()
	if got := r.String("echo " + v + " done"); got != "echo "+dynamicMarker+" done" {
		t.Errorf("%s not masked whole: %q", v, got)
	}
}

// TestRedact_DynamicSetIsBounded: a bucket never holds more than its share of
// unpinned values, and the set stops growing at its cap, each registration
// evicting only from its own bucket; the compiled replacer shrinks with it.
func TestRedact_DynamicSetIsBounded(t *testing.T) {
	r, _ := newBounded(64) // credential share 16, observed share 4
	a := Holder{Tenant: "a", Runs: []string{"run-a"}}
	for i := 0; i < 1000; i++ {
		r.RegisterCredential(a, fmt.Sprintf("a-credential-%04d", i))
	}
	if n := bucketLen(r, bucketCredential, a); n != 16 {
		t.Errorf("tenant a holds %d credentials, want its share of 16", n)
	}
	if held(r, "a-credential-0000") || !held(r, "a-credential-0999") {
		t.Error("the share did not evict the oldest value first")
	}
	u := Holder{Tenant: "a", User: "u1", Runs: []string{"run-a"}}
	for i := 0; i < 100; i++ {
		r.RegisterObserved(u, fmt.Sprintf("url-part-%04d", i))
	}
	if n := bucketLen(r, bucketObserved, u); n != 4 {
		t.Errorf("user u1 holds %d observed values, want the observed share of 4", n)
	}
	// A few tenants: the set stops at the cap.
	for tn := 0; tn < 4; tn++ {
		h := Holder{Tenant: fmt.Sprintf("t%02d", tn)}
		for i := 0; i < 16; i++ {
			r.RegisterCredential(h, fmt.Sprintf("t%02d-credential-%02d", tn, i))
		}
	}
	if n := len(r.dyn.entries); n > 64 {
		t.Errorf("the set holds %d values; want at most the cap of 64", n)
	}
	// Many tenants: each keeps its floor, so past cap/bucketFloor buckets the
	// set holds the floors and no more.
	for tn := 4; tn < 40; tn++ {
		h := Holder{Tenant: fmt.Sprintf("t%02d", tn)}
		for i := 0; i < 16; i++ {
			r.RegisterCredential(h, fmt.Sprintf("t%02d-credential-%02d", tn, i))
		}
	}
	if n, limit := len(r.dyn.entries), len(r.dyn.buckets)*bucketFloor; n > limit {
		t.Errorf("the set holds %d values across %d buckets; want at most their floors, %d", n, len(r.dyn.buckets), limit)
	}
	_ = r.String("compile")
	if r.dyn.repl == nil || r.dyn.dirty {
		t.Error("the replacer was not rebuilt from the bounded set")
	}
}

// TestRedact_RegistrationsNeverEvictAnotherTenantsValue: however many values
// other tenants (or another user's model) register, a tenant's own values stay
// and stay masked.
func TestRedact_RegistrationsNeverEvictAnotherTenantsValue(t *testing.T) {
	r, _ := newBounded(16)
	r.RegisterCredential(Holder{Tenant: "victim"}, "victim-credential")
	r.RegisterOperator("operator-session-token")
	for tn := 0; tn < 50; tn++ {
		for i := 0; i < 8; i++ {
			r.RegisterObserved(Holder{Tenant: fmt.Sprintf("t%02d", tn), User: "u"}, fmt.Sprintf("t%02d-url-part-%d", tn, i))
			r.RegisterCredential(Holder{Tenant: fmt.Sprintf("t%02d", tn)}, fmt.Sprintf("t%02d-cred-%d", tn, i))
		}
	}
	for _, v := range []string{"victim-credential", "operator-session-token"} {
		if !held(r, v) {
			t.Errorf("other tenants' registrations evicted %s", v)
		}
		masked(t, r, v)
	}
	// The same tenant's model-driven values cannot evict its credentials
	// either: the observed bucket is its own.
	for i := 0; i < 100; i++ {
		r.RegisterObserved(Holder{Tenant: "victim", User: "model-driven"}, fmt.Sprintf("victim-url-%03d", i))
	}
	if !held(r, "victim-credential") {
		t.Error("observed values evicted the tenant's credential")
	}
}

// TestRedact_LiveRunsValueIsNeverEvicted: a value a live run registered stays
// however far its own bucket is pushed past its share, and goes once the run
// is over.
func TestRedact_LiveRunsValueIsNeverEvicted(t *testing.T) {
	r, live := newBounded(16) // credential share 4
	live.set("run-live", true)
	h := Holder{Tenant: "t", Runs: []string{"run-live"}}
	r.RegisterCredential(h, "pinned-credential")
	for i := 0; i < 200; i++ {
		r.RegisterCredential(Holder{Tenant: "t", Runs: []string{fmt.Sprintf("run-%d", i)}}, fmt.Sprintf("later-credential-%03d", i))
	}
	if !held(r, "pinned-credential") {
		t.Fatal("a live run's credential was evicted")
	}
	masked(t, r, "pinned-credential")
	if n := bucketLen(r, bucketCredential, h); n != 4 {
		t.Errorf("bucket holds %d, want its share of 4 (the pinned value and the 3 newest)", n)
	}

	// Pinned values alone past the share are kept.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("live-%d", i)
		live.set(id, true)
		r.RegisterCredential(Holder{Tenant: "t", Runs: []string{id}}, fmt.Sprintf("pinned-%d-credential", i))
	}
	for i := 0; i < 10; i++ {
		if v := fmt.Sprintf("pinned-%d-credential", i); !held(r, v) {
			t.Errorf("pinned %s evicted", v)
		}
	}

	// Runs over: the next registrations may evict their values.
	live.set("run-live", false)
	for i := 0; i < 10; i++ {
		live.set(fmt.Sprintf("live-%d", i), false)
	}
	for i := 0; i < 20; i++ {
		r.RegisterCredential(Holder{Tenant: "t"}, fmt.Sprintf("after-%02d-credential", i))
	}
	if held(r, "pinned-credential") {
		t.Error("a value stayed pinned after its run ended")
	}
}

// TestRedact_SharedValueLeavesOnlyWhenEveryOwnerLetsGo: a secret two tenants
// resolved stays while either still holds it.
func TestRedact_SharedValueLeavesOnlyWhenEveryOwnerLetsGo(t *testing.T) {
	r, _ := newBounded(16) // share 4
	a, b := Holder{Tenant: "a"}, Holder{Tenant: "b"}
	r.RegisterCredential(a, "shared-credential")
	r.RegisterCredential(b, "shared-credential")
	for i := 0; i < 10; i++ {
		r.RegisterCredential(a, fmt.Sprintf("a-only-credential-%d", i))
	}
	if !held(r, "shared-credential") {
		t.Fatal("tenant a letting go evicted a value tenant b still holds")
	}
	masked(t, r, "shared-credential")
	for i := 0; i < 10; i++ {
		r.RegisterCredential(b, fmt.Sprintf("b-only-credential-%d", i))
	}
	if held(r, "shared-credential") {
		t.Error("the value stayed after both owners let it go")
	}
}

// TestRedact_JustRegisteredValueIsMaskedImmediately: the String right after a
// registration masks the value, even when the set is full of pinned values
// from elsewhere and the registering bucket is empty.
func TestRedact_JustRegisteredValueIsMaskedImmediately(t *testing.T) {
	r, live := newBounded(4)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("run-%d", i)
		live.set(id, true)
		r.RegisterCredential(Holder{Tenant: fmt.Sprintf("t%d", i), Runs: []string{id}}, fmt.Sprintf("full-%d-credential", i))
	}
	masked(t, r, "full-0-credential")
	r.RegisterCredential(Holder{Tenant: "late"}, "fresh-credential-value")
	masked(t, r, "fresh-credential-value")
	r.RegisterObserved(Holder{Tenant: "late", User: "u"}, "fresh-url-part")
	masked(t, r, "fresh-url-part")
}

// TestRedact_OverlappingValuesMaskLikeOneReplacer: values that overlap (a
// suffix of one is a prefix of another) or nest are masked exactly as one
// longest-first replacer over the whole set masks them, whatever order they
// were registered in and whatever was compiled in between.
func TestRedact_OverlappingValuesMaskLikeOneReplacer(t *testing.T) {
	const a, b = "zzzzzzzzxxxxOVER", "OVERyyyy"
	const outer, inner = "wrap-first-credential-wrap", "first-credential"
	vals := []string{a, b, outer, inner}
	text := "1 " + a + "yyyy 2 " + b + " 3 " + outer + " 4 " + inner + " 5 xxxx" + b
	for _, order := range [][]string{{a, b, outer, inner}, {b, a, inner, outer}} {
		r, _ := newBounded(0)
		for i, v := range order {
			r.RegisterCredential(Holder{Tenant: "t"}, v)
			if i == 0 {
				_ = r.String("compile the first one")
			}
		}
		want := longestFirstReplacer(append([]string(nil), vals...)).Replace(text)
		if got := r.String(text); got != want {
			t.Errorf("order %v:\n got  %q\n want %q", order, got, want)
		}
		for _, v := range vals {
			masked(t, r, v)
		}
	}
}

// TestRedact_RebuildIsCoalesced: registrations between two Strings cost one
// rebuild, and a String with nothing new costs none.
func TestRedact_RebuildIsCoalesced(t *testing.T) {
	r, _ := newBounded(0)
	for i := 0; i < 500; i++ {
		r.RegisterCredential(Holder{Tenant: "t"}, fmt.Sprintf("value-%04d-xyz", i))
	}
	_ = r.String("x")
	_ = r.String("y")
	if r.dyn.rebuildCount != 1 {
		t.Errorf("500 registrations and two Strings rebuilt %d times, want 1", r.dyn.rebuildCount)
	}
	if !strings.Contains(r.String("a value-0042-xyz b"), dynamicMarker) {
		t.Error("value not masked")
	}
}

// BenchmarkRedact_RebuildAtCap measures one rebuild of a full set: what the
// first String after a change pays.
func BenchmarkRedact_RebuildAtCap(b *testing.B) {
	r, _ := newBounded(0)
	for i := 0; i < DefaultDynamicCap; i++ {
		r.RegisterCredential(Holder{Tenant: fmt.Sprintf("t%d", i%4)}, fmt.Sprintf("credential-value-%05d-%x", i, i*7919))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.dyn.mu.Lock()
		r.dyn.dirty = true
		r.dyn.mu.Unlock()
		_ = r.String("x")
	}
}

// TestRedact_OverCapChargesTheBucketsOverTheirFairShare: when the set is full
// because other tenants filled their shares, a tenant registering within its
// fair share keeps its values; the overflow is taken from the buckets furthest
// over theirs.
func TestRedact_OverCapChargesTheBucketsOverTheirFairShare(t *testing.T) {
	r, _ := newBounded(64) // credential share 16
	for _, tn := range []string{"t1", "t2", "t3"} {
		for i := 0; i < 16; i++ {
			r.RegisterCredential(Holder{Tenant: tn}, fmt.Sprintf("%s-credential-%02d", tn, i))
		}
	}
	a := Holder{Tenant: "a"}
	for i := 0; i < 1000; i++ {
		r.RegisterCredential(a, fmt.Sprintf("a-flood-%04d", i))
	}
	b := Holder{Tenant: "b"}
	for i := 0; i < 8; i++ {
		r.RegisterCredential(b, fmt.Sprintf("b-credential-%d", i))
	}
	for i := 0; i < 8; i++ {
		if v := fmt.Sprintf("b-credential-%d", i); !held(r, v) {
			t.Errorf("tenant b, within its fair share, lost %s", v)
		}
	}
	if n := len(r.dyn.entries); n > 64 {
		t.Errorf("the set holds %d values, want at most the cap of 64", n)
	}
	if n := bucketLen(r, bucketCredential, a); n >= 16 {
		t.Errorf("tenant a, furthest over its fair share, still holds %d", n)
	}
}
