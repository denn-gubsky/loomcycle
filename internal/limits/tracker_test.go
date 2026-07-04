package limits

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeStore is an in-memory Store for the tracker tests: canned usage
// aggregates for Seed + a settable limit-row set.
type fakeStore struct {
	aggs   []store.UsageAggregate
	limits []store.TokenLimitRow
	err    error
}

func (f *fakeStore) UsageReport(ctx context.Context, q store.UsageQuery) ([]store.UsageAggregate, error) {
	return f.aggs, f.err
}
func (f *fakeStore) TokenLimitsAll(ctx context.Context) ([]store.TokenLimitRow, error) {
	return f.limits, f.err
}

func i64(v int64) *int64 { return &v }

func agg(tenant, user string, in, out int64) store.UsageAggregate {
	return store.UsageAggregate{TenantID: tenant, UserID: user, InputTokens: in, OutputTokens: out}
}

// TestTracker_UnlimitedWhenNoRows: no limit rows → every run allowed regardless
// of spend, no soft events.
func TestTracker_UnlimitedWhenNoRows(t *testing.T) {
	tr := New(&fakeStore{aggs: []store.UsageAggregate{agg("acme", "u1", 10_000, 5_000)}})
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := tr.Check("acme", "u1")
	if !dec.Allowed || dec.Refusal != nil || len(dec.Soft) != 0 {
		t.Fatalf("no limit rows must allow with no soft events; got %+v", dec)
	}
}

// TestTracker_NilStoreAllows: a store-less tracker always allows + Add no-ops.
func TestTracker_NilStoreAllows(t *testing.T) {
	tr := New(nil)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if crossed := tr.Add("acme", "u1", 1_000_000); crossed != nil {
		t.Fatalf("nil-store Add must be a no-op; got %+v", crossed)
	}
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatalf("nil-store Check must allow; got %+v", dec)
	}
}

// TestTracker_SoftCrossingAllowsWithEvent: a tenant over its soft tier (but under
// hard, or no hard) is allowed, with a soft LimitInfo reported.
func TestTracker_SoftCrossingAllowsWithEvent(t *testing.T) {
	fs := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 600, 0)},
		limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", SoftLimit: i64(500)}},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := tr.Check("acme", "u1")
	if !dec.Allowed || dec.Refusal != nil {
		t.Fatalf("soft-only crossing must allow; got %+v", dec)
	}
	if len(dec.Soft) != 1 || dec.Soft[0].Scope != "tenant" || dec.Soft[0].Severity != "soft" {
		t.Fatalf("want one soft tenant event; got %+v", dec.Soft)
	}
}

// TestTracker_HardCrossingRefuses: a scope at/over its hard tier refuses.
func TestTracker_HardCrossingRefuses(t *testing.T) {
	fs := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 900, 0)},
		limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64(800)}},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := tr.Check("acme", "u1")
	if dec.Allowed || dec.Refusal == nil {
		t.Fatalf("hard crossing must refuse; got %+v", dec)
	}
	if dec.Refusal.Scope != "tenant" || dec.Refusal.Severity != "hard" || dec.Refusal.Limit != 800 {
		t.Fatalf("refusal payload wrong; got %+v", dec.Refusal)
	}
}

