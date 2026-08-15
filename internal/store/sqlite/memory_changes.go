package sqlite

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memory_changes.go implements the RFC CD Part C change-data-capture feed.
// The CDC store decorator calls AppendMemoryChange after each successful
// memory/document write; subscribers read forward from a monotonic seq via
// GetMemoryChangesSince; a retention sweeper calls PruneMemoryChanges.

func (s *Store) AppendMemoryChange(ctx context.Context, c store.MemoryChange) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_changes(tenant_id, change_type, scope, scope_id, key, chunk_id, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.TenantID, string(c.Type), string(c.Scope), c.ScopeID, c.Key, c.ChunkID, time.Now().UnixNano(),
	)
	return err
}

func (s *Store) GetMemoryChangesSince(ctx context.Context, tenantID string, afterSeq int64, limit int) ([]store.MemoryChange, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, tenant_id, change_type, scope, scope_id, key, chunk_id, at
		 FROM memory_changes WHERE tenant_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
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
			at int64
		)
		if err := rows.Scan(&c.Seq, &c.TenantID, &ct, &sc, &c.ScopeID, &c.Key, &c.ChunkID, &at); err != nil {
			return nil, err
		}
		c.Type = store.MemoryChangeType(ct)
		c.Scope = store.MemoryScope(sc)
		c.At = time.Unix(0, at)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PruneMemoryChanges(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_changes WHERE at < ?`, olderThan.UnixNano())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
