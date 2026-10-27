package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC FA §3.3 / §3.7 / §9: how a gate's verified identity becomes a loomcycle
// (tenant, subject), and what a user that gate creates starts with.
//
//	auth:
//	  lifecycle:
//	    new_user: { access_mode: isolated, groups: [everyone, new-users], status: active, token_ttl: 12h }
//	  gates:
//	    google:
//	      kind: oidc
//	      trusted: true
//	      identity_map:
//	        tenant: acme                       # or tenant_claim: hd + allowed_tenants: {acme.com: acme}
//	        subject: sub                       # the claim; the subject is always "<gate>:<value>"
//	        groups: { claim: groups, provision: true, map: { hr-admins: admins } }
//	        provision: auto                    # never | known (default) | auto
//	        new_user: { access_mode: tenant, groups: [staff] }
//	        authoritative_for_users: true
//
// One parser for every gate kind that maps an external identity (oidc, and the
// later header / awsiam / oauth / external kinds): the rules that keep an
// identity out of the wrong tenant and keep a gate from promoting strangers
// live here once, not per kind. The token gate (its bearer already names the
// tenant and subject) and the password gate (its logins are created by
// invites, with their own `new_user`) take no identity_map.

// Provisioning policies (RFC FA §3.3 step 3).
const (
	// ProvisionNever admits only an identity already linked to a user
	// (a user_identities row).
	ProvisionNever = "never"
	// ProvisionKnown also admits an identity whose user an operator
	// registered beforehand (the users row exists); the link is made at its
	// first login. The default.
	ProvisionKnown = "known"
	// ProvisionAuto creates the user at its first login, at the
	// new_user defaults.
	ProvisionAuto = "auto"
)

// New-user statuses (RFC FA §3.7a).
const (
	UserStatusActive = "active"
	// UserStatusPending: a session is issued but every ACL question is
	// NoMatch until an operator approves the user.
	UserStatusPending = "pending"
)

// Lifecycle defaults (RFC FA D11).
const (
	DefaultNewUserAccessMode = "isolated"
	DefaultNewUserStatus     = UserStatusActive
	DefaultNewUserTokenTTL   = 12 * time.Hour
	// maxNewUserTokenTTL bounds a gate-issued token however a gate is
	// configured; LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS may bound it lower.
	maxNewUserTokenTTL = 30 * 24 * time.Hour
	// EmailDomainClaim is the pseudo-claim tenant_claim may name: the domain
	// of a VERIFIED e-mail address (an unverified address yields nothing).
	EmailDomainClaim = "email_domain"
	// defaultSubjectClaim is the claim the subject is made from (D5).
	defaultSubjectClaim = "sub"
	// maxIdentityMapEntries bounds groups.map and allowed_tenants.
	maxIdentityMapEntries = 1000
)

// DefaultNewUserGroups is the deployment's default group floor.
var DefaultNewUserGroups = []string{"everyone", "new-users"}

var (
	identityClaimRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/-]{0,63}$`)
	identityTenantRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	// legacySubjectPrefixRE ends in '-' and holds no ':', so a legacy
	// subject can never name a user in any gate's "<gate>:" namespace.
	legacySubjectPrefixRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}-$`)
)

// AuthLifecycleConfig is `auth.lifecycle`. The removal policy keys
// (on_removed, dormant_after) are resolved in auth_removal.go.
type AuthLifecycleConfig struct {
	NewUser      AuthNewUserConfig   `yaml:"new_user,omitempty"`
	OnRemoved    AuthOnRemovedConfig `yaml:"on_removed,omitempty"`
	DormantAfter string              `yaml:"dormant_after,omitempty"`
}

// AuthNewUserConfig is what a user a gate creates starts with.
type AuthNewUserConfig struct {
	AccessMode string `yaml:"access_mode,omitempty"`
	// Groups, when given (even empty), replaces the default.
	Groups   *[]string `yaml:"groups,omitempty"`
	Status   string    `yaml:"status,omitempty"`
	TokenTTL string    `yaml:"token_ttl,omitempty"`
}

