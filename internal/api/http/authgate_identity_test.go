package http

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const testIssuer = "https://idp.example"

// identityGateCfg is a resolved identity gate as resolveAuth would build it.
func identityGateCfg(name string, m config.AuthIdentityMap) config.AuthGate {
	if m.SubjectClaim == "" {
		m.SubjectClaim = "sub"
	}
	if m.Provision == "" {
		m.Provision = config.ProvisionKnown
	}
	if m.NewUser.AccessMode == "" {
		m.NewUser = config.AuthNewUser{AccessMode: "isolated", Groups: []string{"everyone", "new-users"}, Status: "active"}
	}
	return config.AuthGate{Name: name, Kind: "oidc", Label: name, Identity: &m}
}

func assertion(sub string, claims map[string]any) authgate.Assertion {
	if claims == nil {
		claims = map[string]any{}
	}
	claims["sub"] = sub
	return authgate.Assertion{Subject: sub, Issuer: testIssuer, Claims: claims, Email: sub + "@acme.example", EmailVerified: true}
}

func TestProvision_AutoCreatesAtTheFloorAndLinks(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "everyone"}); err != nil {
		t.Fatal(err)
	}
	g := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
	a := assertion("111", nil)
	a.DisplayName = "Alice"
	a.AccessMode = "tenant" // an untrusted gate's assertion is ignored
	got, err := s.provisionOrRefuse(ctx, g, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "acme" || got.Subject != "google:111" || !got.Created || got.Pending {
		t.Fatalf("got %+v", got)
	}
	u, err := st.UserGet(ctx, "acme", "google:111")
	if err != nil || u.AccessMode != "isolated" || u.Status != "active" || u.DisplayName != "Alice" {
		t.Fatalf("user = %+v %v", u, err)
	}
	id, err := st.UserIdentityGet(ctx, "google", testIssuer, "111")
	if err != nil || id.Subject != "google:111" || id.LastLoginAt.IsZero() {
		t.Fatalf("identity = %+v %v", id, err)
	}
	groups, _ := st.GroupsOfSubject(ctx, "acme", "google:111")
	if !reflect.DeepEqual(groups, []string{"everyone"}) {
		t.Fatalf("floor groups = %v (only existing floor groups are joined)", groups)
	}
	// The second login finds the linked identity and creates nothing.
	got, err = s.provisionOrRefuse(ctx, g, a, "")
	if err != nil || got.Created || got.Subject != "google:111" {
		t.Fatalf("second login: %+v %v", got, err)
	}
}

func TestProvision_KnownAndNeverPolicies(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	known := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme"})
	if _, err := s.provisionOrRefuse(ctx, known, assertion("222", nil), ""); !errors.Is(err, errIdentityRefused) {
		t.Fatalf("known, unregistered: %v", err)
	}
	if _, err := st.UserGet(ctx, "acme", "google:222"); !isNotFound(err) {
		t.Fatal("a known-policy gate created a user")
	}
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:222", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.provisionOrRefuse(ctx, known, assertion("222", nil), "")
	if err != nil || got.Created || got.Subject != "google:222" {
		t.Fatalf("known, registered: %+v %v", got, err)
	}
	never := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionNever})
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:333", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.provisionOrRefuse(ctx, never, assertion("333", nil), ""); !errors.Is(err, errIdentityRefused) {
		t.Fatalf("never, unlinked: %v", err)
	}
	// Already linked (above, through the known gate of the same name): fine.
	if _, err := s.provisionOrRefuse(ctx, never, assertion("222", nil), ""); err != nil {
		t.Fatalf("never, linked: %v", err)
	}
}

