package config

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
)

// PrincipalDef is one config-declared principal (RFC AO). `tenant` MAY be empty
// only for a substrate:admin principal (the shared/operator tenant — where an
// admin identity lives); every other principal must name one. `subject` is
// required (it is the authoritative user id, and the scope_id for user-scoped
// tools). `scopes` is the granted set, validated against the closed catalog;
// empty means a principal that can authenticate but is gated out of everything
// (default-deny — useful for a read-nothing identity, but normally you list
// scopes). `token_env` names a LOOMCYCLE_*-prefixed (or allowlisted) env var
// holding the bearer SECRET — the yaml carries the NAME, never the value.
//
// `agents`, when present, limits the principal to starting those agents as
// top-level runs and to the run endpoints (auth.Restrictions). A pointer so a
// key written with an empty list is refused at load and not read as "no
// limit".
type PrincipalDef struct {
	Tenant   string    `yaml:"tenant"`
	Subject  string    `yaml:"subject"`
	Scopes   []string  `yaml:"scopes"`
	TokenEnv string    `yaml:"token_env"`
	Agents   *[]string `yaml:"agents"`
	// Overrides locks the per-run overrides for runs the principal starts:
	// `none`, or the fields it may still set, comma-separated or as a list.
	Overrides *overridePolicyYAML `yaml:"overrides"`
	// Rate bounds how often the principal may start runs:
	// `rate: {per_minute: 6, burst: 20}`.
	Rate *RateLimitYAML `yaml:"rate"`
	// Retention: `none` makes every run the principal starts a no-trace run.
	Retention string `yaml:"retention"`
}

// RateLimitYAML is a principal's `rate:` block.
type RateLimitYAML struct {
	PerMinute int `yaml:"per_minute"`
	Burst     int `yaml:"burst"`
}

// overridePolicyYAML reads `overrides:` as the scalar `none`, a
// comma-separated scalar, or a list of field names.
type overridePolicyYAML struct{ auth.OverridePolicy }

func (o *overridePolicyYAML) UnmarshalYAML(unmarshal func(any) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		if len(list) == 0 {
			return fmt.Errorf(`overrides: an empty list is ambiguous; write "none" to lock every override`)
		}
		o.Allow = list
		return nil
	}
	var word string
	if err := unmarshal(&word); err != nil {
		return fmt.Errorf(`overrides: want "none" or a list of fields`)
	}
	p, err := auth.ParseOverridePolicy(word)
	if err != nil {
		return err
	}
	o.OverridePolicy = *p
	return nil
}

