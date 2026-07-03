// Package limits implements RFC AW per-scope token budgets: an in-memory,
// month-to-date token counter per (operator | tenant | user) scope, checked at
// run admission against store-backed soft/hard ceilings.
//
// The counter is seeded at boot from the RFC AV usage ledger and incremented on
// every per-call usage record, so admission reads are O(1) with no per-run
// query. A scope with no limit row is unlimited (today's behavior). The window
// is the calendar month in UTC; counters reset on the first mutation after a
// month boundary. Enforcement is advisory (decision #O2): under multi-replica
// each replica counts only its own calls, so a hard ceiling can be briefly
// overshot across replicas — budgets bound spend, they are not a security
// control. Fail-open on a store fault: a budgeting error must never take the
// runtime down.
package limits

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Store is the minimal store surface the tracker needs (RFC AW): the RFC AV
// usage aggregation to seed the month-to-date counters, and the token_limits
// rows for the cached ceilings. Declared locally so the package depends only on
// what it uses.
type Store interface {
	UsageReport(ctx context.Context, q store.UsageQuery) ([]store.UsageAggregate, error)
	TokenLimitsAll(ctx context.Context) ([]store.TokenLimitRow, error)
}

// tierPair is a scope's cached soft/hard ceilings; a nil pointer = that tier
// unset (no ceiling on that severity).
type tierPair struct {
	soft *int64
	hard *int64
}

// Decision is the admission verdict for a run (RFC AW). When Allowed is false a
// hard ceiling tripped and Refusal names it; when Allowed is true, Soft carries
// any soft ceilings the run has already crossed (emit as a warning at run start).
type Decision struct {
	Allowed bool
	Refusal *providers.LimitInfo
	Soft    []providers.LimitInfo
}

// Tracker holds the process-local month-to-date counters + the cached limits.
type Tracker struct {
	mu    sync.RWMutex
	store Store
	// now is the clock, injectable for tests. Defaults to time.Now.
	now func() time.Time

	month    time.Time        // current UTC month-start the counters cover
	operator int64            // operator-global MTD tokens
	tenant   map[string]int64 // tenant_id -> MTD tokens
	user     map[string]int64 // userKey(tenant, subject) -> MTD tokens

	// limits is the cached ceilings, keyed limitKey(scope, tenant, scopeID).
	limits map[string]tierPair
}

// New builds a tracker over st. A nil store yields a no-op tracker: Check always
// allows and Add is a no-op, so a store-less deployment keeps today's unlimited
// behavior.
func New(st Store) *Tracker {
	return &Tracker{
		store:  st,
		now:    time.Now,
		tenant: map[string]int64{},
		user:   map[string]int64{},
		limits: map[string]tierPair{},
	}
}

// monthStartUTC returns the first instant (00:00 UTC) of now's calendar month.
func monthStartUTC(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// tokensOf sums the four token buckets — the total the budget counts (RFC AW #5).
func (t *Tracker) tokensOf(a store.UsageAggregate) int64 {
	return a.InputTokens + a.OutputTokens + a.CacheCreationTokens + a.CacheReadTokens
}

// Seed loads the current month-to-date counters from the RFC AV ledger and the
// ceilings from token_limits. Called once at boot; safe to call again. A no-op
// (nil) for a store-less tracker.
func (t *Tracker) Seed(ctx context.Context) error {
	if t == nil || t.store == nil {
		return nil
	}
	month := monthStartUTC(t.now())
	aggs, err := t.store.UsageReport(ctx, store.UsageQuery{
		From:    month,
		GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageByUser},
	})
	if err != nil {
		return err
	}
	tenant := make(map[string]int64)
	user := make(map[string]int64)
	var operator int64
	for _, a := range aggs {
		tok := t.tokensOf(a)
		operator += tok
		tenant[a.TenantID] += tok
		user[userKey(a.TenantID, a.UserID)] += tok
	}
	lm, err := t.loadLimits(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.month = month
	t.operator = operator
	t.tenant = tenant
	t.user = user
	t.limits = lm
	t.mu.Unlock()
	return nil
}

// PutLimit updates the ONE cached ceiling for a scope after a successful store
// write, so the new budget is enforced immediately. This deliberately avoids a
// full re-read of every row (loadLimits): a re-read can fail on a transient
// store fault, which would leave the row persisted-on-disk but the in-memory
// ceiling stale — a budget silently unenforced until the next reload/reboot.
// A single-key map write under the lock can't fail. Nil-safe (store-less no-op).
func (t *Tracker) PutLimit(row store.TokenLimitRow) {
	if t == nil || t.store == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.limits == nil {
		t.limits = map[string]tierPair{}
	}
	t.limits[limitKey(row.Scope, row.TenantID, row.ScopeID)] = tierPair{soft: row.SoftLimit, hard: row.HardLimit}
}

// DeleteLimit drops the cached ceiling for a scope after a successful store
// delete (→ unlimited again). Same rationale as PutLimit: an in-memory delete
// can't fail, so the cache never lags the persisted state. Nil-safe.
func (t *Tracker) DeleteLimit(scope, tenantID, scopeID string) {
	if t == nil || t.store == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.limits, limitKey(scope, tenantID, scopeID))
}

