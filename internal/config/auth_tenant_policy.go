package config

import (
	"fmt"
	"sort"
	"strings"
)

// RFC FA §4 / P5: the per-tenant gate policy.
//
//	auth:
//	  tenant_policies:
//	    acme: { gates: [google] }       # acme's people sign in only through google
//
// It is enforced when a Web UI session is established and when a gate issues
// a token; an admin can narrow it per tenant at run time
// (PUT /v1/_auth/tenant-policies/{tenant}), never widen it.

// AuthTenantPolicyConfig is one entry of auth.tenant_policies.
type AuthTenantPolicyConfig struct {
	Gates []string `yaml:"gates"`
}

// resolveTenantPolicies validates auth.tenant_policies against the configured
// gates. The shared "" tenant cannot have one: its people are the operator's.
func resolveTenantPolicies(c *Config, gates []AuthGate) (map[string][]string, error) {
	if len(c.Auth.TenantPolicies) == 0 {
		return nil, nil
	}
	kinds := map[string]string{}
	for _, g := range gates {
		kinds[g.Name] = g.Kind
	}
	out := make(map[string][]string, len(c.Auth.TenantPolicies))
	for tenant, pol := range c.Auth.TenantPolicies {
		where := "auth.tenant_policies." + tenant
		if strings.TrimSpace(tenant) == "" || strings.TrimSpace(tenant) != tenant {
			return nil, fmt.Errorf("auth.tenant_policies: %q is not a tenant id", tenant)
		}
		if len(pol.Gates) == 0 {
			return nil, fmt.Errorf("%s.gates: name at least one gate (an empty list would lock the tenant out)", where)
		}
		seen := map[string]bool{}
		for _, name := range pol.Gates {
			if !TenantPolicyGateKind(kinds[name]) {
				return nil, fmt.Errorf("%s.gates: %q is not a sign-in gate in auth.gates", where, name)
			}
			seen[name] = true
		}
		list := make([]string, 0, len(seen))
		for name := range seen {
			list = append(list, name)
		}
		sort.Strings(list)
		out[tenant] = list
	}
	return out, nil
}

// TenantPolicyGateKind reports whether a tenant policy may name a gate of this
// kind ("" = no such gate). Not a device gate: a device login is approved
// through another gate, and that gate is what the policy judges. Not a SCIM
// gate either: it provisions, it signs nobody in.
func TenantPolicyGateKind(kind string) bool {
	return kind != "" && kind != AuthGateKindDevice && kind != AuthGateKindSCIM
}

// TenantGates is the config's gate policy for tenant: the gates its people
// must come through, and whether the config sets one.
func (p AuthPolicy) TenantGates(tenant string) ([]string, bool) {
	g, ok := p.TenantPolicies[tenant]
	return g, ok
}
