package sqlshared

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SCIM* implements store.SCIMStore for both stores (RFC FA P6; postgres
// migration 0126). Every statement names the gate and the tenant: a SCIM
// credential's tenant is its token's, and nothing here reads across tenants.

const scimUserCols = `gate, tenant_id, scim_id, subject, user_name, user_name_key, external_id, attributes, version,
	created_at, updated_at, deleted_at`

func scanSCIMUser(d Dialect, sc Row) (store.SCIMUserRow, error) {
	var r store.SCIMUserRow
	err := sc.Scan(&r.Gate, &r.TenantID, &r.SCIMID, &r.Subject, &r.UserName, &r.UserNameKey, &r.ExternalID, &r.Attributes,
		&r.Version, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt), &nullTimeDest{&r.DeletedAt})
	return r, err
}

func scimAttrs(a string) string {
	if a == "" {
		return "{}"
	}
	return a
}

// SCIMUserCreate is store.SCIMStore.SCIMUserCreate. q is a transaction the
// backend opened holding its per-(gate, tenant) create lock, so the name
// check, the count and the insert are one serial step: concurrent creates
// cannot pass the ceiling together. The name is checked before the insert
// because a failed statement aborts a Postgres transaction.
func SCIMUserCreate(ctx context.Context, q Querier, row store.SCIMUserRow, since time.Time, ceiling int) error {
	if row.Gate == "" || row.TenantID == "" || row.SCIMID == "" || row.Subject == "" || row.UserNameKey == "" {
		return fmt.Errorf("scim user create: gate, tenant, id, subject and user name are required")
	}
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM scim_users WHERE gate = ? AND tenant_id = ? AND user_name_key = ? AND deleted_at IS NULL`,
		row.Gate, row.TenantID, row.UserNameKey).Scan(&one)
	switch {
	case err == nil:
		return &store.ErrConflict{Kind: "scim_user_name", ID: row.UserName}
	case !IsNoRows(err):
		return fmt.Errorf("scim user create: name: %w", err)
	}
	n, err := scimUsersCreatedSince(ctx, q, row.Gate, row.TenantID, since)
	if err != nil {
		return fmt.Errorf("scim user create: count: %w", err)
	}
	if n >= ceiling {
		return store.ErrSCIMCreateCeiling
	}
	d := q.Dialect()
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.Version == 0 {
		row.Version = 1
	}
	if _, err := q.Exec(ctx, `INSERT INTO scim_users (`+scimUserCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		row.Gate, row.TenantID, row.SCIMID, row.Subject, row.UserName, row.UserNameKey, row.ExternalID, scimAttrs(row.Attributes),
		row.Version, d.TimeArg(row.CreatedAt), d.TimeArg(row.UpdatedAt)); err != nil {
		if IsUniqueViolation(err) {
			// The name was free under the lock: the subject's live resource
			// (or the id) is what clashed.
			return &store.ErrConflict{Kind: "scim_user", ID: row.SCIMID}
		}
		return fmt.Errorf("scim user create: %w", err)
	}
	return nil
}

func SCIMUserGet(ctx context.Context, q Querier, gate, tenantID, scimID string) (store.SCIMUserRow, error) {
	r, err := scanSCIMUser(q.Dialect(), q.QueryRow(ctx, `SELECT `+scimUserCols+` FROM scim_users
		WHERE gate = ? AND tenant_id = ? AND scim_id = ? AND deleted_at IS NULL`, gate, tenantID, scimID))
	if err != nil {
		return store.SCIMUserRow{}, NotFound(err, "scim_user", scimID)
	}
	return r, nil
}

// scimWhere is the gate's and tenant's rows matching f (live rows only for
// users).
func scimWhere(gate, tenantID string, f store.SCIMFilter, group bool) (string, []any) {
	clauses := []string{"gate = ?", "tenant_id = ?"}
	args := []any{gate, tenantID}
	if !group {
		clauses = append(clauses, "deleted_at IS NULL")
		if f.UserNameKey != "" {
			clauses = append(clauses, "user_name_key = ?")
			args = append(args, f.UserNameKey)
		}
	} else if f.DisplayKey != "" {
		clauses = append(clauses, "display_key = ?")
		args = append(args, f.DisplayKey)
	}
	if f.ExternalID != "" {
		clauses = append(clauses, "external_id = ?")
		args = append(args, f.ExternalID)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func SCIMUserList(ctx context.Context, q Querier, gate, tenantID string, f store.SCIMFilter, offset, limit int) ([]store.SCIMUserRow, int, error) {
	where, args := scimWhere(gate, tenantID, f, false)
	var total int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM scim_users`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT `+scimUserCols+` FROM scim_users`+where+` ORDER BY created_at, scim_id LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []store.SCIMUserRow{}
	for rows.Next() {
		r, err := scanSCIMUser(q.Dialect(), rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func SCIMUserUpdate(ctx context.Context, q Querier, row store.SCIMUserRow) error {
	if row.UserNameKey == "" {
		return fmt.Errorf("scim user update: user name is required")
	}
	n, err := q.Exec(ctx, `UPDATE scim_users SET user_name = ?, user_name_key = ?, external_id = ?, attributes = ?,
		version = version + 1, updated_at = ? WHERE gate = ? AND tenant_id = ? AND scim_id = ? AND deleted_at IS NULL`,
		row.UserName, row.UserNameKey, row.ExternalID, scimAttrs(row.Attributes),
		q.Dialect().TimeArg(time.Now().UTC()), row.Gate, row.TenantID, row.SCIMID)
	if err != nil {
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "scim_user_name", ID: row.UserName}
		}
		return err
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "scim_user", ID: row.SCIMID}
	}
	return nil
}

func SCIMUserMarkDeleted(ctx context.Context, db DB, gate, tenantID, scimID string, at time.Time) (bool, error) {
	deleted := false
	err := db.InTx(ctx, func(tx Querier) error {
		var subject string
		err := tx.QueryRow(ctx, `UPDATE scim_users SET deleted_at = ?, version = version + 1, updated_at = ?
			WHERE gate = ? AND tenant_id = ? AND scim_id = ? AND deleted_at IS NULL RETURNING subject`,
			tx.Dialect().TimeArg(at), tx.Dialect().TimeArg(at), gate, tenantID, scimID).Scan(&subject)
		if IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		deleted = true
		_, err = tx.Exec(ctx, `DELETE FROM scim_group_members WHERE gate = ? AND tenant_id = ? AND subject = ?`, gate, tenantID, subject)
		return err
	})
	return deleted, err
}

func SCIMUserDiscard(ctx context.Context, q Querier, gate, tenantID, scimID string) error {
	_, err := q.Exec(ctx, `DELETE FROM scim_users WHERE gate = ? AND tenant_id = ? AND scim_id = ?`, gate, tenantID, scimID)
	return err
}

func SCIMRemovalBase(ctx context.Context, q Querier, gate, tenantID string, before time.Time) (int, error) {
	var n int
	// Not disabled now, or disabled by a removal inside the window: the
	// users the gate had when the window began, so the removals the window
	// already made do not shrink its own ceiling either.
	d := q.Dialect()
	err := q.QueryRow(ctx, `SELECT COUNT(*) FROM user_identities i JOIN users u ON u.tenant_id = i.tenant_id AND u.subject = i.subject
		WHERE i.gate = ? AND i.tenant_id = ? AND u.created_at < ?
		AND ((u.status <> 'disabled' AND NOT EXISTS (SELECT 1 FROM scim_users s
				WHERE s.gate = i.gate AND s.tenant_id = u.tenant_id AND s.subject = u.subject AND s.reactivated_at >= ?))
			OR EXISTS (SELECT 1 FROM auth_removals r
				WHERE r.tenant_id = u.tenant_id AND r.subject = u.subject AND r.disabled_at >= ?))`,
		gate, tenantID, d.TimeArg(before), d.TimeArg(before), d.TimeArg(before)).Scan(&n)
	return n, err
}

func scimUsersCreatedSince(ctx context.Context, q Querier, gate, tenantID string, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT COUNT(*) FROM scim_users WHERE gate = ? AND tenant_id = ? AND created_at >= ?`,
		gate, tenantID, q.Dialect().TimeArg(since)).Scan(&n)
	return n, err
}

// scimManyChunk bounds one IN list.
const scimManyChunk = 500

func SCIMUsersGetMany(ctx context.Context, q Querier, gate, tenantID string, ids []string) (map[string]store.SCIMUserRow, error) {
	out := map[string]store.SCIMUserRow{}
	ids = dedupSorted(ids)
	for start := 0; start < len(ids); start += scimManyChunk {
		chunk := ids[start:min(start+scimManyChunk, len(ids))]
		args := []any{gate, tenantID}
		marks := make([]string, len(chunk))
		for i, id := range chunk {
			marks[i] = "?"
			args = append(args, id)
		}
		rows, err := q.Query(ctx, `SELECT `+scimUserCols+` FROM scim_users WHERE gate = ? AND tenant_id = ? AND deleted_at IS NULL
			AND scim_id IN (`+strings.Join(marks, ", ")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scanSCIMUser(q.Dialect(), rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[r.SCIMID] = r
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- groups ----

const scimGroupCols = `gate, tenant_id, scim_id, display_name, display_key, external_id, version, created_at, updated_at`

func scanSCIMGroup(d Dialect, sc Row) (store.SCIMGroupRow, error) {
	var r store.SCIMGroupRow
	err := sc.Scan(&r.Gate, &r.TenantID, &r.SCIMID, &r.DisplayName, &r.DisplayKey, &r.ExternalID, &r.Version,
		d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt))
	return r, err
}

func SCIMGroupCreate(ctx context.Context, q Querier, row store.SCIMGroupRow) error {
	if row.Gate == "" || row.TenantID == "" || row.SCIMID == "" || row.DisplayName == "" || row.DisplayKey == "" {
		return fmt.Errorf("scim group create: gate, tenant, id and display name are required")
	}
	d := q.Dialect()
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.Version == 0 {
		row.Version = 1
	}
	if _, err := q.Exec(ctx, `INSERT INTO scim_groups (`+scimGroupCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Gate, row.TenantID, row.SCIMID, row.DisplayName, row.DisplayKey, row.ExternalID, row.Version,
		d.TimeArg(row.CreatedAt), d.TimeArg(row.UpdatedAt)); err != nil {
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "scim_group_name", ID: row.DisplayName}
		}
		return fmt.Errorf("scim group create: %w", err)
	}
	return nil
}

func SCIMGroupGet(ctx context.Context, q Querier, gate, tenantID, scimID string) (store.SCIMGroupRow, error) {
	r, err := scanSCIMGroup(q.Dialect(), q.QueryRow(ctx, `SELECT `+scimGroupCols+` FROM scim_groups
		WHERE gate = ? AND tenant_id = ? AND scim_id = ?`, gate, tenantID, scimID))
	if err != nil {
		return store.SCIMGroupRow{}, NotFound(err, "scim_group", scimID)
	}
	return r, nil
}

func SCIMGroupList(ctx context.Context, q Querier, gate, tenantID string, f store.SCIMFilter, offset, limit int) ([]store.SCIMGroupRow, int, error) {
	where, args := scimWhere(gate, tenantID, f, true)
	var total int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM scim_groups`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT `+scimGroupCols+` FROM scim_groups`+where+` ORDER BY created_at, scim_id LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []store.SCIMGroupRow{}
	for rows.Next() {
		r, err := scanSCIMGroup(q.Dialect(), rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func SCIMGroupUpdate(ctx context.Context, q Querier, row store.SCIMGroupRow) error {
	if row.DisplayName == "" || row.DisplayKey == "" {
		return fmt.Errorf("scim group update: display name is required")
	}
	n, err := q.Exec(ctx, `UPDATE scim_groups SET display_name = ?, display_key = ?, external_id = ?, version = version + 1, updated_at = ?
		WHERE gate = ? AND tenant_id = ? AND scim_id = ?`, row.DisplayName, row.DisplayKey, row.ExternalID,
		q.Dialect().TimeArg(time.Now().UTC()), row.Gate, row.TenantID, row.SCIMID)
	if err != nil {
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "scim_group_name", ID: row.DisplayName}
		}
		return err
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "scim_group", ID: row.SCIMID}
	}
	return nil
}

func SCIMGroupDelete(ctx context.Context, db DB, gate, tenantID, scimID string) (bool, error) {
	deleted := false
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `DELETE FROM scim_groups WHERE gate = ? AND tenant_id = ? AND scim_id = ?`, gate, tenantID, scimID)
		if err != nil || n == 0 {
			return err
		}
		deleted = true
		_, err = tx.Exec(ctx, `DELETE FROM scim_group_members WHERE gate = ? AND tenant_id = ? AND group_id = ?`, gate, tenantID, scimID)
		return err
	})
	return deleted, err
}

