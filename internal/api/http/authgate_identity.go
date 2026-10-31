package http

// authgate_identity.go — RFC FA §3.3 / §3.7: what loomcycle does with a
// verified external identity, for every gate kind that has an identity map
// (OIDC now; header, AWS IAM, OAuth, device and external gates next). Not
// pluggable on purpose: a gate decides WHO, and this file alone decides the
// tenant, the subject, whether a user is created, which groups follow, and
// whether the person may come in at all.
//
// The order of checks is the security argument:
//  1. the identity map's tenant is resolved FIRST, every login, so a tenant
//     an operator removed from allowed_tenants locks its people out even
//     when their identity is already linked;
//  2. a linked identity (a user_identities row) keeps the subject it names —
//     the only way one person's identity reaches another subject is an
//     explicit link (D14), never an e-mail match;
//  3. a disabled user is refused whatever the gate says;
//  4. only a trusted gate may assert an access_mode or sync groups (D12),
//     and sync touches only the gate's own "<gate>:" groups (D6).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/identity"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// resolveAction says what a login through an identity gate will do.
type resolveAction int

const (
	// actionLogin: the identity is linked already.
	actionLogin resolveAction = iota + 1
	// actionLink: the user exists (an operator registered it); the identity
	// is linked at this first login.
	actionLink
	// actionLinkLegacy: actionLink to the user a bridge made before it was
	// a gate (identity_map.legacy_subject_prefix).
	actionLinkLegacy
	// actionCreate: neither exists and the gate provisions (auto).
	actionCreate
)

var (
	// errIdentityRefused: the identity may not come in through this gate.
	// The reason is logged and audited, never returned to the browser.
	errIdentityRefused = errors.New("auth gate: identity refused")
	// errUserDisabled: the identity maps to a disabled user.
	errUserDisabled = errors.New("auth gate: user disabled")
)

func refused(reason string) error { return fmt.Errorf("%w: %s", errIdentityRefused, reason) }

// identityLogin is the outcome of provisionOrRefuse.
type identityLogin struct {
	TenantID string
	Subject  string
	// Pending: the user awaits approval; its session answers no to every
	// access question.
	Pending bool
	// Created: the user was provisioned by this login.
	Created bool
}

// identityGate returns the configured gate of that name when it maps
// external identities (it has an identity map).
func (s *Server) identityGate(name string) (config.AuthGate, bool) {
	g, ok := s.configuredGate(name)
	if !ok || g.Identity == nil {
		return config.AuthGate{}, false
	}
	return g, true
}

// resolveAssertion maps an assertion to the (tenant, subject) it signs in as
// and what that takes. It writes nothing.
func (s *Server) resolveAssertion(ctx context.Context, g config.AuthGate, a authgate.Assertion) (identity.Target, resolveAction, error) {
	m := g.Identity
	if m == nil {
		return identity.Target{}, 0, refused("the gate has no identity map")
	}
	ext, err := identity.ExternalSubject(a)
	if err != nil {
		return identity.Target{}, 0, refused("no usable subject")
	}
	fresh, err := identity.Resolve(g.Name, m, a)
	if err != nil {
		if errors.Is(err, identity.ErrTenantRefused) {
			return identity.Target{}, 0, refused("tenant not allowed")
		}
		return identity.Target{}, 0, refused("no usable subject")
	}
	row, err := s.store.UserIdentityGet(ctx, g.Name, a.Issuer, ext)
	switch {
	case err == nil:
		if row.TenantID != fresh.TenantID {
			// Linked into a tenant the map no longer gives this identity.
			return identity.Target{}, 0, refused("linked identity is outside the gate's tenants")
		}
		return identity.Target{TenantID: row.TenantID, Subject: row.Subject}, actionLogin, nil
	case !isNotFound(err):
		return identity.Target{}, 0, err
	}
	if m.Provision == config.ProvisionNever {
		return identity.Target{}, 0, refused("unknown identity (provision: never)")
	}
	_, err = s.store.UserGet(ctx, fresh.TenantID, fresh.Subject)
	switch {
	case err == nil:
		return fresh, actionLink, nil
	case !isNotFound(err):
		return identity.Target{}, 0, err
	}
	if legacy, ok, err := s.legacyUser(ctx, g, fresh); err != nil {
		return identity.Target{}, 0, err
	} else if ok {
		return legacy, actionLinkLegacy, nil
	}
	if m.Provision != config.ProvisionAuto {
		return identity.Target{}, 0, refused("unknown user (provision: known)")
	}
	return fresh, actionCreate, nil
}

