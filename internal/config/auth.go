package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RFC FA: how people sign in to the Web UI.
//
//	auth:
//	  mode: strict                 # token (default) | strict; env LOOMCYCLE_AUTH_MODE
//	  session: { idle: 8h, absolute: 168h, reauth_window: 15m }
//	  gates:
//	    paste: { kind: token, label: "Access token" }
//
// Open mode is not a value of auth.mode: it is what a server with no credential
// configured runs as, and naming it here would only invite a config that says
// "open" while a secret is set.

// Auth modes.
const (
	AuthModeToken  = "token"
	AuthModeStrict = "strict"
)

// Session lifetime defaults (RFC FA decision 9).
const (
	DefaultSessionIdle         = 8 * time.Hour
	DefaultSessionAbsolute     = 7 * 24 * time.Hour
	DefaultSessionReauthWindow = 15 * time.Minute
)

// AuthGateKindToken is the gate that accepts a loomcycle bearer (BaseAuthGate).
const AuthGateKindToken = "token"

// tokenGateLabel is the token gate's label when none is configured.
const tokenGateLabel = "Access token"

// laterGateKinds are the gate kinds the RFC defines for later phases. They are
// refused with a message that says so, rather than as unknown, so an operator
// who copied an example learns why it does not load yet.
var laterGateKinds = map[string]bool{
	"password": true, "oidc": true, "oauth": true, "header": true,
	"awsiam": true, "device": true, "external": true,
}

var authGateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// AuthConfig is the yaml `auth:` block.
type AuthConfig struct {
	Mode    string                    `yaml:"mode,omitempty"`
	Session AuthSessionConfig         `yaml:"session,omitempty"`
	Gates   map[string]AuthGateConfig `yaml:"gates,omitempty"`
}

// AuthSessionConfig holds Go durations ("8h", "15m").
type AuthSessionConfig struct {
	Idle     string `yaml:"idle,omitempty"`
	Absolute string `yaml:"absolute,omitempty"`
	// ReauthWindow is how recent a login must be for a sensitive action. It is
	// validated now and enforced once step-up re-authentication exists.
	ReauthWindow string `yaml:"reauth_window,omitempty"`
}

// AuthGateConfig is one entry of `auth.gates`.
type AuthGateConfig struct {
	Kind  string `yaml:"kind"`
	Label string `yaml:"label,omitempty"`
}

// AuthGate is a validated gate: what the login page lists.
type AuthGate struct {
	Name  string
	Kind  string
	Label string
}

// AuthPolicy is the resolved `auth:` block. Its zero value is token mode with
// the default lifetimes, so a Config built without validate (tests) behaves
// like an upgrade with no `auth:` block.
type AuthPolicy struct {
	Mode         string
	Idle         time.Duration
	Absolute     time.Duration
	ReauthWindow time.Duration
	// Gates is the configured gates, sorted by name.
	Gates []AuthGate
}

// Strict reports whether the deployment is in strict mode.
func (p AuthPolicy) Strict() bool { return p.Mode == AuthModeStrict }

// ModeName is the mode as reported to clients.
func (p AuthPolicy) ModeName() string {
	if p.Strict() {
		return AuthModeStrict
	}
	return AuthModeToken
}

// SessionIdle is the idle timeout, defaulted.
func (p AuthPolicy) SessionIdle() time.Duration {
	if p.Idle > 0 {
		return p.Idle
	}
	return DefaultSessionIdle
}

// SessionAbsolute is the absolute lifetime, defaulted.
func (p AuthPolicy) SessionAbsolute() time.Duration {
	if p.Absolute > 0 {
		return p.Absolute
	}
	return DefaultSessionAbsolute
}

// LoginGates are the gates the login page offers. In token mode the paste box
// is always there: when no token gate is listed, an implicit one named "token"
// is added (D1: configuring other gates never takes it away in token mode). In
// strict mode only the listed gates are offered.
func (p AuthPolicy) LoginGates() []AuthGate {
	out := append([]AuthGate(nil), p.Gates...)
	if _, listed := p.listedTokenGate(); !listed && !p.Strict() {
		out = append(out, AuthGate{Name: AuthGateKindToken, Kind: AuthGateKindToken, Label: tokenGateLabel})
	}
	return out
}

// TokenGate is the gate a pasted or handed-off bearer logs in through, and
// whether there is one.
func (p AuthPolicy) TokenGate() (AuthGate, bool) {
	for _, g := range p.LoginGates() {
		if g.Kind == AuthGateKindToken {
			return g, true
		}
	}
	return AuthGate{}, false
}

func (p AuthPolicy) listedTokenGate() (AuthGate, bool) {
	for _, g := range p.Gates {
		if g.Kind == AuthGateKindToken {
			return g, true
		}
	}
	return AuthGate{}, false
}

