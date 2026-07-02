package config

import (
	"fmt"
	"os"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// PrincipalDef is one config-declared principal (RFC AO). `tenant` MAY be empty
// (the shared/operator tenant — where an admin identity lives); `subject` is
// required (it is the authoritative user id, and the scope_id for user-scoped
// tools). `scopes` is the granted set, validated against the closed catalog;
// empty means a principal that can authenticate but is gated out of everything
// (default-deny — useful for a read-nothing identity, but normally you list
// scopes). `token_env` names a LOOMCYCLE_*-prefixed (or allowlisted) env var
// holding the bearer SECRET — the yaml carries the NAME, never the value.
type PrincipalDef struct {
	Tenant   string   `yaml:"tenant"`
	Subject  string   `yaml:"subject"`
	Scopes   []string `yaml:"scopes"`
	TokenEnv string   `yaml:"token_env"`
}

// resolvePrincipals validates the `principals:` block and builds
// c.ResolvedPrincipals (RFC AO). Called from validate after the env block is
// populated; it reads each token_env from the process environment directly
// (mirroring how Load reads every other secret).
//
//   - subject required; token_env required + LOOMCYCLE_*-allowlisted; scopes ∈
//     the closed catalog — a violation FAILS config load (a typo in a scope or
//     env name must not silently ship a mis-scoped or inert identity).
//   - a token_env that is EMPTY at boot → the principal is INERT (skipped, no
//     entry in the table) + a startup warning. Fail-safe: a missing secret means
//     "no token resolves to this identity", never an open door.
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
	for _, name := range names {
		def := c.Principals[name]
		if def.Subject == "" {
			return fmt.Errorf("principals.%s: subject is required", name)
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
		// per-principal tokens). Written once at config-load, before any runtime
		// ExpandEnv reader (the MCPServerDef path); denied even for an inert
		// (empty-at-boot) principal so the name is permanently non-interpolatable.
		expandDenyNames[def.TokenEnv] = true
		if bad := auth.UnknownScopes(def.Scopes); len(bad) > 0 {
			return fmt.Errorf("principals.%s: unknown scope(s) %v (not in the closed catalog)", name, bad)
		}
		secret := os.Getenv(def.TokenEnv)
		if secret == "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf("principals.%s: token_env %s is empty — principal is inert (no token resolves to it)", name, def.TokenEnv))
			continue
		}
		if prev, dup := secretOwner[secret]; dup {
			return fmt.Errorf("principals.%s: token_env %s resolves to the same secret as principals.%s — a bearer cannot map to two identities", name, def.TokenEnv, prev)
		}
		secretOwner[secret] = name
		c.ResolvedPrincipals = append(c.ResolvedPrincipals, auth.DeclaredPrincipal{
			Secret: secret,
			Principal: auth.Principal{
				TenantID:   def.Tenant,
				Subject:    def.Subject,
				Scopes:     append([]string(nil), def.Scopes...),
				TokenDefID: "cfg:" + name,
			},
		})
	}
	return nil
}
