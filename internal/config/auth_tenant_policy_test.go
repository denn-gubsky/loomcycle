package config

import (
	"reflect"
	"strings"
	"testing"
)

func tenantPolicyConfig(pols map[string]AuthTenantPolicyConfig) *Config {
	c := passwordGateConfig("", AuthGateConfig{})
	c.Auth.Gates["paste"] = AuthGateConfig{Kind: AuthGateKindToken}
	c.Auth.TenantPolicies = pols
	return c
}

func TestResolveAuth_TenantPolicyIsSortedAndDeduplicated(t *testing.T) {
	c := tenantPolicyConfig(map[string]AuthTenantPolicyConfig{"acme": {Gates: []string{"paste", "local", "paste"}}})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.ResolvedAuth.TenantGates("acme"); !ok || !reflect.DeepEqual(got, []string{"local", "paste"}) {
		t.Fatalf("acme = %v %v", got, ok)
	}
	if _, ok := c.ResolvedAuth.TenantGates("beta"); ok {
		t.Fatal("a tenant with no policy reports one")
	}
}

// A policy that names no gate, an unknown gate, or the shared tenant would
// lock people out or read as a restriction nothing applies; each fails the load.
func TestResolveAuth_TenantPolicyRefusesWhatCannotBeEnforced(t *testing.T) {
	for name, pols := range map[string]map[string]AuthTenantPolicyConfig{
		"empty list":    {"acme": {Gates: nil}},
		"unknown gate":  {"acme": {Gates: []string{"google"}}},
		"shared tenant": {"": {Gates: []string{"local"}}},
		"padded tenant": {" acme": {Gates: []string{"local"}}},
	} {
		err := resolveAuth(tenantPolicyConfig(pols))
		if err == nil || !strings.Contains(err.Error(), "tenant_policies") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
