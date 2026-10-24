package sqlshared

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Acl* implements store.AclStore for both stores (RFC EY P1). Every write
// runs in one transaction with the tenant's acl_version bump, so a reader
// that sees the new version sees the write.

// nullTimeArg binds t, or NULL when zero.
func nullTimeArg(d Dialect, t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return d.TimeArg(t)
}

// nullTimeDest scans a nullable time column written by nullTimeArg into
// *dst, leaving the zero time for NULL.
type nullTimeDest struct{ dst *time.Time }

func (n *nullTimeDest) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n.dst = time.Time{}
	case int64:
		*n.dst = time.Unix(0, v)
	case time.Time:
		*n.dst = v
	default:
		return fmt.Errorf("sqlshared: nullable time column holds %T", src)
	}
	return nil
}

func bumpAclVersion(ctx context.Context, q Querier, tenantID string) error {
	d := q.Dialect()
	_, err := q.Exec(ctx, `INSERT INTO acl_version (tenant_id, version, updated_at) VALUES (?, 1, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET version = acl_version.version + 1, updated_at = excluded.updated_at`,
		tenantID, d.TimeArg(time.Now().UTC()))
	return err
}

// AclVersion returns the tenant's version (0 before any write).
func AclVersion(ctx context.Context, q Querier, tenantID string) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT version FROM acl_version WHERE tenant_id = ?`, tenantID).Scan(&v)
	if IsNoRows(err) {
		return 0, nil
	}
	return v, err
}

// ---- ACL defs ----

func aclDefSelect(d Dialect) string {
	return `SELECT tenant_id, name, version, content_sha256, catalog_version, ` + d.JSONText("rules") +
		`, description, created_at, updated_at, created_by, retired_at FROM acl_defs`
}

func scanAclDef(d Dialect, sc Row) (store.AclDefRow, error) {
	var (
		r     store.AclDefRow
		rules string
	)
	if err := sc.Scan(&r.TenantID, &r.Name, &r.Version, &r.ContentSHA256, &r.CatalogVersion, &rules, &r.Description,
		d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt), &r.CreatedBy, &nullTimeDest{&r.RetiredAt}); err != nil {
		return store.AclDefRow{}, err
	}
	r.Rules = json.RawMessage(rules)
	return r, nil
}

func collectAclDefs(d Dialect, rows Rows) ([]store.AclDefRow, error) {
	defer rows.Close()
	out := []store.AclDefRow{}
	for rows.Next() {
		r, err := scanAclDef(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AclDefPut validates the rules, computes the hash and catalog version, and
// inserts or updates the row with a version bump.
func AclDefPut(ctx context.Context, db DB, row store.AclDefRow) (store.AclDefRow, error) {
	if !authz.ValidName(row.Name) {
		return store.AclDefRow{}, fmt.Errorf("acl def put: %q is not a valid name", row.Name)
	}
	if authz.IsBuiltin(row.Name) {
		return store.AclDefRow{}, fmt.Errorf("acl def put: %q is a built-in name", row.Name)
	}
	if len(row.Description) > authz.MaxDescriptionBytes {
		return store.AclDefRow{}, fmt.Errorf("acl def put: description is longer than %d bytes", authz.MaxDescriptionBytes)
	}
	rules, err := authz.ParseRules(row.Rules)
	if err != nil {
		return store.AclDefRow{}, fmt.Errorf("acl def put: %w", err)
	}
	if len(rules) == 0 {
		return store.AclDefRow{}, fmt.Errorf("acl def put: no rules; an empty list grants and denies nothing and is a mistake")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, row.Rules); err != nil {
		return store.AclDefRow{}, fmt.Errorf("acl def put: %w", err)
	}
	sum := sha256.Sum256(compact.Bytes())
	sha := hex.EncodeToString(sum[:])
	d := db.Dialect()
	now := d.TimeArg(time.Now().UTC())
	err = db.InTx(ctx, func(tx Querier) error {
		// The UPDATE arm keeps created_at / created_by, bumps the version
		// and clears retired_at (a Put on a retired name un-retires it).
		if _, err := tx.Exec(ctx, `INSERT INTO acl_defs
			(tenant_id, name, version, content_sha256, catalog_version, rules, description, created_at, updated_at, created_by, retired_at)
			VALUES (?, ?, 1, ?, ?, `+d.JSONArg()+`, ?, ?, ?, ?, NULL)
			ON CONFLICT (tenant_id, name) DO UPDATE SET
			    version = acl_defs.version + 1,
			    content_sha256 = excluded.content_sha256,
			    catalog_version = excluded.catalog_version,
			    rules = excluded.rules,
			    description = excluded.description,
			    updated_at = excluded.updated_at,
			    retired_at = NULL`,
			row.TenantID, row.Name, sha, authz.CatalogVersion, compact.String(), row.Description, now, now, row.CreatedBy); err != nil {
			return err
		}
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return store.AclDefRow{}, fmt.Errorf("acl def put: %w", err)
	}
	return AclDefGet(ctx, db, row.TenantID, row.Name)
}

func AclDefGet(ctx context.Context, q Querier, tenantID, name string) (store.AclDefRow, error) {
	d := q.Dialect()
	r, err := scanAclDef(d, q.QueryRow(ctx, aclDefSelect(d)+` WHERE tenant_id = ? AND name = ?`, tenantID, name))
	if err != nil {
		return store.AclDefRow{}, NotFound(err, "acl", name)
	}
	return r, nil
}

func AclDefList(ctx context.Context, q Querier, tenantID string, includeRetired bool) ([]store.AclDefRow, error) {
	d := q.Dialect()
	query := aclDefSelect(d) + ` WHERE tenant_id = ?`
	if !includeRetired {
		query += ` AND retired_at IS NULL`
	}
	rows, err := q.Query(ctx, query+` ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	return collectAclDefs(d, rows)
}

