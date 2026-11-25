package sqlshared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// WorkspaceTemplate* implements store.WorkspaceTemplateStore for both stores
// (RFC GW P6). toolchains is a JSON list kept as text, read whole.

const workspaceTemplateCols = `id, tenant_id, scope, scope_id, name, toolchains, engine, digest, bytes, author_user, author_run, created_at`

func scanWorkspaceTemplate(d Dialect, sc Row) (store.WorkspaceTemplateRow, error) {
	var (
		r     store.WorkspaceTemplateRow
		tools string
	)
	if err := sc.Scan(&r.ID, &r.TenantID, &r.Scope, &r.ScopeID, &r.Name, &tools, &r.Engine, &r.Digest, &r.Bytes,
		&r.AuthorUser, &r.AuthorRun, d.TimeDest(&r.CreatedAt)); err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	if err := json.Unmarshal([]byte(tools), &r.Toolchains); err != nil {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("workspace_template %q: toolchains: %w", r.Name, err)
	}
	if r.Toolchains == nil {
		r.Toolchains = []string{}
	}
	return r, nil
}

// WorkspaceTemplateCreate inserts the row, stamping created_at with the
// server clock. A held key or id is *store.ErrConflict.
func WorkspaceTemplateCreate(ctx context.Context, q Querier, row store.WorkspaceTemplateRow) (store.WorkspaceTemplateRow, error) {
	if row.ID == "" || row.Name == "" || row.Digest == "" ||
		(row.Scope != store.WorkspaceTemplateUser && row.Scope != store.WorkspaceTemplateTenant) {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("workspace_template: id, name, digest and a scope of user or tenant are required")
	}
	if row.Toolchains == nil {
		row.Toolchains = []string{}
	}
	tools, err := json.Marshal(row.Toolchains)
	if err != nil {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("workspace_template: toolchains: %w", err)
	}
	d := q.Dialect()
	_, err = q.Exec(ctx, `INSERT INTO workspace_templates (`+workspaceTemplateCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.TenantID, row.Scope, row.ScopeID, row.Name, string(tools), row.Engine, row.Digest, row.Bytes,
		row.AuthorUser, row.AuthorRun, d.TimeArg(time.Now().UTC()))
	if IsUniqueViolation(err) {
		return store.WorkspaceTemplateRow{}, &store.ErrConflict{Kind: "workspace_template", ID: row.Name}
	}
	if err != nil {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("workspace_template insert: %w", err)
	}
	return WorkspaceTemplateGet(ctx, q, row.TenantID, row.Scope, row.ScopeID, row.Name)
}

// WorkspaceTemplateGet returns *store.ErrNotFound on a miss.
func WorkspaceTemplateGet(ctx context.Context, q Querier, tenantID, scope, scopeID, name string) (store.WorkspaceTemplateRow, error) {
	r, err := scanWorkspaceTemplate(q.Dialect(), q.QueryRow(ctx,
		`SELECT `+workspaceTemplateCols+` FROM workspace_templates WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND name = ?`,
		tenantID, scope, scopeID, name))
	if err != nil {
		return store.WorkspaceTemplateRow{}, NotFound(err, "workspace_template", name)
	}
	return r, nil
}

// WorkspaceTemplateList returns the tenant's templates of one scope, by name.
func WorkspaceTemplateList(ctx context.Context, q Querier, tenantID, scope, scopeID string) ([]store.WorkspaceTemplateRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, `SELECT `+workspaceTemplateCols+` FROM workspace_templates WHERE tenant_id = ? AND scope = ? AND scope_id = ? ORDER BY name`,
		tenantID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.WorkspaceTemplateRow
	for rows.Next() {
		r, err := scanWorkspaceTemplate(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkspaceTemplateListTenant returns every template of the tenant.
func WorkspaceTemplateListTenant(ctx context.Context, q Querier, tenantID string) ([]store.WorkspaceTemplateRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, `SELECT `+workspaceTemplateCols+` FROM workspace_templates WHERE tenant_id = ? ORDER BY scope, scope_id, name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.WorkspaceTemplateRow
	for rows.Next() {
		r, err := scanWorkspaceTemplate(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkspaceTemplateCount counts the tenant's templates.
func WorkspaceTemplateCount(ctx context.Context, q Querier, tenantID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_templates WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// WorkspaceTemplateDelete removes the row; found=false when none existed.
func WorkspaceTemplateDelete(ctx context.Context, q Querier, tenantID, id string) (bool, error) {
	n, err := q.Exec(ctx, `DELETE FROM workspace_templates WHERE tenant_id = ? AND id = ?`, tenantID, id)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
