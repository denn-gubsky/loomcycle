package store

import (
	"context"
	"time"
)

// RFC FA P5 — device login (RFC 8628) and the per-tenant gate policy.

// Device authorization states. A row starts pending; the person's decision
// moves it to approved or denied once; the one poll that redeems an approved
// row moves it to redeemed. No state goes back.
const (
	DeviceAuthPending  = "pending"
	DeviceAuthApproved = "approved"
	DeviceAuthDenied   = "denied"
	DeviceAuthRedeemed = "redeemed"
)

// DeviceAuthRow is one device authorization: the code pair a CLI asked for and
// what became of it. Neither code is stored: DeviceCodeHash is the SHA-256 of
// the 256-bit device code, and UserCodeHash a keyed hash of the short user
// code under a key held outside the database (a plain digest of a 34-bit code
// is reversible by enumeration).
type DeviceAuthRow struct {
	DeviceCodeHash string
	UserCodeHash   string
	// ClientID is the client's own name for itself (a label, shown to the
	// person on the approval page). The token it receives carries the
	// approver's own scopes: a client cannot ask for more, or for a subset.
	ClientID string
	// SourceAddr is the address the request came from, shown on the approval
	// page so a person can tell their own terminal from someone else's.
	SourceAddr string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// Interval is the minimum spacing between polls; a poll that comes early
	// lengthens it (RFC 8628 §3.5). LastPolledAt is zero before the first poll.
	Interval     time.Duration
	LastPolledAt time.Time
	Status       string
	// The person who decided, as their Web UI session named them, and the
	// gate that session came through. Set by the decision (approve or deny).
	DecidedTenant  string
	DecidedSubject string
	DecidedGate    string
	DecidedAt      time.Time
	RedeemedAt     time.Time
}

// Live reports whether the row can still be decided or redeemed at now.
func (r DeviceAuthRow) Live(now time.Time) bool { return now.Before(r.ExpiresAt) }

// DeviceAuthDecision is a person's answer to a device authorization.
type DeviceAuthDecision struct {
	Approve  bool
	TenantID string
	Subject  string
	Gate     string
	At       time.Time
}

// DeviceAuthStore persists device authorizations (RFC FA §7, RFC 8628).
type DeviceAuthStore interface {
	// DeviceAuthCreate inserts a pending authorization.
	// *ErrConflict{Kind:"device_auth"} when either hash is taken.
	DeviceAuthCreate(ctx context.Context, row DeviceAuthRow) error
	// DeviceAuthGetByUserCode returns the authorization a user code names,
	// whatever its state. *ErrNotFound{Kind:"device_auth"}.
	DeviceAuthGetByUserCode(ctx context.Context, userCodeHash string) (DeviceAuthRow, error)
	// DeviceAuthPoll records one poll of a device code, in one statement:
	// early is true when it came before LastPolledAt+Interval, and then the
	// interval grows by step (to at most max) for this and every later poll
	// (RFC 8628 §3.5). LastPolledAt becomes now either way. Returns the row
	// as updated. *ErrNotFound{Kind:"device_auth"} for an unknown code.
	DeviceAuthPoll(ctx context.Context, deviceCodeHash string, now time.Time, step, max time.Duration) (row DeviceAuthRow, early bool, err error)
	// DeviceAuthDecide records the person's decision on a pending, live
	// authorization and reports whether it did; false when the row is
	// missing, expired or already decided, so a code is decided once.
	DeviceAuthDecide(ctx context.Context, userCodeHash string, d DeviceAuthDecision) (bool, error)
	// DeviceAuthRedeem moves an approved, live authorization to redeemed and
	// returns it; ok is false when it is not (missing, pending, denied,
	// expired, or redeemed already), so a code yields one token.
	DeviceAuthRedeem(ctx context.Context, deviceCodeHash string, at time.Time) (row DeviceAuthRow, ok bool, err error)
	// DeviceAuthDeleteExpired removes every authorization that expired before
	// `before`, whatever its state. A redeemed row stays until then, so a
	// second redemption is answered "already used", not "unknown".
	DeviceAuthDeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// TenantAuthPolicyRow is a tenant's gate policy (RFC FA §4, per-tenant strict):
// the tenant's people may sign in, and be issued tokens, only through the
// listed gates. No row = no tenant restriction.
type TenantAuthPolicyRow struct {
	TenantID string
	// Gates are configured gate names, sorted and de-duplicated, never empty.
	Gates     []string
	UpdatedAt time.Time
	UpdatedBy string
}

// TenantAuthPolicyStore persists the admin-managed per-tenant gate policies.
type TenantAuthPolicyStore interface {
	// TenantAuthPolicyGet returns the tenant's policy.
	// *ErrNotFound{Kind:"tenant_auth_policy"} when it has none.
	TenantAuthPolicyGet(ctx context.Context, tenantID string) (TenantAuthPolicyRow, error)
	// TenantAuthPolicyList returns every policy, ordered by tenant. Empty slice
	// when there are none.
	TenantAuthPolicyList(ctx context.Context) ([]TenantAuthPolicyRow, error)
	// TenantAuthPolicyPut inserts or replaces the tenant's policy.
	TenantAuthPolicyPut(ctx context.Context, row TenantAuthPolicyRow) error
	// TenantAuthPolicyDelete removes the tenant's policy and reports whether
	// there was one.
	TenantAuthPolicyDelete(ctx context.Context, tenantID string) (bool, error)
	// TenantAuthPolicyRevokeSessions ends every live Web UI session of the
	// tenant whose gate is not in gates, and returns how many. A tighter
	// policy takes effect on the sessions already issued, not only on the
	// next login.
	TenantAuthPolicyRevokeSessions(ctx context.Context, tenantID string, gates []string, at time.Time) (int, error)
}
