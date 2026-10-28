package config

import (
	"fmt"
	"strings"
)

// RFC FA §4: what strict mode refuses beyond its defaults.
//
//	auth:
//	  mode: strict
//	  strict:
//	    user_tokens: gate_only   # any (default) | gate_only
//
// Strict mode always refuses a hand-minted per-user token as a Web UI sign-in.
// gate_only refuses it on every API transport too, and at mint, so a person's
// API credential is always one a gate issued after they signed in (a device
// login, an app sign-in, an external gate's assertion).

// Values of auth.strict.user_tokens.
const (
	UserTokensAny      = "any"
	UserTokensGateOnly = "gate_only"
)

// AuthStrictConfig is the yaml `auth.strict:` block.
type AuthStrictConfig struct {
	// UserTokens: "any" (default) keeps a hand-minted per-user token working
	// on the API in strict mode; "gate_only" refuses it there too. Operator,
	// service and declared tokens are machine credentials and never affected.
	UserTokens string `yaml:"user_tokens,omitempty"`
}

// resolveStrict validates auth.strict into p. gate_only means nothing outside
// strict mode, so it is refused there rather than loaded as a protection the
// deployment does not get.
func resolveStrict(c *Config, p *AuthPolicy) error {
	switch v := strings.TrimSpace(c.Auth.Strict.UserTokens); v {
	case "", UserTokensAny:
	case UserTokensGateOnly:
		if !p.Strict() {
			return fmt.Errorf("auth.strict.user_tokens: gate_only applies only in auth.mode: strict")
		}
		p.UserTokensGateOnly = true
	default:
		return fmt.Errorf("auth.strict.user_tokens: unknown value %q (want any or gate_only)", v)
	}
	return nil
}