// legacyUser finds the user a bridge made for this identity before it was a
// gate. Only a user nothing else signs in as qualifies: one with a password
// or an identity at any gate is a person's own account, and the prefix is the
// operator's word about the bridge's subjects, not about theirs.
func (s *Server) legacyUser(ctx context.Context, g config.AuthGate, fresh identity.Target) (identity.Target, bool, error) {
	subject, ok := identity.LegacySubject(g.Name, g.Identity, fresh.Subject)
	if !ok {
		return identity.Target{}, false, nil
	}
	if _, err := s.store.UserGet(ctx, fresh.TenantID, subject); err != nil {
		if isNotFound(err) {
			return identity.Target{}, false, nil
		}
		return identity.Target{}, false, err
	}
	ids, err := s.store.UserIdentityListSubject(ctx, fresh.TenantID, subject)
	if err != nil {
		return identity.Target{}, false, err
	}
	if len(ids) > 0 {
		return identity.Target{}, false, nil
	}
	switch _, err := s.store.UserCredentialGet(ctx, fresh.TenantID, subject, store.CredentialKindPassword); {
	case err == nil:
		return identity.Target{}, false, nil
	case !isNotFound(err):
		return identity.Target{}, false, err
	}
	return identity.Target{TenantID: fresh.TenantID, Subject: subject}, true, nil
}

// provisionOrRefuse resolves an assertion and makes it a user: it creates the
// user (auto) at the lifecycle defaults, links the identity, refuses a
// disabled user, applies a trusted gate's access_mode and syncs its groups.
// Every refusal is audited as auth_login_failed with its reason.
func (s *Server) provisionOrRefuse(ctx context.Context, g config.AuthGate, a authgate.Assertion, source string) (identityLogin, error) {
	out, err := s.provision(ctx, g, a)
	if err != nil && (errors.Is(err, errIdentityRefused) || errors.Is(err, errUserDisabled)) {
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: g.Name, AuthReason: err.Error(),
			TargetTenant: out.TenantID, TargetSubject: out.Subject, SourceAddr: source})
	}
	return out, err
}