// resolvePrincipals validates the `principals:` block and builds
// c.ResolvedPrincipals (RFC AO). Called from validate after the env block is
// populated; it reads each token_env from the process environment directly
// (mirroring how Load reads every other secret).
//
//   - subject required; tenant required unless substrate:admin; token_env
//     required + LOOMCYCLE_*-allowlisted; scopes ∈
//     the closed catalog — a violation FAILS config load (a typo in a scope or
//     env name must not silently ship a mis-scoped or inert identity).
//   - a token_env that is EMPTY at boot → the principal is INERT (skipped, no
//     entry in the table) + a startup warning. Fail-safe: a missing secret means
//     "no token resolves to this identity", never an open door — the declaration
//     still turns authentication on (DeclaresPrincipals).
//   - tenant must be "" or one charset-clean path segment, not reserved, and not
//     differ only by case from another principal's tenant → else config-load
//     error (the tenant id names a directory under the dynamic volume root).
//   - two declared principals whose token_env values resolve to the SAME secret
//     → config-load error (a bearer must not map to two identities). Resolution
//     order (minted OperatorTokenDef → declared → legacy) handles a
//     declared-vs-minted value clash deterministically: the minted def wins.
func resolvePrincipals(c *Config) error {
	if len(c.Principals) == 0 {
		return nil
	}
	// Map iteration order is randomized; sort for deterministic warnings, a
	// stable resolved slice, and a deterministic collision-victim message.
	names := make([]string, 0, len(c.Principals))
	for name := range c.Principals {
		names = append(names, name)
	}
	sort.Strings(names)

	secretOwner := make(map[string]string, len(names)) // secret value → first declaring name
	tenantOwner := make(map[string]string, len(names)) // tenant id → first declaring name
	var tenants []string
	for _, name := range names {
		def := c.Principals[name]
		if def.Subject == "" {
			return fmt.Errorf("principals.%s: subject is required", name)
		}
		// The tenant becomes a path segment under the dynamic volume root, so it
		// gets the charset a minted token's tenant gets: "x/.." here would alias
		// tenant acme's volume tree. Checked for inert principals too — adding the
		// secret later must not be what turns a bad tenant live.
		if err := dynvol.ValidTenant(def.Tenant); err != nil {
			return fmt.Errorf("principals.%s: %s", name, err)
		}
		// "Acme" and "acme" are two tenants to loomcycle but ONE directory on a
		// case-insensitive filesystem, so they would share a volume tree.
		if other, clash := dynvol.CaseFoldCollision(def.Tenant, tenants); clash {
			return fmt.Errorf("principals.%s: tenant %q differs only by case from tenant %q (principals.%s) — they would share one volume directory on a case-insensitive filesystem; spell them identically or pick distinct names", name, def.Tenant, other, tenantOwner[other])
		}
		if _, seen := tenantOwner[def.Tenant]; !seen {
			tenantOwner[def.Tenant] = name
			tenants = append(tenants, def.Tenant)
		}
		if def.TokenEnv == "" {
			return fmt.Errorf("principals.%s: token_env is required", name)
		}
		if !ExpandEnvAllowed(def.TokenEnv) {
			return fmt.Errorf("principals.%s: token_env %q must be LOOMCYCLE_*-prefixed (or an allowlisted name)", name, def.TokenEnv)
		}
		// Never let token_env name one of loomcycle's OWN infrastructure secrets
		// (DB DSN, the legacy auth token, the token-hash pepper, the upstream MCP
		// bearer, OTEL headers): reusing an infra secret as a declared-principal
		// bearer is a misconfiguration, and the authoritative denylist already
		// guards these everywhere else (exp7 C2 / the v0.34.0 review S1).
		if expandDenyNames[def.TokenEnv] {
			return fmt.Errorf("principals.%s: token_env %q is one of loomcycle's own infrastructure secrets and cannot be reused as a principal bearer", name, def.TokenEnv)
		}
		// Deny ${...}-interpolation of this principal's bearer. It passed
		// ExpandEnvAllowed (LOOMCYCLE_*) and isn't a built-in infra secret, so
		// without this it stayed interpolatable — a runtime-authored MCPServerDef
		// header/url/env referencing ${LOOMCYCLE_ALICE_TOKEN} would exfiltrate
		// the bearer to the server host (the same S1 exfil class the denylist
		// closes for the built-in infra secrets, never extended to the newer
		// per-principal tokens). Denied even for an inert (empty-at-boot)
		// principal so the name is permanently non-interpolatable. A load has
		// already added it from the raw text (denyDeclaredPrincipalBearers),
		// before expansion; this covers a Config resolved without a load.
		principalBearerEnvs.add(def.TokenEnv)
		if bad := auth.UnknownScopes(def.Scopes); len(bad) > 0 {
			return fmt.Errorf("principals.%s: unknown scope(s) %v (not in the closed catalog)", name, bad)
		}
		if err := auth.ValidateScopeSet(def.Scopes); err != nil {
			return fmt.Errorf("principals.%s: %w", name, err)
		}
		// A tenant-confined principal must NAME its tenant. The store's list
		// queries (ListUsers, UserList, the run and audit lists) read tenant ""
		// as EVERY tenant — the admin all-tenants view — so a non-admin whose
		// tenant is "" is served every tenant's rows by any handler that passes
		// its tenant straight through, GET /v1/_users among them. A minted token
		// cannot carry an empty tenant (the store refuses the row) and the legacy
		// bearer is tenant "default", so this block is the one place such a
		// principal could come from. An admin's "" is its all-tenants view anyway.
		if def.Tenant == "" && !auth.HasScope(def.Scopes, auth.ScopeAdmin) {
			return fmt.Errorf("principals.%s: tenant is required unless the principal is %s — an empty tenant reads as every tenant in the store's list queries", name, auth.ScopeAdmin)
		}
		var restrictions auth.Restrictions
		if def.Agents != nil {
			restrictions.Agents = append([]string(nil), (*def.Agents)...)
		}
		if def.Overrides != nil {
			restrictions.Overrides = &auth.OverridePolicy{Allow: append([]string(nil), def.Overrides.Allow...)}
		}
		if def.Rate != nil {
			restrictions.Rate = &auth.RateLimit{PerMinute: def.Rate.PerMinute, Burst: def.Rate.Burst}
		}
		retention, rerr := auth.ParseRetention(def.Retention)
		if rerr != nil {
			return fmt.Errorf("principals.%s: %w", name, rerr)
		}
		restrictions.Retention = retention
		if err := restrictions.Validate(def.Agents != nil); err != nil {
			return fmt.Errorf("principals.%s: %w", name, err)
		}
		if restrictions.RunPlaneOnly() && auth.HasScope(def.Scopes, auth.ScopeAdmin) {
			return fmt.Errorf("principals.%s: agents is not allowed on a principal holding %s", name, auth.ScopeAdmin)
		}
		secret := os.Getenv(def.TokenEnv)
		if secret == "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf("principals.%s: token_env %s is empty — principal is inert (no token resolves to it; declaring it still turns authentication on)", name, def.TokenEnv))
			continue
		}
		if prev, dup := secretOwner[secret]; dup {
			return fmt.Errorf("principals.%s: token_env %s resolves to the same secret as principals.%s — a bearer cannot map to two identities", name, def.TokenEnv, prev)
		}
		secretOwner[secret] = name
		c.ResolvedPrincipals = append(c.ResolvedPrincipals, auth.DeclaredPrincipal{
			Secret: secret,
			Principal: auth.Principal{
				TenantID:     def.Tenant,
				Subject:      def.Subject,
				Scopes:       append([]string(nil), def.Scopes...),
				TokenDefID:   "cfg:" + name,
				Restrictions: restrictions,
			},
		})
	}
	return nil
}

