package sqlshared

import (
	"context"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The snapshot reads and restores of the RFC EY tables (store.SnapshotReadAcl
// and the SnapshotRestoreAcl* family). Restore follows every other section:
// the row is inserted with its own key, version and times, and a row the
// target already holds on that key stands.

// SnapshotReadAcl reads every tenant's lists, groups, memberships and
// bindings, retired ones included.
func SnapshotReadAcl(ctx context.Context, q Querier) (store.AclSnapshot, error) {
	d := q.Dialect()
	var out store.AclSnapshot
	rows, err := q.Query(ctx, aclDefSelect(d)+` ORDER BY tenant_id, name`)
	if err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read acl_defs: %w", err)
	}
	if out.Defs, err = collectAclDefs(d, rows); err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read acl_defs: %w", err)
	}
	if out.Groups, err = snapshotReadGroups(ctx, q); err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read user_groups: %w", err)
	}
	if out.Members, err = snapshotReadGroupMembers(ctx, q); err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read user_group_members: %w", err)
	}
	rows, err = q.Query(ctx, bindingSelect()+` ORDER BY tenant_id, created_at, binding_id`)
	if err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read acl_bindings: %w", err)
	}
	if out.Bindings, err = collectBindings(d, rows); err != nil {
		return store.AclSnapshot{}, fmt.Errorf("snapshot read acl_bindings: %w", err)
	}
	return out, nil
}

func snapshotReadGroups(ctx context.Context, q Querier) ([]store.GroupRow, error) {
	rows, err := q.Query(ctx, groupSelect()+` ORDER BY tenant_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.GroupRow{}
	for rows.Next() {
		r, err := scanGroup(q.Dialect(), rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func snapshotReadGroupMembers(ctx context.Context, q Querier) ([]store.GroupMemberRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, `SELECT tenant_id, group_name, member_kind, member_id, added_at, added_by FROM user_group_members
		ORDER BY tenant_id, group_name, member_kind, member_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.GroupMemberRow{}
	for rows.Next() {
		var r store.GroupMemberRow
		if err := rows.Scan(&r.TenantID, &r.GroupName, &r.MemberKind, &r.MemberID, d.TimeDest(&r.AddedAt), &r.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// orNow is t, or now when t is zero: a hand-written envelope may leave a time
// out, and the columns are NOT NULL.
func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// SnapshotRestoreAclDef inserts one list keeping its version, times, author
// and retirement; a list already on (tenant_id, name) stands. The rules are
// validated against THIS build's catalog and the hash recomputed, and the row
// is stamped with this catalog's version: a rule a newer build wrote that this
// one cannot enforce is refused, never stored inert.
func SnapshotRestoreAclDef(ctx context.Context, db DB, row store.AclDefRow) (bool, error) {
	compact, sha, err := aclDefContent(row)
	if err != nil {
		return false, fmt.Errorf("acl def restore: %w", err)
	}
	if row.Version < 1 {
		return false, fmt.Errorf("acl def restore: version %d is not positive", row.Version)
	}
	d := db.Dialect()
	created := orNow(row.CreatedAt)
	updated := row.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	var inserted bool
	err = db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `INSERT INTO acl_defs
			(tenant_id, name, version, content_sha256, catalog_version, rules, description, created_at, updated_at, created_by, retired_at)
			VALUES (?, ?, ?, ?, ?, `+d.JSONArg()+`, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, name) DO NOTHING`,
			row.TenantID, row.Name, row.Version, sha, authz.CatalogVersion, compact, row.Description,
			d.TimeArg(created), d.TimeArg(updated), row.CreatedBy, nullTimeArg(d, row.RetiredAt))
		if err != nil || n == 0 {
			return err
		}
		inserted = true
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return false, fmt.Errorf("acl def restore: %w", err)
	}
	return inserted, nil
}

// SnapshotRestoreGroup inserts one group keeping its times and author; a
// group already on (tenant_id, name) stands.
func SnapshotRestoreGroup(ctx context.Context, db DB, row store.GroupRow) (bool, error) {
	if !authz.ValidName(row.Name) {
		return false, fmt.Errorf("group restore: %q is not a valid name", row.Name)
	}
	if len(row.Description) > authz.MaxDescriptionBytes {
		return false, fmt.Errorf("group restore: description is longer than %d bytes", authz.MaxDescriptionBytes)
	}
	d := db.Dialect()
	created := orNow(row.CreatedAt)
	updated := row.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	var inserted bool
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `INSERT INTO user_groups (tenant_id, name, description, external_id, created_at, updated_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, name) DO NOTHING`,
			row.TenantID, row.Name, row.Description, row.ExternalID, d.TimeArg(created), d.TimeArg(updated), row.CreatedBy)
		if err != nil || n == 0 {
			return err
		}
		inserted = true
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return false, fmt.Errorf("group restore: %w", err)
	}
	return inserted, nil
}

