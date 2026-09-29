package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func i64p(v int64) *int64 { return &v }

// recordUsage writes one ledger row now for (tenant, user) totalling tokens.
func recordUsage(t *testing.T, s store.Store, tenant, user string, tokens int64) {
	t.Helper()
	if err := s.RecordCallUsage(context.Background(), store.TokenUsageRow{
		RunID: "run_" + tenant + "_" + user, TenantID: tenant, UserID: user, Provider: "mock", Model: "mock",
		CredentialSource: "operator", InputTokens: int(tokens), TS: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordCallUsage: %v", err)
	}
}

// editSection rewrites one envelope section in place, as a hand-edited
// snapshot would be, dropping the checksum that would otherwise refuse it.
func editSection(t *testing.T, raw []byte, name string, edit func(sec map[string]any)) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	sec, ok := env["sections"].(map[string]any)[name].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no %s section to edit", name)
	}
	edit(sec)
	delete(env, "checksum")
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustCapture(t *testing.T, s store.Store) []byte {
	t.Helper()
	_, raw, err := Capture(context.Background(), s, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return raw
}

func mustRestore(t *testing.T, s store.Store, raw []byte, opts RestoreOptions) RestoreResult {
	t.Helper()
	res, err := Restore(context.Background(), s, raw, opts)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return res
}

func carryFor(t *testing.T, s store.Store, month time.Time) map[limits.UsageKey]int64 {
	t.Helper()
	rows, err := s.UsageCarryForMonth(context.Background(), month)
	if err != nil {
		t.Fatalf("UsageCarryForMonth: %v", err)
	}
	out := map[limits.UsageKey]int64{}
	for _, r := range rows {
		out[limits.UsageKey{TenantID: r.TenantID, UserID: r.UserID}] = r.Tokens
	}
	return out
}

// Every user comes back under its own tenant with every field it had, and a
// second restore of the same snapshot writes nothing.
func TestRoundTrip_UsersKeepTheirTenantAndEveryField(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	at := time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC)
	want := []store.UserRow{
		{TenantID: "", Subject: "op-user", DisplayName: "", AccessMode: "tenant", Status: "active", CreatedAt: at},
		{TenantID: "acme", Subject: "alice", DisplayName: "Alice", AccessMode: "isolated", Status: "disabled", CreatedAt: at.Add(time.Hour), CreatedBy: "admin"},
		{TenantID: "beta", Subject: "alice", DisplayName: "Other Alice", AccessMode: "tenant", Status: "active", CreatedAt: at.Add(2 * time.Hour), CreatedBy: "op@beta"},
	}
	for _, u := range want {
		if err := src.UserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.UsersRestored != 3 || len(res.Warnings) != 0 {
		t.Fatalf("users restored = %d, warnings %v; want 3 and none", res.UsersRestored, res.Warnings)
	}
	got, err := dst.UserList(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i].CreatedAt = got[i].CreatedAt.UTC()
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restored users =\n %+v\nwant\n %+v", got, want)
	}

	again := mustRestore(t, dst, raw, RestoreOptions{})
	if again.UsersRestored != 0 || len(again.Warnings) != 0 {
		t.Errorf("re-restore: users restored = %d, warnings %v; want 0 and none", again.UsersRestored, again.Warnings)
	}
}

