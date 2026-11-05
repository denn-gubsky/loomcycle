package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The store-backed audit stream (RFC FZ Part E); see migration 0131.

// auditListDefault caps one AuditEventsList page when the caller names none.
const auditListDefault = 500

// AuditAppend implements store.AuditStore. The head row is created if absent
// (a concurrent creator waits on the primary key, then does nothing) and
// then locked FOR UPDATE, so every appender to one tenant — on any replica —
// reads the head only after the previous append committed.
func (s *Store) AuditAppend(ctx context.Context, row store.AuditEventRow, seal store.AuditSealFunc) (store.AuditEventRow, error) {
	if row.Action == "" || seal == nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: action and seal are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_heads (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`, row.TenantID); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: head: %w", err)
	}
	var headSeq int64
	var headHash string
	if err := tx.QueryRow(ctx,
		`SELECT seq, hash FROM audit_heads WHERE tenant_id = $1 FOR UPDATE`, row.TenantID).Scan(&headSeq, &headHash); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: lock head: %w", err)
	}
	row.Seq, row.PrevHash = headSeq+1, headHash
	headMAC, err := seal(&row)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: seal: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_events (tenant_id, seq, ts, actor, action, subject, payload, prev_hash, hash, mac)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		row.TenantID, row.Seq, row.TS.UnixNano(), row.Actor, row.Action, row.Subject, row.Payload,
		row.PrevHash, row.Hash, row.MAC); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: insert: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE audit_heads SET seq = $1, hash = $2, mac = $3 WHERE tenant_id = $4`,
		row.Seq, row.Hash, headMAC, row.TenantID); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: move head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit append: commit: %w", err)
	}
	return row, nil
}

// AuditHeadGet implements store.AuditStore.
func (s *Store) AuditHeadGet(ctx context.Context, tenantID string) (store.AuditHead, error) {
	h := store.AuditHead{TenantID: tenantID}
	err := s.pool.QueryRow(ctx,
		`SELECT seq, hash, mac, anchor_seq, anchor_hash, anchor_mac FROM audit_heads WHERE tenant_id = $1`, tenantID,
	).Scan(&h.Seq, &h.Hash, &h.MAC, &h.AnchorSeq, &h.AnchorHash, &h.AnchorMAC)
	if errors.Is(err, pgx.ErrNoRows) {
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
	rows, err := s.pool.Query(ctx,
		`SELECT seq, ts, actor, action, subject, payload, prev_hash, hash, mac FROM audit_events
		 WHERE tenant_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, tenantID, afterSeq, limit)
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
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM audit_heads ORDER BY tenant_id`)
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

// AuditPrune implements store.AuditStore, holding the head lock an append
// takes so the anchor and the head never disagree.
func (s *Store) AuditPrune(ctx context.Context, tenantID string, before time.Time, seal store.AuditAnchorSealFunc) (int, error) {
	if seal == nil {
		return 0, fmt.Errorf("audit prune: seal is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit prune: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var headSeq int64
	err = tx.QueryRow(ctx, `SELECT seq FROM audit_heads WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&headSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("audit prune: lock head: %w", err)
	}
	var last *int64
	if err := tx.QueryRow(ctx,
		`SELECT MAX(seq) FROM audit_events WHERE tenant_id = $1 AND ts < $2`, tenantID, before.UnixNano(),
	).Scan(&last); err != nil {
		return 0, fmt.Errorf("audit prune: find: %w", err)
	}
	if last == nil {
		return 0, nil
	}
	var hash string
	if err := tx.QueryRow(ctx,
		`SELECT hash FROM audit_events WHERE tenant_id = $1 AND seq = $2`, tenantID, *last).Scan(&hash); err != nil {
		return 0, fmt.Errorf("audit prune: anchor row: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq <= $2`, tenantID, *last)
	if err != nil {
		return 0, fmt.Errorf("audit prune: delete: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE audit_heads SET anchor_seq = $1, anchor_hash = $2, anchor_mac = $3 WHERE tenant_id = $4`,
		*last, hash, seal(tenantID, *last, hash), tenantID); err != nil {
		return 0, fmt.Errorf("audit prune: anchor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("audit prune: commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
