package config

import (
	"fmt"
	"strings"
	"time"
)

// RFC FA P6: inbound SCIM 2.0 as an external-gate-compatible sync. A kind:
// scim gate is the deployment's SCIM service: an identity provider holding a
// scim:provision token provisions users and groups into that token's tenant
// through it, by the same identity code an external gate's sync uses — the
// identity map's new_user floor and provisioning, the trusted rule for
// access_mode and groups, group sync into "<gate>:" groups (added AND
// removed), and the removal policy for a deprovisioned user.
//
//	auth:
//	  gates:
//	    okta:
//	      kind: scim
//	      trusted: true                     # may assert accessMode and sync groups
//	      identity_map:
//	        tenant: acme                    # optional: serve only this tenant
//	        groups: { provision: true, map: { engineering: eng } }
//	        new_user: { access_mode: tenant, groups: [staff] }
//	        authoritative_for_users: true   # DELETE / active:false remove users
//	      max_sync_removals: "10%"          # removals per rolling 24 h
//	      on_removed: { unbind_after: 168h }
//
// The tenant is the token's: an identity provider never names one. A fixed
// identity_map.tenant narrows the service to that tenant. Users are always
// provisioned (provision: auto) — SCIM's POST /Users is a provisioning
// request — and keyed on the server's SCIM id ("<gate>:<id>"), never on a
// userName, e-mail or externalId the provider may rename.

// AuthGateKindSCIM is the inbound SCIM 2.0 service.
const AuthGateKindSCIM = "scim"

// AuthSCIMGate is a validated SCIM gate.
type AuthSCIMGate struct {
	// MaxSyncRemovals / MaxSyncRemovalsPercent cap the removals (DELETE,
	// active:false) the gate may start in a tenant per RemovalWindow: a
	// count, or a share of the gate's linked users in the tenant.
	MaxSyncRemovals        int
	MaxSyncRemovalsPercent int
	// Removal is the gate's removal policy (the deployment's, with the
	// gate's on_removed over it).
	Removal AuthRemovalPolicy
}

// SCIMRemovalWindow is the window the removal ceiling counts over.
const SCIMRemovalWindow = 24 * time.Hour

// RemovalCeiling is how many removals the gate may start in a tenant within
// SCIMRemovalWindow, of linked users linked to it there.
func (g *AuthSCIMGate) RemovalCeiling(linked int) int {
	return syncRemovalCeiling(g.MaxSyncRemovals, g.MaxSyncRemovalsPercent, linked)
}

// scimTokenTenant stands in for the tenant while a SCIM identity map is
// validated: the real one is the token's, set per request.
const scimTokenTenant = "scim-token-tenant"

// resolveSCIMGate validates one kind: scim gate and its identity map.
func resolveSCIMGate(c *Config, name string, g AuthGateConfig, floor AuthNewUser, deployment AuthRemovalPolicy) (*AuthSCIMGate, *AuthIdentityMap, error) {
	where := "auth.gates." + name
	e := g.External
	if e.PublicKey != nil || e.HMACSecret != "" || e.HMACSecretEnv != "" || e.CallbackURL != "" || e.ProbeURL != "" || e.IssuedToken != nil {
		return nil, nil, fmt.Errorf("%s: a SCIM gate is authenticated by a scim:provision token; "+
			"public_key, hmac_secret_env, callback_url, probe_url and issued_token do not apply", where)
	}
	m := AuthIdentityMapConfig{}
	if g.IdentityMap != nil {
		m = *g.IdentityMap
	}
	if m.TenantClaim != "" || len(m.AllowedTenants) > 0 || m.Subject != "" || m.LegacySubjectPrefix != "" {
		return nil, nil, fmt.Errorf("%s.identity_map: a SCIM gate's tenant is its token's and its subject is the SCIM id; "+
			"tenant_claim, allowed_tenants, subject and legacy_subject_prefix do not apply", where)
	}
	switch p := strings.TrimSpace(m.Provision); p {
	case "", ProvisionAuto:
		m.Provision = ProvisionAuto
	default:
		return nil, nil, fmt.Errorf("%s.identity_map.provision: a SCIM gate provisions (auto); %q does not apply", where, p)
	}
	fixed := strings.TrimSpace(m.Tenant)
	if fixed == "" {
		m.Tenant = scimTokenTenant
	}
	idm, err := resolveIdentityMap(name, &m, floor, g.Trusted)
	if err != nil {
		return nil, nil, err
	}
	if fixed == "" {
		idm.Tenant = ""
	}
	out := &AuthSCIMGate{}
	if out.MaxSyncRemovals, out.MaxSyncRemovalsPercent, err = parseMaxSyncRemovals(where, e.MaxSyncRemovals); err != nil {
		return nil, nil, err
	}
	if out.Removal, err = gateRemovalPolicy(c, where, e.OnRemoved, deployment); err != nil {
		return nil, nil, err
	}
	return out, idm, nil
}

// SCIMGate is the configured SCIM gate, if any.
func (p AuthPolicy) SCIMGate() (AuthGate, bool) {
	for _, g := range p.Gates {
		if g.Kind == AuthGateKindSCIM {
			return g, true
		}
	}
	return AuthGate{}, false
}
