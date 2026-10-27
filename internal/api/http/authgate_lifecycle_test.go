package http

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const lcDay = 24 * time.Hour

// lifecycleFixture is a gate fixture with a removal policy and a user alice
// in acme who holds a gate-managed group, a local group and a binding.
func lifecycleFixture(t *testing.T, policy config.AuthRemovalPolicy) *gateFixture {
	t.Helper()
	f := newGateFixture(t)
	f.srv.cfg().ResolvedAuth.Removal = policy
	ctx := context.Background()
	f.addUser(t, "acme", "alice", "isolated")
	f.addLogin(t, "acme", "alice", "local", "alice")
	for _, g := range []store.GroupRow{
		{TenantID: "acme", Name: "staff", ExternalID: "tg:staff"},
		{TenantID: "acme", Name: "local-team"},
		{TenantID: "acme", Name: "everyone"},
		{TenantID: "acme", Name: "new-users"},
	} {
		if _, err := f.st.GroupPut(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []string{"staff", "local-team"} {
		if err := f.st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: g, MemberKind: store.GroupMemberUser, MemberID: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "power", SubjectKind: "user", SubjectID: "alice"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *gateFixture) groupsOf(t *testing.T, subject string) []string {
	t.Helper()
	gs, err := f.st.GroupsOfSubject(context.Background(), "acme", subject)
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

func (f *gateFixture) liveUserBindings(t *testing.T, subject string) int {
	t.Helper()
	bs, err := f.st.AclBindingsFor(context.Background(), "acme", subject, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range bs {
		if b.SubjectKind == "user" && b.SubjectID == subject && b.RetiredAt.IsZero() {
			n++
		}
	}
	return n
}

func (f *gateFixture) count(action string) int {
	n := 0
	for _, a := range f.audit.actions() {
		if a == action {
			n++
		}
	}
	return n
}

// The disable stage is immediate and complete: the user is disabled, every
// session ends and every delegated token stops working at once; nothing is
// unbound yet.
func TestRemoval_DisableEndsSessionsAndTokensAtOnceAndKeepsGroups(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay})
	ctx := context.Background()
	code, body := f.call(t, bAcmeOp, "POST", "/v1/_users/alice/tokens", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("mint: %d %v", code, body)
	}
	tok := body["token"].(string)
	if c, _ := f.call(t, tok, "GET", "/v1/_me", ""); c != http.StatusOK {
		t.Fatalf("token before removal: %d", c)
	}
	now := time.Now()
	sess := store.UISessionRow{SessionHash: "s-alice", TenantID: "acme", Subject: "alice", Gate: "local", CredKind: "password",
		CredMAC: "m", IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(lcDay)}
	if err := f.st.UISessionCreate(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.startRemoval(ctx, "acme", "alice", "tg", store.RemovalReasonRevoked, "gate:tg"); err != nil {
		t.Fatal(err)
	}
	u, _ := f.st.UserGet(ctx, "acme", "alice")
	if u.Status != "disabled" {
		t.Fatalf("status = %q", u.Status)
	}
	if c, _ := f.call(t, tok, "GET", "/v1/_me", ""); c != http.StatusUnauthorized {
		t.Fatalf("token after removal: %d, want 401", c)
	}
	if r, _ := f.st.UISessionGet(ctx, "s-alice"); r.RevokedAt.IsZero() {
		t.Fatal("the session outlived the disable")
	}
	if gs := f.groupsOf(t, "alice"); len(gs) != 2 || f.liveUserBindings(t, "alice") != 1 {
		t.Fatalf("the disable unbound: groups %v", gs)
	}
	if f.count("auth_user_disabled") != 1 {
		t.Fatalf("audit = %v", f.audit.actions())
	}
	// A repeated removal neither restarts the clock nor re-audits.
	first, _ := f.st.AuthRemovalGet(ctx, "acme", "alice")
	if err := f.srv.startRemoval(ctx, "acme", "alice", "tg", store.RemovalReasonRevoked, "gate:tg"); err != nil {
		t.Fatal(err)
	}
	again, _ := f.st.AuthRemovalGet(ctx, "acme", "alice")
	if !again.DisabledAt.Equal(first.DisabledAt) || f.count("auth_user_disabled") != 1 {
		t.Fatalf("a repeat restarted the removal: %v → %v", first.DisabledAt, again.DisabledAt)
	}
}

// RFC FA §8 (lifecycle): removal never skips a stage. Long after both due
// times the first tick only unbinds; the erase comes on a later tick, through
// the audited erasure.
func TestRemoval_NeverSkipsAStage(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay, EraseAfter: 30 * lcDay})
	erase := &captureSink{}
	f.srv.SetEraseAudit(erase)
	ctx := context.Background()
	if err := f.srv.startRemoval(ctx, "acme", "alice", "tg", store.RemovalReasonRevoked, "gate:tg"); err != nil {
		t.Fatal(err)
	}
	late := time.Now().Add(365 * lcDay)
	// Before unbind_after, nothing happens.
	f.srv.AuthLifecycleSweep(ctx, time.Now().Add(6*lcDay))
	if r, _ := f.st.AuthRemovalGet(ctx, "acme", "alice"); !r.UnboundAt.IsZero() {
		t.Fatal("unbound before unbind_after")
	}
	f.srv.AuthLifecycleSweep(ctx, late)
	r, _ := f.st.AuthRemovalGet(ctx, "acme", "alice")
	if r.UnboundAt.IsZero() || !r.ErasedAt.IsZero() || len(erase.actions()) != 0 {
		t.Fatalf("first tick: %+v, erase audit %v; want unbound only", r, erase.actions())
	}
	if gs := f.groupsOf(t, "alice"); !slices.Equal(gs, []string{"local-team"}) || f.liveUserBindings(t, "alice") != 0 {
		t.Fatalf("after unbind: groups %v, bindings %d; want only the local group", gs, f.liveUserBindings(t, "alice"))
	}
	if ids, _ := f.st.UserIdentityListSubject(ctx, "acme", "alice"); len(ids) != 1 {
		t.Fatal("the unbind dropped the login identity")
	}
	f.srv.AuthLifecycleSweep(ctx, late)
	r, _ = f.st.AuthRemovalGet(ctx, "acme", "alice")
	if r.ErasedAt.IsZero() || !slices.Contains(erase.actions(), "erase_intent") || !slices.Contains(erase.actions(), "erase_result") {
		t.Fatalf("second tick: %+v, erase audit %v; want the audited erasure", r, erase.actions())
	}
	if f.count("auth_user_unbound") != 1 || f.count("auth_user_erased") != 1 {
		t.Fatalf("audit = %v", f.audit.actions())
	}
}

// RFC FA §8 (lifecycle): erase never runs on its own without an explicit
// erase_after — by default an operator runs it.
func TestRemoval_EraseNeverRunsWithoutEraseAfter(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay})
	erase := &captureSink{}
	f.srv.SetEraseAudit(erase)
	ctx := context.Background()
	if err := f.srv.startRemoval(ctx, "acme", "alice", "", store.RemovalReasonOperator, "acme-op"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.srv.AuthLifecycleSweep(ctx, time.Now().Add(100*365*lcDay))
	}
	r, _ := f.st.AuthRemovalGet(ctx, "acme", "alice")
	if r.UnboundAt.IsZero() || !r.ErasedAt.IsZero() || len(erase.actions()) != 0 || f.count("auth_user_erased") != 0 {
		t.Fatalf("removal %+v, erase audit %v; want unbound and never erased", r, erase.actions())
	}
}

