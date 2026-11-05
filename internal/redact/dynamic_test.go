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

func observedLen(r *Redactor, tenant string) int {
	r.dyn.mu.RLock()
	defer r.dyn.mu.RUnlock()
	if b := r.dyn.observed[tenant]; b != nil {
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

// TestRedact_DynamicSetIsBounded: with no live run, credentials stop at the
// cap (the least recently registered go), a tenant's observed values at its
// share, the operator plane at its share; the compiled replacer follows.
func TestRedact_DynamicSetIsBounded(t *testing.T) {
	r, _ := newBounded(64) // operator share 16, observed share 4
	for tn := 0; tn < 10; tn++ {
		for i := 0; i < 100; i++ {
			r.RegisterCredential(Holder{Tenant: fmt.Sprintf("t%d", tn)}, fmt.Sprintf("t%d-credential-%03d", tn, i))
		}
	}
	if n := r.dyn.creds.Len(); n != 64 {
		t.Errorf("credentials: %d held, want the cap of 64", n)
	}
	if held(r, "t9-credential-035") || !held(r, "t9-credential-036") {
		t.Error("the cap did not keep exactly the most recently registered")
	}
	for i := 0; i < 100; i++ {
		r.RegisterObserved(Holder{Tenant: "t0", User: "u"}, fmt.Sprintf("url-part-%03d", i))
		r.RegisterOperator(fmt.Sprintf("operator-secret-%03d", i))
	}
	if n := observedLen(r, "t0"); n != 4 {
		t.Errorf("observed: %d held, want the share of 4", n)
	}
	if n := r.dyn.operator.Len(); n != 16 {
		t.Errorf("operator: %d held, want the share of 16", n)
	}
	if n := len(r.dyn.entries); n != 64+4+16 {
		t.Errorf("the set holds %d values, want 84", n)
	}
	_ = r.String("compile")
	if r.dyn.repl == nil || r.dyn.dirty {
		t.Error("the replacer was not rebuilt from the bounded set")
	}
}

// TestRedact_ObservedFloodNeverEvictsALiveRunsCredential: however many
// model-influenced values a tenant's runs register, a live run's credential
// (of that tenant or another) stays and stays masked; so does an unpinned
// credential, since observed values never displace credentials.
func TestRedact_ObservedFloodNeverEvictsALiveRunsCredential(t *testing.T) {
	r, live := newBounded(16)
	live.set("run-live", true)
	r.RegisterCredential(Holder{Tenant: "t", Runs: []string{"run-live"}}, "live-run-credential")
	r.RegisterCredential(Holder{Tenant: "t"}, "idle-credential")
	for i := 0; i < 5000; i++ {
		r.RegisterObserved(Holder{Tenant: "t", Runs: []string{fmt.Sprintf("r%d", i)}}, fmt.Sprintf("model-chosen-url-part-%05d", i))
		r.RegisterObserved(Holder{Tenant: "other"}, fmt.Sprintf("other-url-part-%05d", i))
	}
	for _, v := range []string{"live-run-credential", "idle-credential"} {
		if !held(r, v) {
			t.Errorf("observed values evicted %s", v)
		}
		masked(t, r, v)
	}
}

// TestRedact_PinnedValueSurvivesAnyChurn: a credential a live run holds stays
// however many credentials other tenants register, and pinned values alone
// may exceed the cap.
func TestRedact_PinnedValueSurvivesAnyChurn(t *testing.T) {
	r, live := newBounded(16)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("victim-run-%d", i)
		live.set(id, true)
		r.RegisterCredential(Holder{Tenant: "victim", Runs: []string{id}}, fmt.Sprintf("victim-live-credential-%02d", i))
	}
	for tn := 0; tn < 50; tn++ {
		for i := 0; i < 100; i++ {
			r.RegisterCredential(Holder{Tenant: fmt.Sprintf("t%02d", tn)}, fmt.Sprintf("t%02d-churn-%03d", tn, i))
		}
	}
	for i := 0; i < 20; i++ {
		v := fmt.Sprintf("victim-live-credential-%02d", i)
		if !held(r, v) {
			t.Fatalf("other tenants' churn evicted pinned %s", v)
		}
		masked(t, r, v)
	}
	// The churn is bounded: past the pinned values, each registration keeps
	// only itself.
	if n := r.dyn.creds.Len(); n > 21 {
		t.Errorf("credentials: %d held with 20 pinned, want at most 21", n)
	}
}

// TestRedact_UnpinnedValueIsEvictable: once no run that registered a value is
// live, it is an ordinary LRU entry; a value two runs registered stays until
// both have ended.
func TestRedact_UnpinnedValueIsEvictable(t *testing.T) {
	r, live := newBounded(4)
	live.set("run-a", true)
	live.set("run-b", true)
	r.RegisterCredential(Holder{Tenant: "a", Runs: []string{"run-a"}}, "shared-credential")
	r.RegisterCredential(Holder{Tenant: "b", Runs: []string{"run-b"}}, "shared-credential")
	churn := func(tag string) {
		for i := 0; i < 20; i++ {
			r.RegisterCredential(Holder{Tenant: "c"}, fmt.Sprintf("%s-churn-%02d", tag, i))
		}
	}
	live.set("run-a", false)
	churn("one")
	if !held(r, "shared-credential") {
		t.Fatal("evicted while run-b, which registered it too, is live")
	}
	live.set("run-b", false)
	churn("two")
	if held(r, "shared-credential") {
		t.Error("an unpinned value was not evicted by the cap")
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
