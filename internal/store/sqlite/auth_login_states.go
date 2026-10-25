package sqlite

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// RFC FA P1 started redirect logins — the shared-dialect implementation
// (sqlshared/auth_login_states.go).

func (s *Store) AuthLoginStateCreate(ctx context.Context, row store.AuthLoginStateRow) error {
	return sqlshared.AuthLoginStateCreate(ctx, s.shared(), row)
}

func (s *Store) AuthLoginStateConsume(ctx context.Context, stateHash string, now time.Time) (store.AuthLoginStateRow, error) {
	return sqlshared.AuthLoginStateConsume(ctx, s.shared(), stateHash, now)
}

func (s *Store) AuthLoginStateDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	return sqlshared.AuthLoginStateDeleteExpired(ctx, s.shared(), before)
}