// An erasure that could not be audited is not done, and stays due.
func TestRemoval_EraseWithoutAnAuditSinkStaysDue(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: lcDay, EraseAfter: 2 * lcDay})
	ctx := context.Background()
	if err := f.srv.startRemoval(ctx, "acme", "alice", "", store.RemovalReasonOperator, "acme-op"); err != nil {
		t.Fatal(err)
	}
	late := time.Now().Add(10 * lcDay)
	f.srv.AuthLifecycleSweep(ctx, late)
	f.srv.AuthLifecycleSweep(ctx, late)
	r, _ := f.st.AuthRemovalGet(ctx, "acme", "alice")
	if r.UnboundAt.IsZero() || !r.ErasedAt.IsZero() {
		t.Fatalf("removal %+v; want unbound, not erased", r)
	}
	if due, _ := f.st.AuthRemovalDue(ctx, late, 10); len(due) != 1 {
		t.Fatalf("the erase is no longer due: %+v", due)
	}
}

// RFC FA §8 (lifecycle): a reactivated user re-enters at the new-user
// defaults, not at its old groups — even before the unbind removed them.
func TestRemoval_AReactivatedUserGetsTheDefaultsNotItsOldGroups(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay})
	ctx := context.Background()
	if err := f.st.UserUpdate(ctx, "acme", "alice", store.UserPatch{AccessMode: strPtr("tenant")}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.startRemoval(ctx, "acme", "alice", "tg", store.RemovalReasonRevoked, "gate:tg"); err != nil {
		t.Fatal(err)
	}
	code, body := f.call(t, bAcmeOp, "PATCH", "/v1/_users/alice", `{"status":"active"}`)
	if code != http.StatusOK || body["status"] != "active" {
		t.Fatalf("re-enable: %d %v", code, body)
	}
	u, _ := f.st.UserGet(ctx, "acme", "alice")
	if u.Status != "active" || u.AccessMode != "isolated" {
		t.Fatalf("user = %+v, want active isolated", u)
	}
	if gs := f.groupsOf(t, "alice"); !slices.Equal(gs, []string{"everyone", "new-users"}) || f.liveUserBindings(t, "alice") != 0 {
		t.Fatalf("groups %v, bindings %d; want only the floor groups", gs, f.liveUserBindings(t, "alice"))
	}
	if _, err := f.st.AuthRemovalGet(ctx, "acme", "alice"); !isNotFound(err) {
		t.Fatalf("the removal survived the reactivation: %v", err)
	}
	if f.count("auth_user_reactivated") != 1 {
		t.Fatalf("audit = %v", f.audit.actions())
	}
}