func TestProvision_DisabledUserIsRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	g := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
	if _, err := s.provisionOrRefuse(ctx, g, assertion("444", nil), ""); err != nil {
		t.Fatal(err)
	}
	off := "disabled"
	if err := st.UserUpdate(ctx, "acme", "google:444", store.UserPatch{Status: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.provisionOrRefuse(ctx, g, assertion("444", nil), ""); !errors.Is(err, errUserDisabled) {
		t.Fatalf("disabled: %v", err)
	}
}

// RFC FA §8: a tenant claim outside allowed_tenants is refused (never the
// shared tenant), and a linked identity whose tenant the map no longer gives
// is refused too.
func TestProvision_TenantOutsideTheAllowlistIsRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	m := config.AuthIdentityMap{TenantClaim: "tid", AllowedTenants: map[string]string{"t-acme": "acme"}, Provision: config.ProvisionAuto}
	g := identityGateCfg("entra", m)
	if _, err := s.provisionOrRefuse(ctx, g, assertion("o1", map[string]any{"tid": "t-evil"}), ""); !errors.Is(err, errIdentityRefused) {
		t.Fatalf("unlisted tid: %v", err)
	}
	if users, _ := st.UserList(ctx, ""); len(users) != 0 {
		t.Fatalf("users created: %+v", users)
	}
	if _, err := s.provisionOrRefuse(ctx, g, assertion("o1", map[string]any{"tid": "t-acme"}), ""); err != nil {
		t.Fatal(err)
	}
	// The operator re-points the allowlist: the linked identity is locked out.
	m.AllowedTenants = map[string]string{"t-acme": "other"}
	g = identityGateCfg("entra", m)
	if _, err := s.provisionOrRefuse(ctx, g, assertion("o1", map[string]any{"tid": "t-acme"}), ""); !errors.Is(err, errIdentityRefused) {
		t.Fatalf("tenant moved under a linked identity: %v", err)
	}
}

// D14: a linked identity signs in as the subject it was linked to; nothing
// links by e-mail.
func TestProvision_LinkedIdentityKeepsItsSubjectAndEmailNeverLinks(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: "alice", Gate: "google",
		Issuer: testIssuer, ExternalSubject: "555", Email: "alice@acme.example"}); err != nil {
		t.Fatal(err)
	}
	g := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto})
	got, err := s.provisionOrRefuse(ctx, g, assertion("555", nil), "")
	if err != nil || got.Subject != "alice" {
		t.Fatalf("linked: %+v %v", got, err)
	}
	// Another account with alice's e-mail becomes its own user.
	other := assertion("666", nil)
	other.Email = "alice@acme.example"
	got, err = s.provisionOrRefuse(ctx, g, other, "")
	if err != nil || got.Subject != "google:666" {
		t.Fatalf("same e-mail: %+v %v", got, err)
	}
}

func TestProvision_SubjectWithAnotherIdentityAtTheGateIsRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:777", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// google:777 is already linked to a different provider account.
	if err := st.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: "google:777", Gate: "google",
		Issuer: testIssuer, ExternalSubject: "something-else"}); err != nil {
		t.Fatal(err)
	}
	g := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme"})
	if _, err := s.provisionOrRefuse(ctx, g, assertion("777", nil), ""); !errors.Is(err, errIdentityRefused) {
		t.Fatalf("second identity at the gate: %v", err)
	}
}

func TestProvision_PendingFloorMarksTheLoginPending(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	g := identityGateCfg("google", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Status: "pending"}})
	got, err := s.provisionOrRefuse(ctx, g, assertion("888", nil), "")
	if err != nil || !got.Pending {
		t.Fatalf("pending: %+v %v", got, err)
	}
	if u, _ := st.UserGet(ctx, "acme", "google:888"); u.Status != store.UserStatusPending {
		t.Fatalf("status = %q", u.Status)
	}
}

