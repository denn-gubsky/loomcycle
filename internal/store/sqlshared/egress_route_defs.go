package sqlshared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// EgressRouteDef* implements store.EgressRouteDefStore for both stores (RFC
// GW P4): the routes a tenant writes, flat (tenant_id, name) rows. methods is
// a JSON list kept as text on both backends: the core reads it whole and never
// queries into it.

const egressRouteDefCols = `tenant_id, name, url, methods, credential, auth, header, created_by, created_at, updated_at`

func scanEgressRouteDef(d Dialect, sc Row) (store.EgressRouteDefRow, error) {
	var (
		r       store.EgressRouteDefRow
		methods string
	)
	if err := sc.Scan(&r.TenantID, &r.Name, &r.URL, &methods, &r.Credential, &r.Auth, &r.Header,
		&r.CreatedBy, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt)); err != nil {
		return store.EgressRouteDefRow{}, err
	}
	if err := json.Unmarshal([]byte(methods), &r.Methods); err != nil {
		return store.EgressRouteDefRow{}, fmt.Errorf("egress_route_def %q: methods: %w", r.Name, err)
	}
	return r, nil
}

// EgressRouteDefCreate inserts the row, stamping both timestamps with the
// server clock. A (tenant_id, name) already held is *store.ErrConflict.
func EgressRouteDefCreate(ctx context.Context, q Querier, row store.EgressRouteDefRow) (store.EgressRouteDefRow, error) {
	if row.Name == "" || row.URL == "" || len(row.Methods) == 0 {
		return store.EgressRouteDefRow{}, fmt.Errorf("egress_route_def: name, url and methods are required")
	}
	methods, err := json.Marshal(row.Methods)
	if err != nil {
		return store.EgressRouteDefRow{}, fmt.Errorf("egress_route_def: methods: %w", err)
	}
	d := q.Dialect()
	now := time.Now().UTC()
	_, err = q.Exec(ctx, `INSERT INTO egress_route_defs (`+egressRouteDefCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TenantID, row.Name, row.URL, string(methods), row.Credential, row.Auth, row.Header,
		row.CreatedBy, d.TimeArg(now), d.TimeArg(now))
	if IsUniqueViolation(err) {
		return store.EgressRouteDefRow{}, &store.ErrConflict{Kind: "egress_route_def", ID: row.Name}
	}
	if err != nil {
		return store.EgressRouteDefRow{}, fmt.Errorf("egress_route_def insert: %w", err)
	}
	return EgressRouteDefGet(ctx, q, row.TenantID, row.Name)
}

// EgressRouteDefGet returns *store.ErrNotFound on a miss.
func EgressRouteDefGet(ctx context.Context, q Querier, tenantID, name string) (store.EgressRouteDefRow, error) {
	r, err := scanEgressRouteDef(q.Dialect(), q.QueryRow(ctx,
		`SELECT `+egressRouteDefCols+` FROM egress_route_defs WHERE tenant_id = ? AND name = ?`, tenantID, name))
	if err != nil {
		return store.EgressRouteDefRow{}, NotFound(err, "egress_route_def", name)
	}
	return r, nil
}

// EgressRouteDefList returns the tenant's routes ordered by name.
func EgressRouteDefList(ctx context.Context, q Querier, tenantID string) ([]store.EgressRouteDefRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx, `SELECT `+egressRouteDefCols+` FROM egress_route_defs WHERE tenant_id = ? ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.EgressRouteDefRow
	for rows.Next() {
		r, err := scanEgressRouteDef(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EgressRouteDefDelete removes the row; found=false when none existed.
func EgressRouteDefDelete(ctx context.Context, q Querier, tenantID, name string) (bool, error) {
	n, err := q.Exec(ctx, `DELETE FROM egress_route_defs WHERE tenant_id = ? AND name = ?`, tenantID, name)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