// SnapshotRestoreGroupMember inserts one membership edge keeping added_at and
// added_by, with GroupMemberAdd's checks: both groups exist in the tenant and
// the edge closes no cycle and nests no deeper than the cap. An edge already
// there stands. beforeInsert (the backend's manager-link revocation for a
// user member) runs only when the edge is not already there, so re-restoring
// the same snapshot closes no one's links.
func SnapshotRestoreGroupMember(ctx context.Context, db DB, row store.GroupMemberRow, beforeInsert func() error) (bool, error) {
	var one int
	err := db.QueryRow(ctx, `SELECT 1 FROM user_group_members WHERE tenant_id = ? AND group_name = ? AND member_kind = ? AND member_id = ?`,
		row.TenantID, row.GroupName, row.MemberKind, row.MemberID).Scan(&one)
	switch {
	case err == nil:
		return false, nil
	case !IsNoRows(err):
		return false, fmt.Errorf("group member restore: %w", err)
	}
	if beforeInsert != nil {
		if err := beforeInsert(); err != nil {
			return false, fmt.Errorf("group member restore: %w", err)
		}
	}
	return insertGroupMember(ctx, db, row, orNow(row.AddedAt))
}

// SnapshotRestoreAclBinding inserts one binding keeping its id, expiry,
// times, author and retirement; a binding already on the id in the same
// tenant stands. An id held by ANOTHER tenant is an error rather than a
// silent skip — binding ids are global, and the operator should learn the row
// did not land. beforeInsert (the backend's manager-link revocation) runs only
// for a live binding not already there: a retired binding grants nothing.
func SnapshotRestoreAclBinding(ctx context.Context, db DB, row store.AclBindingRow, beforeInsert func() error) (bool, error) {
	b := authz.Binding{ACL: row.ACLName, SubjectKind: authz.SubjectKind(row.SubjectKind), SubjectID: row.SubjectID, ExpiresAt: row.ExpiresAt}
	if err := b.Validate(); err != nil {
		return false, fmt.Errorf("acl binding restore: %w", err)
	}
	if row.BindingID == "" || len(row.BindingID) > authz.MaxSubjectIDBytes {
		return false, fmt.Errorf("acl binding restore: binding_id is empty or longer than %d bytes", authz.MaxSubjectIDBytes)
	}
	var tenant string
	err := db.QueryRow(ctx, `SELECT tenant_id FROM acl_bindings WHERE binding_id = ?`, row.BindingID).Scan(&tenant)
	switch {
	case err == nil && tenant == row.TenantID:
		return false, nil
	case err == nil:
		return false, fmt.Errorf("acl binding restore: id %q is held by another tenant here", row.BindingID)
	case !IsNoRows(err):
		return false, fmt.Errorf("acl binding restore: %w", err)
	}
	if beforeInsert != nil && row.RetiredAt.IsZero() {
		if err := beforeInsert(); err != nil {
			return false, fmt.Errorf("acl binding restore: %w", err)
		}
	}
	d := db.Dialect()
	var inserted bool
	err = db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `INSERT INTO acl_bindings (binding_id, tenant_id, acl_name, subject_kind, subject_id, expires_at, created_at, created_by, retired_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (binding_id) DO NOTHING`,
			row.BindingID, row.TenantID, row.ACLName, row.SubjectKind, row.SubjectID, nullTimeArg(d, row.ExpiresAt),
			d.TimeArg(orNow(row.CreatedAt)), row.CreatedBy, nullTimeArg(d, row.RetiredAt))
		if err != nil || n == 0 {
			return err
		}
		inserted = true
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return false, fmt.Errorf("acl binding restore: %w", err)
	}
	return inserted, nil
}