// D12: only a trusted gate's asserted access_mode is applied.
func TestProvision_TrustedGateAccessModeApplies(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	g := identityGateCfg("hr", config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto, Trusted: true})
	a := assertion("999", nil)
	a.AccessMode = "tenant"
	if _, err := s.provisionOrRefuse(ctx, g, a, ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.UserGet(ctx, "acme", "hr:999"); u.AccessMode != "tenant" {
		t.Fatalf("trusted create mode = %q", u.AccessMode)
	}
	a.AccessMode = "isolated"
	if _, err := s.provisionOrRefuse(ctx, g, a, ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.UserGet(ctx, "acme", "hr:999"); u.AccessMode != "isolated" {
		t.Fatalf("trusted login mode = %q", u.AccessMode)
	}
}

// RFC FA §3.7b / D6: a login moves the user between the gate's groups and
// never touches a local group or another gate's.
func TestProvision_GroupSyncMovesOnlyTheGatesGroups(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	for _, g := range []store.GroupRow{
		{TenantID: "acme", Name: "support", ExternalID: "google:support"},
		{TenantID: "acme", Name: "support-l2", ExternalID: "google:support-l2"},
		{TenantID: "acme", Name: "local-admins"},
		{TenantID: "acme", Name: "okta-ops", ExternalID: "okta:ops"},
	} {
		if _, err := st.GroupPut(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto, Trusted: true, SyncGroups: true,
		GroupsClaim: "groups", GroupsProvision: true, GroupMap: map[string]string{"hr-admins": "hr"}}
	g := identityGateCfg("google", m)
	a := assertion("1010", map[string]any{"groups": []any{"support", "hr-admins"}})
	if _, err := s.provisionOrRefuse(ctx, g, a, ""); err != nil {
		t.Fatal(err)
	}
	// An operator also put the user in a local group and in okta's group.
	for _, n := range []string{"local-admins", "okta-ops"} {
		if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: n, MemberKind: "user", MemberID: "google:1010"}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.GroupsOfSubject(ctx, "acme", "google:1010")
	if !reflect.DeepEqual(got, []string{"hr", "local-admins", "okta-ops", "support"}) {
		t.Fatalf("after first login: %v", got)
	}
	if hr, err := st.GroupGet(ctx, "acme", "hr"); err != nil || hr.ExternalID != "google:hr-admins" {
		t.Fatalf("provisioned group = %+v %v", hr, err)
	}
	// The provider moves the user to support-l2 and drops hr-admins; it also
	// names the local and the okta groups, which must not matter.
	a = assertion("1010", map[string]any{"groups": []any{"support-l2", "local-admins", "okta-ops", "ops"}})
	if _, err := s.provisionOrRefuse(ctx, g, a, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GroupsOfSubject(ctx, "acme", "google:1010")
	if !reflect.DeepEqual(got, []string{"local-admins", "okta-ops", "ops", "support-l2"}) {
		t.Fatalf("after the move: %v", got)
	}
	if slices.Contains(got, "support") {
		t.Fatal("not removed from support")
	}
	// Another member of local-admins is untouched.
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "local-admins", MemberKind: "user", MemberID: "bob"}); err != nil {
		t.Fatal(err)
	}
	a = assertion("1010", map[string]any{})
	if _, err := s.provisionOrRefuse(ctx, g, a, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GroupsOfSubject(ctx, "acme", "google:1010")
	if !reflect.DeepEqual(got, []string{"local-admins", "okta-ops"}) {
		t.Fatalf("no groups asserted: %v", got)
	}
}

// An untrusted map syncs nothing even if its config says sync (defence in
// depth behind the loader's refusal).
func TestProvision_UntrustedGateSyncsNoGroups(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto, SyncGroups: true, GroupsClaim: "groups", GroupsProvision: true}
	g := identityGateCfg("google", m)
	if _, err := s.provisionOrRefuse(ctx, g, assertion("1111", map[string]any{"groups": []any{"admins"}}), ""); err != nil {
		t.Fatal(err)
	}
	if groups, _ := st.GroupList(ctx, "acme"); len(groups) != 0 {
		t.Fatalf("untrusted gate created groups: %+v", groups)
	}
}
