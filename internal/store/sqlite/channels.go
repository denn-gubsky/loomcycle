package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channels.go — v0.11.5 runtime-declared channel CRUD against the
// `channels` table. yaml-declared channels stay in cfg.Channels
// (in-memory); the HTTP admin layer merges both at read time.

// ChannelsList returns every runtime-declared channel ordered by
// name. Empty slice when no runtime channels exist (vs nil on error).
func (s *Store) ChannelsList(ctx context.Context) ([]store.ChannelRow, error) {
	rows, err := s.db.QueryContext(ctx, `
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
		var createdNano int64
		if err := rows.Scan(
			&r.Name, &r.Description, &r.Scope, &r.Semantic,
			&r.DefaultTTL, &r.MaxMessages, &r.Publisher, &r.Period,
			&createdNano,
		); err != nil {
			return nil, fmt.Errorf("channels list scan: %w", err)
		}
		r.CreatedAt = time.Unix(0, createdNano).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChannelsCreate inserts a new runtime channel. Returns
// *store.ErrConflict{Kind:"channel", ID:name} on PK violation
// (the substrate's standard duplicate-row signal).
func (s *Store) ChannelsCreate(ctx context.Context, row store.ChannelRow) error {
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO channels (
			name, description, scope, semantic,
			default_ttl, max_messages, publisher, period, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		row.Name, row.Description, row.Scope, row.Semantic,
		row.DefaultTTL, row.MaxMessages, row.Publisher, row.Period,
		row.CreatedAt.UnixNano(),
	)
	if err != nil {
		// modernc.org/sqlite surfaces UNIQUE-constraint failures
		// as messages containing "UNIQUE constraint failed";
		// catch and translate to the canonical conflict sentinel.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
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
	// Build the SET clause dynamically — sqlite doesn't have COALESCE
	// + named-params ergonomics for partial updates, so we just
	// stitch the patch.
	sets := []string{}
	args := []any{}
	if patch.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *patch.Description)
	}
	if patch.DefaultTTL != nil {
		sets = append(sets, "default_ttl = ?")
		args = append(args, *patch.DefaultTTL)
	}
	if patch.MaxMessages != nil {
		sets = append(sets, "max_messages = ?")
		args = append(args, *patch.MaxMessages)
	}
	if patch.Semantic != nil {
		sets = append(sets, "semantic = ?")
		args = append(args, *patch.Semantic)
	}
	if len(sets) == 0 {
		// Nothing to update — verify existence and return.
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM channels WHERE name = ?`, name).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &store.ErrNotFound{Kind: "channel", ID: name}
			}
			return fmt.Errorf("channels update existence: %w", err)
		}
		return nil
	}
	args = append(args, name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET `+strings.Join(sets, ", ")+` WHERE name = ?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("channels update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("channels update rows-affected: %w", err)
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "channel", ID: name}
	}
	return nil
}

// ChannelsDelete removes a runtime channel + cascades deletion of
// its persisted messages + cursors (since the channels table has no
// FK on the older message/cursor tables, the cascade runs in
// application code — one transaction). Returns
// *store.ErrNotFound{Kind:"channel"} when the name isn't in the
// runtime table.
func (s *Store) ChannelsDelete(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("channels delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("channels delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("channels delete rows-affected: %w", err)
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "channel", ID: name}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_messages WHERE channel = ?`, name); err != nil {
		return fmt.Errorf("channels delete messages cascade: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_cursors WHERE channel = ?`, name); err != nil {
		return fmt.Errorf("channels delete cursors cascade: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("channels delete commit: %w", err)
	}
	return nil
}
