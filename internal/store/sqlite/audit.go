package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// auditListDefault caps one AuditEventsList page when the caller names none.
const auditListDefault = 500

// auditPruneDefault caps one AuditPrune when the caller names no limit: the
// prune reads every row it removes, to have them verified.
const auditPruneDefault = 5000

// AuditAppend implements store.AuditStore. BEGIN IMMEDIATE on a pinned
// connection takes the database write lock before the head is read, so two
// appenders — two connections here, or two processes on one file — read the
// head one after the other and never both extend the same row.
func (s *Store) AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error) {
	if len(rows) == 0 || seal == nil {
		return nil, fmt.Errorf("audit append: rows and seal are required")
	}
	tenant := rows[0].TenantID
	for _, r := range rows {
		if r.Action == "" || r.TenantID != tenant {
			return nil, fmt.Errorf("audit append: every row needs an action and the same tenant")
		}
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit append: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("audit append: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	created, err := auditEnsureHead(ctx, conn, tenant)
	if err != nil {
		return nil, err
	}
	if created && opened != nil && opened.TenantID != tenant {
		if _, err := auditEnsureHead(ctx, conn, opened.TenantID); err != nil {
			return nil, err
		}
		if _, err := auditAppendLocked(ctx, conn, *opened, seal); err != nil {
			return nil, err
		}
	}
	written := make([]store.AuditEventRow, 0, len(rows))
	for _, row := range rows {
		row, err := auditAppendLocked(ctx, conn, row, seal)
		if err != nil {
			return nil, err
		}
		written = append(written, row)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("audit append: commit: %w", err)
	}
	committed = true
	return written, nil
}

// auditEnsureHead creates a tenant's head if it has none, reporting whether
// it did.
func auditEnsureHead(ctx context.Context, conn *sql.Conn, tenantID string) (bool, error) {
	res, err := conn.ExecContext(ctx,
		`INSERT INTO audit_heads (tenant_id) VALUES (?) ON CONFLICT (tenant_id) DO NOTHING`, tenantID)
	if err != nil {
		return false, fmt.Errorf("audit append: head: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("audit append: head: %w", err)
	}
	return n > 0, nil
}

// auditAppendLocked appends one row under the write lock the caller holds.
func auditAppendLocked(ctx context.Context, conn *sql.Conn, row store.AuditEventRow, seal store.AuditSealFunc) (store.AuditEventRow, error) {
	var headSeq int64
	var headHash string
	if err := conn.QueryRowContext(ctx,
		`SELECT seq, hash FROM audit_heads WHERE tenant_id = ?`, row.TenantID).Scan(&headSeq, &headHash); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: read head: %w", err)
	}
	row.Seq, row.PrevHash = headSeq+1, headHash
	headMAC, err := seal(&row)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: seal: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO audit_events (tenant_id, seq, ts, actor, action, subject, payload, prev_hash, hash, mac)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		row.TenantID, row.Seq, row.TS.UnixNano(), row.Actor, row.Action, row.Subject, row.Payload,
		row.PrevHash, row.Hash, row.MAC); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: insert: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE audit_heads SET seq = ?, hash = ?, mac = ? WHERE tenant_id = ?`,
		row.Seq, row.Hash, headMAC, row.TenantID); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: move head: %w", err)
	}
	return row, nil
}

// AuditHeadGet implements store.AuditStore.
func (s *Store) AuditHeadGet(ctx context.Context, tenantID string) (store.AuditHead, error) {
	h := store.AuditHead{TenantID: tenantID}
	err := s.db.QueryRowContext(ctx,
		`SELECT seq, hash, mac, anchor_seq, anchor_hash, anchor_mac, anchor_state FROM audit_heads WHERE tenant_id = ?`, tenantID,
	).Scan(&h.Seq, &h.Hash, &h.MAC, &h.AnchorSeq, &h.AnchorHash, &h.AnchorMAC, &h.AnchorState)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuditHead{}, &store.ErrNotFound{Kind: "audit_head", ID: tenantID}
	}
	if err != nil {
		return store.AuditHead{}, fmt.Errorf("audit head: %w", err)
	}
	return h, nil
}

// AuditEventsList implements store.AuditStore.
func (s *Store) AuditEventsList(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]store.AuditEventRow, error) {
	if limit <= 0 {
		limit = auditListDefault
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, ts, actor, action, subject, payload, prev_hash, hash, mac FROM audit_events
		 WHERE tenant_id = ? AND seq > ? ORDER BY seq LIMIT ?`, tenantID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("audit events: %w", err)
	}
	defer rows.Close()
	var out []store.AuditEventRow
	for rows.Next() {
		r := store.AuditEventRow{TenantID: tenantID}
		var ts int64
		if err := rows.Scan(&r.Seq, &ts, &r.Actor, &r.Action, &r.Subject, &r.Payload, &r.PrevHash, &r.Hash, &r.MAC); err != nil {
			return nil, fmt.Errorf("audit events scan: %w", err)
		}
		r.TS = time.Unix(0, ts).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuditTenants implements store.AuditStore.
func (s *Store) AuditTenants(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id FROM audit_heads ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("audit tenants: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("audit tenants scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AuditPrune implements store.AuditStore, under the same write lock as an
// append so the anchor and the head never disagree. It reads the head and
// every row it would remove (those past the anchor; a row below it is not
// this prune's, and is left for verification to report) inside the
// transaction and hands them to seal, which verifies them before signing; a
// seal error rolls the prune back.
func (s *Store) AuditPrune(ctx context.Context, tenantID string, before time.Time, limit int, seal store.AuditAnchorSealFunc) (int, error) {
	if seal == nil {
		return 0, fmt.Errorf("audit prune: seal is required")
	}
	if limit <= 0 {
		limit = auditPruneDefault
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit prune: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("audit prune: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	head := store.AuditHead{TenantID: tenantID}
	err = conn.QueryRowContext(ctx,
		`SELECT seq, hash, mac, anchor_seq, anchor_hash, anchor_mac, anchor_state FROM audit_heads WHERE tenant_id = ?`, tenantID,
	).Scan(&head.Seq, &head.Hash, &head.MAC, &head.AnchorSeq, &head.AnchorHash, &head.AnchorMAC, &head.AnchorState)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("audit prune: head: %w", err)
	}
	var last sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM audit_events WHERE tenant_id = ? AND seq > ? AND seq <= ? AND ts < ?`,
		tenantID, head.AnchorSeq, head.AnchorSeq+int64(limit), before.UnixNano(),
	).Scan(&last); err != nil {
		return 0, fmt.Errorf("audit prune: find: %w", err)
	}
	if !last.Valid {
		return 0, nil
	}
	rows, err := conn.QueryContext(ctx,
		`SELECT seq, ts, actor, action, subject, payload, prev_hash, hash, mac FROM audit_events
		 WHERE tenant_id = ? AND seq > ? AND seq <= ? ORDER BY seq`, tenantID, head.AnchorSeq, last.Int64)
	if err != nil {
		return 0, fmt.Errorf("audit prune: read: %w", err)
	}
	var pruned []store.AuditEventRow
	for rows.Next() {
		r := store.AuditEventRow{TenantID: tenantID}
		var ts int64
		if err := rows.Scan(&r.Seq, &ts, &r.Actor, &r.Action, &r.Subject, &r.Payload, &r.PrevHash, &r.Hash, &r.MAC); err != nil {
			rows.Close()
			return 0, fmt.Errorf("audit prune: read: %w", err)
		}
		r.TS = time.Unix(0, ts).UTC()
		pruned = append(pruned, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("audit prune: read: %w", err)
	}
	state, mac, headMAC, err := seal(head, pruned)
	if err != nil {
		return 0, fmt.Errorf("audit prune: %w", err)
	}
	end := pruned[len(pruned)-1]
	res, err := conn.ExecContext(ctx, `DELETE FROM audit_events WHERE tenant_id = ? AND seq > ? AND seq <= ?`, tenantID, head.AnchorSeq, end.Seq)
	if err != nil {
		return 0, fmt.Errorf("audit prune: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := conn.ExecContext(ctx,
		`UPDATE audit_heads SET anchor_seq = ?, anchor_hash = ?, anchor_mac = ?, anchor_state = ? WHERE tenant_id = ?`,
		end.Seq, end.Hash, mac, state, tenantID); err != nil {
		return 0, fmt.Errorf("audit prune: anchor: %w", err)
	}
	if headMAC != "" {
		// Pruned up to the head: the head is re-signed with the key that
		// signed the anchor, so a later rotation still verifies it.
		if _, err := conn.ExecContext(ctx, `UPDATE audit_heads SET mac = ? WHERE tenant_id = ?`, headMAC, tenantID); err != nil {
			return 0, fmt.Errorf("audit prune: head: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("audit prune: commit: %w", err)
	}
	committed = true
	return int(n), nil
}