// AuthIdentityMapConfig is a gate's `identity_map`.
type AuthIdentityMapConfig struct {
	// Tenant is the one tenant every identity of this gate lands in. Exactly
	// one of Tenant and TenantClaim is set.
	Tenant string `yaml:"tenant,omitempty"`
	// TenantClaim names the claim (or email_domain) whose value picks the
	// tenant through AllowedTenants; a value not listed is refused.
	TenantClaim    string            `yaml:"tenant_claim,omitempty"`
	AllowedTenants map[string]string `yaml:"allowed_tenants,omitempty"`
	// Subject is the claim the subject is built from (default sub). The
	// subject is always "<gate>:<value>" (D5).
	Subject string `yaml:"subject,omitempty"`
	// Groups enables group sync (§3.7b); it needs trusted: true.
	Groups    *AuthIdentityGroupsConfig `yaml:"groups,omitempty"`
	Provision string                    `yaml:"provision,omitempty"`
	NewUser   *AuthNewUserConfig        `yaml:"new_user,omitempty"`
	// AuthoritativeForUsers: a user the gate stops asserting is removed per
	// the lifecycle removal policy (used by the gates that sync users).
	AuthoritativeForUsers bool `yaml:"authoritative_for_users,omitempty"`
	// LegacySubjectPrefix (external gates only) links a first login to the
	// user a bridge made before it was a gate: prefix + the subject value,
	// ':' read as '-' ("42" → "telegram-42", "conv:ab" → "telegram-conv-ab").
	LegacySubjectPrefix string `yaml:"legacy_subject_prefix,omitempty"`
}

// AuthIdentityGroupsConfig is identity_map.groups.
type AuthIdentityGroupsConfig struct {
	// Claim names the claim holding the group list. Empty: the gate's own
	// group list (a gate that knows groups natively).
	Claim string `yaml:"claim,omitempty"`
	// Provision creates a group the gate asserts that no group carries the
	// external id of yet.
	Provision bool `yaml:"provision,omitempty"`
	// Map renames an asserted group (the provider's name) to the loomcycle
	// group name it is provisioned as.
	Map map[string]string `yaml:"map,omitempty"`
}

// AuthNewUser is a resolved new_user block.
type AuthNewUser struct {
	AccessMode string
	Groups     []string
	Status     string
	TokenTTL   time.Duration
}

// AuthIdentityMap is a validated identity_map, with the gate's effective
// new-user defaults (the deployment's, overridden by the gate's).
type AuthIdentityMap struct {
	Tenant         string
	TenantClaim    string
	AllowedTenants map[string]string
	SubjectClaim   string
	// SyncGroups is set when a groups block is configured.
	SyncGroups      bool
	GroupsClaim     string
	GroupsProvision bool
	GroupMap        map[string]string
	Provision       string
	NewUser         AuthNewUser
	// Trusted is the gate's trusted flag: only a trusted gate may assert an
	// access_mode or sync groups (D12).
	Trusted               bool
	AuthoritativeForUsers bool
	// LegacySubjectPrefix: see AuthIdentityMapConfig.LegacySubjectPrefix.
	LegacySubjectPrefix string
}

// defaultLifecycle is the deployment floor when auth.lifecycle is absent.
func defaultLifecycle() AuthNewUser {
	return AuthNewUser{AccessMode: DefaultNewUserAccessMode, Groups: slices.Clone(DefaultNewUserGroups),
		Status: DefaultNewUserStatus, TokenTTL: DefaultNewUserTokenTTL}
}

// NewUserFloor is the deployment's new-user defaults, defaulted.
func (p AuthPolicy) NewUserFloor() AuthNewUser {
	if p.Lifecycle.AccessMode == "" {
		return defaultLifecycle()
	}
	return p.Lifecycle
}

