package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// The RFC GW P4 egress_route_defs queries live in internal/store/sqlshared,
// written once for both stores.

func (s *Store) EgressRouteDefCreate(ctx context.Context, row store.EgressRouteDefRow) (store.EgressRouteDefRow, error) {
	return sqlshared.EgressRouteDefCreate(ctx, s.shared(), row)
}

func (s *Store) EgressRouteDefGet(ctx context.Context, tenantID, name string) (store.EgressRouteDefRow, error) {
	return sqlshared.EgressRouteDefGet(ctx, s.shared(), tenantID, name)
}

func (s *Store) EgressRouteDefList(ctx context.Context, tenantID string) ([]store.EgressRouteDefRow, error) {
	return sqlshared.EgressRouteDefList(ctx, s.shared(), tenantID)
}

func (s *Store) EgressRouteDefDelete(ctx context.Context, tenantID, name string) (bool, error) {
	return sqlshared.EgressRouteDefDelete(ctx, s.shared(), tenantID, name)
}
