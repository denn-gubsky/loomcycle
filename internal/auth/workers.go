package auth

import (
	"errors"
	"fmt"
	"slices"
)

// ErrWorkerAttachRefused wraps every reason WorkerAttachment refuses a
// principal. The worker endpoint answers it as an authentication failure.
var ErrWorkerAttachRefused = errors.New("worker attach refused")

// WorkerAttach is which tenants an attached workspace worker may serve: its
// token's own tenant, or every tenant when an admin minted the token shared.
type WorkerAttach struct {
	// Shared is true for a worker that serves every tenant (RFC GW §8,
	// ruling Q2).
	Shared bool
	// TenantID is the token's tenant: the one tenant a tenant-bound worker
	// serves, and for a shared one only where its token was minted.
	TenantID string
	// Personal is true for a personal worker (RFC GW §4.5, P13): it serves
	// UserID of TenantID and nobody else.
	Personal bool
	// UserID is a personal worker's user, its token's subject.
	UserID string
}

// ServesUser reports whether the worker may be given an op of user in
// tenant: a personal worker only its own user's, any other worker as Serves
// says.
func (w WorkerAttach) ServesUser(tenant, user string) bool {
	if w.Personal {
		return w.TenantID != "" && w.UserID != "" && tenant == w.TenantID && user == w.UserID
	}
	return w.Serves(tenant)
}

// Serves reports whether the worker may be given an op of tenant. The zero
// value serves nobody.
func (w WorkerAttach) Serves(tenant string) bool {
	// A personal worker serves one user, never a tenant as a whole: a
	// caller that does not ask by user (ServesUser) is refused.
	if w.Personal {
		return false
	}
	if w.Shared {
		return true
	}
	return w.TenantID != "" && tenant == w.TenantID
}

// WorkerAttachment decides whether p may attach a workspace worker, and which
// tenants that worker serves. It is the only place the decision is made; the
// worker endpoint calls it with the principal its bearer resolved to.
//
// A worker runs model-chosen commands, and on the host engine one of them can
// read the worker's token, so every credential that is worth more than "attach
// a worker" is refused:
//
//   - the legacy LOOMCYCLE_AUTH_TOKEN and any substrate:admin token: on a
//     worker either is the whole deployment. HasScope would admit an admin
//     here (substrate:admin satisfies every scope), which is why this reads
//     the scope list itself;
//   - a token whose scopes are anything but exactly [workers:attach];
//   - a person's credential — one an auth gate or an app sign-in issued, one
//     that follows its user's authority, a Web UI session. A worker bound to
//     a user (a personal worker, RFC GW §4.5) is a workers:attach token
//     marked worker_personal and bound to its subject, never the person's
//     own sign-in credential;
//   - a tenant-bound token with no tenant.
//
// It reads nothing but p, so it is safe to call per attach and per stream.
func WorkerAttachment(p Principal) (WorkerAttach, error) {
	refuse := func(reason string) (WorkerAttach, error) {
		return WorkerAttach{}, fmt.Errorf("%w: %s", ErrWorkerAttachRefused, reason)
	}
	switch {
	case p.Legacy:
		return refuse("the legacy operator secret never attaches a worker")
	case HasScope(p.Scopes, ScopeAdmin):
		return refuse("an admin token never attaches a worker; mint a " + ScopeWorkersAttach + " token")
	case !slices.Contains(p.Scopes, ScopeWorkersAttach):
		return refuse("the token does not hold " + ScopeWorkersAttach)
	case len(p.Scopes) != 1:
		return refuse(ScopeWorkersAttach + " must be the token's only scope")
	case p.IssuedByGate != "" || p.OAuthClient != "" || p.InheritsAuthority || p.Login != nil:
		return refuse("a user's sign-in credential never attaches a worker")
	case p.Restrictions.WorkerShared && p.Restrictions.WorkerPersonal:
		return refuse("a worker token is shared or personal, never both")
	case p.Restrictions.WorkerShared:
		return WorkerAttach{Shared: true, TenantID: p.TenantID}, nil
	case p.Restrictions.WorkerPersonal && (p.TenantID == "" || p.Subject == ""):
		return refuse("a personal worker token names no user")
	case p.Restrictions.WorkerPersonal:
		return WorkerAttach{Personal: true, TenantID: p.TenantID, UserID: p.Subject}, nil
	case p.TenantID == "":
		return refuse("a tenant-bound worker token names no tenant")
	}
	return WorkerAttach{TenantID: p.TenantID}, nil
}
