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

	// SCIMRemovalReserve records row (a removal the gate starts) only while
	// the gate has started fewer than ceiling removals in row's tenant since
	// `since` — the count and the insert under one per-(gate, tenant) lock,
	// so concurrent removals on any replica cannot pass the ceiling
	// together. False when the subject already has a removal (nothing
	// recorded); ErrSCIMRemovalCeiling when the ceiling is reached.
	SCIMRemovalReserve(ctx context.Context, row AuthRemovalRow, since time.Time, ceiling int) (bool, error)
}

// ErrSCIMRemovalCeiling: a SCIM gate reached its removal ceiling.
var ErrSCIMRemovalCeiling = errors.New("store: the SCIM gate's removal ceiling is reached")
