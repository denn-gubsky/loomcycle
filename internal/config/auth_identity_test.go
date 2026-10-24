package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func strs(v ...string) *[]string { return &v }

func TestResolveLifecycle_DefaultsAreIsolatedFloorGroupsActive(t *testing.T) {
	nu, err := resolveLifecycle(AuthLifecycleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := AuthNewUser{AccessMode: "isolated", Groups: []string{"everyone", "new-users"}, Status: "active", TokenTTL: 12 * time.Hour}
	if !reflect.DeepEqual(nu, want) {
		t.Fatalf("defaults = %+v, want %+v", nu, want)
	}
	// A zero policy (a Config built without validate) reads the same floor.
	if got := (AuthPolicy{}).NewUserFloor(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zero policy floor = %+v", got)
	}
}

func TestResolveLifecycle_PendingAndGroupsOverride(t *testing.T) {
	nu, err := resolveLifecycle(AuthLifecycleConfig{NewUser: AuthNewUserConfig{Status: "pending", Groups: strs("everyone"), TokenTTL: "1h"}})
	if err != nil {
		t.Fatal(err)
	}
	if nu.Status != "pending" || !reflect.DeepEqual(nu.Groups, []string{"everyone"}) || nu.TokenTTL != time.Hour {
		t.Fatalf("got %+v", nu)
	}
}

// The deployment floor is what strangers get unasked, so it may never be
// tenant reach.
func TestResolveLifecycle_RefusesTenantFloor(t *testing.T) {
	_, err := resolveLifecycle(AuthLifecycleConfig{NewUser: AuthNewUserConfig{AccessMode: "tenant"}})
	if err == nil || !strings.Contains(err.Error(), "must be isolated") {
		t.Fatalf("tenant floor: %v", err)
	}
	for _, bad := range []AuthNewUserConfig{{Status: "approved"}, {AccessMode: "admin"}, {Groups: strs("bad name")}, {TokenTTL: "1000h"}} {
		if _, err := resolveLifecycle(AuthLifecycleConfig{NewUser: bad}); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
}

func TestResolveIdentityMap_FixedTenantDefaults(t *testing.T) {
	m, err := resolveIdentityMap("google", &AuthIdentityMapConfig{Tenant: "acme"}, defaultLifecycle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tenant != "acme" || m.SubjectClaim != "sub" || m.Provision != ProvisionKnown || m.SyncGroups || m.Trusted {
		t.Fatalf("got %+v", m)
	}
	if !reflect.DeepEqual(m.NewUser, defaultLifecycle()) {
		t.Fatalf("new user = %+v", m.NewUser)
	}
}

// Nothing resolves to the shared "" tenant by accident: an identity map must
// name a tenant, and a claim must go through an allowlist of real tenant ids.
func TestResolveIdentityMap_TenantIsNeverImplicit(t *testing.T) {
	cases := map[string]*AuthIdentityMapConfig{
		"missing":          nil,
		"empty":            {},
		"both":             {Tenant: "a", TenantClaim: "hd", AllowedTenants: map[string]string{"x": "a"}},
		"claim no list":    {TenantClaim: "hd"},
		"list to shared":   {TenantClaim: "hd", AllowedTenants: map[string]string{"acme.com": ""}},
		"list to reserved": {TenantClaim: "hd", AllowedTenants: map[string]string{"acme.com": "_shared"}},
		"bad tenant":       {Tenant: "a/b"},
		"list on fixed":    {Tenant: "a", AllowedTenants: map[string]string{"x": "a"}},
	}
	for name, c := range cases {
		if _, err := resolveIdentityMap("g", c, defaultLifecycle(), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m, err := resolveIdentityMap("g", &AuthIdentityMapConfig{TenantClaim: "email_domain",
		AllowedTenants: map[string]string{"ACME.com": "acme"}}, defaultLifecycle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if m.AllowedTenants["acme.com"] != "acme" {
		t.Fatalf("email domains are matched lower-case: %+v", m.AllowedTenants)
	}
}

// D12: an untrusted gate cannot raise what a new user gets, nor sync groups.
func TestResolveIdentityMap_UntrustedGateCannotRaise(t *testing.T) {
	floor := defaultLifecycle()
	raises := map[string]*AuthNewUserConfig{
		"tenant reach": {AccessMode: "tenant"},
		"extra group":  {Groups: strs("everyone", "staff")},
	}
	for name, nu := range raises {
		if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", NewUser: nu}, floor, false); err == nil ||
			!strings.Contains(err.Error(), "trusted: true") {
			t.Errorf("%s on an untrusted gate: %v", name, err)
		}
		if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", NewUser: nu}, floor, true); err != nil {
			t.Errorf("%s on a trusted gate: %v", name, err)
		}
	}
	// Skipping a pending approval is a raise too.
	pending := floor
	pending.Status = UserStatusPending
	if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", NewUser: &AuthNewUserConfig{Status: "active"}}, pending, false); err == nil {
		t.Error("an untrusted gate skipped the pending floor")
	}
	// Narrowing is always allowed.
	m, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", NewUser: &AuthNewUserConfig{Groups: strs("everyone"), Status: "pending"}}, floor, false)
	if err != nil || m.NewUser.Status != "pending" || !reflect.DeepEqual(m.NewUser.Groups, []string{"everyone"}) {
		t.Fatalf("narrowing: %+v, %v", m, err)
	}
	if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", Groups: &AuthIdentityGroupsConfig{Claim: "groups"}}, floor, false); err == nil {
		t.Error("an untrusted gate may sync groups")
	}
}

func TestResolveIdentityMap_Groups(t *testing.T) {
	m, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", Provision: "auto",
		Groups: &AuthIdentityGroupsConfig{Claim: "groups", Provision: true, Map: map[string]string{"hr-admins": "admins"}}},
		defaultLifecycle(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !m.SyncGroups || m.GroupsClaim != "groups" || !m.GroupsProvision || m.GroupMap["hr-admins"] != "admins" || m.Provision != ProvisionAuto {
		t.Fatalf("got %+v", m)
	}
	if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme",
		Groups: &AuthIdentityGroupsConfig{Map: map[string]string{"x": "not a name"}}}, defaultLifecycle(), true); err == nil {
		t.Error("an invalid group_map target was accepted")
	}
	if _, err := resolveIdentityMap("g", &AuthIdentityMapConfig{Tenant: "acme", Provision: "always"}, defaultLifecycle(), true); err == nil {
		t.Error("an unknown provision policy was accepted")
	}
}

func TestResolveAuth_IdentityMapRefusedOnTokenAndPasswordGates(t *testing.T) {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token", IdentityMap: &AuthIdentityMapConfig{Tenant: "a"}}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "identity_map does not apply") {
		t.Fatalf("token gate: %v", err)
	}
	c = passwordGateConfig("", AuthGateConfig{IdentityMap: &AuthIdentityMapConfig{Tenant: "a"}})
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "identity_map does not apply") {
		t.Fatalf("password gate: %v", err)
	}
}

// No fail-open keys: until a gate kind provisions users, an auth.lifecycle
// block (and a kind that would take an identity_map) is refused at load.
func TestResolveAuth_LifecycleRefusedUntilAGateProvisions(t *testing.T) {
	c := &Config{Auth: AuthConfig{Lifecycle: AuthLifecycleConfig{NewUser: AuthNewUserConfig{Status: "pending"}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("lifecycle: %v", err)
	}
	c = &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"g": {Kind: "oidc", IdentityMap: &AuthIdentityMapConfig{Tenant: "a"}}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "not supported in this release yet") {
		t.Fatalf("oidc with identity_map: %v", err)
	}
}
