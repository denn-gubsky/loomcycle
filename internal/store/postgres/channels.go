package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channels.go — v0.11.5 runtime-declared channel CRUD against the
// `channels` table. Sibling of internal/store/sqlite/channels.go.
// yaml-declared channels stay in cfg.Channels (in-memory); the
// HTTP admin layer merges both at read time.

// ChannelsList returns every runtime-declared channel ordered by
// name. Empty slice when no runtime channels exist (vs nil on error).
func (s *Store) ChannelsList(ctx context.Context) ([]store.ChannelRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, tenant_id, description, scope, semantic,
		       default_ttl, max_messages, publisher, period, hold, hooks::text, created_at
		FROM channels
		ORDER BY tenant_id, name
	`)
	if err != nil {
		return nil, fmt.Errorf("channels list: %w", err)
	}
	defer rows.Close()
	out := []store.ChannelRow{}
	for rows.Next() {
		var r store.ChannelRow
		var createdAt time.Time
		var hooks *string
		if err := rows.Scan(
			&r.Name, &r.TenantID, &r.Description, &r.Scope, &r.Semantic,
			&r.DefaultTTL, &r.MaxMessages, &r.Publisher, &r.Period,
			&r.Hold, &hooks, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("channels list scan: %w", err)
		}
		r.Hooks = hooksFrom(hooks)
		r.CreatedAt = createdAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChannelGet returns one runtime-declared channel by name. Returns
// *store.ErrNotFound{Kind:"channel"} when the name has no runtime row
// (exp7 I5: a point lookup for the hot declared-check, so a real query
// fault surfaces as an error instead of an empty scan masquerading as
// "not declared").
func (s *Store) ChannelGet(ctx context.Context, tenantID, name string) (store.ChannelRow, error) {
	var r store.ChannelRow
	var createdAt time.Time
	var hooks *string
	err := s.pool.QueryRow(ctx, `
		SELECT name, tenant_id, description, scope, semantic,
		       default_ttl, max_messages, publisher, period, hold, hooks::text, created_at
		FROM channels
		WHERE tenant_id = $1 AND name = $2
	`, tenantID, name).Scan(
		&r.Name, &r.TenantID, &r.Description, &r.Scope, &r.Semantic,
		&r.DefaultTTL, &r.MaxMessages, &r.Publisher, &r.Period,
		&r.Hold, &hooks, &createdAt,
	)
	r.Hooks = hooksFrom(hooks)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ChannelRow{}, &store.ErrNotFound{Kind: "channel", ID: name}
	}
	if err != nil {
		return store.ChannelRow{}, fmt.Errorf("channel get: %w", err)
	}
	r.CreatedAt = createdAt.UTC()
	return r, nil
}

// ChannelsCreate inserts a new runtime channel. Returns
// *store.ErrConflict{Kind:"channel", ID:name} on PK violation
// (Postgres SQLSTATE 23505 = unique_violation).
func (s *Store) ChannelsCreate(ctx context.Context, row store.ChannelRow) error {
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO channels (
			name, description, scope, semantic,
			default_ttl, max_messages, publisher, period, hold, hooks, created_at, tenant_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12)
	`,
		row.Name, row.Description, row.Scope, row.Semantic,
		row.DefaultTTL, row.MaxMessages, row.Publisher, row.Period, row.Hold, hooksArg(row.Hooks), createdAt, row.TenantID,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &store.ErrConflict{Kind: "channel", ID: row.Name}
		}
		return fmt.Errorf("channels create: %w", err)
	}
	return nil
}

