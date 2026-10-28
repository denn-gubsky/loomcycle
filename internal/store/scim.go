package store

import (
	"context"
	"errors"
	"time"
)

// RFC FA P6 — inbound SCIM 2.0 provisioning through a kind: scim gate. A SCIM
// resource is what the identity provider pushed and expects back; the person
// it stands for is an ordinary gate user (users + user_identities, made by the
// same provisioning code as every identity gate), and the groups it asserts
// reach loomcycle through the same group sync. Every row names its gate and
// tenant, and no method reads across either.

// SCIMUserRow is one provisioned user resource. SCIMID is the server-assigned
// SCIM id, which is also the identity's external subject at the gate, so the
// subject is "<gate>:<SCIMID>". DeletedAt is set by a SCIM DELETE: the
// resource is gone for SCIM, while the user it pointed to goes through the
// gate's removal policy (disabled, kept, unbound later).
type SCIMUserRow struct {
	Gate     string
	TenantID string
	SCIMID   string
	Subject  string
	// UserName is as the provider sent it; UserNameKey is its case-folded
	// form, unique per gate and tenant among live rows (RFC 7643 userName
	// is case-insensitive). Display data only, never an identity input.
	UserName    string
	UserNameKey string
	ExternalID  string
	// Attributes is the rest of the resource the provider sent (name,
	// emails, displayName), as JSON. The store keeps it opaque.
	Attributes string
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  time.Time
}