func (s *Server) provision(ctx context.Context, g config.AuthGate, a authgate.Assertion) (identityLogin, error) {
	if s.store == nil {
		return identityLogin{}, errSessionsUnavailable
	}
	tgt, action, err := s.resolveAssertion(ctx, g, a)
	if err != nil {
		return identityLogin{}, err
	}
	out := identityLogin{TenantID: tgt.TenantID, Subject: tgt.Subject}
	m := g.Identity
	ext, _ := identity.ExternalSubject(a)
	now := time.Now().UTC()
	if action == actionCreate {
		mode := m.NewUser.AccessMode
		if m.Trusted && store.ValidUserAccessMode(a.AccessMode) {
			mode = a.AccessMode
		}
		row := store.UserRow{TenantID: tgt.TenantID, Subject: tgt.Subject, DisplayName: identity.DisplayName(a),
			AccessMode: mode, Status: m.NewUser.Status, CreatedAt: now, CreatedBy: "gate:" + g.Name}
		err := s.store.UserCreate(ctx, row)
		var conflict *store.ErrConflict
		switch {
		case err == nil:
			out.Created = true
		case errors.As(err, &conflict):
			// A concurrent first login made it: link to it like a known user.
		default:
			return out, err
		}
	}
	user, err := s.store.UserGet(ctx, tgt.TenantID, tgt.Subject)
	if err != nil {
		if isNotFound(err) {
			return out, refused("the user is gone")
		}
		return out, err
	}
	if user.Status == "disabled" {
		return out, fmt.Errorf("%w: %s", errUserDisabled, "the user is disabled")
	}
	if action != actionLogin {
		err := s.store.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: tgt.TenantID, Subject: tgt.Subject,
			Gate: g.Name, Issuer: a.Issuer, ExternalSubject: ext, Email: a.Email, EmailVerified: a.EmailVerified, CreatedAt: now})
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			// Either a concurrent first login linked the same identity, or the
			// subject already has a DIFFERENT identity at this gate. Only the
			// first may continue.
			row, gerr := s.store.UserIdentityGet(ctx, g.Name, a.Issuer, ext)
			if gerr != nil || row.TenantID != tgt.TenantID || row.Subject != tgt.Subject {
				return out, refused("the user is linked to another identity at this gate")
			}
		} else if err != nil {
			return out, err
		}
	}
	if action == actionLinkLegacy {
		s.recordAuthEvent(audit.Event{Action: "auth_identity_linked", AuthGate: g.Name, TargetTenant: tgt.TenantID,
			TargetSubject: tgt.Subject, AuthReason: "legacy subject"})
	}
	if out.Created {
		s.joinFloorGroups(ctx, g, tgt, m.NewUser.Groups)
		s.ProvisionIdentityDocs(ctx, tgt.TenantID, tgt.Subject)
		s.recordAuthEvent(audit.Event{Action: "auth_provisioned", AuthGate: g.Name, TargetTenant: tgt.TenantID,
			TargetSubject: tgt.Subject, AuthTargetAccessMode: user.AccessMode, AuthReason: "status " + user.Status})
	} else if m.Trusted && store.ValidUserAccessMode(a.AccessMode) && a.AccessMode != user.AccessMode {
		mode := a.AccessMode
		if err := s.store.UserUpdate(ctx, tgt.TenantID, tgt.Subject, store.UserPatch{AccessMode: &mode}); err != nil {
			return out, err
		}
		s.recordAuthEvent(audit.Event{Action: "auth_access_mode_synced", AuthGate: g.Name, TargetTenant: tgt.TenantID,
			TargetSubject: tgt.Subject, AuthTargetAccessMode: mode})
	}
	if m.SyncGroups {
		if _, _, err := s.syncGroups(ctx, g, tgt.TenantID, tgt.Subject, identity.AssertedGroups(m, a)); err != nil {
			// The login does not go ahead on a half-known membership: a
			// failed removal could leave access the provider took away.
			return out, err
		}
	}
	if err := s.store.UserIdentityTouchLogin(ctx, g.Name, a.Issuer, ext, now); err != nil {
		slog.Warn("auth gate: recording the login time failed", "gate", g.Name, "err", err)
	}
	out.Pending = user.Status == store.UserStatusPending
	return out, nil
}

// joinFloorGroups adds a provisioned user to those of its new_user groups
// that exist in the tenant (as the password gate does). A group that does
// not exist is not created: a floor group is the operator's, with the
// bindings they chose.
func (s *Server) joinFloorGroups(ctx context.Context, g config.AuthGate, tgt identity.Target, groups []string) {
	added := false
	for _, n := range groups {
		err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tgt.TenantID, GroupName: n,
			MemberKind: store.GroupMemberUser, MemberID: tgt.Subject, AddedBy: "gate:" + g.Name})
		var conflict *store.ErrConflict
		switch {
		case err == nil:
			added = true
		case isNotFound(err), errors.As(err, &conflict):
		default:
			slog.Warn("auth gate: adding a provisioned user to a floor group failed", "group", n, "err", err)
		}
	}
	if added {
		s.aclChanged(ctx, tgt.TenantID, "group_member_added", "gate:"+g.Name)
	}
}

