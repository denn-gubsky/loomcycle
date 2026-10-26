package identity

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

func claimMap(t *testing.T, tenant string) *config.AuthIdentityMap {
	t.Helper()
	return &config.AuthIdentityMap{TenantClaim: "tid", AllowedTenants: map[string]string{"t-1": tenant}, SubjectClaim: "sub"}
}

func TestResolve_FixedTenantAndGateSubject(t *testing.T) {
	m := &config.AuthIdentityMap{Tenant: "acme", SubjectClaim: "sub"}
	got, err := Resolve("google", m, authgate.Assertion{Subject: "1234", Claims: map[string]any{"sub": "1234"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != (Target{TenantID: "acme", Subject: "google:1234"}) {
		t.Fatalf("got %+v", got)
	}
	// A gate that sends no claims is keyed by its Subject.
	got, err = Resolve("hr", m, authgate.Assertion{Subject: "e-7"})
	if err != nil || got.Subject != "hr:e-7" {
		t.Fatalf("no claims: %+v %v", got, err)
	}
	// A numeric subject claim reads as its literal.
	got, err = Resolve("gh", m, authgate.Assertion{Claims: map[string]any{"sub": json.Number("98765432109876543")}})
	if err != nil || got.Subject != "gh:98765432109876543" {
		t.Fatalf("numeric sub: %+v %v", got, err)
	}
}

// RFC FA §8: a tenant claim outside allowed_tenants is refused and never
// falls back to the shared tenant.
func TestTenant_ClaimOutsideAllowlistIsRefused(t *testing.T) {
	m := claimMap(t, "acme")
	if _, err := Tenant(m, authgate.Assertion{Claims: map[string]any{"tid": "t-2"}}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("unlisted tid: %v", err)
	}
	if _, err := Tenant(m, authgate.Assertion{Claims: map[string]any{}}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("missing tid: %v", err)
	}
	if _, err := Tenant(m, authgate.Assertion{Claims: map[string]any{"tid": ""}}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("empty tid: %v", err)
	}
	if _, err := Tenant(nil, authgate.Assertion{}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("no map: %v", err)
	}
	if tn, err := Tenant(m, authgate.Assertion{Claims: map[string]any{"tid": "t-1"}}); err != nil || tn != "acme" {
		t.Fatalf("listed tid: %q %v", tn, err)
	}
}

func TestTenant_EmailDomainNeedsAVerifiedAddress(t *testing.T) {
	m := &config.AuthIdentityMap{TenantClaim: config.EmailDomainClaim, AllowedTenants: map[string]string{"acme.com": "acme"}}
	if _, err := Tenant(m, authgate.Assertion{Email: "a@ACME.com"}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("unverified e-mail: %v", err)
	}
	if tn, err := Tenant(m, authgate.Assertion{Email: "a@ACME.com", EmailVerified: true}); err != nil || tn != "acme" {
		t.Fatalf("verified: %q %v", tn, err)
	}
	if _, err := Tenant(m, authgate.Assertion{Email: "a@evil.acme.com.example", EmailVerified: true}); !errors.Is(err, ErrTenantRefused) {
		t.Fatalf("lookalike domain: %v", err)
	}
}

func TestSubject_RefusesUnusableValues(t *testing.T) {
	m := &config.AuthIdentityMap{Tenant: "a", SubjectClaim: "sub"}
	for _, v := range []string{"", "has space", "tab\t", string(make([]byte, 300))} {
		if _, err := Subject("g", m, authgate.Assertion{Claims: map[string]any{"sub": v}}); !errors.Is(err, ErrSubjectInvalid) {
			t.Errorf("%q accepted", v)
		}
	}
	m.SubjectClaim = "oid"
	if _, err := Subject("g", m, authgate.Assertion{Subject: "x"}); !errors.Is(err, ErrSubjectInvalid) {
		t.Error("a missing custom subject claim fell back to sub")
	}
}

func syncMap(provision bool) *config.AuthIdentityMap {
	return &config.AuthIdentityMap{Tenant: "acme", SyncGroups: true, Trusted: true, GroupsClaim: "groups",
		GroupsProvision: provision, GroupMap: map[string]string{"hr-admins": "admins"}}
}

func TestAssertedGroups_FromClaimDedupedAndBounded(t *testing.T) {
	m := syncMap(true)
	a := authgate.Assertion{Claims: map[string]any{"groups": []any{"b", "a", "b", 7, " ", "bad space"}}}
	if got := AssertedGroups(m, a); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	m.SyncGroups = false
	if got := AssertedGroups(m, a); got != nil {
		t.Fatalf("sync off: %v", got)
	}
}

// RFC FA §3.7b / D6: sync adds and removes only this gate's groups.
func TestSyncPlan_TouchesOnlyThisGatesGroups(t *testing.T) {
	groups := []Group{
		{Name: "support", ExternalID: "google:support"},
		{Name: "support-l2", ExternalID: "google:support-l2"},
		{Name: "local-team"},                          // local
		{Name: "okta-ops", ExternalID: "okta:ops"},    // another gate's
		{Name: "admins-local", ExternalID: ""},        // local
		{Name: "staff", ExternalID: "google:staff-x"}, // ours under another provider name
	}
	current := []string{"support", "local-team", "okta-ops", "staff"}
	// The provider moved alice from support to support-l2 and also claims
	// the local and the other gate's groups by name.
	p := SyncPlan("google", "acme", syncMap(false), []string{"support-l2", "local-team", "ops", "okta-ops"}, groups, current)
	if !reflect.DeepEqual(p.Add, []string{"support-l2"}) || !reflect.DeepEqual(p.Remove, []string{"staff", "support"}) || len(p.Create) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestSyncPlan_ProvisionsWithMapButNeverClaimsATakenName(t *testing.T) {
	groups := []Group{{Name: "admins"}} // a local group already named admins
	p := SyncPlan("google", "acme", syncMap(true), []string{"hr-admins", "eng", "bad name!"}, groups, nil)
	if len(p.Create) != 1 || p.Create[0].Name != "eng" || p.Create[0].ExternalID != "google:eng" || p.Create[0].TenantID != "acme" {
		t.Fatalf("create = %+v", p.Create)
	}
	if !reflect.DeepEqual(p.Add, []string{"eng"}) {
		t.Fatalf("add = %v", p.Add)
	}
	// Without provision nothing is created.
	if p := SyncPlan("google", "acme", syncMap(false), []string{"eng"}, nil, nil); len(p.Create)+len(p.Add) != 0 {
		t.Fatalf("no provision: %+v", p)
	}
}

// D12: an untrusted map plans nothing, even if asked.
func TestSyncPlan_UntrustedPlansNothing(t *testing.T) {
	m := syncMap(true)
	m.Trusted = false
	if p := SyncPlan("google", "acme", m, []string{"eng"}, nil, []string{}); len(p.Create)+len(p.Add)+len(p.Remove) != 0 {
		t.Fatalf("untrusted plan = %+v", p)
	}
}

// RFC FA §8 open redirect: return_to is a Web UI path or an exact allowlisted URL.
func TestSafeReturnTo_RefusesForeignURLs(t *testing.T) {
	allowed := []string{"https://loomcycle.cloud/team"}
	ok := map[string]string{"": "/ui/", "/ui/": "/ui/", "/ui/runs?x=1": "/ui/runs?x=1", "/ui": "/ui",
		"https://loomcycle.cloud/team": "https://loomcycle.cloud/team"}
	for in, want := range ok {
		if got, good := SafeReturnTo(in, allowed); !good || got != want {
			t.Errorf("%q → %q %v", in, got, good)
		}
	}
	for _, bad := range []string{"https://evil.example/", "//evil.example/ui/", "/\\evil.example", "/ui/../v1/_me",
		"/v1/_auth/logout", "https://loomcycle.cloud/team/x", "javascript:alert(1)", "/ui/\r\nSet-Cookie:x", "ui/"} {
		if _, good := SafeReturnTo(bad, allowed); good {
			t.Errorf("%q accepted", bad)
		}
	}
}
