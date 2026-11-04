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

// A subject keyed on an address claim names whoever the provider says holds
// that address, so it is made only from an address the provider verified:
// an unverified e-mail (self-registration, a secondary address) would sign
// its typist in as the address's owner. The verification speaks for the
// assertion's e-mail, so a login-name claim (upn) is keyed only when it IS
// that verified address. A non-address claim, and sub, need no e-mail.
func TestSubject_AnAddressClaimIsKeyedOnlyWhenVerified(t *testing.T) {
	m := &config.AuthIdentityMap{Tenant: "a", SubjectClaim: "email"}
	addr := func(claim, v, email string, verified bool) authgate.Assertion {
		return authgate.Assertion{Subject: "u-1", Email: email, EmailVerified: verified, Claims: map[string]any{"sub": "u-1", claim: v}}
	}
	for name, a := range map[string]authgate.Assertion{
		"email unverified": addr("email", "alice@acme.example", "alice@acme.example", false),
		"email no flag":    {Subject: "u-1", Claims: map[string]any{"sub": "u-1", "email": "alice@acme.example"}},
		"email claim only": {Subject: "u-1", EmailVerified: true, Claims: map[string]any{"sub": "u-1", "email": "alice@acme.example"}},
	} {
		if _, err := Subject("g", m, a); !errors.Is(err, ErrSubjectUnverified) {
			t.Errorf("%s: err = %v, want ErrSubjectUnverified", name, err)
		}
	}
	if s, err := Subject("g", m, addr("email", "alice@acme.example", "alice@acme.example", true)); err != nil || s != "g:alice@acme.example" {
		t.Errorf("verified e-mail: %q %v", s, err)
	}
	m.SubjectClaim = "upn"
	if _, err := Subject("g", m, addr("upn", "alice@acme.example", "mallory@acme.example", true)); !errors.Is(err, ErrSubjectUnverified) {
		t.Errorf("upn beside another verified address: %v", err)
	}
	if s, err := Subject("g", m, addr("upn", "Alice@acme.example", "alice@ACME.example", true)); err != nil || s != "g:Alice@acme.example" {
		t.Errorf("upn that is the verified address: %q %v", s, err)
	}
	m.SubjectClaim = "employee_id"
	if s, err := Subject("g", m, addr("employee_id", "e-7", "", false)); err != nil || s != "g:e-7" {
		t.Errorf("a non-address claim: %q %v", s, err)
	}
	m.SubjectClaim = "sub"
	if s, err := Subject("g", m, addr("email", "alice@acme.example", "alice@acme.example", false)); err != nil || s != "g:u-1" {
		t.Errorf("sub beside an unverified e-mail: %q %v", s, err)
	}
}

// The check follows the VALUE, not a list of claim names: a declared claim
// that holds a plain username ("alice") is keyed on the declaration alone,
// while any claim whose value is an address (mail, preferred_username, a
// custom claim) must be the verified e-mail. email and upn are addresses by
// definition. sub is the provider's own id, whatever its shape.
func TestSubject_AnAddressShapedValueMustBeTheVerifiedEmail(t *testing.T) {
	m := &config.AuthIdentityMap{Tenant: "a"}
	as := func(claim, v, email string, verified bool) authgate.Assertion {
		return authgate.Assertion{Subject: "u-1", Email: email, EmailVerified: verified, Claims: map[string]any{"sub": "u-1", claim: v}}
	}
	for name, tc := range map[string]struct {
		claim string
		a     authgate.Assertion
		want  string // "" = ErrSubjectUnverified
	}{
		"username preferred_username":          {"preferred_username", as("preferred_username", "alice", "", false), "g:alice"},
		"username unique_name":                 {"unique_name", as("unique_name", "alice", "", false), "g:alice"},
		"address preferred_username, other":    {"preferred_username", as("preferred_username", "bob@acme.example", "alice@acme.example", true), ""},
		"address preferred_username, verified": {"preferred_username", as("preferred_username", "alice@acme.example", "alice@acme.example", true), "g:alice@acme.example"},
		"mail unverified":                      {"mail", as("mail", "alice@acme.example", "alice@acme.example", false), ""},
		"custom claim address, unverified":     {"work_address", as("work_address", "alice@acme.example", "", false), ""},
		"mail verified":                        {"mail", as("mail", "alice@acme.example", "alice@acme.example", true), "g:alice@acme.example"},
		"upn without @, unverified":            {"upn", as("upn", "alice", "", false), ""},
		"sub that looks like an address":       {"sub", as("sub", "u-1", "", false), "g:u-1"},
	} {
		m.SubjectClaim = tc.claim
		if tc.claim == "sub" {
			tc.a.Claims["sub"] = "alice@acme.example"
			tc.want = "g:alice@acme.example"
		}
		got, err := Subject("g", m, tc.a)
		switch {
		case tc.want == "" && !errors.Is(err, ErrSubjectUnverified):
			t.Errorf("%s: %q %v, want ErrSubjectUnverified", name, got, err)
		case tc.want != "" && (err != nil || got != tc.want):
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
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