// resolveLifecycle validates auth.lifecycle.
func resolveLifecycle(c AuthLifecycleConfig) (AuthNewUser, error) {
	nu, err := mergeNewUser("auth.lifecycle.new_user", defaultLifecycle(), &c.NewUser)
	if err != nil {
		return AuthNewUser{}, err
	}
	if nu.AccessMode != DefaultNewUserAccessMode {
		// The deployment floor is what every gate's strangers get unasked;
		// tenant reach there would hand the whole tenant to anyone an
		// identity provider vouches for. A gate that provisions employees
		// says so itself, with trusted: true.
		return AuthNewUser{}, fmt.Errorf("auth.lifecycle.new_user.access_mode: the deployment default must be isolated; " +
			"set access_mode: tenant on a gate's identity_map.new_user with trusted: true instead")
	}
	return nu, nil
}

// mergeNewUser applies o over base.
func mergeNewUser(where string, base AuthNewUser, o *AuthNewUserConfig) (AuthNewUser, error) {
	out := base
	out.Groups = slices.Clone(base.Groups)
	if o == nil {
		return out, nil
	}
	switch m := strings.TrimSpace(o.AccessMode); m {
	case "":
	case "isolated", "tenant":
		out.AccessMode = m
	default:
		return AuthNewUser{}, fmt.Errorf("%s.access_mode: %q (want isolated or tenant)", where, m)
	}
	switch st := strings.TrimSpace(o.Status); st {
	case "":
	case UserStatusActive, UserStatusPending:
		out.Status = st
	default:
		return AuthNewUser{}, fmt.Errorf("%s.status: %q (want active or pending)", where, st)
	}
	if o.Groups != nil {
		out.Groups = []string{}
		for _, g := range *o.Groups {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			if !authz.ValidName(g) {
				return AuthNewUser{}, fmt.Errorf("%s.groups: %q is not a group name", where, g)
			}
			if !slices.Contains(out.Groups, g) {
				out.Groups = append(out.Groups, g)
			}
		}
	}
	if v := strings.TrimSpace(o.TokenTTL); v != "" {
		d, err := parseSessionDuration(where+".token_ttl", v, 0)
		if err != nil {
			return AuthNewUser{}, err
		}
		if d > maxNewUserTokenTTL {
			return AuthNewUser{}, fmt.Errorf("%s.token_ttl: at most %s, got %s", where, maxNewUserTokenTTL, d)
		}
		out.TokenTTL = d
	}
	return out, nil
}

// raisesFloor reports what of nu goes beyond floor: tenant reach, a group
// the floor does not give, or skipping a pending approval. "" when nothing.
func raisesFloor(nu, floor AuthNewUser) string {
	if nu.AccessMode == "tenant" && floor.AccessMode != "tenant" {
		return "access_mode: tenant"
	}
	for _, g := range nu.Groups {
		if !slices.Contains(floor.Groups, g) {
			return "group " + g
		}
	}
	if nu.Status == UserStatusActive && floor.Status == UserStatusPending {
		return "status: active (the deployment default is pending)"
	}
	return ""
}

