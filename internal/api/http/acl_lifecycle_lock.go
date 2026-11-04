package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The list lock outside the ACL management routes. Deleting, re-enabling or
// erasing a user, and the removal policy's unbind, drop the user's own
// bindings and its group memberships just as retiring a binding or leaving
// a group does, so the same lock (a deny on retire def:acl/<name>) holds
// them (security review of RFC FY P3).
//
// These paths ask for an explicit DENY, whatever the rollout mode, rather
// than for an allow as the management routes do: their callers (an
// auth:manage credential, a SCIM provisioner, the sweeper) manage users,
// not lists, and their baselines grant nothing on def:acl, so asking for an
// allow would refuse every such call under enforce. The lock is a deny, and
// a deny is what is honoured.

// lifecycleLockSubject is the subject the removal policy is judged as when
// no principal acts: a tenant operator of the tenant, so a list locked
// against the tenant's operators (a tenant-wide deny on retiring it) is
// locked against the sweeper too. No user can sign in as it; a binding
// someone writes to it can only add denies, which makes the sweeper keep
// more, never drop more.
const lifecycleLockSubject = "system:auth-lifecycle"

// aclSubjectLists names, sorted, the lists that reach subject in tenant
// through its own live user bindings and through every stored group it is
// a direct member of (each with the groups containing it, aclGroupLists):
// what deleting, re-enabling or erasing the user would take from it.
func (s *Server) aclSubjectLists(ctx context.Context, tenant, subject string) ([]string, error) {
	rows, err := s.store.AclBindingList(ctx, tenant, false)
	if err != nil {
		return nil, fmt.Errorf("bindings: %w", err)
	}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, row := range rows {
		if row.SubjectKind == store.GroupMemberUser && row.SubjectID == subject {
			add(row.ACLName)
		}
	}
	groups, err := s.store.GroupsOfSubject(ctx, tenant, subject)
	if err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	for _, g := range groups {
		lists, err := s.aclGroupLists(ctx, tenant, g)
		if err != nil {
			return nil, err
		}
		for _, name := range lists {
			add(name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// aclRetireDenied returns the first of names whose retire def:acl/<name>
// eff explicitly denies, with the surface and decision. Nothing is
// recorded: the caller decides what a deny means.
func aclRetireDenied(ctx context.Context, eff *authz.Effective, names []string) (authz.Surface, authz.Decision, bool) {
	for _, name := range names {
		surface := authz.Surface{Action: "retire", Type: "def", Name: "acl/" + name}
		d := eff.Decide(authz.Request{Subject: eff.Subject, Action: surface.Action, Resource: surface.Resource(eff.Subject.TenantID),
			Context: authz.Context{Transport: authz.TransportFrom(ctx), Now: time.Now()}})
		if d.Outcome == authz.Denied {
			return surface, d, true
		}
	}
	return authz.Surface{}, authz.Decision{}, false
}

// aclSubjectLockForCaller refuses the calling principal a change that would
// take subject in tenant out of a list the caller's own set locks. Open
// mode, the legacy token and an admin are not held (an admin is who imposes
// a lock); with no resolver wired nothing is locked. A set that could not
// be read refuses (fail closed): the change cannot be undone.
func (s *Server) aclSubjectLockForCaller(ctx context.Context, tenant, subject string) *aclReply {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) || s.aclResolver == nil {
		return nil
	}
	if p.Effective == nil {
		r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "access control could not be evaluated; retry")
		return &r
	}
	names, err := s.aclSubjectLists(ctx, tenant, subject)
	if err != nil {
		slog.ErrorContext(ctx, "authz: the lists a user carries could not be read", "tenant", tenant, "subject", subject, "err", err)
		r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the user's access-control lists could not be read; retry")
		return &r
	}
	surface, d, denied := aclRetireDenied(ctx, p.Effective, names)
	if !denied {
		return nil
	}
	s.recordACLEvent(ctx, p, "acl_denied", surface, d)
	r := aclReply{http.StatusForbidden, aclDeniedBody(surface, d)}
	return &r
}

// aclLockedError carries a lock refusal out of a step that returns errors,
// for its handler to write; cause, when set, is the sentinel another
// transport maps (erasure.ErrLocked).
type aclLockedError struct {
	reply aclReply
	cause error
}

func (e *aclLockedError) Error() string { return aclReplyError(e.reply) }
func (e *aclLockedError) Unwrap() error { return e.cause }

// lifecycleLockSet is the set the removal policy's own steps are judged by
// in tenant (lifecycleLockSubject), or nil when no resolver is wired.
func (s *Server) lifecycleLockSet(ctx context.Context, tenant string) (*authz.Effective, error) {
	if s.aclResolver == nil {
		return nil, nil
	}
	return s.aclResolver.ResolveSubject(ctx, authz.Subject{TenantID: tenant, ID: lifecycleLockSubject, Class: "tenant"})
}

// lockedEraseRecheck is how long a removal's erase stage is deferred when
// the subject carries a locked list: one tick of the hourly lifecycle
// sweeper, so the next sweep re-checks it.
const lockedEraseRecheck = time.Hour

// lifecycleEraseLock names a list locked against the tenant's operators
// (lifecycleLockSet) that subject carries, or "" when none: the removal
// policy's erase stage has no caller and a tenant operator can set a
// removal going, so it is judged as unbindSubject's steps are. Erasure is
// all or nothing, so such a subject is deferred (eraseRemoved) rather than
// erased around the locked rows; the operator who imposed the lock lifts
// it or erases the subject itself. A set or list that cannot be read is an
// error, and the subject is not erased this pass (fail closed). It is asked
// here, not in ErasureExecute, because open mode also erases with no
// principal and is the operator.
func (s *Server) lifecycleEraseLock(ctx context.Context, tenant, subject string) (string, error) {
	lockSet, err := s.lifecycleLockSet(ctx, tenant)
	if err != nil {
		return "", fmt.Errorf("access control for the erase stage: %w", err)
	}
	if lockSet == nil {
		return "", nil
	}
	names, err := s.aclSubjectLists(ctx, tenant, subject)
	if err != nil {
		return "", fmt.Errorf("the lists the subject carries: %w", err)
	}
	surface, _, denied := aclRetireDenied(ctx, lockSet, names)
	if !denied {
		return "", nil
	}
	return strings.TrimPrefix(surface.Name, "acl/"), nil
}

// aclCallerMayReadLists reports whether the caller passes the list-read gate
// (the management floor and read def:acl/*) without recording anything: a
// question asked only to decide what an answer includes is not a request
// that was refused.
func (s *Server) aclCallerMayReadLists(ctx context.Context) bool {
	if tools.RunID(ctx) != "" {
		return false
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	if p.Restrictions.RunPlaneOnly() || (!p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeTenant)) {
		return false
	}
	res := authz.EvaluateSurface(ctx, p.Effective, *aclReadLists(), true)
	return res.Allowed && !res.Unavailable
}
