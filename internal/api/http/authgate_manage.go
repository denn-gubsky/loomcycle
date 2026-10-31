package http

// authgate_manage.go — RFC FA §6.1 (D15–D18): the password gate's management
// API under /v1/_authgate/{gate}/. It issues invitations and reset links,
// removes a password, renames a login and lists a user's logins. It never
// accepts a password (D17): a person always types theirs into loomcycle's own
// page, which redeems the invite or completes the reset. Links are built from
// LOOMCYCLE_PUBLIC_URL and returned to the caller, which delivers them —
// loomcycle sends no mail.
//
// Who may call (D16): a substrate:tenant operator for its own tenant, an
// admin for any, and an auth:manage credential (a website's) for any — the
// latter held to the provisioning floor once the tenant has a user.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// PasswordGateSettings is one `kind: password` gate as the server runs it.
type PasswordGateSettings struct {
	Name string
	// Trusted lifts the provisioning floor for the gate's tenant operators
	// and admins: an invite may then grant access_mode tenant and groups.
	Trusted          bool
	RequireMFA       bool
	InviteDefaultTTL time.Duration
	InviteMaxTTL     time.Duration
	ResetTTL         time.Duration
	// AllowedReturnTo are absolute URLs a redemption may redirect to besides
	// paths on loomcycle's own origin.
	AllowedReturnTo []string
	Throttle        password.Throttle
	// ManagerMayRecoverOwners lets an auth:manage credential issue a reset
	// for a user above the floor, and attach a first password to one — so a
	// website can recover or migrate a tenant's owner. It makes that
	// credential able to take over any owner's account, so it is off unless
	// the deployment's trust model is "the website owns the accounts".
	ManagerMayRecoverOwners bool
	// NewUserAccessMode and NewUserGroups are the floor: what a user the gate
	// provisions gets without anyone's say-so (RFC FA §3.7a). Empty means the
	// deployment default (DefaultFloor).
	NewUserAccessMode string
	NewUserGroups     []string
	// NewUserStatus is the status a website's (auth:manage) invite creates a
	// user with: "pending" holds the person at the approval queue until an
	// operator approves them. "" or "active" creates them active.
	NewUserStatus string
}

// floorPolicy is a provisioning floor: an access mode and the groups every
// new user is in.
type floorPolicy struct {
	accessMode string
	groups     []string
}

// defaultFloor is RFC FA §3.7a's deployment default for a new user (D11):
// isolated, in the everyone and new-users groups.
var defaultFloor = floorPolicy{accessMode: "isolated", groups: []string{"everyone", "new-users"}}

// floorOf is the floor a gate provisions at; nil (the /v1/_users routes,
// which belong to no gate) is the deployment default.
func floorOf(g *PasswordGateSettings) floorPolicy {
	f := defaultFloor
	if g == nil {
		return f
	}
	if g.NewUserAccessMode != "" {
		f.accessMode = g.NewUserAccessMode
	}
	if g.NewUserGroups != nil {
		f.groups = g.NewUserGroups
	}
	return f
}

// PasswordGates is everything the password gates share: their settings, the
// hasher (with the pepper), the policy, the sealer for TOTP secrets and the
// public URL links are built from.
type PasswordGates struct {
	byName    map[string]PasswordGateSettings
	hasher    *password.Hasher
	policy    *password.Policy
	sealer    *credential.Sealer
	publicURL string
}

// NewPasswordGates validates and bundles the password gates. A gate cannot run
// without a pepper (hasher), a sealer that can encrypt TOTP secrets, or a
// public URL to build its links from.
func NewPasswordGates(gates []PasswordGateSettings, hasher *password.Hasher, policy *password.Policy, sealer *credential.Sealer, publicURL string) (*PasswordGates, error) {
	if len(gates) == 0 {
		return nil, nil
	}
	if hasher == nil || policy == nil || sealer == nil || !sealer.Enabled() {
		return nil, errors.New("password gates need LOOMCYCLE_SECRET_KEY (the password pepper and the TOTP secret key derive from it)")
	}
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if publicURL == "" {
		return nil, errors.New("password gates need LOOMCYCLE_PUBLIC_URL: invite and reset links are built from it")
	}
	pg := &PasswordGates{byName: map[string]PasswordGateSettings{}, hasher: hasher, policy: policy, sealer: sealer, publicURL: publicURL}
	for _, g := range gates {
		if g.InviteDefaultTTL <= 0 || g.InviteMaxTTL < g.InviteDefaultTTL || g.ResetTTL <= 0 || g.Throttle.MaxFailures <= 0 || g.Throttle.Lockout <= 0 {
			return nil, fmt.Errorf("password gate %q: invalid lifetimes or throttle", g.Name)
		}
		pg.byName[g.Name] = g
	}
	return pg, nil
}

