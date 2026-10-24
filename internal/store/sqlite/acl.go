package sqlite

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// RFC EY AclStore — the shared-dialect implementation (sqlshared/acl.go).

func (s *Store) AclDefPut(ctx context.Context, row store.AclDefRow) (store.AclDefRow, error) {
	return sqlshared.AclDefPut(ctx, s.shared(), row)
}

func (s *Store) AclDefGet(ctx context.Context, tenantID, name string) (store.AclDefRow, error) {
	return sqlshared.AclDefGet(ctx, s.shared(), tenantID, name)
}

func (s *Store) AclDefList(ctx context.Context, tenantID string, includeRetired bool) ([]store.AclDefRow, error) {
	return sqlshared.AclDefList(ctx, s.shared(), tenantID, includeRetired)
}

func (s *Store) AclDefRetire(ctx context.Context, tenantID, name string) (bool, error) {
	return sqlshared.AclDefRetire(ctx, s.shared(), tenantID, name)
}

func (s *Store) GroupPut(ctx context.Context, row store.GroupRow) (store.GroupRow, error) {
	return sqlshared.GroupPut(ctx, s.shared(), row)
}

func (s *Store) GroupGet(ctx context.Context, tenantID, name string) (store.GroupRow, error) {
	return sqlshared.GroupGet(ctx, s.shared(), tenantID, name)
}

func (s *Store) GroupList(ctx context.Context, tenantID string) ([]store.GroupRow, error) {
	return sqlshared.GroupList(ctx, s.shared(), tenantID)
}

func (s *Store) GroupDelete(ctx context.Context, tenantID, name string) (bool, error) {
	return sqlshared.GroupDelete(ctx, s.shared(), tenantID, name)
}

func (s *Store) GroupMemberAdd(ctx context.Context, row store.GroupMemberRow) error {
	return sqlshared.GroupMemberAdd(ctx, s.shared(), row)
}

func (s *Store) GroupMemberRemove(ctx context.Context, tenantID, groupName, memberKind, memberID string) (bool, error) {
	return sqlshared.GroupMemberRemove(ctx, s.shared(), tenantID, groupName, memberKind, memberID)
}

func (s *Store) GroupMembers(ctx context.Context, tenantID, groupName string) ([]store.GroupMemberRow, error) {
	return sqlshared.GroupMembers(ctx, s.shared(), tenantID, groupName)
}

func (s *Store) GroupGraph(ctx context.Context, tenantID string) (map[string][]string, error) {
	return sqlshared.GroupGraph(ctx, s.shared(), tenantID)
}

func (s *Store) GroupsOfSubject(ctx context.Context, tenantID, subject string) ([]string, error) {
	return sqlshared.GroupsOfSubject(ctx, s.shared(), tenantID, subject)
}

func (s *Store) AclBindingPut(ctx context.Context, row store.AclBindingRow) (store.AclBindingRow, error) {
	return sqlshared.AclBindingPut(ctx, s.shared(), row)
}

func (s *Store) AclBindingRetire(ctx context.Context, tenantID, bindingID string) (bool, error) {
	return sqlshared.AclBindingRetire(ctx, s.shared(), tenantID, bindingID)
}

func (s *Store) AclBindingList(ctx context.Context, tenantID string, includeRetired bool) ([]store.AclBindingRow, error) {
	return sqlshared.AclBindingList(ctx, s.shared(), tenantID, includeRetired)
}

func (s *Store) AclBindingsFor(ctx context.Context, tenantID, subject string, groups []string, tokenDefID string) ([]store.AclBindingRow, error) {
	return sqlshared.AclBindingsFor(ctx, s.shared(), tenantID, subject, groups, tokenDefID)
}

func (s *Store) AclEraseSubject(ctx context.Context, tenantID, subject string) (int, int, error) {
	return sqlshared.AclEraseSubject(ctx, s.shared(), tenantID, subject)
}

func (s *Store) AclEraseTokenBindings(ctx context.Context, tenantID string, defIDs []string) (int, error) {
	return sqlshared.AclEraseTokenBindings(ctx, s.shared(), tenantID, defIDs)
}

func (s *Store) AclVersion(ctx context.Context, tenantID string) (int64, error) {
	return sqlshared.AclVersion(ctx, s.shared(), tenantID)
}
