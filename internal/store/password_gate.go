package store

import (
	"context"
	"time"
)

// RFC FA P2 — the password gate's persistent state.
//
// Everything here is either a pointer from a login to a user (identities) or a
// secret the database must not be able to replay: a password is an Argon2id
// PHC string peppered with a key outside the database, a TOTP secret is sealed
// under the deployment key, and every invite, reset and recovery code is kept
// only as a hash. A full read of these tables yields no usable credential.

// CredentialKindPassword is the one user_credentials kind P2 writes.
const CredentialKindPassword = "password"

// UserIdentityRow maps a login at one gate to a loomcycle user (RFC FA §5,
// D18). (Gate, Issuer, ExternalSubject) is unique across every tenant, which
// is what lets a login form ask for no tenant. For the password gate Issuer is
// "" and ExternalSubject is the normalised login name; an OIDC gate (P1) puts
// the provider's issuer and `sub` there. A subject has at most one identity
// per gate.
type UserIdentityRow struct {
	TenantID        string
	Subject         string
	Gate            string
	Issuer          string
	ExternalSubject string
	Email           string
	EmailVerified   bool
	CreatedAt       time.Time
	// LastLoginAt is zero until the first login through this identity.
	LastLoginAt time.Time
}

// UserCredentialRow is one stored credential of a user (RFC FA §6). For kind
// "password" PHC is the Argon2id PHC string whose digest is HMAC-peppered, and
// PepperKID names the pepper that keyed it, so a key rotation is detected on
// the next login and the password re-hashed.
type UserCredentialRow struct {
	TenantID  string
	Subject   string
	Kind      string
	PHC       string
	PepperKID string
	UpdatedAt time.Time
	// FailedCount and LockedUntil are the per-account throttle. A successful
	// login and a new password reset both.
	FailedCount int
	LockedUntil time.Time
}

// UserTOTPRow is a user's TOTP second factor (RFC 6238). Sealed is the
// JSON-encoded credential.Sealed envelope of the shared secret, bound to the
// row's tenant and subject. ConfirmedAt is zero while enrolment is pending: an
// unconfirmed secret never satisfies a login. LastStep is the last 30-second
// step a code was accepted for; a code for that step or an earlier one is a
// replay and is refused.
type UserTOTPRow struct {
	TenantID    string
	Subject     string
	Sealed      string
	CreatedAt   time.Time
	ConfirmedAt time.Time
	LastStep    int64
}

// AuthInviteRow is a single-use invitation to a password gate (RFC FA §6.1,
// D15). CodeHash is the hex SHA-256 of the code; the code itself is shown to
// the inviter once and never stored.
type AuthInviteRow struct {
	InviteID string
	CodeHash string
	Gate     string
	TenantID string
	// Subject, when set, attaches the password to that existing user instead
	// of creating one; AccessMode and Groups are then empty.
	Subject    string
	AccessMode string
	Groups     []string
	// Login, when set, is the login the invitee must take (normalised).
	Login    string
	Email    string
	ReturnTo string
	// Bootstrap marks the owner invite of an empty tenant (the one case an
	// untrusted inviter may grant access_mode tenant). Its redemption re-checks
	// that the tenant still has no user.
	Bootstrap bool
	// IssuedByManager is set when an auth:manage credential issued the
	// invite. Such an invite is re-checked against the provisioning floor at
	// redemption, and revoked when its target's authority is raised
	// (AuthRevokeManagerIssued), so a link minted while the target was
	// unprivileged cannot be redeemed after it became privileged.
	IssuedByManager bool
	CreatedBy       string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	RedeemedAt      time.Time
	RedeemedSubject string
	RevokedAt       time.Time
}

// Live reports whether the invite can still be redeemed at now.
func (r AuthInviteRow) Live(now time.Time) bool {
	return r.RedeemedAt.IsZero() && r.RevokedAt.IsZero() && now.Before(r.ExpiresAt)
}