// syncGroups reconciles the user's memberships in the gate's own groups with
// what the gate asserts (§3.7b, D6): adds AND removes, never touching a local
// group or another gate's. Audited as auth_groups_synced, and the tenant's
// cached ACL sets are refreshed when anything changed.
func (s *Server) syncGroups(ctx context.Context, g config.AuthGate, tenant, subject string, asserted []string) (added, removed []string, err error) {
	m := g.Identity
	if m == nil || !m.SyncGroups || !m.Trusted {
		return nil, nil, nil
	}
	all, err := s.store.GroupList(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	added, removed, _, err = s.syncGroupsWith(ctx, g, tenant, subject, asserted, all)
	return added, removed, err
}

// syncGroupsWith is syncGroups over a group listing the caller read (one
// listing for many subjects); created reports that it made a group, so the
// caller's listing is stale.
func (s *Server) syncGroupsWith(ctx context.Context, g config.AuthGate, tenant, subject string, asserted []string,
	all []store.GroupRow) (added, removed []string, created bool, err error) {
	m := g.Identity
	if m == nil || !m.SyncGroups || !m.Trusted {
		return nil, nil, false, nil
	}
	current, err := s.store.GroupsOfSubject(ctx, tenant, subject)
	if err != nil {
		return nil, nil, false, err
	}
	groups := make([]identity.Group, len(all))
	for i, r := range all {
		groups[i] = identity.Group{TenantID: r.TenantID, Name: r.Name, ExternalID: r.ExternalID, Description: r.Description, CreatedBy: r.CreatedBy}
	}
	plan := identity.SyncPlan(g.Name, tenant, m, asserted, groups, current)
	changed := false
	defer func() {
		if changed {
			s.aclChanged(ctx, tenant, "auth_groups_synced", subject)
			s.recordAuthEvent(audit.Event{Action: "auth_groups_synced", AuthGate: g.Name, TargetTenant: tenant,
				TargetSubject: subject, AuthGroupsAdded: added, AuthGroupsRemoved: removed})
		}
	}()
	made := map[string]bool{}
	for _, c := range plan.Create {
		row := store.GroupRow{TenantID: c.TenantID, Name: c.Name, ExternalID: c.ExternalID, Description: c.Description, CreatedBy: c.CreatedBy}
		ok, err := s.store.GroupCreate(ctx, row)
		if err != nil {
			return added, removed, len(made) > 0, err
		}
		if ok {
			changed = true
			made[row.Name] = true
		}
	}
	// Removals first: a failure part-way leaves the user with less than the
	// provider says, never more.
	for _, name := range plan.Remove {
		ok, err := s.store.GroupMemberRemove(ctx, tenant, name, store.GroupMemberUser, subject)
		if err != nil {
			return added, removed, len(made) > 0, err
		}
		if ok {
			changed = true
			removed = append(removed, name)
		}
	}
	for _, name := range plan.Add {
		if !made[name] && !s.groupStillGates(ctx, g.Name, tenant, name) {
			continue
		}
		err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: name,
			MemberKind: store.GroupMemberUser, MemberID: subject, AddedBy: "gate:" + g.Name})
		var conflict *store.ErrConflict
		switch {
		case err == nil:
			changed = true
			added = append(added, name)
		case errors.As(err, &conflict):
		default:
			return added, removed, len(made) > 0, err
		}
	}
	return added, removed, len(made) > 0, nil
}

// groupStillGates re-reads a group the plan adds to: the plan was made from a
// listing, and a group whose external_id changed meanwhile is no longer this
// gate's to fill.
func (s *Server) groupStillGates(ctx context.Context, gate, tenant, name string) bool {
	row, err := s.store.GroupGet(ctx, tenant, name)
	if err != nil {
		return false
	}
	return len(row.ExternalID) > len(gate) && row.ExternalID[:len(gate)+1] == gate+":"
}

