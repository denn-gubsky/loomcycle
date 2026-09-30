package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An active pointer says which version of a definition serves (tenant, name).
// A live promote (*DefSetActive) accepts only a def of that same tenant and
// name, and the reads behind every lookup (*DefGetActive) then load the def by
// its id alone, trusting the promote's check. A restore writes the pointer as
// the envelope has it, so it makes the promote's check itself: an envelope
// pointing acme/gate at another tenant's def id would otherwise have acme's
// runs served that tenant's definition — the agent lookup even stamps it as
// acme's own.
//
// The check reads the def from the target store, after the defs are restored,
// not from the envelope: the def a pointer names may be one the target already
// had, and a def the envelope carries may not have been written.

// defOwner reads the tenant and name of the definition defID names on the
// restore target. A missing def is a *store.ErrNotFound.
type defOwner func(ctx context.Context, s store.Store, defID string) (tenantID, name string, err error)

func agentDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.AgentDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

func skillDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.SkillDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

func teamDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.TeamDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

func hookDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.HookDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

func mcpServerDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.MCPServerDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

func scheduleDefOwner(ctx context.Context, s store.Store, defID string) (string, string, error) {
	r, err := s.ScheduleDefGet(ctx, defID)
	return r.TenantID, r.Name, err
}

// admitActivePointer reports whether the pointer (tenantID, name) → defID may
// be written: its def is on the target under the same tenant and name, which
// is what a promote requires. A refused pointer is counted and named in a
// warning by its own tenant and name; the warning never says whose the def is
// or what it holds. A def that cannot be read is refused too: the pointer is
// not written on a check that did not run.
func admitActivePointer(ctx context.Context, s store.Store, owner defOwner, section, tenantID, name, defID string, result *RestoreResult) bool {
	defTenant, defName, err := owner(ctx, s, defID)
	var nf *store.ErrNotFound
	var reason string
	switch {
	case errors.As(err, &nf):
		reason = "which is not on this instance"
	case err != nil:
		reason = fmt.Sprintf("which could not be read to check it: %v", err)
	case defTenant != tenantID:
		reason = "which belongs to another tenant"
	case defName != name:
		reason = "which is a definition of another name"
	default:
		return true
	}
	result.ActivePointersRefused++
	result.Warnings = append(result.Warnings, fmt.Sprintf(
		"%s %s: not restored: it names def %s, %s (a promote refuses that too)",
		section, qualifiedName(tenantID, name), defID, reason))
	return false
}