// A user the target already has keeps its live row. When the snapshot's row
// is stricter the restore says so, naming both values; a looser snapshot row
// says nothing, because the live row is already the stricter.
func TestRestore_LiveUserStandsAndAStricterSnapshotRowWarns(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	for _, u := range []store.UserRow{
		{TenantID: "acme", Subject: "alice", AccessMode: "isolated", Status: "disabled"},
		{TenantID: "acme", Subject: "carol", AccessMode: "tenant", Status: "active"},
	} {
		if err := src.UserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	live := []store.UserRow{
		{TenantID: "acme", Subject: "alice", DisplayName: "live", AccessMode: "tenant", Status: "active"},
		{TenantID: "acme", Subject: "carol", DisplayName: "live", AccessMode: "isolated", Status: "disabled"},
	}
	for _, u := range live {
		if err := dst.UserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
	if res.UsersRestored != 0 {
		t.Errorf("users restored = %d, want 0 (both already exist)", res.UsersRestored)
	}
	for _, u := range live {
		got, err := dst.UserGet(ctx, u.TenantID, u.Subject)
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessMode != u.AccessMode || got.Status != u.Status || got.DisplayName != u.DisplayName {
			t.Errorf("%s after restore = %s/%s/%q, want the live %s/%s/%q", u.Subject,
				got.AccessMode, got.Status, got.DisplayName, u.AccessMode, u.Status, u.DisplayName)
		}
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{
		"user acme/alice: the snapshot has access_mode=isolated but this instance has tenant",
		"user acme/alice: the snapshot has status=disabled but this instance has active",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "carol") {
		t.Errorf("a looser snapshot row warned:\n%s", joined)
	}
	if len(res.Warnings) != 2 {
		t.Errorf("warnings = %v, want exactly the two for alice", res.Warnings)
	}
}

// Every budget comes back with its tiers exactly — an unset tier stays unset,
// a zero stays zero — except where the target already has one on that key:
// the live budget stands. Only the rows written are handed to the refresh.
func TestRoundTrip_TokenLimitsKeepTheirTiersAndTheLiveBudgetStands(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	at := time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC)
	rows := []store.TokenLimitRow{
		{TenantID: "", Scope: "operator", HardLimit: i64p(1_000_000), UpdatedAt: at, UpdatedBy: "root"},
		{TenantID: "acme", Scope: "tenant", SoftLimit: i64p(0), HardLimit: i64p(800), UpdatedAt: at, UpdatedBy: "root"},
		{TenantID: "acme", Scope: "user", ScopeID: "u1", SoftLimit: i64p(50), UpdatedAt: at, UpdatedBy: "op@acme"},
	}
	for _, r := range rows {
		if err := src.TokenLimitPut(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	liveTenant := store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(5), UpdatedAt: at.Add(time.Hour), UpdatedBy: "live"}
	if err := dst.TokenLimitPut(ctx, liveTenant); err != nil {
		t.Fatal(err)
	}

	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.TokenLimitsRestored != 2 || len(res.Warnings) != 0 {
		t.Fatalf("token limits restored = %d, warnings %v; want 2 and none", res.TokenLimitsRestored, res.Warnings)
	}
	if len(res.Refresh.TokenLimits) != 2 {
		t.Errorf("refresh rows = %+v, want the 2 inserted (not the live one)", res.Refresh.TokenLimits)
	}

	got, err := dst.TokenLimitsAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]store.TokenLimitRow{}
	for _, r := range got {
		r.UpdatedAt = r.UpdatedAt.UTC()
		byKey[r.TenantID+"|"+r.Scope+"|"+r.ScopeID] = r
	}
	for _, want := range []store.TokenLimitRow{rows[0], rows[2], liveTenant} {
		if g := byKey[want.TenantID+"|"+want.Scope+"|"+want.ScopeID]; !reflect.DeepEqual(g, want) {
			t.Errorf("budget %s/%s/%s = %+v, want %+v", want.TenantID, want.Scope, want.ScopeID, g, want)
		}
	}

	again := mustRestore(t, dst, raw, RestoreOptions{})
	if again.TokenLimitsRestored != 0 || len(again.Refresh.TokenLimits) != 0 || len(again.Warnings) != 0 {
		t.Errorf("re-restore: restored %d, refresh %v, warnings %v; want nothing", again.TokenLimitsRestored, again.Refresh.TokenLimits, again.Warnings)
	}
}

// A hand-edited budget no author could have written is skipped with a
// warning: a negative ceiling would refuse every run in its scope.
func TestRestore_AnInvalidBudgetIsSkippedWithAWarning(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	if err := src.TokenLimitPut(context.Background(), store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(10)}); err != nil {
		t.Fatal(err)
	}
	raw := editSection(t, mustCapture(t, src), "token_limits", func(sec map[string]any) {
		sec["entries"].([]any)[0].(map[string]any)["hard_limit"] = -1
	})
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.TokenLimitsRestored != 0 || !strings.Contains(strings.Join(res.Warnings, "\n"), "token_limit acme/tenant/: a negative ceiling") {
		t.Fatalf("restored %d, warnings %v; want the row skipped with a warning", res.TokenLimitsRestored, res.Warnings)
	}
	if got, _ := dst.TokenLimitsAll(context.Background()); len(got) != 0 {
		t.Errorf("budgets after restore = %+v, want none", got)
	}
}

// What a capture carries as month-to-date usage is exactly what the source's
// tracker enforces — limits.MonthToDate over the ledger AND the source's own
// carry — so a second hop keeps the first source's usage.
func TestCapture_UsageMTDIsWhatTheSourceEnforcesAcrossTwoHops(t *testing.T) {
	a, aClose := newTestStore(t)
	defer aClose()
	b, bClose := newTestStore(t)
	defer bClose()
	c, cClose := newTestStore(t)
	defer cClose()
	ctx := context.Background()

	recordUsage(t, a, "acme", "u1", 100)
	recordUsage(t, a, "acme", "", 7)
	mustRestore(t, b, mustCapture(t, a), RestoreOptions{})

	// B spends on its own too, so its capture must add ledger and carry.
	recordUsage(t, b, "acme", "u1", 20)
	recordUsage(t, b, "beta", "u2", 5)

	month := limits.MonthStart(time.Now())
	wantB, err := limits.MonthToDate(ctx, b, month)
	if err != nil {
		t.Fatal(err)
	}
	if wantB[limits.UsageKey{TenantID: "acme", UserID: "u1"}] != 120 {
		t.Fatalf("B's month-to-date = %v; the carry from A did not reach it", wantB)
	}

	rawB := mustCapture(t, b)
	var env Envelope
	if err := json.Unmarshal(rawB, &env); err != nil {
		t.Fatal(err)
	}
	mtd := env.Sections.TokenLimits.UsageMTD
	if mtd == nil || !mtd.Month.Equal(month) {
		t.Fatalf("B's usage_mtd = %+v, want a block for %v", mtd, month)
	}
	got := map[limits.UsageKey]int64{}
	for _, e := range mtd.Entries {
		got[limits.UsageKey{TenantID: e.TenantID, UserID: e.UserID}] = e.Tokens
	}
	if !reflect.DeepEqual(got, wantB) {
		t.Errorf("B's captured usage_mtd = %v, want MonthToDate(B) = %v", got, wantB)
	}

	// And it is the number B's own tracker enforces.
	tr := limits.New(b)
	if err := tr.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if used := tr.UsedFor("user", "acme", "u1"); used != 120 {
		t.Errorf("B's tracker enforces %d for acme/u1, want the captured 120", used)
	}

	mustRestore(t, c, rawB, RestoreOptions{})
	if gotC := carryFor(t, c, month); !reflect.DeepEqual(gotC, wantB) {
		t.Errorf("C's carry after the second hop = %v, want %v (A's usage kept)", gotC, wantB)
	}
}

// Restoring the same snapshot twice raises no counter the second time: the
// carry takes the maximum, it does not add.
func TestRestore_UsageCarryReRestoreAddsNothing(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	recordUsage(t, src, "acme", "u1", 900)
	raw := mustCapture(t, src)

	first := mustRestore(t, dst, raw, RestoreOptions{})
	if first.UsageCarryRestored != 1 || len(first.Refresh.UsageCarried) != 1 || first.Refresh.UsageCarried[0].Tokens != 900 {
		t.Fatalf("first restore: carried %d, refresh %+v; want one carry of 900", first.UsageCarryRestored, first.Refresh.UsageCarried)
	}
	second := mustRestore(t, dst, raw, RestoreOptions{})
	if second.UsageCarryRestored != 0 || len(second.Refresh.UsageCarried) != 0 {
		t.Errorf("re-restore: carried %d, refresh %+v; want nothing", second.UsageCarryRestored, second.Refresh.UsageCarried)
	}
	month := limits.MonthStart(time.Now())
	if got := carryFor(t, dst, month)[limits.UsageKey{TenantID: "acme", UserID: "u1"}]; got != 900 {
		t.Errorf("stored carry after two restores = %d, want 900", got)
	}
}

// A carry captured in an earlier month is dropped with a warning — the budget
// window rolled over — while the budgets themselves still restore.
func TestRestore_UsageCarryFromAPastMonthIsDroppedWithAWarning(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	recordUsage(t, src, "acme", "u1", 900)
	if err := src.TokenLimitPut(context.Background(), store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(1000)}); err != nil {
		t.Fatal(err)
	}
	raw := mustCapture(t, src)

	nextMonth := limits.MonthStart(time.Now()).AddDate(0, 1, 3)
	res := mustRestore(t, dst, raw, RestoreOptions{Now: func() time.Time { return nextMonth }})
	if res.UsageCarryRestored != 0 || len(res.Refresh.UsageCarried) != 0 {
		t.Errorf("carried %d (refresh %+v) into a later month, want none", res.UsageCarryRestored, res.Refresh.UsageCarried)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "not carried: the budget window has rolled over") {
		t.Errorf("warnings = %v, want the rollover warning", res.Warnings)
	}
	if res.TokenLimitsRestored != 1 {
		t.Errorf("token limits restored = %d, want 1 (the budget still travels)", res.TokenLimitsRestored)
	}
	for _, m := range []time.Time{limits.MonthStart(time.Now()), limits.MonthStart(nextMonth)} {
		if got := carryFor(t, dst, m); len(got) != 0 {
			t.Errorf("carry rows for %s = %v, want none", m.Format("2006-01"), got)
		}
	}
}

