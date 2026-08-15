package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memory_changes.go implements the RFC CD Part C change-data-capture feed
// (mirrors the sqlite backend). The at column is stamped by the DB (now()).

func (s *Store) AppendMemoryChange(ctx context.Context, c store.MemoryChange) error {
	// Like AppendEvent, this is on every write when the feed is enabled, so it
	// absorbs transient connection-pool errors rather than surfacing a gap.
	return retryOnTransientConn(ctx, func() error {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO memory_changes(tenant_id, change_type, scope, scope_id, key, chunk_id)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			c.TenantID, string(c.Type), string(c.Scope), c.ScopeID, c.Key, c.ChunkID,
		)
		return err
	})
}

func (s *Store) GetMemoryChangesSince(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]store.MemoryChange, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT seq, tenant_id, change_type, scope, scope_id, key, chunk_id, at
		 FROM memory_changes WHERE tenant_id = $1 AND seq > $2 ORDER BY seq ASC LIMIT $3`,
		tenantID, afterSeq, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.MemoryChange
	for rows.Next() {
		var (
			c  store.MemoryChange
			ct string
			sc string
		)
		if err := rows.Scan(&c.Seq, &c.TenantID, &ct, &sc, &c.ScopeID, &c.Key, &c.ChunkID, &c.At); err != nil {
			return nil, err
		}
		c.Type = store.MemoryChangeType(ct)
		c.Scope = store.MemoryScope(sc)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PruneMemoryChanges(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_changes WHERE at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) GetChangeSubscriptionCursor(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT last_seq FROM change_subscription_cursors WHERE name = $1`, name).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

func (s *Store) SetChangeSubscriptionCursor(ctx context.Context, name string, seq int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO change_subscription_cursors(name, last_seq, updated_at) VALUES ($1, $2, now())
		 ON CONFLICT(name) DO UPDATE SET last_seq = EXCLUDED.last_seq, updated_at = now()`,
		name, seq,
	)
	return err
}
