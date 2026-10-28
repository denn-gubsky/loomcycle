package store

import (
	"context"
	"errors"
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
	RemovalReasonSCIM     = "scim"     // a SCIM gate's DELETE or active:false (RFC FA P6)
	// RemovalReasonSCIMPending is RemovalReasonSCIM for a user that was
	// pending when removed: a SCIM re-enable brings it back pending, never
	// active (approval stays an operator's).
	RemovalReasonSCIMPending = "scim_pending"
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
	// Stage is the stage the sweeper has claimed and is running
	// (RemovalStageUnbinding, RemovalStageErasing), "" between stages.
	// While it is set the removal is not re-enabled by anyone.
	Stage string
	// PriorStatus is the user's status when the removal disabled it
	// (UserStatusPending carries an approval still owed); "" when the user
	// was disabled already or the row predates it.
	PriorStatus string
}

// RestoredStatus is the status a re-enable from removal r gives back: pending
// when the user owed an approval, else active.
func RestoredStatus(r AuthRemovalRow) string {
	if r.PriorStatus == UserStatusPending || r.Reason == RemovalReasonSCIMPending || r.Reason == RemovalReasonSCIMReactivatingPending {
		return UserStatusPending
	}
	return "active"
}

// The stages the sweeper claims (AuthRemovalClaimStage).
const (
	RemovalStageUnbinding = "unbinding"
	RemovalStageErasing   = "erasing"
)

// ErrRemovalBusy: the removal's stage is running (or the user was erased),
// so it cannot be re-enabled now.
var ErrRemovalBusy = errors.New("store: the removal is running a stage")

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

	// AuthRemovalStart disables the user and records its removal, in ONE
	// transaction (the user row locked first), and reports whether a row
	// was written. The row's PriorStatus is the user's status before (""
	// when it was disabled already). An existing removal of a user who is
	// disabled is never restarted, so a repeated revoke or sync cannot
	// postpone its stages; a row behind a user who is NOT disabled is stale
	// and is replaced by a fresh one. One exception: a removal of any other
	// reason takes over a SCIM gate's (its gate, reason and created_by; the
	// timings stay; a pending SCIM reason carries into PriorStatus), so an
	// operator's or another gate's removal is never left behind a row SCIM
	// could re-enable. *ErrNotFound{Kind:"user"} for an unknown user.
	AuthRemovalStart(ctx context.Context, row AuthRemovalRow) (bool, error)
	// AuthRemovalClaimStage claims a due stage for the sweeper, in one
	// transaction (the user row locked first): only while the user is
	// still disabled, the removal exists, is not a SCIM re-enable in
	// progress, and runs no other stage, does Stage become `stage` (a
	// retry of the same stage claims again). false otherwise. Marking the
	// stage done clears it.
	AuthRemovalClaimStage(ctx context.Context, tenantID, subject, stage string) (bool, error)
	// AuthRemovalReactivate ends a removal and re-enables the user, in ONE
	// transaction (the user row locked first): only while the user is still
	// disabled and its removal is still `seen` (same reason and disabled_at),
	// idle (no stage claimed) and not erased, is the row deleted and the user
	// set to the status it had — pending when it owed an approval
	// (PriorStatus pending, or a pending SCIM reason), else active. Anything
	// changed since `seen` was read answers false and changes nothing, so a
	// disable or a stage that landed meanwhile is never overwritten.
	AuthRemovalReactivate(ctx context.Context, seen AuthRemovalRow) (restored string, ok bool, err error)
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
	// AuthRemovalMarkUnbound sets unbound_at if it is unset (and clears the
	// claimed stage); false when it was already set or there is no row.
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
