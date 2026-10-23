package store

import (
	"context"
	"encoding/json"
	"time"
)

// RFC EY access control — the four tenant-scoped tables behind user groups,
// ACL rule lists and bindings, plus the per-tenant version the engine's
// cache keys on.
//
// Deliberately NOT the content-addressed fork/promote lifecycle the agent
// and skill families use. A binding resolves "the current list of this
// name" and nothing resolves an older one, so an ACL is a flat
// (tenant_id, name) row whose version bumps on every write (the engine
// reports it in a decision's reasons) and whose history is the audit log.
// Retire is reversible: a Put on a retired name un-retires it with a new
// version. Groups and memberships are plain rows; bindings have an id so a
// subject may carry several and one may be retired on its own.
//
// Every write bumps acl_version for the tenant in the same transaction, so
// "did anything change for this tenant" is one integer compare (the
// read-side cache invalidation of RFC EY §7.3).

// AclDefRow is one ACL rule list.
type AclDefRow struct {
	// TenantID is the RFC N tenant-isolation axis; "" = the shared/operator
	// tenant (config-declared lists). Set from the principal, never the wire.
	TenantID string `json:"tenant_id,omitempty"`
	Name     string `json:"name"`
	// Version bumps on every Put; a decision's reason names it.
	Version int `json:"version"`
	// ContentSHA256 is over the compacted Rules only (description is not
	// content).
	ContentSHA256 string `json:"content_sha256"`
	// CatalogVersion is authz.CatalogVersion at the time of the write; a
	// row older than the running catalog is re-validated at read.
	CatalogVersion int             `json:"catalog_version"`
	Rules          json.RawMessage `json:"rules"`
	Description    string          `json:"description,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CreatedBy      string          `json:"created_by,omitempty"`
	// RetiredAt is zero while the list is live.
	RetiredAt time.Time `json:"retired_at,omitempty"`
}

// Retired reports whether the list is retired.
func (r AclDefRow) Retired() bool { return !r.RetiredAt.IsZero() }

// GroupRow is one user group.
type GroupRow struct {
	TenantID    string `json:"tenant_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ExternalID is set on a group an identity provider pushes (RFC FA);
	// the management surfaces treat such a group as read-only locally.
	ExternalID string    `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	CreatedBy  string    `json:"created_by,omitempty"`
}

// Group member kinds.
const (
	GroupMemberUser  = "user"
	GroupMemberGroup = "group"
)

// GroupMemberRow is one membership edge: MemberID (a subject or a group
// name, per MemberKind) is a member of GroupName.
type GroupMemberRow struct {
	TenantID   string    `json:"tenant_id,omitempty"`
	GroupName  string    `json:"group"`
	MemberKind string    `json:"member_kind"`
	MemberID   string    `json:"member_id"`
	AddedAt    time.Time `json:"added_at"`
	AddedBy    string    `json:"added_by,omitempty"`
}

// AclBindingRow attaches an ACL (by name) to a subject.
type AclBindingRow struct {
	BindingID string `json:"binding_id"`
	TenantID  string `json:"tenant_id,omitempty"`
	ACLName   string `json:"acl"`
	// SubjectKind is user | group | token | tenant (authz.SubjectKind).
	SubjectKind string `json:"subject_kind"`
	// SubjectID is the subject, group name or token def id; "" for tenant.
	SubjectID string    `json:"subject_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	RetiredAt time.Time `json:"retired_at,omitempty"`
}

