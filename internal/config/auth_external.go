package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/auth/gatekey"
)

// RFC FA §3.5: an external gate — a separate process (a bridge, a website, a
// broker in front of an identity provider) that authenticates people however
// it likes and tells loomcycle who they are in a JWT signed under the one key
// the operator pins here.
//
//	auth:
//	  gates:
//	    telegram:
//	      kind: external
//	      public_key: { kty: OKP, crv: Ed25519, x: "…" }   # or hmac_secret_env: LOOMCYCLE_GATE_TG_SECRET
//	      callback_url: https://gate.internal/login         # optional: a browser login starts there
//	      allowed_return_to: [https://www.example.com/app]  # where a browser login may land
//	      issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 }, max_active: 2 }
//	      on_removed: { unbind_after: 72h }                 # overrides auth.lifecycle.on_removed
//	      identity_map: { tenant: acme, provision: auto }
//
// Registration is config, not a runtime-mutable definition (RFC FA §3.5 says
// "config or /v1/_authgate"; this release takes config only): a gate's key is
// a trust anchor, and a trust anchor an API call could add is one a stolen
// admin token could add too. GET /v1/_authgate lists and probes them.

// AuthGateKindExternal is an external gate (RFC FA §3.5).
const AuthGateKindExternal = "external"

// AuthExternalGateConfig is the external gate's own keys of a gate entry.
type AuthExternalGateConfig struct {
	PublicKey map[string]any `yaml:"public_key,omitempty"`
	// HMACSecretEnv names the env var (LOOMCYCLE_*) holding a co-located
	// gate's HS256 secret. HMACSecret exists only to refuse a secret written
	// in yaml, literal or $cred: (see resolveExternalGate).
	HMACSecretEnv string `yaml:"hmac_secret_env,omitempty"`
	HMACSecret    string `yaml:"hmac_secret,omitempty"`
	CallbackURL   string `yaml:"callback_url,omitempty"`
	// ProbeURL is where the gate serves /.well-known/loomcycle-authgate;
	// default: callback_url's origin.
	ProbeURL string `yaml:"probe_url,omitempty"`
	// MaxSyncRemovals caps the users one complete sync may remove: a count
	// ("25") or a share of the gate's linked users ("20%"). Default: 10%,
	// never fewer than five.
	MaxSyncRemovals string                 `yaml:"max_sync_removals,omitempty"`
	IssuedToken     *AuthIssuedTokenConfig `yaml:"issued_token,omitempty"`
	OnRemoved       *AuthOnRemovedConfig   `yaml:"on_removed,omitempty"`
}

// AuthIssuedTokenConfig is the ceiling of the tokens a gate gets issued.
type AuthIssuedTokenConfig struct {
	TTL       string   `yaml:"ttl,omitempty"`
	Agents    []string `yaml:"agents,omitempty"`
	Overrides []string `yaml:"overrides,omitempty"`
	Rate      *struct {
		PerMinute int `yaml:"per_minute"`
		Burst     int `yaml:"burst,omitempty"`
	} `yaml:"rate,omitempty"`
	MaxActive int `yaml:"max_active,omitempty"`
}

// AuthExternalGate is a validated external gate.
type AuthExternalGate struct {
	// Key is the pinned verification key: a public key from this config,
	// or an HS256 secret from the env var HMACSecretEnv names. Both are
	// operator-controlled; nothing a tenant can write verifies an identity.
	Key             gatekey.Key
	HMACSecretEnv   string
	CallbackURL     string
	DiscoveryURL    string
	AllowedReturnTo []string
	// TokenTTL is the issued tokens' lifetime ceiling; TokenRestrictions
	// what every issued token is held to (an assertion may only narrow it);
	// MaxActive the live issued tokens per user (0: the mint path's default).
	TokenTTL          time.Duration
	TokenRestrictions auth.Restrictions
	MaxActive         int
	// MaxSyncRemovals / MaxSyncRemovalsPercent: the complete-sync removal
	// ceiling, a count or a percentage of the gate's linked users (one is
	// set; neither is the default, SyncRemovalCeiling).
	MaxSyncRemovals        int
	MaxSyncRemovalsPercent int
	// Removal is the gate's removal policy: the deployment's, with the
	// gate's on_removed over it.
	Removal AuthRemovalPolicy
}

// Default complete-sync removal ceiling: 10% of the gate's linked users,
// and never fewer than five, so a small deployment can still offboard a few
// people in one sync.
const (
	defaultSyncRemovalPercent = 10
	minSyncRemovalCeiling     = 5
)

// SyncRemovalCeiling is how many users one complete sync may remove, of
// linked users linked to the gate. A sync over it is refused whole: a gate
// that is compromised, or whose platform API returned only a first page,
// must not disable everyone in one request.
func (g *AuthExternalGate) SyncRemovalCeiling(linked int) int {
	return syncRemovalCeiling(g.MaxSyncRemovals, g.MaxSyncRemovalsPercent, linked)
}

// Interactive reports whether a browser can sign in through the gate.
func (g *AuthExternalGate) Interactive() bool { return g != nil && g.CallbackURL != "" }