// A user re-enabled behind the policy's back (a store write, another
// replica's older binary) is never unbound or erased: the removal ends.
func TestRemoval_AUserNoLongerDisabledEndsItsRemoval(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: lcDay})
	ctx := context.Background()
	if err := f.srv.startRemoval(ctx, "acme", "alice", "", store.RemovalReasonOperator, "acme-op"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UserUpdate(ctx, "acme", "alice", store.UserPatch{Status: strPtr("active")}); err != nil {
		t.Fatal(err)
	}
	f.srv.AuthLifecycleSweep(ctx, time.Now().Add(2*lcDay))
	if gs := f.groupsOf(t, "alice"); len(gs) != 2 {
		t.Fatalf("an active user was unbound: %v", gs)
	}
	if _, err := f.st.AuthRemovalGet(ctx, "acme", "alice"); !isNotFound(err) {
		t.Fatalf("removal kept: %v", err)
	}
}

// dormant_after disables a user nobody signed in as through any gate for that
// long; a recent login through another gate keeps the user.
func TestRemoval_DormantUsersAreDisabledAndRecentOnesKept(t *testing.T) {
	f := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay, DormantAfter: 90 * lcDay})
	ctx := context.Background()
	f.addUser(t, "acme", "bob", "isolated")
	f.addLogin(t, "acme", "bob", "local", "bob")
	f.addLogin(t, "acme", "bob", "tg", "77")
	if err := f.st.UserIdentityTouchLogin(ctx, "tg", "", "77", time.Now().Add(100*lcDay)); err != nil {
		t.Fatal(err)
	}
	f.srv.AuthLifecycleSweep(ctx, time.Now().Add(120*lcDay))
	a, _ := f.st.UserGet(ctx, "acme", "alice")
	b, _ := f.st.UserGet(ctx, "acme", "bob")
	if a.Status != "disabled" || b.Status != "active" {
		t.Fatalf("alice %q bob %q; want alice dormant-disabled, bob kept", a.Status, b.Status)
	}
	if r, err := f.st.AuthRemovalGet(ctx, "acme", "alice"); err != nil || r.Reason != store.RemovalReasonDormant {
		t.Fatalf("removal = %+v %v", r, err)
	}
	// Off by default: no dormancy without dormant_after.
	g := lifecycleFixture(t, config.AuthRemovalPolicy{UnbindAfter: 7 * lcDay})
	g.srv.AuthLifecycleSweep(ctx, time.Now().Add(1000*lcDay))
	if u, _ := g.st.UserGet(ctx, "acme", "alice"); u.Status != "active" {
		t.Fatal("a user was dormant-disabled with dormant_after unset")
	}
}