// authModeRank orders the modes from least to most restrictive.
func authModeRank(mode string) int {
	if mode == AuthModeStrict {
		return 1
	}
	return 0
}

// AuthModeLowered reports whether going from `from` to `to` lowers the mode.
func AuthModeLowered(from, to AuthPolicy) bool {
	return authModeRank(to.ModeName()) < authModeRank(from.ModeName())
}

func parseAuthMode(where, v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", nil
	case AuthModeToken:
		return AuthModeToken, nil
	case AuthModeStrict:
		return AuthModeStrict, nil
	case "open":
		return "", fmt.Errorf("%s: %q is not a mode: open mode is what a server with no credential "+
			"configured runs as; use token or strict", where, v)
	}
	return "", fmt.Errorf("%s: unknown mode %q (want token or strict)", where, v)
}

func parseSessionDuration(where, v string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", where, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %s", where, v)
	}
	return d, nil
}

// resolveAuth validates c.Auth and fills c.ResolvedAuth.
//
// The mode is the yaml value when set, else LOOMCYCLE_AUTH_MODE, else token.
// A yaml value below the env value is refused rather than silently winning: an
// operator who sets the fleet to strict should not be undone by one config
// file, and a config that wants less has to say so where the fleet is set.
func resolveAuth(c *Config) error {
	envMode, err := parseAuthMode("LOOMCYCLE_AUTH_MODE", c.Env.AuthMode)
	if err != nil {
		return err
	}
	cfgMode, err := parseAuthMode("auth.mode", c.Auth.Mode)
	if err != nil {
		return err
	}
	mode := cfgMode
	if mode == "" {
		mode = envMode
	}
	if mode == "" {
		mode = AuthModeToken
	}
	if cfgMode != "" && envMode != "" && authModeRank(cfgMode) < authModeRank(envMode) {
		return fmt.Errorf("auth.mode: %s would lower LOOMCYCLE_AUTH_MODE=%s; unset LOOMCYCLE_AUTH_MODE "+
			"or set the same mode in both", cfgMode, envMode)
	}
	p := AuthPolicy{Mode: mode}
	if p.Idle, err = parseSessionDuration("auth.session.idle", c.Auth.Session.Idle, DefaultSessionIdle); err != nil {
		return err
	}
	if p.Absolute, err = parseSessionDuration("auth.session.absolute", c.Auth.Session.Absolute, DefaultSessionAbsolute); err != nil {
		return err
	}
	if p.ReauthWindow, err = parseSessionDuration("auth.session.reauth_window", c.Auth.Session.ReauthWindow, DefaultSessionReauthWindow); err != nil {
		return err
	}
	if p.Idle > p.Absolute {
		return fmt.Errorf("auth.session: idle (%s) is longer than absolute (%s)", p.Idle, p.Absolute)
	}
	names := make([]string, 0, len(c.Auth.Gates))
	for name := range c.Auth.Gates {
		names = append(names, name)
	}
	sort.Strings(names)
	tokenGates, interactive := 0, 0
	for _, name := range names {
		g := c.Auth.Gates[name]
		if !authGateNameRE.MatchString(name) {
			return fmt.Errorf("auth.gates: invalid gate name %q (lowercase letters, digits, - and _, at most 32)", name)
		}
		kind := strings.ToLower(strings.TrimSpace(g.Kind))
		// The one place a gate kind is accepted: a kind that becomes available
		// is a case here (and leaves laterGateKinds).
		switch {
		case kind == AuthGateKindToken:
			tokenGates++
			interactive++
		case laterGateKinds[kind]:
			return fmt.Errorf("auth.gates.%s: kind %q is not supported in this release yet; only kind: token is", name, kind)
		case kind == "":
			return fmt.Errorf("auth.gates.%s: kind is required (token)", name)
		default:
			return fmt.Errorf("auth.gates.%s: unknown kind %q (want token)", name, g.Kind)
		}
		label := strings.TrimSpace(g.Label)
		if label == "" {
			label = name
			if kind == AuthGateKindToken {
				label = tokenGateLabel
			}
		}
		p.Gates = append(p.Gates, AuthGate{Name: name, Kind: kind, Label: label})
	}
	if tokenGates > 1 {
		return fmt.Errorf("auth.gates: at most one gate may be kind: token, got %d", tokenGates)
	}
	// Strict mode with no gate a person can sign in through would lock
	// everyone out of the Web UI.
	if p.Strict() && interactive == 0 {
		return fmt.Errorf("auth.mode: strict needs at least one interactive gate in auth.gates " +
			"(e.g. `paste: {kind: token}`), or nobody can sign in to the Web UI")
	}
	c.ResolvedAuth = p
	return nil
}