// resolveIdentityMap validates one gate's identity_map against the
// deployment floor. trusted is the gate's trusted flag.
func resolveIdentityMap(gate string, m *AuthIdentityMapConfig, floor AuthNewUser, trusted bool) (*AuthIdentityMap, error) {
	where := "auth.gates." + gate + ".identity_map"
	if m == nil {
		return nil, fmt.Errorf("%s: required — it names the tenant this gate's identities land in "+
			"(tenant: <id>, or tenant_claim + allowed_tenants)", where)
	}
	out := &AuthIdentityMap{Trusted: trusted, AuthoritativeForUsers: m.AuthoritativeForUsers}
	tenant, claim := strings.TrimSpace(m.Tenant), strings.TrimSpace(m.TenantClaim)
	switch {
	case tenant != "" && claim != "":
		return nil, fmt.Errorf("%s: set tenant or tenant_claim, not both", where)
	case tenant != "":
		if len(m.AllowedTenants) > 0 {
			return nil, fmt.Errorf("%s.allowed_tenants: applies only with tenant_claim", where)
		}
		if !validIdentityTenant(tenant) {
			return nil, fmt.Errorf("%s.tenant: %q is not a tenant id (letters, digits, - and _, at most 64)", where, tenant)
		}
		out.Tenant = tenant
	case claim != "":
		if !identityClaimRE.MatchString(claim) {
			return nil, fmt.Errorf("%s.tenant_claim: %q is not a claim name", where, claim)
		}
		if len(m.AllowedTenants) == 0 {
			// A claim with no allowlist would let the provider name any
			// tenant, the shared one included.
			return nil, fmt.Errorf("%s.allowed_tenants: required with tenant_claim — "+
				"map each accepted %s value to a tenant id", where, claim)
		}
		if len(m.AllowedTenants) > maxIdentityMapEntries {
			return nil, fmt.Errorf("%s.allowed_tenants: at most %d entries", where, maxIdentityMapEntries)
		}
		out.TenantClaim = claim
		out.AllowedTenants = make(map[string]string, len(m.AllowedTenants))
		for k, v := range m.AllowedTenants {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "" || !validIdentityTenant(v) {
				return nil, fmt.Errorf("%s.allowed_tenants: %q → %q: the value must be a tenant id "+
					"(letters, digits, - and _, at most 64); the shared tenant cannot be named", where, k, v)
			}
			if claim == EmailDomainClaim {
				k = strings.ToLower(k)
			}
			out.AllowedTenants[k] = v
		}
	default:
		return nil, fmt.Errorf("%s: set tenant (one tenant) or tenant_claim + allowed_tenants: "+
			"an identity never lands in the shared tenant by default", where)
	}
	out.SubjectClaim = defaultSubjectClaim
	if s := strings.TrimSpace(m.Subject); s != "" {
		if !identityClaimRE.MatchString(s) || s == EmailDomainClaim {
			return nil, fmt.Errorf("%s.subject: %q is not a claim name", where, s)
		}
		out.SubjectClaim = s
	}
	switch p := strings.TrimSpace(m.Provision); p {
	case "":
		out.Provision = ProvisionKnown
	case ProvisionNever, ProvisionKnown, ProvisionAuto:
		out.Provision = p
	default:
		return nil, fmt.Errorf("%s.provision: %q (want never, known or auto)", where, p)
	}
	if g := m.Groups; g != nil {
		if !trusted {
			// D12: a gate that adds people to groups raises their authority;
			// only a gate an admin marked trusted may.
			return nil, fmt.Errorf("%s.groups: group sync needs trusted: true on the gate", where)
		}
		out.SyncGroups = true
		out.GroupsProvision = g.Provision
		if c := strings.TrimSpace(g.Claim); c != "" {
			if !identityClaimRE.MatchString(c) {
				return nil, fmt.Errorf("%s.groups.claim: %q is not a claim name", where, c)
			}
			out.GroupsClaim = c
		}
		if len(g.Map) > maxIdentityMapEntries {
			return nil, fmt.Errorf("%s.groups.map: at most %d entries", where, maxIdentityMapEntries)
		}
		if len(g.Map) > 0 {
			out.GroupMap = make(map[string]string, len(g.Map))
			names := make([]string, 0, len(g.Map))
			for k := range g.Map {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				v := strings.TrimSpace(g.Map[k])
				if strings.TrimSpace(k) == "" || !authz.ValidName(v) {
					return nil, fmt.Errorf("%s.groups.map: %q → %q: the value must be a group name", where, k, v)
				}
				out.GroupMap[k] = v
			}
		}
	}
	nu, err := mergeNewUser(where+".new_user", floor, m.NewUser)
	if err != nil {
		return nil, err
	}
	if what := raisesFloor(nu, floor); what != "" && !trusted {
		return nil, fmt.Errorf("%s.new_user: %s goes beyond auth.lifecycle.new_user and needs trusted: true on the gate", where, what)
	}
	out.NewUser = nu
	if p := strings.TrimSpace(m.LegacySubjectPrefix); p != "" {
		if !legacySubjectPrefixRE.MatchString(p) {
			return nil, fmt.Errorf("%s.legacy_subject_prefix: %q must be letters, digits, - and _, ending in - (e.g. telegram-)", where, p)
		}
		out.LegacySubjectPrefix = p
	}
	return out, nil
}

func validIdentityTenant(t string) bool {
	// "_shared" and other leading-underscore ids are reserved path segments.
	return identityTenantRE.MatchString(t)
}