// credKindIdentity binds a session to the user_identities row it signed in
// through and to the user row that identity names (RFC FA §3.3 step 4). The
// binding MAC covers the identity's issuer, external subject and creation
// time, and the user row's creation time, so unlinking the identity (or
// linking a different provider account in its place), deleting the user
// (or deleting and re-creating one of the same subject) ends the session on
// its next request. Status is read live: disabled ends it, pending makes it
// answer no to everything.
const credKindIdentity = "identity"

// identityBindingHash is what an identity session's MAC covers.
func identityBindingHash(id store.UserIdentityRow, u store.UserRow) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("identity\x00%s\x00%s\x00%s\x00%d\x00%d",
		id.Gate, id.Issuer, id.ExternalSubject, id.CreatedAt.UnixNano(), u.CreatedAt.UnixNano())))
	return hex.EncodeToString(sum[:])
}

// sessionIdentityRow is the subject's identity at the gate, as stored.
func (s *Server) sessionIdentityRow(ctx context.Context, tenant, subject, gate string) (store.UserIdentityRow, error) {
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		return store.UserIdentityRow{}, err
	}
	for _, id := range ids {
		if id.Gate == gate {
			return id, nil
		}
	}
	return store.UserIdentityRow{}, &store.ErrNotFound{Kind: "user_identity", ID: subject}
}

// identitySessionBinding is an identity-gate session's binding.
func (s *Server) identitySessionBinding(ctx context.Context, ident sessionIdentity) (kind, ref, credHash string, err error) {
	id, err := s.sessionIdentityRow(ctx, ident.TenantID, ident.Subject, ident.Gate)
	if err != nil {
		return "", "", "", err
	}
	u, err := s.store.UserGet(ctx, ident.TenantID, ident.Subject)
	if err != nil {
		return "", "", "", err
	}
	if u.Status == "disabled" {
		return "", "", "", errUserDisabled
	}
	return credKindIdentity, "", identityBindingHash(id, u), nil
}

// identitySessionPrincipal re-resolves an identity-gate session: the gate is
// still configured with an identity map, the identity and the user are the
// ones the session was made from, and the user is not disabled. A pending
// user's principal holds no scope and an empty ACL set, so every access
// question is NoMatch; the middleware then admits it to GET /v1/_me alone.
func (s *Server) identitySessionPrincipal(ctx context.Context, row store.UISessionRow, bound func(string) bool, pending *bool) (auth.Principal, bool, bool) {
	if _, ok := s.identityGate(row.Gate); !ok {
		return auth.Principal{}, false, true
	}
	id, err := s.sessionIdentityRow(ctx, row.TenantID, row.Subject, row.Gate)
	if err != nil {
		return auth.Principal{}, false, isNotFound(err)
	}
	u, err := s.store.UserGet(ctx, row.TenantID, row.Subject)
	if err != nil {
		return auth.Principal{}, false, isNotFound(err)
	}
	if !bound(identityBindingHash(id, u)) {
		return auth.Principal{}, false, true
	}
	switch u.Status {
	case "active":
		return s.userSessionPrincipal(ctx, u)
	case store.UserStatusPending:
		return pendingSessionPrincipal(u, pending)
	}
	return auth.Principal{}, false, true
}

// pendingSessionPrincipal is the session principal of a user awaiting
// approval, for every gate that signs one in (identity and password): no
// scope and an empty ACL set, so every access question is NoMatch, and
// *pending set so the middleware admits it to GET /v1/_me alone.
func pendingSessionPrincipal(u store.UserRow, pending *bool) (auth.Principal, bool, bool) {
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: u.TenantID, ID: u.Subject}, Now: time.Now()})
	if err != nil {
		return auth.Principal{}, false, false
	}
	*pending = true
	return auth.Principal{TenantID: u.TenantID, Subject: u.Subject, Scopes: []string{}, Effective: eff}, true, true
}
