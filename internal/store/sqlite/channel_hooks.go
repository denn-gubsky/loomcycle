package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Channel hooks: messages awaiting their channel's hooks sit at the reserved
// hook instant (store.ChannelHookHeldVisibleAt), and channel_hook_state holds
// the hook chain's lease and progress on each. The message's visible_at is the
// truth about whether it still awaits a decision; the state row is advisory.

func hookTenantArg(m store.ChannelMessage) any {
	if m.HookTenant == "" {
		return nil
	}
	return m.HookTenant
}

func requestedVisibleArg(m store.ChannelMessage) any {
	if m.RequestedVisibleAt.IsZero() {
		return nil
	}
	return m.RequestedVisibleAt.UnixNano()
}

// keyArgs is the key in the column order every query below uses.
func keyArgs(k store.ChannelMessageKey) []any {
	return []any{k.TenantID, k.Channel, string(k.Scope), k.ScopeID, k.ID}
}

const keyWhere = `tenant_id = ? AND channel = ? AND scope = ? AND scope_id = ? AND id = ?`

// ChannelReleaseHookHeld implements store.Store.
func (s *Store) ChannelReleaseHookHeld(ctx context.Context, key store.ChannelMessageKey, payload json.RawMessage, to time.Time) (bool, error) {
	now := time.Now()
	// Never earlier than now: a release that landed in the past would sort
	// behind a subscriber that has already read past it.
	if to.IsZero() || to.Before(now) {
		to = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("channel hook release begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	set, args := `visible_at = ?`, []any{to.UnixNano()}
	if payload != nil {
		set, args = `visible_at = ?, payload = ?`, append(args, string(payload))
	}
	args = append(args, keyArgs(key)...)
	args = append(args, store.ChannelHookHeldVisibleAt().UnixNano(), now.UnixNano())
	res, err := tx.ExecContext(ctx,
		`UPDATE channel_messages SET `+set+`
		  WHERE `+keyWhere+` AND visible_at = ? AND (expires_at IS NULL OR expires_at > ?)`, args...)
	if err != nil {
		return false, fmt.Errorf("channel hook release: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_hook_state WHERE `+keyWhere, keyArgs(key)...); err != nil {
		return false, fmt.Errorf("channel hook release state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("channel hook release commit: %w", err)
	}
	return n > 0, nil
}

// ChannelDropHookHeld implements store.Store.
func (s *Store) ChannelDropHookHeld(ctx context.Context, key store.ChannelMessageKey) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("channel hook drop begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM channel_messages WHERE `+keyWhere+` AND visible_at = ?`,
		append(keyArgs(key), store.ChannelHookHeldVisibleAt().UnixNano())...)
	if err != nil {
		return false, fmt.Errorf("channel hook drop: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_hook_state WHERE `+keyWhere, keyArgs(key)...); err != nil {
		return false, fmt.Errorf("channel hook drop state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("channel hook drop commit: %w", err)
	}
	return n > 0, nil
}

// ChannelHookClaim implements store.Store. BEGIN IMMEDIATE takes the write
// lock before the candidate read, so two claimers cannot both pick the same
// message — the same pattern as ChannelRelease.
func (s *Store) ChannelHookClaim(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]store.ChannelHookWork, error) {
	if limit <= 0 {
		limit = 1
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("channel hook claim begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	nowNs := now.UnixNano()
	rows, err := conn.QueryContext(ctx,
		`SELECT m.tenant_id, m.channel, m.scope, m.scope_id, m.id, m.payload, m.published_at, m.expires_at,
		        m.visible_at, m.published_by_user_id, m.origin, m.hook_tenant, m.requested_visible_at,
		        h.run_id, h.chain_pos, h.body, h.journal, h.attempts, h.next_attempt_at, h.last_error
		   FROM channel_messages m
		   LEFT JOIN channel_hook_state h
		     ON h.tenant_id = m.tenant_id AND h.channel = m.channel AND h.scope = m.scope
		    AND h.scope_id = m.scope_id AND h.id = m.id
		  WHERE m.visible_at = ? AND (m.expires_at IS NULL OR m.expires_at > ?)
		    AND (h.id IS NULL OR (h.lease_until < ? AND h.next_attempt_at <= ?))
		  ORDER BY m.id ASC
		  LIMIT ?`,
		store.ChannelHookHeldVisibleAt().UnixNano(), nowNs, nowNs, nowNs, limit)
	if err != nil {
		return nil, fmt.Errorf("channel hook claim select: %w", err)
	}
	var out []store.ChannelHookWork
	for rows.Next() {
		w, err := scanHookWork(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, w := range out {
		m := w.Message
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO channel_hook_state(tenant_id, channel, scope, scope_id, id, lease_owner, lease_until, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(tenant_id, channel, scope, scope_id, id)
			 DO UPDATE SET lease_owner = excluded.lease_owner, lease_until = excluded.lease_until, updated_at = excluded.updated_at`,
			m.TenantID, m.Channel, string(m.Scope), m.ScopeID, m.ID, owner, leaseUntil.UnixNano(), nowNs, nowNs); err != nil {
			return nil, fmt.Errorf("channel hook claim lease: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("channel hook claim commit: %w", err)
	}
	committed = true
	return out, nil
}

func scanHookWork(rows *sql.Rows) (store.ChannelHookWork, error) {
	var (
		m                                 store.ChannelMessage
		scope, payload                    string
		publishedNs, visibleNs            int64
		expiresNs, requestedNs            sql.NullInt64
		publishedBy, hookTenant           sql.NullString
		runID, body, journal, lastError   sql.NullString
		chainPos, attempts, nextAttemptNs sql.NullInt64
	)
	if err := rows.Scan(&m.TenantID, &m.Channel, &scope, &m.ScopeID, &m.ID, &payload, &publishedNs, &expiresNs,
		&visibleNs, &publishedBy, &m.Origin, &hookTenant, &requestedNs,
		&runID, &chainPos, &body, &journal, &attempts, &nextAttemptNs, &lastError); err != nil {
		return store.ChannelHookWork{}, fmt.Errorf("channel hook claim scan: %w", err)
	}
	m.Scope = store.MemoryScope(scope)
	m.Payload = json.RawMessage(payload)
	m.PublishedAt = time.Unix(0, publishedNs)
	m.VisibleAt = time.Unix(0, visibleNs)
	if expiresNs.Valid {
		m.ExpiresAt = time.Unix(0, expiresNs.Int64)
	}
	if requestedNs.Valid {
		m.RequestedVisibleAt = time.Unix(0, requestedNs.Int64)
	}
	m.PublishedByUserID = publishedBy.String
	m.HookTenant = hookTenant.String
	p := store.ChannelHookProgress{
		RunID:     runID.String,
		ChainPos:  int(chainPos.Int64),
		Attempts:  int(attempts.Int64),
		LastError: lastError.String,
	}
	if body.Valid {
		p.Body = json.RawMessage(body.String)
	}
	if journal.Valid {
		p.Journal = json.RawMessage(journal.String)
	}
	if nextAttemptNs.Valid && nextAttemptNs.Int64 > 0 {
		p.NextAttemptAt = time.Unix(0, nextAttemptNs.Int64)
	}
	return store.ChannelHookWork{Message: m, Progress: p}, nil
}

// ChannelHookRenew implements store.Store.
func (s *Store) ChannelHookRenew(ctx context.Context, key store.ChannelMessageKey, owner string, leaseUntil time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_hook_state SET lease_until = ?, updated_at = ? WHERE `+keyWhere+` AND lease_owner = ?`,
		append(append([]any{leaseUntil.UnixNano(), time.Now().UnixNano()}, keyArgs(key)...), owner)...)
	if err != nil {
		return false, fmt.Errorf("channel hook renew: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ChannelHookSaveProgress implements store.Store.
func (s *Store) ChannelHookSaveProgress(ctx context.Context, key store.ChannelMessageKey, owner string, p store.ChannelHookProgress, leaseUntil time.Time) (bool, error) {
	var nextNs int64
	if !p.NextAttemptAt.IsZero() {
		nextNs = p.NextAttemptAt.UnixNano()
	}
	args := []any{p.RunID, p.ChainPos, rawOrNil(p.Body), rawOrNil(p.Journal), p.Attempts, nextNs, p.LastError,
		leaseUntil.UnixNano(), time.Now().UnixNano()}
	args = append(append(args, keyArgs(key)...), owner)
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_hook_state
		    SET run_id = ?, chain_pos = ?, body = ?, journal = ?, attempts = ?, next_attempt_at = ?, last_error = ?,
		        lease_until = ?, updated_at = ?
		  WHERE `+keyWhere+` AND lease_owner = ?`, args...)
	if err != nil {
		return false, fmt.Errorf("channel hook save progress: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func rawOrNil(r json.RawMessage) any {
	if r == nil {
		return nil
	}
	return string(r)
}

// ChannelHookGC implements store.Store.
func (s *Store) ChannelHookGC(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_hook_state WHERE rowid IN (
		   SELECT h.rowid FROM channel_hook_state h
		   LEFT JOIN channel_messages m
		     ON m.tenant_id = h.tenant_id AND m.channel = h.channel AND m.scope = h.scope
		    AND m.scope_id = h.scope_id AND m.id = h.id
		  WHERE m.id IS NULL OR m.visible_at != ?
		  LIMIT ?)`,
		store.ChannelHookHeldVisibleAt().UnixNano(), limit)
	if err != nil {
		return 0, fmt.Errorf("channel hook gc: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