// SCIMGroupRow is one provisioned group resource. Its members are SCIM users
// (scim_group_members); what they mean in loomcycle is the gate's group sync:
// DisplayName is the provider group name, so the loomcycle group it lands in
// is the one with external_id "<gate>:<DisplayName>".
type SCIMGroupRow struct {
	Gate        string
	TenantID    string
	SCIMID      string
	DisplayName string
	// DisplayKey is the case-folded display name, unique per gate and
	// tenant.
	DisplayKey string
	ExternalID string
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SCIMFilter selects by one attribute's exact value; empty fields do not
// filter.
type SCIMFilter struct {
	UserNameKey string
	DisplayKey  string
	ExternalID  string
}

// SCIMStore persists SCIM resources (RFC FA P6).
type SCIMStore interface {
	// SCIMUserCreate inserts a live user resource.
	// *ErrConflict{Kind:"scim_user_name"} when a live resource of the gate
	// and tenant has the same UserNameKey; *ErrConflict{Kind:"scim_user"}
	// when the subject already has a live resource.
	SCIMUserCreate(ctx context.Context, row SCIMUserRow) error
	// SCIMUserGet returns a live resource. *ErrNotFound{Kind:"scim_user"}
	// for an unknown, another gate's or tenant's, or a deleted one.
	SCIMUserGet(ctx context.Context, gate, tenantID, scimID string) (SCIMUserRow, error)
	// SCIMUserList returns the live resources matching f, ordered by
	// creation, from offset (0-based) up to limit, and the total count.
	SCIMUserList(ctx context.Context, gate, tenantID string, f SCIMFilter, offset, limit int) ([]SCIMUserRow, int, error)
	// SCIMUserUpdate replaces UserName, UserNameKey, ExternalID and
	// Attributes of a live resource, bumps its version and stamps
	// UpdatedAt. *ErrNotFound{Kind:"scim_user"};
	// *ErrConflict{Kind:"scim_user_name"}.
	SCIMUserUpdate(ctx context.Context, row SCIMUserRow) error
	// SCIMUserMarkDeleted ends a live resource (DeletedAt = at) and removes
	// its SCIM group memberships, in one transaction, and reports whether it
	// did. The user is the caller's to remove.
	SCIMUserMarkDeleted(ctx context.Context, gate, tenantID, scimID string, at time.Time) (bool, error)
	// SCIMUserDiscard removes a resource row outright: a create whose user
	// could not be provisioned. Never for a resource the provider was told
	// exists.
	SCIMUserDiscard(ctx context.Context, gate, tenantID, scimID string) error

	// SCIMGroupCreate inserts a group resource.
	// *ErrConflict{Kind:"scim_group_name"} when the gate and tenant have a
	// resource of that display name.
	SCIMGroupCreate(ctx context.Context, row SCIMGroupRow) error
	// SCIMGroupGet returns a resource. *ErrNotFound{Kind:"scim_group"}.
	SCIMGroupGet(ctx context.Context, gate, tenantID, scimID string) (SCIMGroupRow, error)
	// SCIMGroupList is SCIMUserList for groups.
	SCIMGroupList(ctx context.Context, gate, tenantID string, f SCIMFilter, offset, limit int) ([]SCIMGroupRow, int, error)
	// SCIMGroupUpdate replaces DisplayName, DisplayKey and ExternalID and
	// bumps the version. *ErrNotFound; *ErrConflict{Kind:"scim_group_name"}.
	SCIMGroupUpdate(ctx context.Context, row SCIMGroupRow) error
	// SCIMGroupDelete removes the resource and its memberships in one
	// transaction, and reports whether it did. No loomcycle group is
	// touched: the caller re-syncs the former members.
	SCIMGroupDelete(ctx context.Context, gate, tenantID, scimID string) (bool, error)
	// SCIMGroupMembers returns the subjects of a group's members, sorted.
	SCIMGroupMembers(ctx context.Context, gate, tenantID, groupID string) ([]string, error)
	// SCIMGroupMembersAdd adds subjects to a group (an existing membership
	// is kept) and SCIMGroupMembersRemove removes them; each reports the
	// subjects it changed.
	SCIMGroupMembersAdd(ctx context.Context, gate, tenantID, groupID string, subjects []string) ([]string, error)
	SCIMGroupMembersRemove(ctx context.Context, gate, tenantID, groupID string, subjects []string) ([]string, error)
	// SCIMGroupsOfSubject returns the group resources a subject is a member
	// of, ordered by display key.
	SCIMGroupsOfSubject(ctx context.Context, gate, tenantID, subject string) ([]SCIMGroupRow, error)

	// SCIMRemoveUser is a SCIM gate's removal of one user, all or nothing in
	// one transaction under a per-(gate, tenant) lock, so concurrent removals
	// on any replica cannot pass the ceiling together.
	//
	// The SCIM removal state machine (a user's status, and its removal row):
	//
	//   - disabled (any row or none) --remove--> unchanged, false: a disable
	//     SCIM did not do stays its owner's; nothing is counted.
	//   - active|pending, any row or none --remove-->
	//       at the ceiling (the gate's removals in the tenant since `since`,
	//       this subject's own row not counted): unchanged,
	//       ErrSCIMRemovalCeiling;
	//       else: a stale row (one behind a user who is not disabled, so no
	//       longer in force) is deleted with its deadlines; the user becomes
	//       disabled under a FRESH row = row's gate, timings and created_by,
	//       reason RemovalReasonSCIM (was active) or RemovalReasonSCIMPending
	//       (was pending; row.Reason is ignored); true.
	//   - disabled + this gate's SCIM row --SCIMReactivate--> the recorded
	//     status (active, or pending: SCIM never approves), row deleted.
	//   - disabled + any other row, or none --SCIMReactivate--> unchanged.
	//   - an operator's or another gate's AuthRemovalStart over a SCIM row
	//     takes it over (gate, reason, created_by), so SCIM can no longer
	//     re-enable that user.
	//
	// *ErrNotFound{Kind:"user"} for an unknown user.
	SCIMRemoveUser(ctx context.Context, row AuthRemovalRow, since time.Time, ceiling int) (bool, error)
	// SCIMReactivateClaim is the first half of a SCIM re-enable, in one
	// transaction: when the subject's CURRENT removal is the gate's with a
	// SCIM reason (a claim in progress included, so a retry continues), its
	// reason becomes the matching "reactivating" one and the user stays
	// disabled. The caller then resets the user to the floor and finishes
	// with SCIMReactivate. Any other removal (an operator's, another gate's)
	// or none answers false and changes nothing. An operator's or another
	// gate's removal still takes a claimed row over (AuthRemovalStart).
	SCIMReactivateClaim(ctx context.Context, gate, tenantID, subject string) (bool, error)
	// SCIMReactivate finishes a claimed re-enable, in one transaction: only
	// while the CURRENT removal is still the gate's claim does the user
	// return to the status recorded at removal (active, or pending) and the
	// row go. Otherwise false, and nothing changes.
	SCIMReactivate(ctx context.Context, gate, tenantID, subject string) (restored string, ok bool, err error)
	// SCIMRemovalDisown deletes the subject's removal row when it is a SCIM
	// one (any SCIM reason): an operator's plain disable owns the user from
	// then on, so SCIM can no longer re-enable it.
	SCIMRemovalDisown(ctx context.Context, tenantID, subject string) (bool, error)
	// SCIMRemovalBase is the base a percentage removal ceiling is taken of:
	// the users linked to the gate in the tenant who were created before
	// `before` (the window's start) and are not disabled — or were disabled
	// by a removal inside the window. Users the provider creates inside the
	// window cannot raise its own ceiling, and its removals there do not
	// shrink it.
	SCIMRemovalBase(ctx context.Context, gate, tenantID string, before time.Time) (int, error)
	// SCIMUsersCreatedSince counts the user resources the gate created in
	// the tenant since `since`, deleted ones included (a discarded create,
	// whose user was refused, made no user and is not counted).
	SCIMUsersCreatedSince(ctx context.Context, gate, tenantID string, since time.Time) (int, error)
	// SCIMUsersGetMany returns the live resources among ids, keyed by id.
	// Unknown, deleted and other tenants' ids are absent.
	SCIMUsersGetMany(ctx context.Context, gate, tenantID string, ids []string) (map[string]SCIMUserRow, error)
}

// Reasons of a SCIM removal being re-enabled (SCIMReactivateClaim).
const (
	RemovalReasonSCIMReactivating        = "scim_reactivating"
	RemovalReasonSCIMReactivatingPending = "scim_reactivating_pending"
)

// SCIMRemovalReason reports whether reason is one a SCIM gate's removal
// writes.
func SCIMRemovalReason(reason string) bool {
	switch reason {
	case RemovalReasonSCIM, RemovalReasonSCIMPending, RemovalReasonSCIMReactivating, RemovalReasonSCIMReactivatingPending:
		return true
	}
	return false
}

// SCIMRemovalReasons is every SCIM removal reason, for SQL IN lists.
const SCIMRemovalReasons = `'scim', 'scim_pending', 'scim_reactivating', 'scim_reactivating_pending'`

// ErrSCIMRemovalCeiling: a SCIM gate reached its removal ceiling.
var ErrSCIMRemovalCeiling = errors.New("store: the SCIM gate's removal ceiling is reached")
