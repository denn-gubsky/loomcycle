package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Channel hooks: messages awaiting their channel's hooks sit at the reserved
// hook instant (store.ChannelHookHeldVisibleAt), and channel_hook_state holds
// the hook chain's lease and progress on each (migration 0084). The message's
// visible_at is the truth about whether it still awaits a decision; the state
// row is advisory.

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func keyArgs(k store.ChannelMessageKey) []any {
	return []any{k.TenantID, k.Channel, string(k.Scope), k.ScopeID, k.ID}
}

const keyWhere = `tenant_id = $1 AND channel = $2 AND scope = $3 AND scope_id = $4 AND id = $5`

// ChannelReleaseHookHeld implements store.Store. The release instant is
// never earlier than NOW() — the same clock a publish takes its visible_at
// from — so a released message cannot sort behind a subscriber that has
// already read past it.
func (s *Store) ChannelReleaseHookHeld(ctx context.Context, key store.ChannelMessageKey, owner string, payload json.RawMessage, to time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("channel hook release begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var body any
	if payload != nil {
		body = string(payload)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE channel_messages
		    SET visible_at = GREATEST(NOW(), COALESCE($6::timestamptz, NOW())),
		        payload = COALESCE($7::jsonb, payload)
		  WHERE `+keyWhere+` AND visible_at = $8 AND (expires_at IS NULL OR expires_at > NOW())
		    AND EXISTS (SELECT 1 FROM channel_hook_state h
		                 WHERE h.tenant_id = $1 AND h.channel = $2 AND h.scope = $3 AND h.scope_id = $4 AND h.id = $5
		                   AND h.lease_owner = $9)`,
		append(keyArgs(key), timeOrNil(to), body, store.ChannelHookHeldVisibleAt(), owner)...)
	if err != nil {
		return false, fmt.Errorf("channel hook release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // decided elsewhere, gone, or not this worker's to decide
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_hook_state WHERE `+keyWhere, keyArgs(key)...); err != nil {
		return false, fmt.Errorf("channel hook release state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("channel hook release commit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ChannelDropHookHeld implements store.Store.
func (s *Store) ChannelDropHookHeld(ctx context.Context, key store.ChannelMessageKey, owner string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("channel hook drop begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM channel_messages WHERE `+keyWhere+` AND visible_at = $6
		AND EXISTS (SELECT 1 FROM channel_hook_state h
		             WHERE h.tenant_id = $1 AND h.channel = $2 AND h.scope = $3 AND h.scope_id = $4 AND h.id = $5
		               AND h.lease_owner = $7)`,
		append(keyArgs(key), store.ChannelHookHeldVisibleAt(), owner)...)
	if err != nil {
		return false, fmt.Errorf("channel hook drop: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_hook_state WHERE `+keyWhere, keyArgs(key)...); err != nil {
		return false, fmt.Errorf("channel hook drop state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("channel hook drop commit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ChannelHookClaim implements store.Store. One statement: the candidates, then
// an upsert of their leases that takes a lease only where none is live
// (ON CONFLICT ... DO UPDATE ... WHERE is evaluated against the row as
// committed, under its lock) — so of two concurrent claimers of a message,
// one inserts or updates the lease and the other's WHERE finds it live and
// returns nothing.
func (s *Store) ChannelHookClaim(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]store.ChannelHookWork, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.pool.Query(ctx,
		`WITH cand AS (
		   SELECT m.tenant_id, m.channel, m.scope, m.scope_id, m.id
		     FROM channel_messages m
		     LEFT JOIN channel_hook_state h
		       ON h.tenant_id = m.tenant_id AND h.channel = m.channel AND h.scope = m.scope
		      AND h.scope_id = m.scope_id AND h.id = m.id
		    WHERE m.visible_at = $1 AND (m.expires_at IS NULL OR m.expires_at > $2)
		      AND (h.id IS NULL OR (h.lease_until < $2 AND h.next_attempt_at <= $2))
		    ORDER BY m.id
		    LIMIT $3
		 ), leased AS (
		   INSERT INTO channel_hook_state (tenant_id, channel, scope, scope_id, id, lease_owner, lease_until, created_at, updated_at)
		   SELECT tenant_id, channel, scope, scope_id, id, $4, $5, $2, $2 FROM cand
		   ON CONFLICT (tenant_id, channel, scope, scope_id, id) DO UPDATE
		      SET lease_owner = EXCLUDED.lease_owner, lease_until = EXCLUDED.lease_until, updated_at = EXCLUDED.updated_at
		    WHERE channel_hook_state.lease_until < $2 AND channel_hook_state.next_attempt_at <= $2
		   RETURNING tenant_id, channel, scope, scope_id, id, run_id, chain_pos, body::text, journal::text,
		             attempts, next_attempt_at, last_error
		 )
		 SELECT m.tenant_id, m.channel, m.scope, m.scope_id, m.id, m.payload::text, m.published_at, m.expires_at,
		        m.visible_at, m.published_by_user_id, m.origin, m.hook_tenant, m.requested_visible_at,
		        l.run_id, l.chain_pos, l.body, l.journal, l.attempts, l.next_attempt_at, l.last_error
		   FROM leased l
		   JOIN channel_messages m
		     ON m.tenant_id = l.tenant_id AND m.channel = l.channel AND m.scope = l.scope
		    AND m.scope_id = l.scope_id AND m.id = l.id
		  ORDER BY m.id`,
		store.ChannelHookHeldVisibleAt(), now.UTC(), limit, owner, leaseUntil.UTC())
	if err != nil {
		return nil, fmt.Errorf("channel hook claim: %w", err)
	}
	defer rows.Close()
	var out []store.ChannelHookWork
	for rows.Next() {
		w, err := scanHookWork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanHookWork(rows pgx.Rows) (store.ChannelHookWork, error) {
	var (
		m                       store.ChannelMessage
		scope, payload          string
		expires, requested      *time.Time
		publishedBy, hookTenant *string
		body, journal           *string
		chainPos, attempts      int
		nextAttempt             time.Time
		runID, lastError        string
	)
	if err := rows.Scan(&m.TenantID, &m.Channel, &scope, &m.ScopeID, &m.ID, &payload, &m.PublishedAt, &expires,
		&m.VisibleAt, &publishedBy, &m.Origin, &hookTenant, &requested,
		&runID, &chainPos, &body, &journal, &attempts, &nextAttempt, &lastError); err != nil {
		return store.ChannelHookWork{}, fmt.Errorf("channel hook claim scan: %w", err)
	}
	m.Scope = store.MemoryScope(scope)
	m.Payload = json.RawMessage(payload)
	if expires != nil {
		m.ExpiresAt = *expires
	}
	if requested != nil {
		m.RequestedVisibleAt = *requested
	}
	if publishedBy != nil {
		m.PublishedByUserID = *publishedBy
	}
	if hookTenant != nil {
		m.HookTenant = *hookTenant
	}
	p := store.ChannelHookProgress{RunID: runID, ChainPos: chainPos, Attempts: attempts, LastError: lastError}
	if body != nil {
		p.Body = json.RawMessage(*body)
	}
	if journal != nil {
		p.Journal = json.RawMessage(*journal)
	}
	if nextAttempt.Unix() > 0 {
		p.NextAttemptAt = nextAttempt
	}
	return store.ChannelHookWork{Message: m, Progress: p}, nil
}

// ChannelHookRenew implements store.Store.
func (s *Store) ChannelHookRenew(ctx context.Context, key store.ChannelMessageKey, owner string, leaseUntil time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE channel_hook_state SET lease_until = $6, updated_at = NOW() WHERE `+keyWhere+` AND lease_owner = $7`,
		append(keyArgs(key), leaseUntil.UTC(), owner)...)
	if err != nil {
		return false, fmt.Errorf("channel hook renew: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ChannelHookSaveProgress implements store.Store.
func (s *Store) ChannelHookSaveProgress(ctx context.Context, key store.ChannelMessageKey, owner string, p store.ChannelHookProgress, leaseUntil time.Time) (bool, error) {
	var body, journal any
	if p.Body != nil {
		body = string(p.Body)
	}
	if p.Journal != nil {
		journal = string(p.Journal)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE channel_hook_state
		    SET run_id = $6, chain_pos = $7, body = $8::jsonb, journal = $9::jsonb, attempts = $10,
		        next_attempt_at = COALESCE($11::timestamptz, 'epoch'), last_error = $12,
		        lease_until = $13, updated_at = NOW()
		  WHERE `+keyWhere+` AND lease_owner = $14`,
		append(keyArgs(key), p.RunID, p.ChainPos, body, journal, p.Attempts, timeOrNil(p.NextAttemptAt), p.LastError,
			leaseUntil.UTC(), owner)...)
	if err != nil {
		return false, fmt.Errorf("channel hook save progress: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ChannelHookGC implements store.Store.
func (s *Store) ChannelHookGC(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM channel_hook_state
		  WHERE (tenant_id, channel, scope, scope_id, id) IN (
		    SELECT h.tenant_id, h.channel, h.scope, h.scope_id, h.id
		      FROM channel_hook_state h
		      LEFT JOIN channel_messages m
		        ON m.tenant_id = h.tenant_id AND m.channel = h.channel AND m.scope = h.scope
		       AND m.scope_id = h.scope_id AND m.id = h.id
		     WHERE m.id IS NULL OR m.visible_at <> $1
		     LIMIT $2)`,
		store.ChannelHookHeldVisibleAt(), limit)
	if err != nil {
		return 0, fmt.Errorf("channel hook gc: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