func AclDefRetire(ctx context.Context, db DB, tenantID, name string) (bool, error) {
	d := db.Dialect()
	var n int64
	err := db.InTx(ctx, func(tx Querier) error {
		var err error
		n, err = tx.Exec(ctx, `UPDATE acl_defs SET retired_at = ? WHERE tenant_id = ? AND name = ? AND retired_at IS NULL`,
			d.TimeArg(time.Now().UTC()), tenantID, name)
		if err != nil || n == 0 {
			return err
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	return n > 0, err
}

// ---- groups ----

func groupSelect() string {
	return `SELECT tenant_id, name, description, external_id, created_at, updated_at, created_by FROM user_groups`
}

func scanGroup(d Dialect, sc Row) (store.GroupRow, error) {
	var r store.GroupRow
	if err := sc.Scan(&r.TenantID, &r.Name, &r.Description, &r.ExternalID, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt), &r.CreatedBy); err != nil {
		return store.GroupRow{}, err
	}
	return r, nil
}

func GroupPut(ctx context.Context, db DB, row store.GroupRow) (store.GroupRow, error) {
	if !authz.ValidName(row.Name) {
		return store.GroupRow{}, fmt.Errorf("group put: %q is not a valid name", row.Name)
	}
	if len(row.Description) > authz.MaxDescriptionBytes {
		return store.GroupRow{}, fmt.Errorf("group put: description is longer than %d bytes", authz.MaxDescriptionBytes)
	}
	d := db.Dialect()
	now := d.TimeArg(time.Now().UTC())
	err := db.InTx(ctx, func(tx Querier) error {
		if _, err := tx.Exec(ctx, `INSERT INTO user_groups (tenant_id, name, description, external_id, created_at, updated_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, name) DO UPDATE SET
			    description = excluded.description,
			    external_id = excluded.external_id,
			    updated_at = excluded.updated_at`,
			row.TenantID, row.Name, row.Description, row.ExternalID, now, now, row.CreatedBy); err != nil {
			return err
		}
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return store.GroupRow{}, fmt.Errorf("group put: %w", err)
	}
	return GroupGet(ctx, db, row.TenantID, row.Name)
}

func GroupGet(ctx context.Context, q Querier, tenantID, name string) (store.GroupRow, error) {
	r, err := scanGroup(q.Dialect(), q.QueryRow(ctx, groupSelect()+` WHERE tenant_id = ? AND name = ?`, tenantID, name))
	if err != nil {
		return store.GroupRow{}, NotFound(err, "group", name)
	}
	return r, nil
}

func GroupList(ctx context.Context, q Querier, tenantID string) ([]store.GroupRow, error) {
	rows, err := q.Query(ctx, groupSelect()+` WHERE tenant_id = ? ORDER BY name`, tenantID)
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

func GroupDelete(ctx context.Context, db DB, tenantID, name string) (bool, error) {
	var n int64
	err := db.InTx(ctx, func(tx Querier) error {
		var err error
		n, err = tx.Exec(ctx, `DELETE FROM user_groups WHERE tenant_id = ? AND name = ?`, tenantID, name)
		if err != nil || n == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_group_members WHERE tenant_id = ? AND (group_name = ? OR (member_kind = 'group' AND member_id = ?))`,
			tenantID, name, name); err != nil {
			return err
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	return n > 0, err
}

func groupExists(ctx context.Context, q Querier, tenantID, name string) (bool, error) {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM user_groups WHERE tenant_id = ? AND name = ?`, tenantID, name).Scan(&one)
	if IsNoRows(err) {
		return false, nil
	}
	return err == nil, err
}

func GroupMemberAdd(ctx context.Context, db DB, row store.GroupMemberRow) error {
	if row.MemberKind != store.GroupMemberUser && row.MemberKind != store.GroupMemberGroup {
		return fmt.Errorf("group member add: member_kind must be user or group, got %q", row.MemberKind)
	}
	if row.MemberID == "" || len(row.MemberID) > authz.MaxSubjectIDBytes {
		return fmt.Errorf("group member add: member_id is empty or longer than %d bytes", authz.MaxSubjectIDBytes)
	}
	if row.MemberKind == store.GroupMemberGroup && row.MemberID == row.GroupName {
		return fmt.Errorf("group member add: group %q cannot be a member of itself", row.GroupName)
	}
	d := db.Dialect()
	return db.InTx(ctx, func(tx Querier) error {
		if ok, err := groupExists(ctx, tx, row.TenantID, row.GroupName); err != nil {
			return err
		} else if !ok {
			return &store.ErrNotFound{Kind: "group", ID: row.GroupName}
		}
		if row.MemberKind == store.GroupMemberGroup {
			if ok, err := groupExists(ctx, tx, row.TenantID, row.MemberID); err != nil {
				return err
			} else if !ok {
				return &store.ErrNotFound{Kind: "group", ID: row.MemberID}
			}
			graph, err := GroupGraph(ctx, tx, row.TenantID)
			if err != nil {
				return err
			}
			graph[row.GroupName] = append(graph[row.GroupName], row.MemberID)
			if err := authz.ValidateGroupGraph(graph); err != nil {
				return fmt.Errorf("group member add: %w", err)
			}
		}
		n, err := tx.Exec(ctx, `INSERT INTO user_group_members (tenant_id, group_name, member_kind, member_id, added_at, added_by)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (tenant_id, group_name, member_kind, member_id) DO NOTHING`,
			row.TenantID, row.GroupName, row.MemberKind, row.MemberID, d.TimeArg(time.Now().UTC()), row.AddedBy)
		if err != nil {
			return err
		}
		if n == 0 {
			return &store.ErrConflict{Kind: "group_member", ID: row.MemberKind + ":" + row.MemberID}
		}
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
}

func GroupMemberRemove(ctx context.Context, db DB, tenantID, groupName, memberKind, memberID string) (bool, error) {
	var n int64
	err := db.InTx(ctx, func(tx Querier) error {
		var err error
		n, err = tx.Exec(ctx, `DELETE FROM user_group_members WHERE tenant_id = ? AND group_name = ? AND member_kind = ? AND member_id = ?`,
			tenantID, groupName, memberKind, memberID)
		if err != nil || n == 0 {
			return err
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	return n > 0, err
}

func GroupMembers(ctx context.Context, q Querier, tenantID, groupName string) ([]store.GroupMemberRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, `SELECT tenant_id, group_name, member_kind, member_id, added_at, added_by FROM user_group_members
		WHERE tenant_id = ? AND group_name = ? ORDER BY member_kind, member_id`, tenantID, groupName)
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

// GroupGraph returns group → direct member groups; every group of the
// tenant is a key.
func GroupGraph(ctx context.Context, q Querier, tenantID string) (map[string][]string, error) {
	graph := map[string][]string{}
	rows, err := q.Query(ctx, `SELECT name FROM user_groups WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		graph[name] = nil
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT group_name, member_id FROM user_group_members WHERE tenant_id = ? AND member_kind = 'group' ORDER BY group_name, member_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g, m string
		if err := rows.Scan(&g, &m); err != nil {
			return nil, err
		}
		graph[g] = append(graph[g], m)
	}
	return graph, rows.Err()
}

func GroupsOfSubject(ctx context.Context, q Querier, tenantID, subject string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT group_name FROM user_group_members WHERE tenant_id = ? AND member_kind = 'user' AND member_id = ?`, tenantID, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// AclEraseSubject removes the subject's user-kind memberships and
// user-kind bindings in one transaction with a single version bump.
func AclEraseSubject(ctx context.Context, db DB, tenantID, subject string) (memberships, bindings int, err error) {
	if subject == "" {
		return 0, 0, fmt.Errorf("acl erase subject: subject is required")
	}
	err = db.InTx(ctx, func(tx Querier) error {
		m, err := tx.Exec(ctx, `DELETE FROM user_group_members WHERE tenant_id = ? AND member_kind = 'user' AND member_id = ?`,
			tenantID, subject)
		if err != nil {
			return err
		}
		b, err := tx.Exec(ctx, `DELETE FROM acl_bindings WHERE tenant_id = ? AND subject_kind = 'user' AND subject_id = ?`,
			tenantID, subject)
		if err != nil {
			return err
		}
		memberships, bindings = int(m), int(b)
		if m+b == 0 {
			return nil
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	if err != nil {
		return 0, 0, err
	}
	return memberships, bindings, nil
}

// UserDeleteUnbind deletes one user row and, in the same transaction, unbinds
// the subject (RFC FA §3.7c): every live user-kind binding to it in the tenant
// is retired (kept, so the record of what it held stays) and its user-kind
// group memberships are removed, with one acl_version bump. A user given the
// same subject later — operator-chosen subjects repeat — then starts with none
// of the deleted user's authority. Group-kind rows whose id equals the subject
// are a group's, and stay. Reports whether a user row went.
func UserDeleteUnbind(ctx context.Context, db DB, tenantID, subject string) (bool, error) {
	var removed bool
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `DELETE FROM users WHERE tenant_id = ? AND subject = ?`, tenantID, subject)
		if err != nil || n == 0 {
			return err
		}
		removed = true
		b, err := tx.Exec(ctx, `UPDATE acl_bindings SET retired_at = ?
			WHERE tenant_id = ? AND subject_kind = 'user' AND subject_id = ? AND retired_at IS NULL`,
			tx.Dialect().TimeArg(time.Now().UTC()), tenantID, subject)
		if err != nil {
			return err
		}
		m, err := tx.Exec(ctx, `DELETE FROM user_group_members WHERE tenant_id = ? AND member_kind = 'user' AND member_id = ?`,
			tenantID, subject)
		if err != nil {
			return err
		}
		if b+m == 0 {
			return nil
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	if err != nil {
		return false, fmt.Errorf("user delete: %w", err)
	}
	return removed, nil
}

// AclEraseTokenBindings removes the token-kind bindings of defIDs.
func AclEraseTokenBindings(ctx context.Context, db DB, tenantID string, defIDs []string) (int, error) {
	if len(defIDs) == 0 {
		return 0, nil
	}
	n := 0
	// One DELETE per id rather than an IN list: a subject holds a handful of
	// tokens, and this keeps the statement the same on both dialects.
	err := db.InTx(ctx, func(tx Querier) error {
		n = 0
		for _, id := range defIDs {
			res, err := tx.Exec(ctx, `DELETE FROM acl_bindings WHERE tenant_id = ? AND subject_kind = 'token' AND subject_id = ?`,
				tenantID, id)
			if err != nil {
				return err
			}
			n += int(res)
		}
		if n == 0 {
			return nil
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---- bindings ----

func mintBindingID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "aclb_" + hex.EncodeToString(b[:])
}

func bindingSelect() string {
	return `SELECT binding_id, tenant_id, acl_name, subject_kind, subject_id, expires_at, created_at, created_by, retired_at FROM acl_bindings`
}

func scanBinding(d Dialect, sc Row) (store.AclBindingRow, error) {
	var r store.AclBindingRow
	if err := sc.Scan(&r.BindingID, &r.TenantID, &r.ACLName, &r.SubjectKind, &r.SubjectID, &nullTimeDest{&r.ExpiresAt},
		d.TimeDest(&r.CreatedAt), &r.CreatedBy, &nullTimeDest{&r.RetiredAt}); err != nil {
		return store.AclBindingRow{}, err
	}
	return r, nil
}

func collectBindings(d Dialect, rows Rows) ([]store.AclBindingRow, error) {
	defer rows.Close()
	out := []store.AclBindingRow{}
	for rows.Next() {
		r, err := scanBinding(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func AclBindingPut(ctx context.Context, db DB, row store.AclBindingRow) (store.AclBindingRow, error) {
	b := authz.Binding{ACL: row.ACLName, SubjectKind: authz.SubjectKind(row.SubjectKind), SubjectID: row.SubjectID, ExpiresAt: row.ExpiresAt}
	if err := b.Validate(); err != nil {
		return store.AclBindingRow{}, fmt.Errorf("acl binding put: %w", err)
	}
	if row.BindingID == "" {
		row.BindingID = mintBindingID()
	}
	d := db.Dialect()
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `INSERT INTO acl_bindings (binding_id, tenant_id, acl_name, subject_kind, subject_id, expires_at, created_at, created_by, retired_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL) ON CONFLICT (binding_id) DO NOTHING`,
			row.BindingID, row.TenantID, row.ACLName, row.SubjectKind, row.SubjectID, nullTimeArg(d, row.ExpiresAt), d.TimeArg(time.Now().UTC()), row.CreatedBy)
		if err != nil {
			return err
		}
		if n == 0 {
			return &store.ErrConflict{Kind: "acl_binding", ID: row.BindingID}
		}
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	if err != nil {
		return store.AclBindingRow{}, err
	}
	r, err := scanBinding(d, db.QueryRow(ctx, bindingSelect()+` WHERE binding_id = ?`, row.BindingID))
	if err != nil {
		return store.AclBindingRow{}, NotFound(err, "acl_binding", row.BindingID)
	}
	return r, nil
}

func AclBindingRetire(ctx context.Context, db DB, tenantID, bindingID string) (bool, error) {
	d := db.Dialect()
	var n int64
	err := db.InTx(ctx, func(tx Querier) error {
		var err error
		n, err = tx.Exec(ctx, `UPDATE acl_bindings SET retired_at = ? WHERE tenant_id = ? AND binding_id = ? AND retired_at IS NULL`,
			d.TimeArg(time.Now().UTC()), tenantID, bindingID)
		if err != nil || n == 0 {
			return err
		}
		return bumpAclVersion(ctx, tx, tenantID)
	})
	return n > 0, err
}

func AclBindingList(ctx context.Context, q Querier, tenantID string, includeRetired bool) ([]store.AclBindingRow, error) {
	d := q.Dialect()
	query := bindingSelect() + ` WHERE tenant_id = ?`
	if !includeRetired {
		query += ` AND retired_at IS NULL`
	}
	rows, err := q.Query(ctx, query+` ORDER BY created_at, binding_id`, tenantID)
	if err != nil {
		return nil, err
	}
	return collectBindings(d, rows)
}

func AclBindingsFor(ctx context.Context, q Querier, tenantID, subject string, groups []string, tokenDefID string) ([]store.AclBindingRow, error) {
	d := q.Dialect()
	var (
		clauses []string
		args    = []any{tenantID}
	)
	clauses = append(clauses, `subject_kind = 'tenant'`)
	if subject != "" {
		clauses = append(clauses, `(subject_kind = 'user' AND subject_id = ?)`)
		args = append(args, subject)
	}
	if tokenDefID != "" {
		clauses = append(clauses, `(subject_kind = 'token' AND subject_id = ?)`)
		args = append(args, tokenDefID)
	}
	if len(groups) > 0 {
		clauses = append(clauses, `(subject_kind = 'group' AND subject_id IN (`+Placeholders(len(groups))+`))`)
		for _, g := range groups {
			args = append(args, g)
		}
	}
	rows, err := q.Query(ctx, bindingSelect()+` WHERE tenant_id = ? AND retired_at IS NULL AND (`+strings.Join(clauses, " OR ")+`) ORDER BY created_at, binding_id`, args...)
	if err != nil {
		return nil, err
	}
	return collectBindings(d, rows)
}
