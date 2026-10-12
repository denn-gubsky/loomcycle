package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
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

func TestResolvePrincipals_RefusesNonCharsetTenant(t *testing.T) {
	// The tenant id names a directory under the dynamic volume root, so a
	// tenant that is not one charset-clean path segment would alias another
	// tenant's tree ("x/.." → acme's). Refused before the secret is read.
	for i, tenant := range []string{"x/..", "a/b", "..", ".", "acme.corp", "_shared", "_EPHEMERAL"} {
		// A distinct token_env per case: a principal that passes marks its
		// token_env non-interpolatable, which would refuse the next case for
		// the wrong reason.
		c := &Config{Principals: map[string]PrincipalDef{
			"p": {Tenant: tenant, Subject: "p", Scopes: []string{auth.ScopeTenant}, TokenEnv: fmt.Sprintf("LOOMCYCLE_TOKEN_TENANT_CHARSET_%d", i)},
		}}
		err := resolvePrincipals(c)
		if err == nil || !strings.Contains(err.Error(), "tenant id") {
			t.Errorf("tenant %q: err = %v, want a tenant-id config error", tenant, err)
		}
	}
}

func TestResolvePrincipals_RefusesCaseFoldTenantCollision(t *testing.T) {
	// "Acme" and "acme" are one directory on a case-insensitive filesystem.
	t.Setenv("LOOMCYCLE_TOKEN_FOLD_A", "lct_fold_a")
	t.Setenv("LOOMCYCLE_TOKEN_FOLD_B", "lct_fold_b")
	c := &Config{Principals: map[string]PrincipalDef{
		"a": {Tenant: "Acme", Subject: "a", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_FOLD_A"},
		"b": {Tenant: "acme", Subject: "b", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_FOLD_B"},
	}}
	err := resolvePrincipals(c)
	if err == nil || !strings.Contains(err.Error(), "differs only by case") {
		t.Fatalf("err = %v, want a case-collision config error", err)
	}
}

func TestResolvePrincipals_SameTenantSpelledIdenticallyIsAccepted(t *testing.T) {
	// Two principals in ONE tenant is the ordinary case and must keep loading.
	t.Setenv("LOOMCYCLE_TOKEN_SAME_A", "lct_same_a")
	t.Setenv("LOOMCYCLE_TOKEN_SAME_B", "lct_same_b")
	c := &Config{Principals: map[string]PrincipalDef{
		"a": {Tenant: "acme", Subject: "a", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_SAME_A"},
		"b": {Tenant: "acme", Subject: "b", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_TOKEN_SAME_B"},
		"o": {Tenant: "", Subject: "o", Scopes: []string{auth.ScopeAdmin}, TokenEnv: "LOOMCYCLE_TOKEN_SAME_O"},
	}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}
	if got := c.PrincipalTenants(); strings.Join(got, ",") != ",acme" {
		t.Errorf("PrincipalTenants = %q, want [\"\" acme]", got)
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

// A declared principal can carry an agent allowlist, and the same mistakes a
// mint refuses are refused at load.
func TestResolvePrincipals_Agents(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_SCORER", "lct_scorer_secret")
	list := func(names ...string) *[]string { return &names }
	c := &Config{Principals: map[string]PrincipalDef{
		"scorer": {Tenant: "acme", Subject: "scorer", Scopes: []string{auth.ScopeRunsCreate}, TokenEnv: "LOOMCYCLE_TOKEN_SCORER", Agents: list("score")},
	}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}
	r := c.ResolvedPrincipals[0].Principal.Restrictions
	if !r.RunPlaneOnly() || !r.AgentAllowed("score") || r.AgentAllowed("judge") {
		t.Errorf("restrictions = %+v, want the list [score]", r)
	}
	// Each case names its OWN token_env: resolving a principal records its
	// env name as non-interpolatable for the rest of the process, and a later
	// principal reusing the name is refused for that reason instead of the
	// one under test.
	for i, c := range []struct {
		name   string
		scopes []string
		agents *[]string
	}{
		{"empty list", []string{auth.ScopeRunsCreate}, list()},
		{"bad name", []string{auth.ScopeRunsCreate}, list("has space")},
		{"admin", []string{auth.ScopeAdmin}, list("score")},
	} {
		env := "LOOMCYCLE_TOKEN_AGENTS_CASE_" + strconv.Itoa(i)
		t.Setenv(env, "lct_case_secret_"+strconv.Itoa(i))
		def := PrincipalDef{Tenant: "acme", Subject: "s", Scopes: c.scopes, TokenEnv: env, Agents: c.agents}
		err := resolvePrincipals(&Config{Principals: map[string]PrincipalDef{"p": def}})
		if err == nil || !strings.Contains(err.Error(), "agents") {
			t.Errorf("%s: err = %v, want a refusal naming agents", c.name, err)
		}
		// The same principal without the list loads, so the refusal above is
		// about the list and nothing else.
		env2 := env + "_OK"
		t.Setenv(env2, "lct_case_ok_"+strconv.Itoa(i))
		def.TokenEnv, def.Agents = env2, nil
		if err := resolvePrincipals(&Config{Principals: map[string]PrincipalDef{"p": def}}); err != nil {
			t.Errorf("%s without agents: %v", c.name, err)
		}
	}
}

// `overrides:` on a declared principal reads as `none`, a comma-separated
// scalar or a list, and anything else fails the load.
func TestPrincipalDef_OverridesYAML(t *testing.T) {
	for in, want := range map[string][]string{
		"overrides: none":           {},
		"overrides: effort, tier":   {"effort", "tier"},
		"overrides: [effort, tier]": {"effort", "tier"},
	} {
		var def PrincipalDef
		if err := yaml.Unmarshal([]byte(in), &def); err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if def.Overrides == nil || len(def.Overrides.Allow) != len(want) {
			t.Errorf("%q read as %+v, want allow %v", in, def.Overrides, want)
		}
	}
	for _, bad := range []string{"overrides: all,", "overrides: []", "overrides: {a: b}"} {
		var def PrincipalDef
		if err := yaml.Unmarshal([]byte(bad), &def); err == nil && (def.Overrides == nil || len(def.Overrides.Allow) == 0) && bad != "overrides: all," {
			t.Errorf("%q accepted as %+v", bad, def.Overrides)
		}
	}
	var absent PrincipalDef
	if err := yaml.Unmarshal([]byte("subject: s"), &absent); err != nil || absent.Overrides != nil {
		t.Errorf("an absent key read as %+v (err %v)", absent.Overrides, err)
	}

	t.Setenv("LOOMCYCLE_TOKEN_OVR_OK", "lct_ovr_ok")
	t.Setenv("LOOMCYCLE_TOKEN_OVR_BAD", "lct_ovr_bad")
	c := &Config{Principals: map[string]PrincipalDef{"p": {Tenant: "acme", Subject: "s", Scopes: []string{auth.ScopeRunsCreate},
		TokenEnv: "LOOMCYCLE_TOKEN_OVR_OK", Overrides: &overridePolicyYAML{}}}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}
	if r := c.ResolvedPrincipals[0].Principal.Restrictions; r.Overrides == nil || r.OverrideAllowed("model") {
		t.Errorf("resolved restrictions = %+v, want every override locked", r)
	}
	bad := &overridePolicyYAML{}
	bad.Allow = []string{"temperature"}
	err := resolvePrincipals(&Config{Principals: map[string]PrincipalDef{"p": {Tenant: "acme", Subject: "s", Scopes: []string{auth.ScopeRunsCreate},
		TokenEnv: "LOOMCYCLE_TOKEN_OVR_BAD", Overrides: bad}}})
	if err == nil || !strings.Contains(err.Error(), "overrides") {
		t.Errorf("an unknown override name: err = %v, want a refusal naming overrides", err)
	}
}

func TestResolvePrincipals_Rate(t *testing.T) {
	t.Setenv("LOOMCYCLE_TOKEN_RATE_OK", "lct_rate_ok")
	t.Setenv("LOOMCYCLE_TOKEN_RATE_BAD", "lct_rate_bad")
	var def PrincipalDef
	if err := yaml.Unmarshal([]byte("tenant: acme\nsubject: s\nscopes: [runs:create]\ntoken_env: LOOMCYCLE_TOKEN_RATE_OK\nrate: {per_minute: 6, burst: 20}\n"), &def); err != nil {
		t.Fatal(err)
	}
	c := &Config{Principals: map[string]PrincipalDef{"p": def}}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}
	if r := c.ResolvedPrincipals[0].Principal.Restrictions.Rate; r == nil || r.PerMinute != 6 || r.Burst != 20 {
		t.Errorf("resolved rate = %+v, want 6/min burst 20", r)
	}
	bad := PrincipalDef{Tenant: "acme", Subject: "s", Scopes: []string{auth.ScopeRunsCreate}, TokenEnv: "LOOMCYCLE_TOKEN_RATE_BAD", Rate: &RateLimitYAML{PerMinute: 0, Burst: 5}}
	if err := resolvePrincipals(&Config{Principals: map[string]PrincipalDef{"p": bad}}); err == nil || !strings.Contains(err.Error(), "rate") {
		t.Errorf("a rate with no per_minute: err = %v, want a refusal naming rate", err)
	}
}
