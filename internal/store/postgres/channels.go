package postgres

import (
	"context"
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
		SELECT name, description, scope, semantic,
		       default_ttl, max_messages, publisher, period, created_at
		FROM channels
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("channels list: %w", err)
	}
	defer rows.Close()
	out := []store.ChannelRow{}
	for rows.Next() {
		var r store.ChannelRow
		var createdAt time.Time
		if err := rows.Scan(
			&r.Name, &r.Description, &r.Scope, &r.Semantic,
			&r.DefaultTTL, &r.MaxMessages, &r.Publisher, &r.Period,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("channels list scan: %w", err)
		}
		r.CreatedAt = createdAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
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
			default_ttl, max_messages, publisher, period, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		row.Name, row.Description, row.Scope, row.Semantic,
		row.DefaultTTL, row.MaxMessages, row.Publisher, row.Period, createdAt,
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
func (s *Store) ChannelsUpdate(ctx context.Context, name string, patch store.ChannelPatch) error {
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
	if len(sets) == 0 {
		// Nothing to update — verify existence and return.
		var one int
		if err := s.pool.QueryRow(ctx, `SELECT 1 FROM channels WHERE name = $1`, name).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &store.ErrNotFound{Kind: "channel", ID: name}
			}
			return fmt.Errorf("channels update existence: %w", err)
		}
		return nil
	}
	args = append(args, name)
	tag, err := s.pool.Exec(ctx,
		`UPDATE channels SET `+strings.Join(sets, ", ")+fmt.Sprintf(` WHERE name = $%d`, idx),
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
func (s *Store) ChannelsDelete(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("channels delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `DELETE FROM channels WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("channels delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "channel", ID: name}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_messages WHERE channel = $1`, name); err != nil {
		return fmt.Errorf("channels delete messages cascade: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_cursors WHERE channel = $1`, name); err != nil {
		return fmt.Errorf("channels delete cursors cascade: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("channels delete commit: %w", err)
	}
	return nil
}

// ChannelPurge deletes every channel_messages row for `name` and
// returns the count. Leaves the channels row + channel_cursors intact
// — see store.Store.ChannelPurge. One DELETE; no transaction needed.
func (s *Store) ChannelPurge(ctx context.Context, name string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM channel_messages WHERE channel = $1`, name)
	if err != nil {
		return 0, fmt.Errorf("channel purge: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