// DeclaresPrincipals reports whether the config declares any principal, inert
// ones included. Declaring one turns authentication on (RFC ES F2): an inert
// principal (its token_env empty at boot) authenticates nobody, but it says the
// operator meant the server to require a bearer, so a missing env var must not
// read as "serve everyone". ResolvedPrincipals is consulted too because it is
// the table the resolver matches against, and a caller may build it directly.
func (c *Config) DeclaresPrincipals() bool {
	return len(c.Principals) > 0 || len(c.ResolvedPrincipals) > 0
}

// AdminPrincipalResolved reports whether a declared principal that can log in
// (its secret set at boot) holds substrate:admin.
func (c *Config) AdminPrincipalResolved() bool {
	for _, dp := range c.ResolvedPrincipals {
		if dp.Secret != "" && auth.HasScope(dp.Principal.Scopes, auth.ScopeAdmin) {
			return true
		}
	}
	return false
}

// denyDeclaredPrincipalBearers adds every principals.*.token_env named in the
// layers' RAW text to principalBearerEnvs. It must run before any layer is
// ${...}-expanded: expansion happens on the text, long before validate reaches
// resolvePrincipals, so without it the first load in a process interpolated a
// principal's bearer into, say, a static MCP header, and checks that refuse a
// bearer as a credential source passed at boot and failed on reload. All layers
// are scanned first, because a principal declared in a later layer must protect
// its bearer from an earlier layer's text.
//
// Best effort by design: a layer that cannot be read or parsed is skipped (the
// load itself reports it), and a name stays denied even when the load fails —
// the set only ever widens.
func denyDeclaredPrincipalBearers(layers []Layer) {
	for _, l := range layers {
		raw := l.Data
		if raw == nil {
			b, err := os.ReadFile(l.Name)
			if err != nil {
				continue
			}
			raw = b
		}
		var doc struct {
			Principals map[string]struct {
				TokenEnv string `yaml:"token_env"`
			} `yaml:"principals"`
			// An external gate's HMAC secret (RFC FA P4) is a credential
			// that verifies identities: denied from expansion the same way.
			Auth struct {
				Gates map[string]struct {
					HMACSecretEnv string `yaml:"hmac_secret_env"`
				} `yaml:"gates"`
			} `yaml:"auth"`
		}
		// A partial decode still yields the names it reached.
		_ = yaml.Unmarshal(raw, &doc)
		for _, p := range doc.Principals {
			if p.TokenEnv != "" {
				principalBearerEnvs.add(p.TokenEnv)
			}
		}
		for _, g := range doc.Auth.Gates {
			if g.HMACSecretEnv != "" {
				principalBearerEnvs.add(g.HMACSecretEnv)
			}
		}
	}
}

// PrincipalTenants returns the distinct tenant ids the `principals:` block
// declares, inert principals included, sorted. Token minting checks a new
// tenant against these so a minted "Acme" cannot join a declared "acme".
func (c *Config) PrincipalTenants() []string {
	seen := make(map[string]bool, len(c.Principals))
	var out []string
	for _, def := range c.Principals {
		if !seen[def.Tenant] {
			seen[def.Tenant] = true
			out = append(out, def.Tenant)
		}
	}
	sort.Strings(out)
	return out
}