// PasswordGatesFromConfig builds the password gates the config declares (nil
// when it declares none). The pepper and the TOTP key derive from
// LOOMCYCLE_SECRET_KEY (with LOOMCYCLE_SECRET_KEY_PREVIOUS still verifying
// and opening during a rotation); the policy refuses the built-in breached list
// plus every gate's denylist_file — one policy for all gates, since a password
// breached anywhere is breached.
func PasswordGatesFromConfig(cfg *config.Config) (*PasswordGates, error) {
	var gates []PasswordGateSettings
	var lists []io.Reader
	for _, g := range cfg.ResolvedAuth.Gates {
		p := g.Password
		if p == nil {
			continue
		}
		gates = append(gates, PasswordGateSettings{
			Name: g.Name, Trusted: p.Trusted, RequireMFA: p.RequireMFA, ManagerMayRecoverOwners: p.ManagerMayRecoverOwners,
			InviteDefaultTTL: p.InviteDefaultTTL, InviteMaxTTL: p.InviteMaxTTL, ResetTTL: p.ResetTTL,
			AllowedReturnTo: p.AllowedReturnTo, Throttle: password.Throttle{MaxFailures: p.MaxFailures, Lockout: p.Lockout},
			NewUserAccessMode: p.NewUserAccessMode, NewUserGroups: p.NewUserGroups, NewUserStatus: p.NewUserStatus,
		})
		if p.DenylistFile != "" {
			b, err := os.ReadFile(p.DenylistFile)
			if err != nil {
				return nil, fmt.Errorf("auth.gates.%s.password.denylist_file: %w", g.Name, err)
			}
			lists = append(lists, bytes.NewReader(b), strings.NewReader("\n"))
		}
	}
	if len(gates) == 0 {
		return nil, nil
	}
	cur, err := credential.DerivePasswordPepper(cfg.Env.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
	}
	prev, err := credential.DerivePasswordPepper(cfg.Env.SecretKeyPrevious)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
	}
	hasher, err := password.NewHasher(cur, prev)
	if err != nil {
		return nil, err
	}
	var extra io.Reader
	if len(lists) > 0 {
		extra = io.MultiReader(lists...)
	}
	policy, err := password.NewPolicy(extra)
	if err != nil {
		return nil, err
	}
	sealer, err := credential.NewSealer(cfg.Env.SecretKey, cfg.Env.SecretKeyPrevious)
	if err != nil {
		return nil, err
	}
	return NewPasswordGates(gates, hasher, policy, sealer, cfg.Env.PublicURL)
}

// Names lists the configured password gates, sorted.
func (pg *PasswordGates) Names() []string {
	out := make([]string, 0, len(pg.byName))
	for n := range pg.byName {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// PolicySize is how many breached passwords the policy refuses (for the boot
// log; never the entries).
func (pg *PasswordGates) PolicySize() int { return pg.policy.Size() }

// passwordGatesHolder is read on every management request and swapped by
// SetPasswordGates (boot, config reload).
type passwordGatesHolder struct{ p atomic.Pointer[PasswordGates] }

// SetPasswordGates installs the password gates; nil removes them all.
func (s *Server) SetPasswordGates(pg *PasswordGates) { s.pwGates.p.Store(pg) }

// passwordGate resolves {gate} to a configured password gate, answering an
// opaque 404 for anything else (another gate kind, or no such gate).
func (s *Server) passwordGate(w http.ResponseWriter, r *http.Request) (PasswordGateSettings, *PasswordGates, bool) {
	pg := s.pwGates.p.Load()
	if pg != nil {
		if g, ok := pg.byName[r.PathValue("gate")]; ok {
			return g, pg, true
		}
	}
	writeJSONError(w, http.StatusNotFound, "gate_not_found", "no password gate of that name")
	return PasswordGateSettings{}, nil, false
}

// gateCaller is who is calling the management API.
type gateCaller int

const (
	gateCallerNone gateCaller = iota
	// gateCallerAdmin: substrate:admin, the legacy secret, or open mode.
	gateCallerAdmin
	// gateCallerTenant: a substrate:tenant operator, confined to its tenant.
	gateCallerTenant
	// gateCallerManager: an auth:manage credential, any tenant, held to the
	// floor once the tenant has a user.
	gateCallerManager
)

func callerOf(ctx context.Context) gateCaller {
	p, ok := auth.PrincipalFromContext(ctx)
	switch {
	case !ok:
		return gateCallerAdmin // open mode: every route is the operator's
	case auth.IsAuthManager(p):
		return gateCallerManager
	case p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin):
		return gateCallerAdmin
	case auth.HasScope(p.Scopes, auth.ScopeTenant):
		return gateCallerTenant
	}
	return gateCallerNone
}

// manageTenant resolves the tenant a user-management request acts on. A
// tenant operator acts on its own: an omitted tenant defaults to it and a
// different one is refused. An admin defaults to its own and may name any. An
// auth:manage credential must name one — its own token's tenant means
// nothing. It writes the refusal and returns false.
func (s *Server) manageTenant(w http.ResponseWriter, ctx context.Context, wire string) (string, gateCaller, bool) {
	wire = strings.TrimSpace(wire)
	caller := callerOf(ctx)
	switch caller {
	case gateCallerManager:
		if wire == "" {
			writeJSONError(w, http.StatusBadRequest, "tenant_required", "an "+auth.ScopeAuthManage+" token must name the tenant")
			return "", caller, false
		}
		return wire, caller, true
	case gateCallerAdmin:
		if wire != "" {
			return wire, caller, true
		}
		return tenantFromCtx(ctx), caller, true
	case gateCallerTenant:
		own := tenantFromCtx(ctx)
		if wire != "" && wire != own {
			writeJSONError(w, http.StatusForbidden, "tenant_mismatch", "a tenant operator manages only its own tenant")
			return "", caller, false
		}
		return own, caller, true
	}
	writeJSONError(w, http.StatusForbidden, "insufficient_scope", "user management requires "+auth.ScopeTenant)
	return "", caller, false
}

// decodeStrict reads a JSON body, refusing unknown fields — so a `password`
// field sent to a management route is an error, never silently dropped
// (D17: no management route takes a password).
func decodeStrict(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return false
	}
	return true
}

