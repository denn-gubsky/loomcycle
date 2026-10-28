package postgres

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// RFC FL P0b runtime app clients — the shared-dialect implementation
// (sqlshared/client_defs.go).

func (s *Store) ClientDefCreate(ctx context.Context, row store.ClientDefRow) (store.ClientDefRow, error) {
	return sqlshared.ClientDefCreate(ctx, s.shared(), row)
}

func (s *Store) ClientDefGet(ctx context.Context, defID string) (store.ClientDefRow, error) {
	return sqlshared.ClientDefGet(ctx, s.shared(), defID)
}

func (s *Store) ClientDefGetActive(ctx context.Context, name string) (store.ClientDefRow, error) {
	return sqlshared.ClientDefGetActive(ctx, s.shared(), name)
}

func (s *Store) ClientDefList(ctx context.Context) ([]store.ClientDefRow, error) {
	return sqlshared.ClientDefList(ctx, s.shared())
}

func (s *Store) ClientDefRetire(ctx context.Context, name string, at time.Time) (bool, error) {
	return sqlshared.ClientDefRetire(ctx, s.shared(), name, at)
}
