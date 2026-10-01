package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func TestResolvePrincipals_Valid(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_MARKETING", "lct_marketing_secret")
	c := &Config{Principals: map[string]PrincipalDef{
		"marketing": {Tenant: "acme", Subject: "marketing", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_MARKETING"},
	}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}
	if len(c.ResolvedPrincipals) != 1 {
		t.Fatalf("got %d resolved principals, want 1", len(c.ResolvedPrincipals))
	}
	dp := c.ResolvedPrincipals[0]
	if dp.Secret != "lct_marketing_secret" {
		t.Errorf("secret = %q, want the env value", dp.Secret)
	}
	if dp.Principal.TenantID != "acme" || dp.Principal.Subject != "marketing" {
		t.Errorf("principal = %+v, want acme/marketing", dp.Principal)
	}
	if dp.Principal.TokenDefID != "cfg:marketing" {
		t.Errorf("TokenDefID = %q, want cfg:marketing", dp.Principal.TokenDefID)
	}
	if dp.Principal.Legacy {
		t.Error("a declared principal must not be Legacy")
	}
}

func TestResolvePrincipals_MissingSubject(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_X", "lct_x")
	c := &Config{Principals: map[string]PrincipalDef{
		"x": {Tenant: "acme", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_X"},
	}}
	if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "subject is required") {
		t.Errorf("err = %v, want a 'subject is required' error", err)
	}
}

func TestResolvePrincipals_UnknownScope(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_X", "lct_x")
	c := &Config{Principals: map[string]PrincipalDef{
		"x": {Subject: "x", Scopes: []string{"not:a:real:scope"}, TokenEnv: "LOOMCYCLE_TOKEN_X"},
	}}
	if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("err = %v, want an 'unknown scope' error", err)
	}
}

func TestResolvePrincipals_NonAllowlistedTokenEnv(t *testing.T) {
	t.Setenv("FOO_TOKEN", "lct_x")
	c := &Config{Principals: map[string]PrincipalDef{
		"x": {Subject: "x", Scopes: []string{auth.ScopeTenant}, TokenEnv: "FOO_TOKEN"},
	}}
	if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_") {
		t.Errorf("err = %v, want a token_env-allowlist error", err)
	}
}

func TestResolvePrincipals_InfraSecretTokenEnvIsError(t *testing.T) {
	// Pointing token_env at one of loomcycle's own infra secrets must be refused
	// even though it is LOOMCYCLE_*-prefixed (denylist guard).
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "legacy-secret")
	c := &Config{Principals: map[string]PrincipalDef{
		"x": {Subject: "x", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_AUTH_TOKEN"},
	}}
	if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "infrastructure secret") {
		t.Errorf("err = %v, want an infra-secret token_env error", err)
	}
}

func TestResolvePrincipals_EmptyTokenEnvIsInert(t *testing.T) {
	// LOOMCYCLE_TOKEN_ABSENT is deliberately NOT set → the principal is inert.
	c := &Config{Principals: map[string]PrincipalDef{
		"absent": {Tenant: "acme", Subject: "absent", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_ABSENT"},
	}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("an empty token_env must NOT fail load: %v", err)
	}
	if len(c.ResolvedPrincipals) != 0 {
		t.Errorf("an inert principal must not be in the resolved table; got %d", len(c.ResolvedPrincipals))
	}
	if len(c.Warnings) == 0 || !strings.Contains(strings.Join(c.Warnings, "\n"), "inert") {
		t.Errorf("an empty token_env must warn (inert); warnings = %v", c.Warnings)
	}
}

func TestResolvePrincipals_DuplicateSecretIsError(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_A", "lct_same")
	t.Setenv("LOOMCYCLE_TOKEN_B", "lct_same") // same value → ambiguous identity
	c := &Config{Principals: map[string]PrincipalDef{
		"a": {Tenant: "acme", Subject: "a", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_A"},
		"b": {Tenant: "acme", Subject: "b", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_B"},
	}}
	if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "same secret") {
		t.Errorf("err = %v, want a duplicate-secret config error", err)
	}
}

// TestResolvePrincipals_NonAdminWithoutTenantIsError: the store's list queries
// read tenant "" as every tenant, so a tenant-confined principal declared with
// no tenant was served every tenant's users and runs by GET /v1/_users. It must
// not load; an admin — whose "" is the all-tenants view by design — still does.
func TestResolvePrincipals_NonAdminWithoutTenantIsError(t *testing.T) {
	// A distinct token_env per case: resolving one registers its name on the
	// package's interpolation denylist, which a reused name would then trip.
	for i, scopes := range [][]string{{auth.ScopeTenant}, {auth.ScopeRunsCreate, auth.ScopeRunsRead}, nil} {
		env := "LOOMCYCLE_TOKEN_NOTENANT_" + strconv.Itoa(i)
		t.Setenv(env, "lct_notenant_"+strconv.Itoa(i))
		c := &Config{Principals: map[string]PrincipalDef{
			"x": {Subject: "x", Scopes: scopes, TokenEnv: env},
		}}
		if err := resolvePrincipals(c); err == nil || !strings.Contains(err.Error(), "tenant is required") {
			t.Errorf("scopes %v with no tenant: err = %v, want a 'tenant is required' error", scopes, err)
		}
	}
	t.Setenv("LOOMCYCLE_TOKEN_NOTENANT_ADMIN", "lct_notenant_admin")
	c := &Config{Principals: map[string]PrincipalDef{
		"ops": {Subject: "ops", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_TOKEN_NOTENANT_ADMIN"},
	}}
	if err := resolvePrincipals(c); err != nil || len(c.ResolvedPrincipals) != 1 {
		t.Errorf("an admin principal with no tenant: err = %v, resolved %d — want it to load", err, len(c.ResolvedPrincipals))
	}
}