// AuthResetRow is a one-time password reset link (RFC FA §6.1). CodeHash is
// the hex SHA-256 of the code.
type AuthResetRow struct {
	ResetID  string
	CodeHash string
	Gate     string
	TenantID string
	Subject  string
	// IssuedByManager is set when an auth:manage credential issued the reset.
	// Such a reset is re-checked against the provisioning floor at
	// completion, and used up when its target's authority is raised
	// (AuthRevokeManagerIssued).
	IssuedByManager bool
	CreatedBy       string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UsedAt          time.Time
}

// Live reports whether the reset can still be completed at now.
func (r AuthResetRow) Live(now time.Time) bool {
	return r.UsedAt.IsZero() && now.Before(r.ExpiresAt)
}

// TenantBootstrapRow is one tenant_bootstraps marker: the tenant had a user
// at At, first written through Via ("users", "invite:<gate>", "backfill").
// At reads back zero for a sqlite backfill row, whose time is unknown.
type TenantBootstrapRow struct {
	TenantID string
	At       time.Time
	Via      string
}

// AuthSecondFactor is the TOTP enrolment a redemption or a reset completes in
// the same transaction as the password, so a gate that requires MFA never
// leaves a user with a password and no second factor.
type AuthSecondFactor struct {
	TOTP UserTOTPRow
	// RecoveryHashes replace every recovery code the subject had.
	RecoveryHashes []string
}

// AuthInviteRedemption is everything one invite redemption writes, applied in
// one transaction by AuthInviteRedeem.
type AuthInviteRedemption struct {
	CodeHash string
	Now      time.Time
	// User is the row to create; nil when the invite attaches to an existing
	// subject (Identity.Subject), which must then exist in the invite's tenant.
	User       *UserRow
	Identity   UserIdentityRow
	Credential UserCredentialRow
	// SecondFactor is set when the gate requires MFA (or the invitee enrolled).
	SecondFactor *AuthSecondFactor
	// OwnerBinding, set only for a bootstrap invite, is the user-kind ACL
	// binding that makes the new user its tenant's operator. It is written in
	// the same transaction as the user and its login, so no owner is ever
	// half-made.
	OwnerBinding *AclBindingRow
}

// AuthResetCompletion is everything one reset completion writes, in one
// transaction (AuthResetComplete).
type AuthResetCompletion struct {
	CodeHash     string
	Now          time.Time
	Credential   UserCredentialRow
	SecondFactor *AuthSecondFactor
}

// AuthIdentityStore persists the login → user mapping every gate shares.
type AuthIdentityStore interface {
	// UserIdentityCreate inserts an identity. *ErrConflict{Kind:"user_identity"}
	// when (gate, issuer, external_subject) is taken, or when the subject
	// already has an identity at that gate.
	UserIdentityCreate(ctx context.Context, row UserIdentityRow) error
	// UserIdentityGet returns the identity a login maps to.
	// *ErrNotFound{Kind:"user_identity"} when there is none.
	UserIdentityGet(ctx context.Context, gate, issuer, externalSubject string) (UserIdentityRow, error)
	// UserIdentityListSubject returns the subject's identities at every gate,
	// ordered by gate. Empty slice when there are none.
	UserIdentityListSubject(ctx context.Context, tenantID, subject string) ([]UserIdentityRow, error)
	// UserIdentityRename moves the subject's identity at gate (issuer "") to
	// newExternalSubject. *ErrNotFound when the subject has none there;
	// *ErrConflict{Kind:"user_identity"} when the new name is taken.
	UserIdentityRename(ctx context.Context, tenantID, subject, gate, newExternalSubject string) error
	// UserIdentityTouchLogin stamps last_login_at.
	UserIdentityTouchLogin(ctx context.Context, gate, issuer, externalSubject string, at time.Time) error
	// UserIdentityDelete removes the subject's identity at gate (an account
	// unlink) and reports whether one went.
	UserIdentityDelete(ctx context.Context, tenantID, subject, gate string) (bool, error)
	// UserIdentityListGate returns every identity at gate, in every tenant,
	// ordered by (tenant_id, subject). An authoritative gate's sync diffs
	// what it asserts against it. Empty slice when there are none.
	UserIdentityListGate(ctx context.Context, gate string) ([]UserIdentityRow, error)
}

