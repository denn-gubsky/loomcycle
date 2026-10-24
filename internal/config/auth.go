package config

import (
	"fmt"
	"net/netip"
	"net/textproto"
	"net/url"
	"os"
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

// AuthGateKindPassword is the password gate (NaiveAuthGate, RFC FA §6).
const AuthGateKindPassword = "password"

// Password gate defaults and caps (RFC FA §6.1).
const (
	DefaultInviteTTL       = 7 * 24 * time.Hour
	MaxInviteTTL           = 30 * 24 * time.Hour
	DefaultResetTTL        = time.Hour
	MaxResetTTL            = 24 * time.Hour
	DefaultMaxFailures     = 10
	DefaultLockoutMinutes  = 15
	maxPasswordMaxFailures = 1000
	maxLockoutMinutes      = 24 * 60
)

// tokenGateLabel is the token gate's label when none is configured.
const tokenGateLabel = "Access token"

// laterGateKinds are the gate kinds the RFC defines for later phases. They are
// refused with a message that says so, rather than as unknown, so an operator
// who copied an example learns why it does not load yet.
var laterGateKinds = map[string]bool{
	"oidc": true, "oauth": true, "header": true,
	"awsiam": true, "device": true, "external": true,
}

var authGateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

var authHeaderNameRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// AuthConfig is the yaml `auth:` block.
type AuthConfig struct {
	Mode    string                    `yaml:"mode,omitempty"`
	Session AuthSessionConfig         `yaml:"session,omitempty"`
	Gates   map[string]AuthGateConfig `yaml:"gates,omitempty"`
	// TrustedProxies are the CIDRs of the reverse proxies in front of the
	// server (a cloudflared container's network, loopback for tailscale
	// serve). Only a request from one of them may say who the client is,
	// through ClientIPHeader or else X-Forwarded-For; any other peer is the
	// client itself. The password gate throttles by that client address.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty"`
	// ClientIPHeader is the header a trusted proxy puts the client address
	// in (Cloudflare: CF-Connecting-IP). Empty: the right-most
	// X-Forwarded-For entry that is not a trusted proxy.
	ClientIPHeader string `yaml:"client_ip_header,omitempty"`
}

// AuthSessionConfig holds Go durations ("8h", "15m").
type AuthSessionConfig struct {
	Idle     string `yaml:"idle,omitempty"`
	Absolute string `yaml:"absolute,omitempty"`
	// ReauthWindow is how recent a login must be for a sensitive action. It is
	// validated now and enforced once step-up re-authentication exists.
	ReauthWindow string `yaml:"reauth_window,omitempty"`
}

// AuthGateConfig is one entry of `auth.gates`. Every key after Label is the
// password gate's (kind: password) and is refused on any other kind.
type AuthGateConfig struct {
	Kind  string `yaml:"kind"`
	Label string `yaml:"label,omitempty"`

	// Trusted lifts the provisioning floor for the gate's tenant operators
	// and admins: an invite may grant access_mode tenant and groups.
	Trusted bool `yaml:"trusted,omitempty"`
	// RequireMFA: nil defaults to true in strict mode, false in token mode
	// (RFC FA D3).
	RequireMFA *bool `yaml:"require_mfa,omitempty"`
	// ManagerMayRecoverOwners lets an auth:manage credential (a website)
	// reset the password of a user above the floor — a tenant's owner — and
	// attach a first password to one. It makes that credential able to take
	// over any owner's account; set it only where the website owns the
	// accounts (loomcycle.cloud). Default false.
	ManagerMayRecoverOwners bool                  `yaml:"manager_may_recover_owners,omitempty"`
	Invites                 AuthGateInvitesConfig `yaml:"invites,omitempty"`
	Resets                  AuthGateResetsConfig  `yaml:"resets,omitempty"`
	// AllowedReturnTo are the absolute URLs (https, or http on loopback) a
	// redemption may send the browser to besides this server's own paths.
	AllowedReturnTo []string               `yaml:"allowed_return_to,omitempty"`
	Password        AuthGatePasswordConfig `yaml:"password,omitempty"`
	NewUser         AuthGateNewUserConfig  `yaml:"new_user,omitempty"`
}

// AuthGateInvitesConfig holds Go durations.
type AuthGateInvitesConfig struct {
	DefaultTTL string `yaml:"default_ttl,omitempty"`
	MaxTTL     string `yaml:"max_ttl,omitempty"`
}

// AuthGateResetsConfig holds a Go duration.
type AuthGateResetsConfig struct {
	TTL string `yaml:"ttl,omitempty"`
}

// AuthGatePasswordConfig is the password policy's operator knobs.
type AuthGatePasswordConfig struct {
	// DenylistFile is an extra breached-password list (one per line, #
	// comments) refused beside the built-in top-10k.
	DenylistFile   string `yaml:"denylist_file,omitempty"`
	MaxFailures    int    `yaml:"max_failures,omitempty"`
	LockoutMinutes int    `yaml:"lockout_minutes,omitempty"`
}

// AuthGateNewUserConfig is the gate's provisioning floor (RFC FA §3.7a).
type AuthGateNewUserConfig struct {
	AccessMode string `yaml:"access_mode,omitempty"`
	// Groups, when given (even empty), replaces the default [everyone,
	// new-users].
	Groups *[]string `yaml:"groups,omitempty"`
}

// AuthGate is a validated gate: what the login page lists.
type AuthGate struct {
	Name  string
	Kind  string
	Label string
	// Password is set for kind: password.
	Password *AuthPasswordGate
}

// AuthPasswordGate is a validated password gate.
type AuthPasswordGate struct {
	Trusted                 bool
	RequireMFA              bool
	ManagerMayRecoverOwners bool
	InviteDefaultTTL        time.Duration
	InviteMaxTTL            time.Duration
	ResetTTL                time.Duration
	AllowedReturnTo         []string
	DenylistFile            string
	MaxFailures             int
	Lockout                 time.Duration
	// NewUserAccessMode is "" for the deployment default; NewUserGroups nil
	// likewise.
	NewUserAccessMode string
	NewUserGroups     []string
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
	// TrustedProxies and ClientIPHeader are the validated auth.trusted_proxies
	// and auth.client_ip_header (canonical form).
	TrustedProxies []netip.Prefix
	ClientIPHeader string
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
	for _, cidr := range c.Auth.TrustedProxies {
		pfx, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			addr, aerr := netip.ParseAddr(strings.TrimSpace(cidr))
			if aerr != nil {
				return fmt.Errorf("auth.trusted_proxies: %q is not a CIDR or an address", cidr)
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		p.TrustedProxies = append(p.TrustedProxies, pfx.Masked())
	}
	if h := strings.TrimSpace(c.Auth.ClientIPHeader); h != "" {
		if len(p.TrustedProxies) == 0 {
			// A client-address header with no proxy allowed to set it would
			// be read from nobody; say so rather than ignore it.
			return fmt.Errorf("auth.client_ip_header: set auth.trusted_proxies too — only a trusted proxy's header is read")
		}
		if !authHeaderNameRE.MatchString(h) {
			return fmt.Errorf("auth.client_ip_header: %q is not a header name", h)
		}
		p.ClientIPHeader = textproto.CanonicalMIMEHeaderKey(h)
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
		case kind == AuthGateKindPassword:
			interactive++
		case laterGateKinds[kind]:
			return fmt.Errorf("auth.gates.%s: kind %q is not supported in this release yet; only kind: token and kind: password are", name, kind)
		case kind == "":
			return fmt.Errorf("auth.gates.%s: kind is required (token or password)", name)
		default:
			return fmt.Errorf("auth.gates.%s: unknown kind %q (want token or password)", name, g.Kind)
		}
		var pw *AuthPasswordGate
		if kind == AuthGateKindPassword {
			if pw, err = resolvePasswordGate(c, name, g, p.Strict()); err != nil {
				return err
			}
		} else if hasPasswordKeys(g) {
			return fmt.Errorf("auth.gates.%s: the password gate's keys (trusted, require_mfa, invites, resets, "+
				"allowed_return_to, password, new_user, manager_may_recover_owners) apply only to kind: password", name)
		}
		label := strings.TrimSpace(g.Label)
		if label == "" {
			label = name
			if kind == AuthGateKindToken {
				label = tokenGateLabel
			}
		}
		p.Gates = append(p.Gates, AuthGate{Name: name, Kind: kind, Label: label, Password: pw})
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

func hasPasswordKeys(g AuthGateConfig) bool {
	return g.Trusted || g.RequireMFA != nil || g.ManagerMayRecoverOwners || g.Invites != (AuthGateInvitesConfig{}) ||
		g.Resets != (AuthGateResetsConfig{}) || len(g.AllowedReturnTo) > 0 || g.Password != (AuthGatePasswordConfig{}) ||
		g.NewUser.AccessMode != "" || g.NewUser.Groups != nil
}

// resolvePasswordGate validates one kind: password gate. A password gate
// cannot run without LOOMCYCLE_SECRET_KEY (its pepper and the TOTP secrets'
// key derive from it) or LOOMCYCLE_PUBLIC_URL (its invite and reset links are
// built from it), so both are load errors — `loomcycle validate` reports them
// rather than the first invite failing.
func resolvePasswordGate(c *Config, name string, g AuthGateConfig, strict bool) (*AuthPasswordGate, error) {
	where := "auth.gates." + name
	if strings.TrimSpace(c.Env.SecretKey) == "" {
		return nil, fmt.Errorf("%s: a password gate needs LOOMCYCLE_SECRET_KEY (the password pepper and the TOTP secret key derive from it)", where)
	}
	if strings.TrimSpace(c.Env.PublicURL) == "" {
		return nil, fmt.Errorf("%s: a password gate needs LOOMCYCLE_PUBLIC_URL: its invite and reset links are built from it", where)
	}
	pw := &AuthPasswordGate{Trusted: g.Trusted, RequireMFA: strict, ManagerMayRecoverOwners: g.ManagerMayRecoverOwners}
	if g.RequireMFA != nil {
		pw.RequireMFA = *g.RequireMFA
	}
	var err error
	if pw.InviteDefaultTTL, err = parseSessionDuration(where+".invites.default_ttl", g.Invites.DefaultTTL, DefaultInviteTTL); err != nil {
		return nil, err
	}
	if pw.InviteMaxTTL, err = parseSessionDuration(where+".invites.max_ttl", g.Invites.MaxTTL, MaxInviteTTL); err != nil {
		return nil, err
	}
	if pw.InviteMaxTTL > MaxInviteTTL || pw.InviteDefaultTTL > pw.InviteMaxTTL {
		return nil, fmt.Errorf("%s.invites: default_ttl (%s) must not exceed max_ttl (%s), and max_ttl not %s",
			where, pw.InviteDefaultTTL, pw.InviteMaxTTL, MaxInviteTTL)
	}
	if pw.ResetTTL, err = parseSessionDuration(where+".resets.ttl", g.Resets.TTL, DefaultResetTTL); err != nil {
		return nil, err
	}
	if pw.ResetTTL > MaxResetTTL {
		return nil, fmt.Errorf("%s.resets.ttl: at most %s, got %s", where, MaxResetTTL, pw.ResetTTL)
	}
	for _, rt := range g.AllowedReturnTo {
		u, err := url.Parse(strings.TrimSpace(rt))
		loopback := err == nil && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
		if err != nil || u.Host == "" || (u.Scheme != "https" && !loopback) || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("%s.allowed_return_to: %q must be an absolute https URL (http only on loopback), with no user info or fragment", where, rt)
		}
		pw.AllowedReturnTo = append(pw.AllowedReturnTo, strings.TrimSpace(rt))
	}
	pw.MaxFailures, pw.Lockout = DefaultMaxFailures, DefaultLockoutMinutes*time.Minute
	if n := g.Password.MaxFailures; n != 0 {
		if n < 1 || n > maxPasswordMaxFailures {
			return nil, fmt.Errorf("%s.password.max_failures: between 1 and %d, got %d", where, maxPasswordMaxFailures, n)
		}
		pw.MaxFailures = n
	}
	if m := g.Password.LockoutMinutes; m != 0 {
		if m < 1 || m > maxLockoutMinutes {
			return nil, fmt.Errorf("%s.password.lockout_minutes: between 1 and %d, got %d", where, maxLockoutMinutes, m)
		}
		pw.Lockout = time.Duration(m) * time.Minute
	}
	if f := strings.TrimSpace(g.Password.DenylistFile); f != "" {
		st, err := os.Stat(f)
		if err != nil || st.IsDir() {
			return nil, fmt.Errorf("%s.password.denylist_file: %q is not a readable file", where, f)
		}
		pw.DenylistFile = f
	}
	switch m := strings.TrimSpace(g.NewUser.AccessMode); m {
	case "", "isolated", "tenant":
		pw.NewUserAccessMode = m
	default:
		return nil, fmt.Errorf("%s.new_user.access_mode: %q (want isolated or tenant)", where, m)
	}
	if pw.NewUserAccessMode == "tenant" && !pw.Trusted {
		// The floor is what anyone the gate provisions gets unasked; a floor
		// of tenant reach on an untrusted gate would hand it to every invitee
		// of a website's credential.
		return nil, fmt.Errorf("%s.new_user.access_mode: tenant needs trusted: true", where)
	}
	if g.NewUser.Groups != nil {
		pw.NewUserGroups = []string{}
		for _, gr := range *g.NewUser.Groups {
			if gr = strings.TrimSpace(gr); gr != "" {
				pw.NewUserGroups = append(pw.NewUserGroups, gr)
			}
		}
	}
	return pw, nil
}