// tenantBootstrapped reports whether the tenant has ever had a user (a
// persistent marker, or a users row), which closes the owner bootstrap of an
// auth:manage credential for good: deleting every user does not re-open it.
// A legacy cloud tenant created by the old mint path holds tokens but has
// never had a users row; it is not bootstrapped, so a website can give it its
// owner exactly once (the planned migration). Operator tokens in the tenant do
// not count.
func (s *Server) tenantBootstrapped(ctx context.Context, tenant string) (bool, error) {
	if tenant == "" {
		// The shared tenant is never a website's to bootstrap.
		return true, nil
	}
	return s.store.TenantBootstrapped(ctx, tenant)
}

// aboveFloor is THE floor predicate: whether a user holds any authority a
// user the gate provisions would not have. It is true when any of
//
//   - its access_mode is above the floor's;
//   - it is in a group, directly or through nesting, stored or declared in
//     config, that is not one of the floor's groups;
//   - an ACL binding applies to it other than tenant-wide ones and those on
//     the floor's groups — on its subject, a group above, or one of its tokens;
//   - it holds a live token with a scope its floor access mode does not grant.
//
// Group membership and bindings come from the ACL resolver's own Input — the
// stored rows, the config-declared ones and the nesting closure, exactly what
// the engine compiles — so the predicate cannot see less authority than the
// engine grants. It fails CLOSED: any error but a missing user reads as above
// the floor (the error is returned too). A missing user is (false, ErrNotFound).
func (s *Server) aboveFloor(ctx context.Context, tenant, subject string, g *PasswordGateSettings) (bool, store.UserRow, error) {
	u, err := s.store.UserGet(ctx, tenant, subject)
	if err != nil {
		return !isNotFound(err), store.UserRow{}, err
	}
	f := floorOf(g)
	if accessRank(u.AccessMode) > accessRank(f.accessMode) {
		return true, u, nil
	}
	res := s.aclResolver
	if res == nil {
		// No resolver wired (open or compat deployments, tests): the same
		// computation over the store alone.
		res = aclres.New(s.store, authz.ModeCompat, 0)
	}
	floorGroup := func(name string) bool { return slices.Contains(f.groups, name) }
	beyondFloor := func(in authz.Input) bool {
		for _, gr := range in.Subject.Groups {
			if !floorGroup(gr) {
				return true
			}
		}
		for _, b := range in.Bindings {
			// A built-in list counts too when it is bound to the user (an
			// owner binding grants the operator plane); the class baselines
			// the resolver adds are tenant-wide and skipped below.
			if !b.AppliesTo(in.Subject) {
				continue
			}
			switch b.SubjectKind {
			case authz.SubjectTenant:
				// Every user of the tenant has it, a new one too.
			case authz.SubjectGroup:
				if !floorGroup(b.SubjectID) {
					return true
				}
			default:
				return true
			}
		}
		return false
	}
	in, err := res.Input(ctx, authz.Subject{TenantID: tenant, ID: subject})
	if err != nil {
		return true, u, err
	}
	if beyondFloor(in) {
		return true, u, nil
	}
	floorScopes, err := auth.GrantableUserScopes(f.accessMode)
	if err != nil {
		return true, u, err
	}
	tokens, err := s.store.OperatorTokenDefListBySubject(ctx, tenant, subject)
	if err != nil {
		return true, u, err
	}
	for _, t := range tokens {
		if !tokenActive(t) {
			continue
		}
		for _, sc := range t.AllowedScopes {
			if !slices.Contains(floorScopes, sc) {
				return true, u, nil
			}
		}
		tin, err := res.Input(ctx, authz.Subject{TenantID: tenant, ID: subject, TokenDefID: t.DefID})
		if err != nil {
			return true, u, err
		}
		if beyondFloor(tin) {
			return true, u, nil
		}
	}
	return false, u, nil
}

