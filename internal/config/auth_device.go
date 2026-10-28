package config

import (
	"fmt"
	"strings"
	"time"
)

// RFC FA P5: the device login gate (RFC 8628).
//
//	auth:
//	  gates:
//	    cli:
//	      kind: device                  # the CLI and the MCP thin client sign in by a code
//	      token_ttl: 12h                # issued tokens; also capped by the approving gate and
//	                                    # LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS
//	      rate: { per_minute: 30, burst: 10 }
//
// A device gate is not a way in by itself: the person approves the code in the
// Web UI, signed in through an interactive gate, and the issued token is
// theirs, tagged with THAT gate. So it is never on the login page, never counts
// as the interactive gate strict mode needs, takes no identity_map, and is not
// something a tenant policy can name (the policy judges the approving gate).

// AuthGateKindDevice is the device login gate (RFC FA §3.2, RFC 8628).
const AuthGateKindDevice = "device"

// DefaultDeviceTokenTTL is a device-issued token's lifetime before the caps.
const DefaultDeviceTokenTTL = 12 * time.Hour

// AuthGateRateConfig is a token's run-start rate (RFC EF restrictions.rate).
type AuthGateRateConfig struct {
	PerMinute int `yaml:"per_minute,omitempty"`
	Burst     int `yaml:"burst,omitempty"`
}

// AuthGateDeviceConfig holds the device gate's keys, inlined into
// AuthGateConfig; they are refused on any other kind.
type AuthGateDeviceConfig struct {
	TokenTTL string              `yaml:"token_ttl,omitempty"`
	Rate     *AuthGateRateConfig `yaml:"rate,omitempty"`
}

// AuthDeviceGate is a validated device gate.
type AuthDeviceGate struct {
	// TokenTTL is the issued token's lifetime before the approving gate's
	// and the server's user-token caps are applied.
	TokenTTL time.Duration
	// RatePerMinute / RateBurst restrict how often an issued token may start
	// runs; zero RatePerMinute = no rate restriction.
	RatePerMinute int
	RateBurst     int
}

func hasDeviceKeys(g AuthGateConfig) bool { return g.TokenTTL != "" || g.Rate != nil }

// resolveDeviceGate validates one kind: device gate.
//
// The verification link a CLI shows is built from LOOMCYCLE_PUBLIC_URL, never
// from the request's Host (behind a proxy that is the proxy's view, and a
// forged one would point the person at someone else's page), so the gate
// needs it. It also needs a token pepper (LOOMCYCLE_SECRET_KEY or
// LOOMCYCLE_OPERATOR_TOKEN_PEPPER): the short user code is stored as a keyed
// hash, and without a key a database reader could reverse it by enumeration.
func resolveDeviceGate(c *Config, name string, g AuthGateConfig) (*AuthDeviceGate, error) {
	where := "auth.gates." + name
	if strings.TrimSpace(c.Env.PublicURL) == "" {
		return nil, fmt.Errorf("%s: a device gate needs LOOMCYCLE_PUBLIC_URL: the verification link a CLI shows is built from it", where)
	}
	if strings.TrimSpace(c.Env.SecretKey) == "" && strings.TrimSpace(c.Env.OperatorTokenPepper) == "" {
		return nil, fmt.Errorf("%s: a device gate needs LOOMCYCLE_SECRET_KEY (or LOOMCYCLE_OPERATOR_TOKEN_PEPPER): "+
			"device codes are stored as hashes keyed by it", where)
	}
	if g.IdentityMap != nil {
		return nil, fmt.Errorf("%s: a device gate takes no identity_map: the person signs in through another gate, whose map applies", where)
	}
	d := &AuthDeviceGate{}
	var err error
	if d.TokenTTL, err = parseSessionDuration(where+".token_ttl", g.TokenTTL, DefaultDeviceTokenTTL); err != nil {
		return nil, err
	}
	if g.Rate != nil {
		if g.Rate.PerMinute < 1 || g.Rate.PerMinute > 60000 || g.Rate.Burst < 0 || g.Rate.Burst > 60000 {
			return nil, fmt.Errorf("%s.rate: per_minute between 1 and 60000 and burst between 0 and 60000, got %d/%d",
				where, g.Rate.PerMinute, g.Rate.Burst)
		}
		d.RatePerMinute, d.RateBurst = g.Rate.PerMinute, g.Rate.Burst
	}
	return d, nil
}

// DeviceGate is the configured device gate, and whether there is one.
func (p AuthPolicy) DeviceGate() (AuthGate, bool) {
	for _, g := range p.Gates {
		if g.Kind == AuthGateKindDevice && g.Device != nil {
			return g, true
		}
	}
	return AuthGate{}, false
}