func (t *Tracker) loadLimits(ctx context.Context) (map[string]tierPair, error) {
	rows, err := t.store.TokenLimitsAll(ctx)
	if err != nil {
		return nil, err
	}
	lm := make(map[string]tierPair, len(rows))
	for _, r := range rows {
		lm[limitKey(r.Scope, r.TenantID, r.ScopeID)] = tierPair{soft: r.SoftLimit, hard: r.HardLimit}
	}
	return lm, nil
}

// Add increments the three scope counters (operator, tenant, user) by tokens and
// returns the tiers this add NEWLY crossed (was below the tier before this add,
// at or above it after) across the three scopes. Dedup once-per-run is the
// caller's job. A no-op returning nil for a store-less tracker or a zero add.
func (t *Tracker) Add(tenantID, userID string, tokens int64) []providers.LimitInfo {
	if t == nil || t.store == nil || tokens <= 0 {
		return nil
	}
	uk := userKey(tenantID, userID)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolloverLocked()

	var crossed []providers.LimitInfo
	crossed = appendCrossings(crossed, "operator", "", t.operator, t.operator+tokens, t.limits[limitKey("operator", "", "")])
	t.operator += tokens

	tb := t.tenant[tenantID]
	crossed = appendCrossings(crossed, "tenant", tenantID, tb, tb+tokens, t.limits[limitKey("tenant", tenantID, "")])
	t.tenant[tenantID] = tb + tokens

	ub := t.user[uk]
	crossed = appendCrossings(crossed, "user", userID, ub, ub+tokens, t.limits[limitKey("user", tenantID, userID)])
	t.user[uk] = ub + tokens

	return crossed
}

// Check is the admission verdict for a run's (tenant, user): refuse when ANY
// scope's hard tier is set and month-to-date usage is at/over it (most-
// restrictive wins; the first tripped scope in operator→tenant→user order is
// named); otherwise allow, reporting any soft tiers already exceeded. A store-
// less tracker always allows.
func (t *Tracker) Check(tenantID, userID string) Decision {
	if t == nil || t.store == nil {
		return Decision{Allowed: true}
	}
	uk := userKey(tenantID, userID)
	t.mu.RLock()
	defer t.mu.RUnlock()

	// After a month boundary but before the next Add resets the counters, treat
	// usage as 0 so admission doesn't refuse against last month's total (Add
	// performs the actual reset on the next write).
	rolled := !monthStartUTC(t.now()).Equal(t.month)
	usedOf := func(cur int64) int64 {
		if rolled {
			return 0
		}
		return cur
	}

	scopes := []struct {
		scope, scopeID string
		used           int64
		tp             tierPair
	}{
		{"operator", "", usedOf(t.operator), t.limits[limitKey("operator", "", "")]},
		{"tenant", tenantID, usedOf(t.tenant[tenantID]), t.limits[limitKey("tenant", tenantID, "")]},
		{"user", userID, usedOf(t.user[uk]), t.limits[limitKey("user", tenantID, userID)]},
	}

	for _, sc := range scopes {
		if sc.tp.hard != nil && sc.used >= *sc.tp.hard {
			info := makeLimitInfo(sc.scope, sc.scopeID, "hard", sc.used, *sc.tp.hard)
			return Decision{Allowed: false, Refusal: &info}
		}
	}
	var soft []providers.LimitInfo
	for _, sc := range scopes {
		if sc.tp.soft != nil && sc.used >= *sc.tp.soft {
			soft = append(soft, makeLimitInfo(sc.scope, sc.scopeID, "soft", sc.used, *sc.tp.soft))
		}
	}
	return Decision{Allowed: true, Soft: soft}
}