// floorTarget is the verdict on an auth:manage caller acting on an existing
// user.
type floorTarget struct {
	// recovery: the user is above the floor and the gate's
	// manager_may_recover_owners allowed the action.
	recovery   bool
	accessMode string
}

// holdManagerToFloorTarget applies the floor to the TARGET of an auth:manage
// action, not only to what it grants (D16): such a caller may delete,
// disable, re-enable, re-mode, rename, strip or reset only a user at the
// floor. A user above it (the tenant's owner, anyone in a group) is the
// tenant's own operators' to manage — otherwise the website's credential could
// delete or demote an owner, or reset its password and sign in as it. The one
// exception is `recoverable` actions (a reset, attaching a first password) on
// a gate with manager_may_recover_owners, audited as a recovery. Tenant
// operators and admins pass. A missing user passes, so the caller's own 404
// answers it. The refusal is written and audited here; ok=false then.
func (s *Server) holdManagerToFloorTarget(w http.ResponseWriter, ctx context.Context, caller gateCaller, g *PasswordGateSettings, tenant, subject, op string, recoverable bool) (floorTarget, bool) {
	if caller != gateCallerManager {
		return floorTarget{}, true
	}
	above, u, err := s.aboveFloor(ctx, tenant, subject, g)
	if isNotFound(err) {
		return floorTarget{}, true
	}
	if !above {
		return floorTarget{accessMode: u.AccessMode}, true
	}
	if err != nil {
		// Could not tell: refused like a user above the floor, recovery
		// included.
		slog.Warn("authgate: floor check failed (refusing)", "tenant", tenant, "err", err)
	} else if recoverable && g != nil && g.ManagerMayRecoverOwners {
		return floorTarget{recovery: true, accessMode: u.AccessMode}, true
	}
	gate := ""
	if g != nil {
		gate = g.Name
	}
	p, _ := auth.PrincipalFromContext(ctx)
	s.recordAuthAudit(audit.Event{Action: "auth_manage_refused", AuthGate: gate, AuthReason: op,
		TargetTenant: tenant, TargetSubject: subject, AuthTargetAccessMode: u.AccessMode,
		ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix})
	writeJSONError(w, http.StatusForbidden, "target_beyond_floor", "an "+auth.ScopeAuthManage+" token cannot change this user")
	return floorTarget{}, false
}

// --- invites ---

type createInviteRequest struct {
	Tenant     string   `json:"tenant"`
	Subject    string   `json:"subject"`
	AccessMode string   `json:"access_mode"`
	Groups     []string `json:"groups"`
	Login      string   `json:"login"`
	Email      string   `json:"email"`
	TTL        string   `json:"ttl"`
	ReturnTo   string   `json:"return_to"`
}

type createdLink struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// wireInvite is an invite as listed: never its code.
type wireInvite struct {
	InviteID        string     `json:"invite_id"`
	Gate            string     `json:"gate"`
	TenantID        string     `json:"tenant_id"`
	Subject         string     `json:"subject,omitempty"`
	AccessMode      string     `json:"access_mode,omitempty"`
	Groups          []string   `json:"groups"`
	Login           string     `json:"login,omitempty"`
	Email           string     `json:"email,omitempty"`
	ReturnTo        string     `json:"return_to,omitempty"`
	Bootstrap       bool       `json:"bootstrap,omitempty"`
	CreatedBy       string     `json:"created_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	State           string     `json:"state"`
	RedeemedAt      *time.Time `json:"redeemed_at,omitempty"`
	RedeemedSubject string     `json:"redeemed_subject,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toWireInvite(r store.AuthInviteRow, now time.Time) wireInvite {
	state := "live"
	switch {
	case !r.RedeemedAt.IsZero():
		state = "redeemed"
	case !r.RevokedAt.IsZero():
		state = "revoked"
	case !now.Before(r.ExpiresAt):
		state = "expired"
	}
	groups := r.Groups
	if groups == nil {
		groups = []string{}
	}
	return wireInvite{
		InviteID: r.InviteID, Gate: r.Gate, TenantID: r.TenantID, Subject: r.Subject, AccessMode: r.AccessMode,
		Groups: groups, Login: r.Login, Email: r.Email, ReturnTo: r.ReturnTo, Bootstrap: r.Bootstrap,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, State: state,
		RedeemedAt: timePtr(r.RedeemedAt), RedeemedSubject: r.RedeemedSubject, RevokedAt: timePtr(r.RevokedAt),
	}
}

// validReturnTo accepts a path on loomcycle's own origin ("/ui/…", never
// "//host" or a backslash form a browser reads as another host) or an exact
// entry of the gate's allowed_return_to. Anything else is an open redirect.
func validReturnTo(g PasswordGateSettings, v string) bool {
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n\t") {
		return true
	}
	return slices.Contains(g.AllowedReturnTo, v)
}