// The carry is budget state, never billing data: a restore leaves the
// target's usage report exactly as it was.
func TestRestore_UsageCarryNeverReachesTheUsageReport(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	recordUsage(t, src, "acme", "u1", 900)
	recordUsage(t, dst, "acme", "u1", 3)

	report := func() []store.UsageAggregate {
		rows, err := dst.UsageReport(ctx, store.UsageQuery{GroupBy: []store.UsageDimension{store.UsageByTenant, store.UsageByUser}})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := report()
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
	if res.UsageCarryRestored != 1 {
		t.Fatalf("carried %d, want 1 — the report check would prove nothing", res.UsageCarryRestored)
	}
	if after := report(); !reflect.DeepEqual(after, before) {
		t.Errorf("usage report after restore = %+v, want unchanged %+v", after, before)
	}
}

// A snapshot taken before these sections existed restores as it always did:
// no users, no budgets, no carry, no warnings.
func TestRestore_WithoutUsersAndTokenLimitsSectionsRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	if err := src.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := src.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(10)}); err != nil {
		t.Fatal(err)
	}
	recordUsage(t, src, "acme", "alice", 9)

	raw := withoutSection(t, withoutSection(t, mustCapture(t, src), "users"), "token_limits")
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.UsersRestored != 0 || res.TokenLimitsRestored != 0 || res.UsageCarryRestored != 0 || len(res.Warnings) != 0 {
		t.Fatalf("restore = %+v; want no users, budgets, carry or warnings", res)
	}
	users, _ := dst.UserList(ctx, "")
	budgets, _ := dst.TokenLimitsAll(ctx)
	if len(users) != 0 || len(budgets) != 0 || len(carryFor(t, dst, limits.MonthStart(time.Now()))) != 0 {
		t.Errorf("an old-format restore wrote users %v, budgets %v", users, budgets)
	}
}