const maxIssuedTokenTTL = 30 * 24 * time.Hour

func hasExternalKeys(g AuthGateConfig) bool {
	e := g.External
	return e.PublicKey != nil || e.HMACSecret != "" || e.HMACSecretEnv != "" || e.CallbackURL != "" || e.ProbeURL != "" ||
		e.IssuedToken != nil || e.OnRemoved != nil
}

func gateURL(where, v string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	loopback := err == nil && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
	if err != nil || u.Host == "" || (u.Scheme != "https" && !loopback) || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%s: %q must be an absolute https URL (http only on loopback), with no user info or fragment", where, v)
	}
	return strings.TrimSpace(v), nil
}

// resolveExternalGate validates one kind: external gate. It needs
// LOOMCYCLE_PUBLIC_URL: every assertion's aud must be this deployment's
// issuer URL, and a browser login returns to it.
func resolveExternalGate(c *Config, name string, g AuthGateConfig, deployment AuthRemovalPolicy) (*AuthExternalGate, error) {
	where := "auth.gates." + name
	if strings.TrimSpace(c.Env.PublicURL) == "" {
		return nil, fmt.Errorf("%s: an external gate needs LOOMCYCLE_PUBLIC_URL: its assertions are addressed (aud) to it", where)
	}
	e := g.External
	out := &AuthExternalGate{Removal: deployment}
	if e.HMACSecret != "" {
		// A verification secret must come from the operator alone: written
		// in yaml it sits in every copy of the config, and as a $cred:
		// reference it would live in the credential store, where a tenant
		// that can write credentials could set it and forge any identity.
		return nil, fmt.Errorf("%s.hmac_secret: not accepted (neither a literal nor a $cred: reference); "+
			"put the secret in an env var and name it with hmac_secret_env", where)
	}
	switch {
	case e.PublicKey != nil && e.HMACSecretEnv != "":
		return nil, fmt.Errorf("%s: set public_key or hmac_secret_env, not both", where)
	case e.PublicKey != nil:
		raw, err := json.Marshal(e.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("%s.public_key: %w", where, err)
		}
		if out.Key, err = gatekey.ParsePublicJWK(raw); err != nil {
			return nil, fmt.Errorf("%s.public_key: %w", where, err)
		}
	case e.HMACSecretEnv != "":
		env := strings.TrimSpace(e.HMACSecretEnv)
		if !ExpandEnvAllowed(env) || expandDenyNames[env] {
			return nil, fmt.Errorf("%s.hmac_secret_env: %q must be a LOOMCYCLE_*-prefixed env var that is not one of loomcycle's own secrets", where, env)
		}
		// Never interpolated into a definition (the same rule as a
		// principal's token_env): ${...} must not carry it to a server.
		principalBearerEnvs.add(env)
		secret := os.Getenv(env)
		if secret == "" {
			return nil, fmt.Errorf("%s.hmac_secret_env: %s is empty", where, env)
		}
		var err error
		if out.Key, err = gatekey.HMACKey([]byte(secret)); err != nil {
			return nil, fmt.Errorf("%s.hmac_secret_env: %w", where, err)
		}
		out.HMACSecretEnv = env
	default:
		return nil, fmt.Errorf("%s: public_key (an Ed25519 or P-256 public JWK) or hmac_secret_env is required", where)
	}
	var err error
	if out.MaxSyncRemovals, out.MaxSyncRemovalsPercent, err = parseMaxSyncRemovals(where, e.MaxSyncRemovals); err != nil {
		return nil, err
	}
	if e.CallbackURL != "" {
		if out.CallbackURL, err = gateURL(where+".callback_url", e.CallbackURL); err != nil {
			return nil, err
		}
	}
	if e.ProbeURL != "" {
		if out.DiscoveryURL, err = gateURL(where+".probe_url", e.ProbeURL); err != nil {
			return nil, err
		}
	} else if out.CallbackURL != "" {
		u, _ := url.Parse(out.CallbackURL)
		out.DiscoveryURL = u.Scheme + "://" + u.Host
	}
	for _, rt := range g.AllowedReturnTo {
		v, err := gateURL(where+".allowed_return_to", rt)
		if err != nil {
			return nil, err
		}
		out.AllowedReturnTo = append(out.AllowedReturnTo, v)
	}
	if len(out.AllowedReturnTo) > 0 && out.CallbackURL == "" {
		return nil, fmt.Errorf("%s.allowed_return_to: applies only with callback_url (a browser login)", where)
	}
	out.TokenTTL = DefaultNewUserTokenTTL
	if t := e.IssuedToken; t != nil {
		if t.TTL != "" {
			if out.TokenTTL, err = parseLifecycleAge(where+".issued_token.ttl", t.TTL, false); err != nil {
				return nil, err
			}
			if out.TokenTTL < time.Minute || out.TokenTTL > maxIssuedTokenTTL {
				return nil, fmt.Errorf("%s.issued_token.ttl: between 1m and %s", where, maxIssuedTokenTTL)
			}
		}
		if t.Agents != nil {
			out.TokenRestrictions.Agents = append([]string(nil), t.Agents...)
		}
		if t.Overrides != nil {
			out.TokenRestrictions.Overrides = &auth.OverridePolicy{Allow: append([]string(nil), t.Overrides...)}
		}
		if t.Rate != nil {
			out.TokenRestrictions.Rate = &auth.RateLimit{PerMinute: t.Rate.PerMinute, Burst: t.Rate.Burst}
		}
		if err := out.TokenRestrictions.Validate(t.Agents != nil); err != nil {
			return nil, fmt.Errorf("%s.issued_token: %w", where, err)
		}
		if t.MaxActive < 0 || t.MaxActive > 100 {
			return nil, fmt.Errorf("%s.issued_token.max_active: between 0 and 100", where)
		}
		out.MaxActive = t.MaxActive
	}
	if out.Removal, err = gateRemovalPolicy(c, where, e.OnRemoved, deployment); err != nil {
		return nil, err
	}
	return out, nil
}