// handleAuthgateCreateInvite serves POST /v1/_authgate/{gate}/invites.
func (s *Server) handleAuthgateCreateInvite(w http.ResponseWriter, r *http.Request) {
	g, pg, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	var req createInviteRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	tenant, caller, ok := s.manageTenant(w, ctx, req.Tenant)
	if !ok {
		return
	}
	now := time.Now().UTC()
	ttl := g.InviteDefaultTTL
	if strings.TrimSpace(req.TTL) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(req.TTL))
		if err != nil || d <= 0 || d > g.InviteMaxTTL {
			writeJSONError(w, http.StatusBadRequest, "invalid_ttl", fmt.Sprintf("ttl must be a duration between 1s and %s", g.InviteMaxTTL))
			return
		}
		ttl = d
	}
	if !validReturnTo(g, req.ReturnTo) {
		writeJSONError(w, http.StatusBadRequest, "invalid_return_to", "return_to must be a path on this server or one of the gate's allowed_return_to")
		return
	}
	row := store.AuthInviteRow{
		Gate: g.Name, TenantID: tenant, ReturnTo: req.ReturnTo, Email: strings.TrimSpace(req.Email),
		CreatedBy: principalSubject(ctx), CreatedAt: now, ExpiresAt: now.Add(ttl),
		// A website's link is held to the floor again when it is redeemed,
		// and revoked if its target's authority is raised before then.
		IssuedByManager: caller == gateCallerManager,
	}
	if strings.TrimSpace(req.Login) != "" {
		login, err := password.NormalizeLogin(req.Login)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_login", err.Error())
			return
		}
		if _, err := s.store.UserIdentityGet(ctx, g.Name, "", login); err == nil {
			writeJSONError(w, http.StatusConflict, "login_taken", "that login is already in use at this gate")
			return
		} else if !isNotFound(err) {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		row.Login = login
	}
	var grant inviteGrant
	if subject := strings.TrimSpace(req.Subject); subject != "" {
		grant, ok = s.attachGrant(w, ctx, g, caller, tenant, subject, req)
	} else {
		grant, ok = s.newUserGrant(w, ctx, g, caller, tenant, req, now)
	}
	if !ok {
		return
	}
	row.Subject, row.AccessMode, row.Groups, row.Bootstrap = grant.subject, grant.accessMode, grant.groups, grant.bootstrap
	code, hash, err := password.NewCode(password.InvitePrefix)
	if err == nil {
		row.InviteID, err = password.NewID("inv_")
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not generate the invite")
		return
	}
	row.CodeHash = hash
	if err := s.store.AuthInviteCreate(ctx, row); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordGateAudit(ctx, "auth_invite_created", g.Name, row.InviteID, tenant, row.Subject, floorTarget{recovery: grant.recovery, accessMode: grant.targetMode})
	writeJSON(w, http.StatusCreated, map[string]any{
		"invite_id": row.InviteID, "url": pg.publicURL + "/ui/invite#" + code, "expires_at": row.ExpiresAt,
	})
}

type inviteGrant struct {
	subject    string
	accessMode string
	groups     []string
	bootstrap  bool
	// recovery and targetMode are recorded on the audit event of an
	// auth:manage attach to a user above the floor.
	recovery   bool
	targetMode string
}

// newUserGrant decides what an invite for a NEW user may grant.
//
// The floor (RFC FA §3.7a): access_mode isolated and no groups. A tenant
// operator or an admin may go beyond it only on a gate marked trusted. An
// auth:manage credential never may, with one exception: the owner invite of
// a tenant with no user yet (bootstrap), which may grant access_mode tenant
// and nothing else, and of which only one may be live at a time.
func (s *Server) newUserGrant(w http.ResponseWriter, ctx context.Context, g PasswordGateSettings, caller gateCaller, tenant string, req createInviteRequest, now time.Time) (inviteGrant, bool) {
	mode := strings.TrimSpace(req.AccessMode)
	if mode == "" {
		mode = floorOf(&g).accessMode
	}
	if !store.ValidUserAccessMode(mode) {
		writeJSONError(w, http.StatusBadRequest, "invalid_access_mode", "access_mode must be one of tenant|isolated")
		return inviteGrant{}, false
	}
	groups, ok := s.existingGroups(w, ctx, tenant, req.Groups)
	if !ok {
		return inviteGrant{}, false
	}
	f := floorOf(&g)
	beyondFloor := accessRank(mode) > accessRank(f.accessMode)
	for _, gr := range groups {
		if !slices.Contains(f.groups, gr) {
			beyondFloor = true
		}
	}
	if !beyondFloor {
		return inviteGrant{accessMode: mode, groups: groups}, true
	}
	if g.Trusted && caller != gateCallerManager {
		return inviteGrant{accessMode: mode, groups: groups}, true
	}
	if caller == gateCallerManager && mode == "tenant" && len(groups) == 0 {
		has, err := s.tenantBootstrapped(ctx, tenant)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return inviteGrant{}, false
		}
		live, err := s.store.AuthInviteLiveBootstrap(ctx, tenant, now)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return inviteGrant{}, false
		}
		switch {
		case has:
			writeJSONError(w, http.StatusForbidden, "tenant_not_empty",
				"access_mode tenant is allowed only for the first user of a tenant that never had one")
			return inviteGrant{}, false
		case live:
			writeJSONError(w, http.StatusConflict, "bootstrap_invite_live",
				"this tenant already has a live owner invite; revoke it first")
			return inviteGrant{}, false
		}
		return inviteGrant{accessMode: mode, bootstrap: true}, true
	}
	writeJSONError(w, http.StatusForbidden, "beyond_floor",
		"an invite through this gate may grant only the gate's new-user access mode and groups (mark the gate trusted to lift this)")
	return inviteGrant{}, false
}