// AclStore persists RFC EY groups, ACL rule lists and bindings.
type AclStore interface {
	// AclDefPut creates the (tenant_id, name) list or updates it in place,
	// bumping Version, re-stamping UpdatedAt and clearing RetiredAt. The
	// rules are validated against the catalog before the write (an
	// unenforceable list is never stored) and ContentSHA256 / CatalogVersion
	// are computed here, not supplied. A name with the authz builtin prefix
	// is refused. CreatedAt and CreatedBy are kept from the original create.
	AclDefPut(ctx context.Context, row AclDefRow) (AclDefRow, error)
	// AclDefGet returns the list, retired or not; *ErrNotFound on a miss
	// (an opaque miss across tenants).
	AclDefGet(ctx context.Context, tenantID, name string) (AclDefRow, error)
	// AclDefList returns the tenant's lists ordered by name; retired ones
	// only when includeRetired. Empty slice (not nil) when none.
	AclDefList(ctx context.Context, tenantID string, includeRetired bool) ([]AclDefRow, error)
	// AclDefRetire marks the list retired; (false, nil) when there is no
	// live row of that name. Bindings to it stay and match nothing.
	AclDefRetire(ctx context.Context, tenantID, name string) (bool, error)

	// GroupPut creates or updates the (tenant_id, name) group (description
	// and external_id); CreatedAt / CreatedBy are kept on an update.
	GroupPut(ctx context.Context, row GroupRow) (GroupRow, error)
	GroupGet(ctx context.Context, tenantID, name string) (GroupRow, error)
	// GroupList returns the tenant's groups ordered by name.
	GroupList(ctx context.Context, tenantID string) ([]GroupRow, error)
	// GroupDelete removes the group and every membership edge that names
	// it, as the group or as a member; (false, nil) when absent. Bindings
	// to the group stay and apply to no one.
	GroupDelete(ctx context.Context, tenantID, name string) (bool, error)
	// GroupMemberAdd adds one edge. *ErrNotFound when the group (or, for a
	// group member, the member group) does not exist in the tenant;
	// *ErrConflict when the edge exists; an error naming the group when the
	// edge would close a cycle or nest deeper than the engine's cap.
	GroupMemberAdd(ctx context.Context, row GroupMemberRow) error
	// GroupMemberRemove removes one edge; (false, nil) when absent.
	GroupMemberRemove(ctx context.Context, tenantID, groupName, memberKind, memberID string) (bool, error)
	// GroupMembers lists the group's direct members ordered by kind, id.
	GroupMembers(ctx context.Context, tenantID, groupName string) ([]GroupMemberRow, error)
	// GroupGraph returns group → its direct member GROUPS for the tenant
	// (the input to authz.Closure / authz.ValidateGroupGraph); every group
	// appears as a key, with nil for no member groups.
	GroupGraph(ctx context.Context, tenantID string) (map[string][]string, error)
	// GroupsOfSubject returns the groups the subject is a DIRECT member of,
	// sorted. Transitive membership is authz.Closure over GroupGraph.
	GroupsOfSubject(ctx context.Context, tenantID, subject string) ([]string, error)

	// AclBindingPut inserts a binding, minting BindingID when empty;
	// *ErrConflict when the id exists. The ACL need not exist yet (a
	// binding to a missing list matches nothing and the doctor names it).
	AclBindingPut(ctx context.Context, row AclBindingRow) (AclBindingRow, error)
	// AclBindingRetire marks one binding retired; (false, nil) when there is
	// no live binding of that id in the tenant.
	AclBindingRetire(ctx context.Context, tenantID, bindingID string) (bool, error)
	// AclBindingList returns the tenant's bindings ordered by created_at,
	// id; retired ones only when includeRetired.
	AclBindingList(ctx context.Context, tenantID string, includeRetired bool) ([]AclBindingRow, error)
	// AclBindingsFor returns the LIVE bindings that apply to a principal:
	// the tenant-wide ones, the subject's own, those of any of the given
	// (already closed) groups, and the token definition's. Expiry is left
	// to the engine (authz.Binding.Active), so a just-expired binding is
	// still returned and Explain can show it.
	AclBindingsFor(ctx context.Context, tenantID, subject string, groups []string, tokenDefID string) ([]AclBindingRow, error)

	// AclVersion returns the tenant's current version: 0 before any write,
	// bumped by every write above. Replicas compare it to decide whether a
	// cached Effective set is current.
	AclVersion(ctx context.Context, tenantID string) (int64, error)
}

// optTime is a time the JSON form leaves out when it was never set:
// `omitempty` does nothing for a time.Time (a struct is never "empty"), so
// an unset expiry or retirement was written as 0001-01-01T00:00:00Z. A set
// time marshals exactly as before.
func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// MarshalJSON leaves RetiredAt out while the list is live.
func (r AclDefRow) MarshalJSON() ([]byte, error) {
	type plain AclDefRow
	return json.Marshal(struct {
		plain
		RetiredAt *time.Time `json:"retired_at,omitempty"`
	}{plain(r), optTime(r.RetiredAt)})
}

// MarshalJSON leaves ExpiresAt and RetiredAt out when they are unset.
func (r AclBindingRow) MarshalJSON() ([]byte, error) {
	type plain AclBindingRow
	return json.Marshal(struct {
		plain
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
		RetiredAt *time.Time `json:"retired_at,omitempty"`
	}{plain(r), optTime(r.ExpiresAt), optTime(r.RetiredAt)})
}
