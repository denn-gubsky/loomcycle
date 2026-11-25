package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC GW P6 workspace_templates queries live in internal/store/sqlshared,
// written once for both stores.

func (s *Store) WorkspaceTemplateCreate(ctx context.Context, row store.WorkspaceTemplateRow) (store.WorkspaceTemplateRow, error) {
	return sqlshared.WorkspaceTemplateCreate(ctx, s.shared(), row)
}

func (s *Store) WorkspaceTemplateGet(ctx context.Context, tenantID, scope, scopeID, name string) (store.WorkspaceTemplateRow, error) {
	return sqlshared.WorkspaceTemplateGet(ctx, s.shared(), tenantID, scope, scopeID, name)
}

func (s *Store) WorkspaceTemplateList(ctx context.Context, tenantID, scope, scopeID string) ([]store.WorkspaceTemplateRow, error) {
	return sqlshared.WorkspaceTemplateList(ctx, s.shared(), tenantID, scope, scopeID)
}

func (s *Store) WorkspaceTemplateCount(ctx context.Context, tenantID string) (int, error) {
	return sqlshared.WorkspaceTemplateCount(ctx, s.shared(), tenantID)
}

func (s *Store) WorkspaceTemplateDelete(ctx context.Context, tenantID, id string) (bool, error) {
	return sqlshared.WorkspaceTemplateDelete(ctx, s.shared(), tenantID, id)
}