// attachGrant checks an invite that attaches a password to an existing user.
// The user keeps its subject, access mode and groups: the invite may restate
// them but never raise them — an attach is never a promotion.
func (s *Server) attachGrant(w http.ResponseWriter, ctx context.Context, g PasswordGateSettings, caller gateCaller, tenant, subject string, req createInviteRequest) (inviteGrant, bool) {
	user, err := s.store.UserGet(ctx, tenant, subject)
	if isNotFound(err) {
		writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
		return inviteGrant{}, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return inviteGrant{}, false
	}
	if has, err := s.hasGateCredential(ctx, g.Name, tenant, subject); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return inviteGrant{}, false
	} else if has {
		writeJSONError(w, http.StatusConflict, "credential_exists",
			"this user already has a password or a login at this gate; issue a reset instead")
		return inviteGrant{}, false
	}
	// Attaching a password to a user above the floor is a recovery: only
	// with manager_may_recover_owners, and only here, where the user has no
	// credential at this gate yet (the legacy-owner migration).
	target, ok := s.holdManagerToFloorTarget(w, ctx, caller, &g, tenant, subject, "attach", true)
	if !ok {
		return inviteGrant{}, false
	}
	if m := strings.TrimSpace(req.AccessMode); m != "" && accessRank(m) > accessRank(user.AccessMode) {
		writeJSONError(w, http.StatusForbidden, "promotion_refused", "an invite for an existing user cannot raise its access_mode")
		return inviteGrant{}, false
	}
	if len(req.Groups) > 0 {
		have, err := s.store.GroupsOfSubject(ctx, tenant, subject)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return inviteGrant{}, false
		}
		for _, gr := range req.Groups {
			if !slices.Contains(have, strings.TrimSpace(gr)) {
				writeJSONError(w, http.StatusForbidden, "promotion_refused", "an invite for an existing user cannot add it to a group")
				return inviteGrant{}, false
			}
		}
	}
	return inviteGrant{subject: user.Subject, recovery: target.recovery, targetMode: target.accessMode}, true
}

// accessRank orders access modes from least to most reach.
func accessRank(mode string) int {
	if mode == "tenant" {
		return 1
	}
	return 0
}

// hasGateCredential reports whether the subject already has a login at the
// gate or a stored password.
func (s *Server) hasGateCredential(ctx context.Context, gate, tenant, subject string) (bool, error) {
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id.Gate == gate {
			return true, nil
		}
	}
	_, err = s.store.UserCredentialGet(ctx, tenant, subject, store.CredentialKindPassword)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// existingGroups trims and de-duplicates names and refuses a group that does