// TestTracker_MostRestrictiveWins: a user hard tier tighter than the tenant's
// decides the refusal (both would refuse; the user scope is named because its
// ceiling is the one actually crossed).
func TestTracker_MostRestrictiveWins(t *testing.T) {
	fs := &fakeStore{
		aggs: []store.UsageAggregate{agg("acme", "u1", 150, 0)},
		limits: []store.TokenLimitRow{
			{TenantID: "acme", Scope: "tenant", HardLimit: i64(10_000)},           // not crossed
			{TenantID: "acme", Scope: "user", ScopeID: "u1", HardLimit: i64(100)}, // crossed
		},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := tr.Check("acme", "u1")
	if dec.Allowed || dec.Refusal == nil {
		t.Fatalf("user hard crossing must refuse; got %+v", dec)
	}
	if dec.Refusal.Scope != "user" || dec.Refusal.ScopeID != "u1" {
		t.Fatalf("want user scope refusal; got %+v", dec.Refusal)
	}
}

// TestTracker_OperatorScopeSumsAllTenants: the operator-global counter is the sum
// across every tenant/user, so an operator hard tier refuses even a fresh tenant.
func TestTracker_OperatorScopeSumsAllTenants(t *testing.T) {
	fs := &fakeStore{
		aggs: []store.UsageAggregate{
			agg("acme", "u1", 400, 0),
			agg("beta", "u2", 400, 0),
		},
		limits: []store.TokenLimitRow{{Scope: "operator", HardLimit: i64(500)}},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A brand-new tenant with zero of its own spend is still refused by the
	// operator-global cap (800 >= 500).
	dec := tr.Check("gamma", "u9")
	if dec.Allowed || dec.Refusal == nil || dec.Refusal.Scope != "operator" {
		t.Fatalf("operator-global cap must refuse across tenants; got %+v", dec)
	}
}

// TestTracker_AddReturnsCrossingsOnce: Add reports a tier only on the increment
// that crosses it, not on subsequent adds already past it.
func TestTracker_AddReturnsCrossingsOnce(t *testing.T) {
	fs := &fakeStore{limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", SoftLimit: i64(100), HardLimit: i64(300)}}}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	// First add stays below soft → no crossing.
	if c := tr.Add("acme", "u1", 50); len(c) != 0 {
		t.Fatalf("below-soft add should cross nothing; got %+v", c)
	}
	// Second add crosses soft (50→150).
	c := tr.Add("acme", "u1", 100)
	if len(c) != 1 || c[0].Severity != "soft" {
		t.Fatalf("want one soft crossing; got %+v", c)
	}
	// Third add stays between soft and hard → no new crossing.
	if c := tr.Add("acme", "u1", 100); len(c) != 0 {
		t.Fatalf("already-past-soft add should cross nothing; got %+v", c)
	}
	// Fourth add crosses hard (250→350).
	c = tr.Add("acme", "u1", 100)
	if len(c) != 1 || c[0].Severity != "hard" {
		t.Fatalf("want one hard crossing; got %+v", c)
	}
}

// TestTracker_MonthRolloverResets: a wall-clock month boundary resets the
// counters on the next Add, so last month's spend no longer refuses.
func TestTracker_MonthRolloverResets(t *testing.T) {
	fs := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 900, 0)},
		limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64(800)}},
	}
	tr := New(fs)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return now }
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); dec.Allowed {
		t.Fatal("January spend over hard must refuse")
	}
	// Advance the clock into February. Check reads zero (rolled), and Add resets.
	now = time.Date(2026, 2, 1, 0, 0, 1, 0, time.UTC)
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatalf("new month must reset admission; got %+v", dec)
	}
	tr.Add("acme", "u1", 10)
	if got := tr.UsedFor("tenant", "acme", ""); got != 10 {
		t.Fatalf("after rollover UsedFor = %d, want 10 (reset + this add)", got)
	}
}

// TestTracker_PutDeleteLimit picks up / drops a ceiling in the live cache
// without a re-seed of counters — the O(1) path the CRUD handlers use so a
// persisted budget is never left stored-but-unenforced by a failed re-read.
func TestTracker_PutDeleteLimit(t *testing.T) {
	fs := &fakeStore{aggs: []store.UsageAggregate{agg("acme", "u1", 900, 0)}}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatal("no rows yet → allowed")
	}
	// PutLimit reflects a persisted ceiling into the cache immediately.
	tr.PutLimit(store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64(800)})
	if dec := tr.Check("acme", "u1"); dec.Allowed {
		t.Fatal("after PutLimit the hard tier must refuse")
	}
	// DeleteLimit drops it again → unlimited.
	tr.DeleteLimit("tenant", "acme", "")
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatal("after DeleteLimit the scope must be unlimited again")
	}
}

// TestTracker_OperatorCrossingRedactsFigures verifies the RFC AW cross-tenant
// oracle fix: an operator-scope (platform-wide) budget crossing delivered over
// a run channel carries NO used/limit numbers (they aggregate every tenant),
// only a generic message — while a tenant/user scope keeps its own figures.
func TestTracker_OperatorCrossingRedactsFigures(t *testing.T) {
	fs := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 900, 0)},
		limits: []store.TokenLimitRow{{Scope: "operator", HardLimit: i64(800)}},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := tr.Check("acme", "u1")
	if dec.Allowed || dec.Refusal == nil {
		t.Fatal("operator hard cap (800) < used (900) must refuse")
	}
	r := dec.Refusal
	if r.Scope != "operator" {
		t.Fatalf("refusal scope = %q, want operator", r.Scope)
	}
	if r.Used != 0 || r.Limit != 0 {
		t.Fatalf("operator figures leaked to a tenant caller: used=%d limit=%d (want 0/0)", r.Used, r.Limit)
	}
	if r.ScopeID != "" {
		t.Fatalf("operator scope_id leaked: %q", r.ScopeID)
	}
	// A tenant-scope refusal, by contrast, keeps its own (non-cross-tenant) figures.
	fs2 := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 900, 0)},
		limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64(800)}},
	}
	tr2 := New(fs2)
	if err := tr2.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2 := tr2.Check("acme", "u1")
	if d2.Allowed || d2.Refusal == nil || d2.Refusal.Scope != "tenant" {
		t.Fatal("tenant hard cap must refuse with scope=tenant")
	}
	if d2.Refusal.Used != 900 || d2.Refusal.Limit != 800 {
		t.Fatalf("tenant figures wrongly redacted: used=%d limit=%d (want 900/800)", d2.Refusal.Used, d2.Refusal.Limit)
	}
}
