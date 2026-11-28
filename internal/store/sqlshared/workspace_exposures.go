package sqlshared

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// WorkspaceExposure* implements store.WorkspaceExposureStore (RFC GW P9) for
// both stores. See internal/store/workspace_exposures.go for the contract.

const (
	exposureInsertCols = "id, tenant_id, user_id, workspace_id, port, title, kind, created_at, last_used_at, workspace_name, run_id, idle_timeout_s"
	exposureCols       = exposureInsertCols + ", lease_expires_at, closed_at, closed_reason"
	// exposureLive selects the rows that serve a page (RFC HI D6).
	exposureLive = "closed_at IS NULL"
)

func scanExposure(d Dialect, sc Row) (store.WorkspaceExposure, error) {
	var e store.WorkspaceExposure
	var idle sql.NullInt64
	var lease, closed time.Time
	if err := sc.Scan(&e.ID, &e.TenantID, &e.UserID, &e.WorkspaceID, &e.Port, &e.Title, &e.Kind,
		d.TimeDest(&e.CreatedAt), d.TimeDest(&e.LastUsedAt), &e.WorkspaceName, &e.RunID, &idle,
		&nullTimeDest{&lease}, &nullTimeDest{&closed}, &e.ClosedReason); err != nil {
		return store.WorkspaceExposure{}, err
	}
	e.CreatedAt, e.LastUsedAt = e.CreatedAt.UTC(), e.LastUsedAt.UTC()
	if idle.Valid {
		v := int(idle.Int64)
		e.IdleTimeoutS = &v
	}
	if !lease.IsZero() {
		v := lease.UTC()
		e.LeaseExpiresAt = &v
	}
	if !closed.IsZero() {
		v := closed.UTC()
		e.ClosedAt = &v
	}
	return e, nil
}