// not exist in the tenant (so an invite cannot be made today for a group an
// operator creates tomorrow with other bindings).
func (s *Server) existingGroups(w http.ResponseWriter, ctx context.Context, tenant string, names []string) ([]string, bool) {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if _, err := s.store.GroupGet(ctx, tenant, n); err != nil {
			if isNotFound(err) {
				writeJSONError(w, http.StatusBadRequest, "unknown_group", "no group "+n+" in this tenant")
			} else {
				writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			}
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// handleAuthgateListInvites serves GET /v1/_authgate/{gate}/invites. An admin
// naming no tenant lists every tenant's; nobody ever sees a code.
func (s *Server) handleAuthgateListInvites(w http.ResponseWriter, r *http.Request) {
	g, _, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	wire := r.URL.Query().Get("tenant")
	tenant, caller, ok := s.manageTenant(w, ctx, wire)
	if !ok {
		return
	}
	// An admin naming no tenant sees every tenant's, as on the user roster —
	// by its own store method, so no tenant value (the shared tenant is "")
	// can stand for "all".
	var rows []store.AuthInviteRow
	var err error
	if caller == gateCallerAdmin && strings.TrimSpace(wire) == "" {
		rows, err = s.store.AuthInviteListAllTenants(ctx, g.Name)
	} else {
		rows, err = s.store.AuthInviteList(ctx, g.Name, tenant)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	now := time.Now()
	out := make([]wireInvite, 0, len(rows))
	for _, row := range rows {
		out = append(out, toWireInvite(row, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

// handleAuthgateRevokeInvite serves DELETE /v1/_authgate/{gate}/invites/{id}.
// Another tenant's invite is an opaque 404.
func (s *Server) handleAuthgateRevokeInvite(w http.ResponseWriter, r *http.Request) {
	g, _, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	tenant, caller, ok := s.manageTenant(w, ctx, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	row, err := s.store.AuthInviteGet(ctx, r.PathValue("id"))
	if err != nil && !isNotFound(err) {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// An admin may revoke an invite of any tenant by id alone.
	visible := err == nil && row.Gate == g.Name && (row.TenantID == tenant || (caller == gateCallerAdmin && r.URL.Query().Get("tenant") == ""))
	if !visible {
		writeJSONError(w, http.StatusNotFound, "invite_not_found", "no such invite")
		return
	}
	revoked, err := s.store.AuthInviteRevoke(ctx, row.InviteID, time.Now().UTC())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !revoked {
		writeJSONError(w, http.StatusConflict, "invite_not_live", "the invite was already redeemed, revoked or expired")
		return
	}
	s.recordGateAudit(ctx, "auth_invite_revoked", g.Name, row.InviteID, row.TenantID, row.Subject, floorTarget{})
	w.WriteHeader(http.StatusNoContent)
}

// --- resets ---

type createResetRequest struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
}

// handleAuthgateCreateReset serves POST /v1/_authgate/{gate}/resets: a
// one-time link that sets a new password for a user who has a login at this
// gate. It replaces the "admin sets a temporary password" of the original
// RFC (D17): nobody but the person ever types the password.
func (s *Server) handleAuthgateCreateReset(w http.ResponseWriter, r *http.Request) {
	g, pg, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	var req createResetRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	tenant, caller, ok := s.manageTenant(w, ctx, req.Tenant)
	if !ok {
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	if _, ok := s.gateIdentity(w, ctx, g.Name, tenant, subject); !ok {
		return
	}
	target, ok := s.holdManagerToFloorTarget(w, ctx, caller, &g, tenant, subject, "reset", true)
	if !ok {
		return
	}
	now := time.Now().UTC()
	code, hash, err := password.NewCode(password.ResetPrefix)
	var id string
	if err == nil {
		id, err = password.NewID("rst_")
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not generate the reset")
		return
	}
	row := store.AuthResetRow{ResetID: id, CodeHash: hash, Gate: g.Name, TenantID: tenant, Subject: subject,
		CreatedBy: principalSubject(ctx), CreatedAt: now, ExpiresAt: now.Add(g.ResetTTL),
		IssuedByManager: caller == gateCallerManager}
	if err := s.store.AuthResetCreate(ctx, row); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordGateAudit(ctx, "auth_reset_issued", g.Name, id, tenant, subject, target)
	writeJSON(w, http.StatusCreated, map[string]any{
		"reset_id": id, "url": pg.publicURL + "/ui/reset#" + code, "expires_at": row.ExpiresAt,
	})
}

// gateIdentity returns the subject's login at the gate; an opaque 404 when
// the user does not exist in the tenant or has no login there.
func (s *Server) gateIdentity(w http.ResponseWriter, ctx context.Context, gate, tenant, subject string) (store.UserIdentityRow, bool) {
	if _, err := s.store.UserGet(ctx, tenant, subject); err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "login_not_found", "no such user with a login at this gate")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return store.UserIdentityRow{}, false
	}
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return store.UserIdentityRow{}, false
	}
	for _, id := range ids {
		if id.Gate == gate {
			return id, true
		}
	}
	writeJSONError(w, http.StatusNotFound, "login_not_found", "no such user with a login at this gate")
	return store.UserIdentityRow{}, false
}

// --- credentials, logins, identities ---

// handleAuthgateDeleteCredentials serves DELETE
// /v1/_authgate/{gate}/credentials/{subject}: the user's password, TOTP,
// recovery codes and passkeys go, and the login stays reserved so a later
// reset restores it.
func (s *Server) handleAuthgateDeleteCredentials(w http.ResponseWriter, r *http.Request) {
	g, _, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	tenant, caller, ok := s.manageTenant(w, ctx, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if _, ok := s.gateIdentity(w, ctx, g.Name, tenant, subject); !ok {
		return
	}
	if _, ok := s.holdManagerToFloorTarget(w, ctx, caller, &g, tenant, subject, "remove_credentials", false); !ok {
		return
	}
	removed, err := s.store.UserCredentialDelete(ctx, tenant, subject, store.CredentialKindPassword)
	if err == nil {
		_, err = s.store.UserTOTPDelete(ctx, tenant, subject)
	}
	// Passkeys too (RFC FA P6): a lost authenticator is recovered the same
	// way as a lost phone, and its passkey sessions end with it.
	var keys int
	if err == nil {
		keys, err = s.store.UserPasskeyDeleteSubject(ctx, tenant, subject)
	}
	removed = removed || keys > 0
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if removed {
		s.recordGateAudit(ctx, "auth_credential_removed", g.Name, "", tenant, subject, floorTarget{})
	}
	w.WriteHeader(http.StatusNoContent)
}

type renameLoginRequest struct {
	Tenant string `json:"tenant"`
	Login  string `json:"login"`
}

// handleAuthgateRenameLogin serves PATCH /v1/_authgate/{gate}/logins/{subject}.
// The subject does not change, so nothing the user owns moves.
func (s *Server) handleAuthgateRenameLogin(w http.ResponseWriter, r *http.Request) {
	g, _, ok := s.passwordGate(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	var req renameLoginRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	tenant, caller, ok := s.manageTenant(w, ctx, req.Tenant)
	if !ok {
		return
	}
	login, err := password.NormalizeLogin(req.Login)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_login", err.Error())
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if _, ok := s.gateIdentity(w, ctx, g.Name, tenant, subject); !ok {
		return
	}
	if _, ok := s.holdManagerToFloorTarget(w, ctx, caller, &g, tenant, subject, "rename_login", false); !ok {
		return
	}
	if err := s.store.UserIdentityRename(ctx, tenant, subject, g.Name, login); err != nil {
		var conflict *store.ErrConflict
		switch {
		case errors.As(err, &conflict):
			writeJSONError(w, http.StatusConflict, "login_taken", "that login is already in use at this gate")
		case isNotFound(err):
			writeJSONError(w, http.StatusNotFound, "login_not_found", "no such user with a login at this gate")
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	s.recordGateAudit(ctx, "auth_login_renamed", g.Name, "", tenant, subject, floorTarget{})
	writeJSON(w, http.StatusOK, map[string]any{"subject": subject, "login": login})
}

type wireIdentity struct {
	Gate          string     `json:"gate"`
	Login         string     `json:"login"`
	Email         string     `json:"email,omitempty"`
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	HasPassword   bool       `json:"has_password"`
	HasTOTP       bool       `json:"has_totp"`
}

// handleAuthgateListIdentities serves GET
// /v1/_authgate/{gate}/identities?subject=: the user's logins at every gate
// and whether it has a password and a confirmed TOTP — never a secret.
func (s *Server) handleAuthgateListIdentities(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.passwordGate(w, r); !ok || !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	tenant, _, ok := s.manageTenant(w, ctx, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	if _, err := s.store.UserGet(ctx, tenant, subject); err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	_, perr := s.store.UserCredentialGet(ctx, tenant, subject, store.CredentialKindPassword)
	totp, terr := s.store.UserTOTPGet(ctx, tenant, subject)
	for _, err := range []error{perr, terr} {
		if err != nil && !isNotFound(err) {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	out := make([]wireIdentity, 0, len(ids))
	for _, id := range ids {
		out = append(out, wireIdentity{Gate: id.Gate, Login: id.ExternalSubject, Email: id.Email, EmailVerified: id.EmailVerified,
			CreatedAt: id.CreatedAt, LastLoginAt: timePtr(id.LastLoginAt),
			HasPassword: perr == nil, HasTOTP: terr == nil && !totp.ConfirmedAt.IsZero()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant_id": tenant, "subject": subject, "identities": out})
}

// handleAuthgateTenantStatus serves GET /v1/_authgate/{gate}/tenant-status:
// whether the tenant has (or ever had) a user and whether an owner invite is
// live —
// what a website needs to decide on a bootstrap invite, and nothing about
// who the users are.
func (s *Server) handleAuthgateTenantStatus(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.passwordGate(w, r); !ok || !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	tenant, _, ok := s.manageTenant(w, ctx, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	booted, err := s.tenantBootstrapped(ctx, tenant)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	users, err := s.store.UserList(ctx, tenant)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	live, err := s.store.AuthInviteLiveBootstrap(ctx, tenant, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// bootstrapped (ever had a user) is what decides an owner bootstrap;
	// has_users is whether it has one now.
	writeJSON(w, http.StatusOK, map[string]any{"tenant_id": tenant, "has_users": len(users) > 0,
		"bootstrapped": booted, "bootstrap_invite_live": live})
}

// requireStore answers 503 when the server runs without a store.
func (s *Server) requireStore(w http.ResponseWriter) bool {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; the password gate requires a persistent store")
		return false
	}
	return true
}

// recordGateAudit writes one password-gate management record: never a code,
// a link or a password. A recovery (see holdManagerToFloorTarget) is marked
// with the target's access mode.
func (s *Server) recordGateAudit(ctx context.Context, action, gate, recordID, tenant, subject string, t floorTarget) {
	p, _ := auth.PrincipalFromContext(ctx)
	ev := audit.Event{Action: action, AuthGate: gate, AuthRecordID: recordID, TargetTenant: tenant, TargetSubject: subject,
		ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix}
	if t.recovery {
		ev.AuthRecovery, ev.AuthTargetAccessMode = true, t.accessMode
	}
	s.recordAuthAudit(ev)
}

// recordAuthAudit writes an audit record best-effort: it never fails the
// request.
func (s *Server) recordAuthAudit(ev audit.Event) {
	if s.tokenAudit == nil {
		return
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("authgate: audit write failed", "action", ev.Action, "err", err)
	}
}
