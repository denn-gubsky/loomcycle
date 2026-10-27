package store

import (
	"context"
	"time"
)

// RFC FA P4 — what external gates and the user removal policy keep.
//
// Two tables. auth_assertion_jtis is the replay cache every replica shares:
// an external gate's assertion is single-use, and a replay sent to another
// replica must be refused too, so the cache cannot live in process memory.
// auth_removals is the §3.7c stage machine for one removed user — disable at
// once, unbind after unbind_after, erase only when erase_after says so (or an
// operator runs the erasure). The timings are fixed when the removal starts,
// so a later config change never makes an old removal erase early.

// Removal reasons (AuthRemovalRow.Reason).
const (
	RemovalReasonRevoked  = "revoked"  // an external gate's POST /v1/_auth/revoke
	RemovalReasonSync     = "sync"     // an authoritative gate's sync no longer lists the user
	RemovalReasonDormant  = "dormant"  // no login through any gate for dormant_after
	RemovalReasonOperator = "operator" // an operator started it
)

// AuthRemovalRow is one user's removal (RFC FA §3.7c). Stages never skip:
// UnboundAt is set only after DisabledAt, ErasedAt only after UnboundAt.
type AuthRemovalRow struct {
	TenantID string
	Subject  string
	// Gate is the gate that removed the user; "" for a dormancy or operator
	// removal.
	Gate   string
	Reason string
	// DisabledAt is when the user was disabled: the row's creation.
	DisabledAt time.Time
	// UnbindDueAt is when the unbind stage becomes due.
	UnbindDueAt time.Time
	// EraseDueAt is when the erase stage becomes due; zero means an
	// operator must run it (erase_after: never).
	EraseDueAt time.Time
	UnboundAt  time.Time
	ErasedAt   time.Time
	CreatedBy  string
}

// AuthLifecycleStore persists the external gates' replay cache and the
// removal stage machine.
type AuthLifecycleStore interface {
	// AuthAssertionUseOnce records (gate, jti) until expiresAt and reports
	// whether it was new. An existing row that expired before now is
	// replaced (the jti is new again only once its assertion could no
	// longer verify). Atomic across replicas: exactly one of two concurrent
	// calls for the same (gate, jti) gets true.
	AuthAssertionUseOnce(ctx context.Context, gate, jti string, expiresAt, now time.Time) (bool, error)
	// AuthAssertionDeleteExpired removes rows that expired before `before`.
	AuthAssertionDeleteExpired(ctx context.Context, before time.Time) (int, error)

	// AuthRemovalStart inserts a removal unless the subject already has one,
	// and reports whether it did. An existing removal is never restarted,
	// so a repeated revoke or sync cannot postpone its stages.
	AuthRemovalStart(ctx context.Context, row AuthRemovalRow) (bool, error)
	// AuthRemovalGet returns the subject's removal.
	// *ErrNotFound{Kind:"auth_removal"} when there is none.
	AuthRemovalGet(ctx context.Context, tenantID, subject string) (AuthRemovalRow, error)
	// AuthRemovalList returns the removals in exactly tenantID ("" is the
	// shared tenant, never "all"), oldest first.
	AuthRemovalList(ctx context.Context, tenantID string) ([]AuthRemovalRow, error)
	// AuthRemovalListAllTenants returns every removal, oldest first: the
	// admin's deliberate cross-tenant view.
	AuthRemovalListAllTenants(ctx context.Context) ([]AuthRemovalRow, error)
	// AuthRemovalDue returns up to limit removals with a stage due at now:
	// not yet unbound and UnbindDueAt <= now, or unbound, not erased, with a
	// non-zero EraseDueAt <= now. Oldest first.
	AuthRemovalDue(ctx context.Context, now time.Time, limit int) ([]AuthRemovalRow, error)
	// AuthRemovalMarkUnbound sets unbound_at if it is unset; false when it
	// was already set or there is no row.
	AuthRemovalMarkUnbound(ctx context.Context, tenantID, subject string, at time.Time) (bool, error)
	// AuthRemovalMarkErased sets erased_at if unbound_at is set and
	// erased_at is not; false otherwise (an erase never skips the unbind).
	AuthRemovalMarkErased(ctx context.Context, tenantID, subject string, at time.Time) (bool, error)
	// AuthRemovalDelete removes the subject's removal (a reactivation, or a
	// user re-enabled by an operator) and reports whether one went.
	AuthRemovalDelete(ctx context.Context, tenantID, subject string) (bool, error)

	// AuthDormantCandidates returns up to limit ACTIVE users that have at
	// least one gate identity, no removal, and no identity whose
	// last_login_at (or, never used, created_at) is at or after `before`:
	// nobody has signed in as them through any gate since then. Users with
	// no identity were not made by a gate and are never dormant.
	AuthDormantCandidates(ctx context.Context, before time.Time, limit int) ([]UserRow, error)
}
