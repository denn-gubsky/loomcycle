package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/external"
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
	ProbeURL    string                 `yaml:"probe_url,omitempty"`
	IssuedToken *AuthIssuedTokenConfig `yaml:"issued_token,omitempty"`
	OnRemoved   *AuthOnRemovedConfig   `yaml:"on_removed,omitempty"`
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
	Key             external.Key
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
	// Removal is the gate's removal policy: the deployment's, with the
	// gate's on_removed over it.
	Removal AuthRemovalPolicy
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
		if out.Key, err = external.ParsePublicJWK(raw); err != nil {
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
		if out.Key, err = external.HMACKey([]byte(secret)); err != nil {
			return nil, fmt.Errorf("%s.hmac_secret_env: %w", where, err)
		}
		out.HMACSecretEnv = env
	default:
		return nil, fmt.Errorf("%s: public_key (an Ed25519 or P-256 public JWK) or hmac_secret_env is required", where)
	}
	var err error
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
	if o := e.OnRemoved; o != nil {
		sub := *c
		sub.Auth.Lifecycle.OnRemoved = mergeOnRemoved(c.Auth.Lifecycle.OnRemoved, *o)
		r, err := resolveRemoval(&sub)
		if err != nil {
			return nil, fmt.Errorf("%s.on_removed: %w", where, err)
		}
		r.DormantAfter = deployment.DormantAfter
		out.Removal = r
	}
	return out, nil
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