// parseMaxSyncRemovals reads a removal ceiling: a count ("25") or a share of
// the gate's linked users ("20%"); "" is neither (the default).
func parseMaxSyncRemovals(where, v string) (count, percent int, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, nil
	}
	n, pct := strings.CutSuffix(v, "%")
	k, err := strconv.Atoi(strings.TrimSpace(n))
	switch {
	case err != nil || k < 1:
		return 0, 0, fmt.Errorf("%s.max_sync_removals: %q must be a positive count (25) or percentage (20%%)", where, v)
	case pct && k > 100:
		return 0, 0, fmt.Errorf("%s.max_sync_removals: %q is over 100%%", where, v)
	case pct:
		return 0, k, nil
	}
	return k, 0, nil
}

// syncRemovalCeiling is the removal ceiling of count or percent (one set, or
// neither for the default) over linked users.
func syncRemovalCeiling(count, percent, linked int) int {
	switch {
	case count > 0:
		return count
	case percent > 0:
		return linked * percent / 100
	}
	return max(minSyncRemovalCeiling, linked*defaultSyncRemovalPercent/100)
}

// gateRemovalPolicy is a gate's removal policy: the deployment's, with the
// gate's on_removed laid over it key by key.
func gateRemovalPolicy(c *Config, where string, o *AuthOnRemovedConfig, deployment AuthRemovalPolicy) (AuthRemovalPolicy, error) {
	if o == nil {
		return deployment, nil
	}
	sub := *c
	sub.Auth.Lifecycle.OnRemoved = mergeOnRemoved(c.Auth.Lifecycle.OnRemoved, *o)
	r, err := resolveRemoval(&sub)
	if err != nil {
		return AuthRemovalPolicy{}, fmt.Errorf("%s.on_removed: %w", where, err)
	}
	r.DormantAfter = deployment.DormantAfter
	return r, nil
}

// mergeOnRemoved lays a gate's on_removed over the deployment's, key by key.
func mergeOnRemoved(base, over AuthOnRemovedConfig) AuthOnRemovedConfig {
	if over.Disable != "" {
		base.Disable = over.Disable
	}
	if over.UnbindAfter != "" {
		base.UnbindAfter = over.UnbindAfter
	}
	if over.EraseAfter != "" {
		base.EraseAfter = over.EraseAfter
	}
	if over.Reactivate != "" {
		base.Reactivate = over.Reactivate
	}
	return base
}

// checkExternalGateKeysExclusive refuses an external gate key that is not that
// gate's alone (RFC FA §8.1: one gate's key never verifies another's). A gate
// is named in its assertions' iss, which the key holder writes, so two gates
// on one key — the same public key, or the same HMAC secret under one env var
// or two — would let either gate sign as the other: an untrusted bridge as a
// trusted, authoritative gate. And an HMAC secret env var that also holds a
// principal's bearer or an identity provider's client secret would let
// whoever holds that credential forge the gate's assertions.
func checkExternalGateKeysExclusive(c *Config, gates []AuthGate) error {
	other := map[string]string{}
	for name, pd := range c.Principals {
		if env := strings.TrimSpace(pd.TokenEnv); env != "" {
			other[env] = "principals." + name + ".token_env"
		}
	}
	for name, g := range c.Auth.Gates {
		if env := strings.TrimSpace(g.ClientSecretEnv); env != "" {
			other[env] = "auth.gates." + name + ".client_secret_env"
		}
	}
	var ext []AuthGate
	for _, g := range gates {
		if g.External == nil {
			continue
		}
		if env := g.External.HMACSecretEnv; env != "" {
			if what, ok := other[env]; ok {
				return fmt.Errorf("auth.gates.%s.hmac_secret_env: %s already holds another credential (%s); "+
					"give the gate a secret of its own", g.Name, env, what)
			}
		}
		for _, prev := range ext {
			if prev.External.Key.Same(g.External.Key) {
				return fmt.Errorf("auth.gates.%s and auth.gates.%s have the same key: either could sign as the other; "+
					"give each gate a key of its own", prev.Name, g.Name)
			}
		}
		ext = append(ext, g)
	}
	return nil
}
