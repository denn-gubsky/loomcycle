package postgres

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// RFC FL app sign-ins — the shared-dialect implementation (sqlshared/oauth.go).

func (s *Store) OAuthCodeCreate(ctx context.Context, row store.OAuthCodeRow) error {
	return sqlshared.OAuthCodeCreate(ctx, s.shared(), row)
}

func (s *Store) OAuthCodeConsume(ctx context.Context, codeHash, familyID string, now time.Time) (store.OAuthCodeRow, bool, error) {
	return sqlshared.OAuthCodeConsume(ctx, s.shared(), codeHash, familyID, now)
}

func (s *Store) OAuthCodeInvalidateClient(ctx context.Context, clientID string, at time.Time) (int, error) {
	return sqlshared.OAuthCodeInvalidateClient(ctx, s.shared(), clientID, at)
}

func (s *Store) OAuthFamilyCreate(ctx context.Context, row store.OAuthFamilyRow) error {
	return sqlshared.OAuthFamilyCreate(ctx, s.shared(), row)
}

func (s *Store) OAuthFamilyGet(ctx context.Context, familyID string) (store.OAuthFamilyRow, error) {
	return sqlshared.OAuthFamilyGet(ctx, s.shared(), familyID)
}

func (s *Store) OAuthFamilyTouch(ctx context.Context, familyID string, lastUsed, idleExpires time.Time) error {
	return sqlshared.OAuthFamilyTouch(ctx, s.shared(), familyID, lastUsed, idleExpires)
}

func (s *Store) OAuthFamilyRevoke(ctx context.Context, familyID, reason string, at time.Time) (bool, error) {
	return sqlshared.OAuthFamilyRevoke(ctx, s.shared(), familyID, reason, at)
}

func (s *Store) OAuthFamilyRevokeClient(ctx context.Context, clientID, reason string, at time.Time) ([]string, error) {
	return sqlshared.OAuthFamilyRevokeClient(ctx, s.shared(), clientID, reason, at)
}

func (s *Store) OAuthRefreshCreate(ctx context.Context, row store.OAuthRefreshRow) error {
	return sqlshared.OAuthRefreshCreate(ctx, s.shared(), row)
}

func (s *Store) OAuthRefreshUse(ctx context.Context, tokenHash string, now time.Time) (store.OAuthRefreshRow, bool, error) {
	return sqlshared.OAuthRefreshUse(ctx, s.shared(), tokenHash, now)
}

func (s *Store) OAuthTokenRetire(ctx context.Context, clientID, familyID string, at time.Time) (int, error) {
	return sqlshared.OAuthTokenRetire(ctx, s.shared(), clientID, familyID, at)
}

func (s *Store) OAuthDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	return sqlshared.OAuthDeleteExpired(ctx, s.shared(), before)
}
