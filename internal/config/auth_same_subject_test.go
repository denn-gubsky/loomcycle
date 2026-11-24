package config

import (
	"strings"
	"testing"
)

const (
	entraTID1 = "11111111-1111-1111-1111-111111111111"
	entraTID2 = "22222222-2222-2222-2222-222222222222"
)

// sameSubjectCfg is an Entra OIDC gate "entra" keyed on oid and an external
// gate "teams" linking to it by same_subject_as (RFC HD); mutate adjusts
// either before the load.
func sameSubjectCfg(t *testing.T, mutate func(entra, teams *AuthGateConfig)) *Config {
	t.Helper()
	c := oidcConfig(t, "", AuthGateConfig{Preset: "entra", AllowedTIDs: []string{entraTID1},
		IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Subject: "oid", Provision: "known"}})
	entra := c.Auth.Gates["sso"]
	delete(c.Auth.Gates, "sso")
	teams := AuthGateConfig{Kind: "external", Trusted: true,
		IdentityMap: &AuthIdentityMapConfig{Tenant: "acme", Provision: "known", SameSubjectAs: "entra"}}
	teams.External.PublicKey = edJWK(t)
	teams.External.IssuedToken = unrestricted()
	if mutate != nil {
		mutate(&entra, &teams)
	}
	c.Auth.Gates["entra"] = entra
	c.Auth.Gates["teams"] = teams
	return c
}

func TestResolveAuth_SameSubjectAsLoadsForATrustedExternalGateNamingAnOIDGate(t *testing.T) {
	c := sameSubjectCfg(t, nil)
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	for _, g := range c.ResolvedAuth.Gates {
		if g.Name == "teams" && g.Identity.SameSubjectAs != "entra" {
			t.Fatalf("same_subject_as = %q", g.Identity.SameSubjectAs)
		}
	}
	// D3: it may sit beside legacy_subject_prefix.
	c = sameSubjectCfg(t, func(_, teams *AuthGateConfig) { teams.IdentityMap.LegacySubjectPrefix = "teams-" })
	if err := resolveAuth(c); err != nil {
		t.Fatalf("with legacy_subject_prefix: %v", err)
	}
}

func TestResolveAuth_SameSubjectAsRefusesAPairingThatCouldJoinTwoPeople(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(entra, teams *AuthGateConfig)
		want   string
	}{
		"untrusted gate": {func(_, teams *AuthGateConfig) { teams.Trusted = false }, "needs trusted: true"},
		"named gate keys on sub": {func(entra, _ *AuthGateConfig) { entra.IdentityMap.Subject = "" },
			"subject: oid"},
		"named gate admits several directories": {func(entra, _ *AuthGateConfig) { entra.AllowedTIDs = []string{entraTID1, entraTID2} },
			"exactly one"},
		"no such gate":   {func(_, teams *AuthGateConfig) { teams.IdentityMap.SameSubjectAs = "nope" }, `no gate named "nope"`},
		"names itself":   {func(_, teams *AuthGateConfig) { teams.IdentityMap.SameSubjectAs = "teams" }, "this gate itself"},
		"another tenant": {func(entra, _ *AuthGateConfig) { entra.IdentityMap.Tenant = "other" }, "same single tenant"},
		"bad name":       {func(_, teams *AuthGateConfig) { teams.IdentityMap.SameSubjectAs = "En Tra" }, "not a gate name"},
		"tenant by claim": {func(_, teams *AuthGateConfig) {
			teams.IdentityMap.Tenant = ""
			teams.IdentityMap.TenantClaim = "tid"
			teams.IdentityMap.AllowedTenants = map[string]string{"x": "acme"}
		}, "single tenant"},
		"on an oidc gate": {func(entra, _ *AuthGateConfig) { entra.IdentityMap.SameSubjectAs = "teams" }, "applies only to kind: external"},
	} {
		t.Run(name, func(t *testing.T) {
			err := resolveAuth(sameSubjectCfg(t, tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// RFC HD D1 (revised after review): unlinking on a revoke is the operator's
// opt-in per external gate, never the default.
func TestResolveAuth_OnRevokeUnlinkIsAnExternalGatesOptIn(t *testing.T) {
	c := sameSubjectCfg(t, nil)
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	for _, g := range c.ResolvedAuth.Gates {
		if g.Name == "teams" && g.External.UnlinkOnRevoke {
			t.Fatal("unlink on revoke is on by default")
		}
	}
	c = sameSubjectCfg(t, func(_, teams *AuthGateConfig) { teams.External.OnRevoke = "unlink" })
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	for _, g := range c.ResolvedAuth.Gates {
		if g.Name == "teams" && !g.External.UnlinkOnRevoke {
			t.Fatal("on_revoke: unlink did not load")
		}
	}
	if err := resolveAuth(sameSubjectCfg(t, func(_, teams *AuthGateConfig) { teams.External.OnRevoke = "forget" })); err == nil ||
		!strings.Contains(err.Error(), "on_revoke") {
		t.Fatalf("a bad on_revoke: %v", err)
	}
	if err := resolveAuth(sameSubjectCfg(t, func(entra, _ *AuthGateConfig) { entra.External.OnRevoke = "unlink" })); err == nil ||
		!strings.Contains(err.Error(), "on_revoke applies only to kind: external") {
		t.Fatalf("on_revoke on an oidc gate: %v", err)
	}
}
