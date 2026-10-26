package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// MigrateTenantDocumentRows implements store.Store; see sqlshared.
func (s *Store) MigrateTenantDocumentRows(ctx context.Context) (store.TenantDocumentKeyspaceReport, error) {
	return sqlshared.MigrateTenantDocumentRows(ctx, s.shared())
}