func SCIMGroupMembers(ctx context.Context, q Querier, gate, tenantID, groupID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT subject FROM scim_group_members WHERE gate = ? AND tenant_id = ? AND group_id = ? ORDER BY subject`,
		gate, tenantID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SCIMGroupMembersAdd inserts each membership; ON CONFLICT DO NOTHING keeps an
// existing one and lets the row count say what changed.
func SCIMGroupMembersAdd(ctx context.Context, db DB, gate, tenantID, groupID string, subjects []string) ([]string, error) {
	var added []string
	err := db.InTx(ctx, func(tx Querier) error {
		added = nil
		for _, s := range dedupSorted(subjects) {
			n, err := tx.Exec(ctx, `INSERT INTO scim_group_members (gate, tenant_id, group_id, subject) VALUES (?, ?, ?, ?)
				ON CONFLICT DO NOTHING`, gate, tenantID, groupID, s)
			if err != nil {
				return err
			}
			if n == 1 {
				added = append(added, s)
			}
		}
		return nil
	})
	return added, err
}

func SCIMGroupMembersRemove(ctx context.Context, db DB, gate, tenantID, groupID string, subjects []string) ([]string, error) {
	var removed []string
	err := db.InTx(ctx, func(tx Querier) error {
		removed = nil
		for _, s := range dedupSorted(subjects) {
			n, err := tx.Exec(ctx, `DELETE FROM scim_group_members WHERE gate = ? AND tenant_id = ? AND group_id = ? AND subject = ?`,
				gate, tenantID, groupID, s)
			if err != nil {
				return err
			}
			if n == 1 {
				removed = append(removed, s)
			}
		}
		return nil
	})
	return removed, err
}

func dedupSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i, s := range out {
		if i == 0 || s != out[j-1] {
			out[j] = s
			j++
		}
	}
	return out[:j]
}

func SCIMGroupsOfSubject(ctx context.Context, q Querier, gate, tenantID, subject string) ([]store.SCIMGroupRow, error) {
	rows, err := q.Query(ctx, `SELECT g.gate, g.tenant_id, g.scim_id, g.display_name, g.display_key, g.external_id, g.version,
		g.created_at, g.updated_at FROM scim_groups g JOIN scim_group_members m
		ON m.gate = g.gate AND m.tenant_id = g.tenant_id AND m.group_id = g.scim_id
		WHERE m.gate = ? AND m.tenant_id = ? AND m.subject = ? ORDER BY g.display_key`, gate, tenantID, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.SCIMGroupRow{}
	for rows.Next() {
		r, err := scanSCIMGroup(q.Dialect(), rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