// ChannelsUpdate patches mutable fields on a runtime channel. Nil
// pointers in `patch` leave the corresponding field unchanged.
// Returns *store.ErrNotFound{Kind:"channel"} when the name isn't in
// the runtime table.
func (s *Store) ChannelsUpdate(ctx context.Context, tenantID, name string, patch store.ChannelPatch) error {
	sets := []string{}
	args := []any{}
	idx := 1
	if patch.Description != nil {
		sets = append(sets, fmt.Sprintf("description = $%d", idx))
		args = append(args, *patch.Description)
		idx++
	}
	if patch.DefaultTTL != nil {
		sets = append(sets, fmt.Sprintf("default_ttl = $%d", idx))
		args = append(args, *patch.DefaultTTL)
		idx++
	}
	if patch.MaxMessages != nil {
		sets = append(sets, fmt.Sprintf("max_messages = $%d", idx))
		args = append(args, *patch.MaxMessages)
		idx++
	}
	if patch.Semantic != nil {
		sets = append(sets, fmt.Sprintf("semantic = $%d", idx))
		args = append(args, *patch.Semantic)
		idx++
	}
	if patch.Hold != nil {
		sets = append(sets, fmt.Sprintf("hold = $%d", idx))
		args = append(args, *patch.Hold)
		idx++
	}
	if patch.Hooks != nil {
		sets = append(sets, fmt.Sprintf("hooks = $%d::jsonb", idx))
		args = append(args, hooksArg(*patch.Hooks))
		idx++
	}
	if len(sets) == 0 {
		// Nothing to update — verify existence and return.
		var one int
		if err := s.pool.QueryRow(ctx, `SELECT 1 FROM channels WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &store.ErrNotFound{Kind: "channel", ID: name}
			}
			return fmt.Errorf("channels update existence: %w", err)
		}
		return nil
	}
	args = append(args, tenantID, name)
	tag, err := s.pool.Exec(ctx,
		`UPDATE channels SET `+strings.Join(sets, ", ")+fmt.Sprintf(` WHERE tenant_id = $%d AND name = $%d`, idx, idx+1),
		args...,
	)
	if err != nil {
		return fmt.Errorf("channels update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "channel", ID: name}
	}
	return nil
}

// ChannelsDelete removes a runtime channel + cascades deletion of
// its persisted messages + cursors in one transaction. Returns
// *store.ErrNotFound{Kind:"channel"} when the name isn't in the
// runtime table.
func (s *Store) ChannelsDelete(ctx context.Context, tenantID, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("channels delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read the def's scope first: the cascade must delete messages/cursors
	// from the keyspaces they actually live in — this tenant's, and for a
	// global channel every tenant's layer of it (see store.ChannelReadTenants).
	var scope string
	if err := tx.QueryRow(ctx, `SELECT scope FROM channels WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&scope); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.ErrNotFound{Kind: "channel", ID: name}
		}
		return fmt.Errorf("channels delete scope lookup: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channels WHERE tenant_id = $1 AND name = $2`, tenantID, name); err != nil {
		return fmt.Errorf("channels delete: %w", err)
	}
	// Cascade scoped by keyspace so deleting one tenant's channel never
	// touches another tenant's same-named channel: this tenant's rows, plus —
	// for a global channel — every tenant's global layer of it, which the
	// channel's deletion orphans. (Global channels are an admin's to create.)
	where := `tenant_id = $1 AND channel = $2`
	args := []any{tenantID, name}
	if store.MemoryScope(scope) == store.MemoryScopeGlobal {
		where = `channel = $2 AND (tenant_id = $1 OR scope = $3)`
		args = append(args, string(store.MemoryScopeGlobal))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_messages WHERE `+where, args...); err != nil {
		return fmt.Errorf("channels delete messages cascade: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_cursors WHERE `+where, args...); err != nil {
		return fmt.Errorf("channels delete cursors cascade: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_hook_state WHERE `+where, args...); err != nil {
		return fmt.Errorf("channels delete hook state cascade: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("channels delete commit: %w", err)
	}
	return nil
}

// ChannelPurge deletes every channel_messages row for `name`, with the hook
// progress of those awaiting hooks, and returns the message count. Leaves the
// channels row + channel_cursors intact — see store.Store.ChannelPurge.
func (s *Store) ChannelPurge(ctx context.Context, tenantID, name string, scope store.MemoryScope) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("channel purge begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// $3 = '' purges every scope.
	where := `tenant_id = $1 AND channel = $2 AND ($3 = '' OR scope = $3)`
	tag, err := tx.Exec(ctx, `DELETE FROM channel_messages WHERE `+where, tenantID, name, string(scope))
	if err != nil {
		return 0, fmt.Errorf("channel purge: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_hook_state WHERE `+where, tenantID, name, string(scope)); err != nil {
		return 0, fmt.Errorf("channel purge hook state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("channel purge commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// hooksArg is a channel's hooks as stored: NULL for none (nil, empty, JSON
// null or an empty object), so "no hooks" has one spelling.
func hooksArg(h json.RawMessage) any {
	if store.NoChannelHooks(h) {
		return nil
	}
	return string(h)
}

func hooksFrom(v *string) json.RawMessage {
	if v == nil || *v == "" {
		return nil
	}
	return json.RawMessage(*v)
}