func collectExposures(d Dialect, rows Rows) ([]store.WorkspaceExposure, error) {
	defer rows.Close()
	out := []store.WorkspaceExposure{}
	for rows.Next() {
		e, err := scanExposure(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WorkspaceExposureCreate counts the live rows and inserts in one
// transaction. Under Postgres' read-committed two creates racing on one
// workspace can both see max-1 and pass: the bound is a resource limit, and
// one over it for a moment is harmless; the unique (workspace_id, port) index
// over live rows is exact.
func WorkspaceExposureCreate(ctx context.Context, db DB, e store.WorkspaceExposure, max int) (store.WorkspaceExposure, error) {
	if e.ID == "" || e.WorkspaceID == "" || e.Port < 1 || e.Port > 65535 {
		return store.WorkspaceExposure{}, fmt.Errorf("workspace exposure create: id, workspace_id and a port are required")
	}
	if e.Kind != store.WorkspaceExposurePage && e.Kind != store.WorkspaceExposurePanel {
		return store.WorkspaceExposure{}, fmt.Errorf("workspace exposure create: unknown kind %q", e.Kind)
	}
	err := db.InTx(ctx, func(tx Querier) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_exposures WHERE tenant_id = ? AND workspace_id = ? AND `+exposureLive,
			e.TenantID, e.WorkspaceID).Scan(&n); err != nil {
			return fmt.Errorf("workspace exposure count: %w", err)
		}
		if max > 0 && n >= max {
			return &store.ErrExposureLimit{Max: max}
		}
		d := tx.Dialect()
		now := time.Now().UTC()
		var idle any
		if e.IdleTimeoutS != nil {
			idle = *e.IdleTimeoutS
		}
		_, err := tx.Exec(ctx, `INSERT INTO workspace_exposures (`+exposureInsertCols+`) VALUES (`+Placeholders(12)+`)`,
			e.ID, e.TenantID, e.UserID, e.WorkspaceID, e.Port, e.Title, e.Kind, d.TimeArg(now), d.TimeArg(now),
			e.WorkspaceName, e.RunID, idle)
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "workspace_exposure", ID: fmt.Sprint(e.Port)}
		}
		return err
	})
	var lim *store.ErrExposureLimit
	var conflict *store.ErrConflict
	if errors.As(err, &lim) || errors.As(err, &conflict) {
		return store.WorkspaceExposure{}, err
	}
	if err != nil {
		return store.WorkspaceExposure{}, fmt.Errorf("workspace exposure create: %w", err)
	}
	return WorkspaceExposureGet(ctx, db, e.TenantID, e.ID)
}

func WorkspaceExposureGet(ctx context.Context, q Querier, tenantID, id string) (store.WorkspaceExposure, error) {
	e, err := scanExposure(q.Dialect(), q.QueryRow(ctx,
		`SELECT `+exposureCols+` FROM workspace_exposures WHERE tenant_id = ? AND id = ?`, tenantID, id))
	if err != nil {
		return store.WorkspaceExposure{}, NotFound(err, "workspace_exposure", id)
	}
	return e, nil
}

func WorkspaceExposureListByWorkspace(ctx context.Context, q Querier, tenantID, workspaceID string) ([]store.WorkspaceExposure, error) {
	rows, err := q.Query(ctx, `SELECT `+exposureCols+` FROM workspace_exposures WHERE tenant_id = ? AND workspace_id = ? AND `+exposureLive+` ORDER BY created_at, id`,
		tenantID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace exposure list: %w", err)
	}
	return collectExposures(q.Dialect(), rows)
}

func WorkspaceExposureListByUser(ctx context.Context, q Querier, tenantID, userID string) ([]store.WorkspaceExposure, error) {
	rows, err := q.Query(ctx, `SELECT `+exposureCols+` FROM workspace_exposures WHERE tenant_id = ? AND user_id = ? AND `+exposureLive+` ORDER BY created_at, id`,
		tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("workspace exposure list by user: %w", err)
	}
	return collectExposures(q.Dialect(), rows)
}

// WorkspaceExposureClose marks a live row closed. Only a live row matches,
// so a second close keeps the first reason and reports false.
func WorkspaceExposureClose(ctx context.Context, q Querier, tenantID, id, reason string, at time.Time) (bool, error) {
	n, err := q.Exec(ctx, `UPDATE workspace_exposures SET closed_at = ?, closed_reason = ? WHERE tenant_id = ? AND id = ? AND `+exposureLive,
		q.Dialect().TimeArg(at.UTC()), reason, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("workspace exposure close: %w", err)
	}
	return n > 0, nil
}

func WorkspaceExposurePurgeClosed(ctx context.Context, q Querier, before time.Time) (int64, error) {
	n, err := q.Exec(ctx, `DELETE FROM workspace_exposures WHERE closed_at < ?`, q.Dialect().TimeArg(before.UTC()))
	if err != nil {
		return 0, fmt.Errorf("workspace exposure purge: %w", err)
	}
	return n, nil
}

func WorkspaceExposureTouch(ctx context.Context, q Querier, tenantID, id string, at time.Time) error {
	if _, err := q.Exec(ctx, `UPDATE workspace_exposures SET last_used_at = ? WHERE tenant_id = ? AND id = ? AND `+exposureLive,
		q.Dialect().TimeArg(at.UTC()), tenantID, id); err != nil {
		return fmt.Errorf("workspace exposure touch: %w", err)
	}
	return nil
}

func WorkspaceExposureListIdle(ctx context.Context, q Querier, before time.Time) ([]store.WorkspaceExposure, error) {
	rows, err := q.Query(ctx, `SELECT `+exposureCols+` FROM workspace_exposures WHERE last_used_at < ? AND `+exposureLive+` ORDER BY last_used_at, id`,
		q.Dialect().TimeArg(before.UTC()))
	if err != nil {
		return nil, fmt.Errorf("workspace exposure list idle: %w", err)
	}
	return collectExposures(q.Dialect(), rows)
}