// UsedFor returns a scope's current month-to-date token total (RFC AW), for the
// /v1/_limits UI showing usage against each ceiling. scopeID is the tenant id
// for scope=tenant and the user subject for scope=user.
func (t *Tracker) UsedFor(scope, tenantID, scopeID string) int64 {
	if t == nil || t.store == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !monthStartUTC(t.now()).Equal(t.month) {
		return 0 // a new month with no writes yet reads as zero spend
	}
	switch scope {
	case "operator":
		return t.operator
	case "tenant":
		return t.tenant[tenantID]
	case "user":
		return t.user[userKey(tenantID, scopeID)]
	}
	return 0
}

// rolloverLocked resets the counters when the wall clock has crossed into a new
// calendar month. Caller must hold the write lock.
func (t *Tracker) rolloverLocked() {
	if cur := monthStartUTC(t.now()); !cur.Equal(t.month) {
		t.month = cur
		t.operator = 0
		t.tenant = map[string]int64{}
		t.user = map[string]int64{}
	}
}

// appendCrossings appends a LimitInfo for each tier (soft, then hard) that the
// [before, after) increment newly crossed.
func appendCrossings(dst []providers.LimitInfo, scope, scopeID string, before, after int64, tp tierPair) []providers.LimitInfo {
	if tp.soft != nil && before < *tp.soft && after >= *tp.soft {
		dst = append(dst, makeLimitInfo(scope, scopeID, "soft", after, *tp.soft))
	}
	if tp.hard != nil && before < *tp.hard && after >= *tp.hard {
		dst = append(dst, makeLimitInfo(scope, scopeID, "hard", after, *tp.hard))
	}
	return dst
}

func userKey(tenantID, subject string) string { return tenantID + "\x00" + subject }

func limitKey(scope, tenantID, scopeID string) string {
	return scope + "|" + tenantID + "|" + scopeID
}

func makeLimitInfo(scope, scopeID, severity string, used, limit int64) providers.LimitInfo {
	// The operator scope's used/limit are PLATFORM-WIDE aggregates summed across
	// every tenant. This LimitInfo is delivered over a RUN channel — the 429
	// refusal body, the SSE/transcript limit event, the gRPC event, the MCP
	// spawn_run result — all visible to the run's own tenant+user. Exposing the
	// operator figures to a tenant is therefore a cross-tenant oracle (a tenant
	// could subtract its own spend to infer other tenants' aggregate activity),
	// which is exactly what writeTokenLimitError promises NOT to do. Redact the
	// numbers here and carry only severity + a generic message; an operator still
	// reads the real operator figures from the admin console (/v1/_limits →
	// Tracker.UsedFor), which never routes through this constructor.
	if scope == "operator" {
		return providers.LimitInfo{
			Scope:    scope,
			Severity: severity,
			Window:   "month",
			Message:  fmt.Sprintf("service %s token budget reached; please retry later", severity),
		}
	}
	label := scope
	if scopeID != "" {
		label = scope + " " + scopeID
	}
	return providers.LimitInfo{
		Scope:    scope,
		ScopeID:  scopeID,
		Severity: severity,
		Window:   "month",
		Used:     used,
		Limit:    limit,
		Message:  fmt.Sprintf("%s %s token budget reached: %d of %d tokens this month", label, severity, used, limit),
	}
}