// Login-state purposes.
const (
	LoginPurposeLogin = "login"
	LoginPurposeLink  = "link"
)

// AuthLoginStateRow is one started redirect login (RFC FA §3.5, §5): an OIDC
// or OAuth code flow, between the redirect to the provider and its callback.
// StateHash is the SHA-256 of the OAuth state; BindingHash is that of the
// secret in the browser's login cookie, from which the PKCE verifier and the
// nonce derive — neither is stored, so this row alone cannot finish a login.
type AuthLoginStateRow struct {
	StateHash   string
	Gate        string
	BindingHash string
	// Purpose is LoginPurposeLogin, or LoginPurposeLink for an account link,
	// which names the user and the session that asked for it.
	Purpose         string
	ReturnTo        string
	Reauth          bool
	LinkTenant      string
	LinkSubject     string
	LinkSessionHash string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	ConsumedAt      time.Time
}

// AuthLoginStateStore persists started redirect logins.
type AuthLoginStateStore interface {
	// AuthLoginStateCreate inserts a login state. *ErrConflict on a duplicate
	// state hash.
	AuthLoginStateCreate(ctx context.Context, row AuthLoginStateRow) error
	// AuthLoginStateConsume marks the state used and returns it, once: only a
	// state that is unconsumed and unexpired at now is returned, so a
	// replayed callback finds nothing. *ErrNotFound{Kind:"auth_login_state"}
	// otherwise.
	AuthLoginStateConsume(ctx context.Context, stateHash string, now time.Time) (AuthLoginStateRow, error)
	// AuthLoginStateDeleteExpired removes states that expired or were used
	// before `before`.
	AuthLoginStateDeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// PasswordGateStore persists the password gate's credentials, second factors,
// invitations, resets and throttle counters (RFC FA §6).
type PasswordGateStore interface {
	// UserCredentialGet returns the subject's credential of kind.
	// *ErrNotFound{Kind:"user_credential"} when there is none.
	UserCredentialGet(ctx context.Context, tenantID, subject, kind string) (UserCredentialRow, error)
	// UserCredentialSet inserts or replaces the credential, resetting its
	// throttle (failed_count 0, no lock).
	UserCredentialSet(ctx context.Context, row UserCredentialRow) error
	// UserCredentialRehash replaces PHC and PepperKID only if the stored PHC
	// still equals oldPHC (a login re-hashing under new parameters must not
	// overwrite a password changed meanwhile). Reports whether it did.
	UserCredentialRehash(ctx context.Context, tenantID, subject, kind, oldPHC, newPHC, pepperKID string, at time.Time) (bool, error)
	// UserCredentialRecordFailure atomically increments failed_count and, when
	// lockFor(new count) > 0, sets locked_until to now+lockFor (never moving an
	// existing later lock earlier). Returns the new count and the lock.
	// *ErrNotFound when there is no such credential.
	UserCredentialRecordFailure(ctx context.Context, tenantID, subject, kind string, now time.Time, lockFor func(failures int) time.Duration) (int, time.Time, error)
	// UserCredentialRecordSuccess clears failed_count and locked_until.
	UserCredentialRecordSuccess(ctx context.Context, tenantID, subject, kind string) error
	// UserCredentialDelete removes the credential and reports whether one went.
	UserCredentialDelete(ctx context.Context, tenantID, subject, kind string) (bool, error)

	// UserTOTPGet returns the subject's TOTP row. *ErrNotFound{Kind:"user_totp"}.
	UserTOTPGet(ctx context.Context, tenantID, subject string) (UserTOTPRow, error)
	// UserTOTPPut inserts or replaces the subject's TOTP row.
	UserTOTPPut(ctx context.Context, row UserTOTPRow) error
	// UserTOTPUseStep accepts a code for step: it sets last_step = step (and
	// confirmed_at = at when it was unset) only if step > last_step. False is
	// a replay (or no row): the caller refuses the code.
	UserTOTPUseStep(ctx context.Context, tenantID, subject string, step int64, at time.Time) (bool, error)
	// UserTOTPDelete removes the subject's TOTP row and recovery codes.
	UserTOTPDelete(ctx context.Context, tenantID, subject string) (bool, error)

	// UserRecoveryCodesReplace drops every recovery code of the subject and
	// stores hashes in their place.
	UserRecoveryCodesReplace(ctx context.Context, tenantID, subject string, hashes []string, at time.Time) error
	// UserRecoveryCodeConsume marks one unused code used and reports whether it
	// did; false for an unknown or already used code, so a code works once even
	// under concurrent logins.
	UserRecoveryCodeConsume(ctx context.Context, tenantID, subject, hash string, at time.Time) (bool, error)
	// UserRecoveryCodesRemaining counts the subject's unused codes.
	UserRecoveryCodesRemaining(ctx context.Context, tenantID, subject string) (int, error)

	// AuthInviteCreate inserts an invite. *ErrConflict{Kind:"auth_invite"} on
	// a duplicate id or code hash.
	AuthInviteCreate(ctx context.Context, row AuthInviteRow) error
	// AuthInviteGet returns an invite by id; AuthInviteGetByCode by code hash.
	// Both return the row whatever its state; *ErrNotFound{Kind:"auth_invite"}.
	AuthInviteGet(ctx context.Context, inviteID string) (AuthInviteRow, error)
	AuthInviteGetByCode(ctx context.Context, codeHash string) (AuthInviteRow, error)
	// AuthInviteList returns the gate's invites in exactly tenantID, newest
	// first. "" is the shared tenant like any other, never "every tenant": a
	// caller whose tenant resolved to "" must not read the others'.
	AuthInviteList(ctx context.Context, gate, tenantID string) ([]AuthInviteRow, error)
	// AuthInviteListAllTenants returns the gate's invites in every tenant,
	// newest first: the admin's deliberate cross-tenant view, a separate
	// method so no tenant value can select it.
	AuthInviteListAllTenants(ctx context.Context, gate string) ([]AuthInviteRow, error)
	// AuthInviteRevoke revokes a live invite and reports whether it did.
	AuthInviteRevoke(ctx context.Context, inviteID string, at time.Time) (bool, error)
	// AuthInviteRedeem applies a redemption in one transaction: it consumes the
	// invite (live at red.Now, else *ErrNotFound{Kind:"auth_invite"}), refuses a
	// bootstrap invite whose tenant was ever bootstrapped (TenantBootstrapped;
	// *ErrConflict{Kind:"tenant_not_empty"}),
	// creates red.User or checks the attached subject exists
	// (*ErrNotFound{Kind:"user"}), and writes the identity (*ErrConflict on a
	// taken login), the credential and the second factor. Any failure leaves
	// nothing written and the invite live.
	AuthInviteRedeem(ctx context.Context, red AuthInviteRedemption) error
	// AuthInviteLiveBootstrap reports whether the tenant has a live bootstrap
	// invite at now.
	AuthInviteLiveBootstrap(ctx context.Context, tenantID string, now time.Time) (bool, error)
	// AuthInviteDeleteExpired removes invites that expired, were redeemed or
	// were revoked before `before`.
	AuthInviteDeleteExpired(ctx context.Context, before time.Time) (int, error)

	// AuthResetCreate inserts a reset and marks every earlier live reset of
	// the same (tenant, subject) used, so only the newest link works.
	AuthResetCreate(ctx context.Context, row AuthResetRow) error
	// AuthResetGetByCode returns a reset by code hash, whatever its state.
	// *ErrNotFound{Kind:"auth_reset"}.
	AuthResetGetByCode(ctx context.Context, codeHash string) (AuthResetRow, error)
	// AuthResetComplete applies a completion in one transaction: it consumes the
	// reset (live at c.Now, else *ErrNotFound{Kind:"auth_reset"}) and every other
	// live reset of its subject, replaces the credential (resetting its
	// throttle) and writes the second factor. A failure writes nothing.
	AuthResetComplete(ctx context.Context, c AuthResetCompletion) error
	// AuthResetDeleteExpired removes resets that expired or were used before
	// `before`.
	AuthResetDeleteExpired(ctx context.Context, before time.Time) (int, error)

	// AuthRevokeManagerIssued closes every manager-issued link aimed at
	// (tenantID, subject) that is live at `at`: a reset is marked used
	// (used_at = at) and an invite attaching to the subject is revoked
	// (revoked_at = at). Operator-issued links are untouched. Returns how many
	// rows it changed.
	//
	// A manager's link is checked against the provisioning floor when it is
	// issued; raising the target's authority afterwards would let the link
	// carry a password onto a now-privileged account. So every write that
	// raises a user's authority calls this BEFORE the write and aborts the
	// write when it fails (a revoked link is harmless if the grant then
	// fails; a grant with a live stale link is the bug). Both backends call
	// it inside these store methods, so every transport and a snapshot
	// restore are covered:
	//   - GroupMemberAdd with MemberKind user (row.TenantID, row.MemberID);
	//   - AclBindingPut with SubjectKind user (SubjectID), or token (the
	//     token def's TenantID/Subject; a missing token def revokes nothing);
	//   - UserUpdate setting AccessMode to "tenant";
	//   - OperatorTokenDefCreate, always, for (row.TenantID, row.Subject).
	AuthRevokeManagerIssued(ctx context.Context, tenantID, subject string, at time.Time) (int, error)

	// AuthThrottleGet returns a throttle key's failures inside its current
	// window and its block. Zero values for an unknown key or a window that
	// ended before now.
	AuthThrottleGet(ctx context.Context, key string, now time.Time) (failures int, blockedUntil time.Time, err error)
	// AuthThrottleRecordFailure atomically counts one failure for key in a
	// window of length window (a failure after the window ended starts a new
	// one at 1) and, when blockFor(new count) > 0, sets blocked_until to
	// now+blockFor (never earlier than an existing block).
	AuthThrottleRecordFailure(ctx context.Context, key string, now time.Time, window time.Duration, blockFor func(failures int) time.Duration) (int, time.Time, error)
	// AuthThrottleClear forgets key.
	AuthThrottleClear(ctx context.Context, key string) error
	// AuthThrottleDeleteExpired removes keys whose window and block both ended
	// before `before`.
	AuthThrottleDeleteExpired(ctx context.Context, before time.Time) (int, error)

	// UserCreateOwner creates the first user of a tenant together with the
	// user-kind ACL binding that makes it the tenant's operator, in one
	// transaction that also writes the bootstrap marker. It refuses
	// (*ErrConflict{Kind:"tenant_not_empty"}) a tenant that was ever
	// bootstrapped, checked inside the transaction, and writes nothing then.
	UserCreateOwner(ctx context.Context, user UserRow, owner AclBindingRow) error

	// TenantBootstrapped reports whether the tenant has ever had a user: a
	// tenant_bootstraps marker (written before the first users row of a
	// tenant by every user-creating path, never deleted) or a users row. A
	// website's owner bootstrap is allowed only when this is false.
	TenantBootstrapped(ctx context.Context, tenantID string) (bool, error)
	// TenantBootstrapList returns every tenant_bootstraps marker, ordered by
	// tenant_id. Empty slice when there are none. A snapshot carries these so
	// a tenant whose users were all deleted stays bootstrapped on restore.
	TenantBootstrapList(ctx context.Context) ([]TenantBootstrapRow, error)
	// TenantBootstrapMark writes a marker unless the tenant already has one;
	// an existing marker is never moved (its At and Via stay). A zero At is
	// stamped with the current time.
	TenantBootstrapMark(ctx context.Context, row TenantBootstrapRow) error

	// AuthSubjectDelete removes every gate row of a subject: identities,
	// credentials, TOTP, recovery codes, its invites and resets, and its
	// removal (auth_removals). Called
	// when the user is deleted, so a login never outlives its user.
	AuthSubjectDelete(ctx context.Context, tenantID, subject string) error
}
