package postgres

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// RFC FA P6 passkeys, WebAuthn ceremonies and SCIM resources: the shared
// implementation in sqlshared (postgres migration 0126).

// UserPasskeyCreate implements store.PasskeyStore.
func (s *Store) UserPasskeyCreate(ctx context.Context, row store.UserPasskeyRow) error {
	return sqlshared.UserPasskeyCreate(ctx, s.shared(), row)
}

// UserPasskeyGet implements store.PasskeyStore.
func (s *Store) UserPasskeyGet(ctx context.Context, credentialID []byte) (store.UserPasskeyRow, error) {
	return sqlshared.UserPasskeyGet(ctx, s.shared(), credentialID)
}

// UserPasskeyList implements store.PasskeyStore.
func (s *Store) UserPasskeyList(ctx context.Context, tenantID, subject string) ([]store.UserPasskeyRow, error) {
	return sqlshared.UserPasskeyList(ctx, s.shared(), tenantID, subject)
}

// UserPasskeyRecordUse implements store.PasskeyStore.
func (s *Store) UserPasskeyRecordUse(ctx context.Context, credentialID []byte, signCount uint32, flags uint8, at time.Time) (bool, error) {
	return sqlshared.UserPasskeyRecordUse(ctx, s.shared(), credentialID, signCount, flags, at)
}

// UserPasskeyFlag implements store.PasskeyStore.
func (s *Store) UserPasskeyFlag(ctx context.Context, credentialID []byte, reason string, at time.Time) (bool, error) {
	return sqlshared.UserPasskeyFlag(ctx, s.shared(), credentialID, reason, at)
}

// UserPasskeyRename implements store.PasskeyStore.
func (s *Store) UserPasskeyRename(ctx context.Context, tenantID, subject string, credentialID []byte, name string) (bool, error) {
	return sqlshared.UserPasskeyRename(ctx, s.shared(), tenantID, subject, credentialID, name)
}

// UserPasskeyDelete implements store.PasskeyStore.
func (s *Store) UserPasskeyDelete(ctx context.Context, tenantID, subject string, credentialID []byte) (bool, error) {
	return sqlshared.UserPasskeyDelete(ctx, s.shared(), tenantID, subject, credentialID)
}

// UserPasskeyDeleteSubject implements store.PasskeyStore.
func (s *Store) UserPasskeyDeleteSubject(ctx context.Context, tenantID, subject string) (int, error) {
	return sqlshared.UserPasskeyDeleteSubject(ctx, s.shared(), tenantID, subject)
}

// WebAuthnCeremonyCreate implements store.PasskeyStore.
func (s *Store) WebAuthnCeremonyCreate(ctx context.Context, row store.WebAuthnCeremonyRow) error {
	return sqlshared.WebAuthnCeremonyCreate(ctx, s.shared(), row)
}

// WebAuthnCeremonyTake implements store.PasskeyStore.
func (s *Store) WebAuthnCeremonyTake(ctx context.Context, ceremonyHash string, now time.Time) (store.WebAuthnCeremonyRow, error) {
	return sqlshared.WebAuthnCeremonyTake(ctx, s.shared(), ceremonyHash, now)
}

// WebAuthnCeremonyDeleteExpired implements store.PasskeyStore.
func (s *Store) WebAuthnCeremonyDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	return sqlshared.WebAuthnCeremonyDeleteExpired(ctx, s.shared(), before)
}

// SCIMUserCreate implements store.SCIMStore.
func (s *Store) SCIMUserCreate(ctx context.Context, row store.SCIMUserRow) error {
	return sqlshared.SCIMUserCreate(ctx, s.shared(), row)
}

// SCIMUserGet implements store.SCIMStore.
func (s *Store) SCIMUserGet(ctx context.Context, gate, tenantID, scimID string) (store.SCIMUserRow, error) {
	return sqlshared.SCIMUserGet(ctx, s.shared(), gate, tenantID, scimID)
}

// SCIMUserList implements store.SCIMStore.
func (s *Store) SCIMUserList(ctx context.Context, gate, tenantID string, f store.SCIMFilter, offset, limit int) ([]store.SCIMUserRow, int, error) {
	return sqlshared.SCIMUserList(ctx, s.shared(), gate, tenantID, f, offset, limit)
}

// SCIMUserUpdate implements store.SCIMStore.
func (s *Store) SCIMUserUpdate(ctx context.Context, row store.SCIMUserRow) error {
	return sqlshared.SCIMUserUpdate(ctx, s.shared(), row)
}

// SCIMUserMarkDeleted implements store.SCIMStore.
func (s *Store) SCIMUserMarkDeleted(ctx context.Context, gate, tenantID, scimID string, at time.Time) (bool, error) {
	return sqlshared.SCIMUserMarkDeleted(ctx, s.shared(), gate, tenantID, scimID, at)
}

// SCIMUserDiscard implements store.SCIMStore.
func (s *Store) SCIMUserDiscard(ctx context.Context, gate, tenantID, scimID string) error {
	return sqlshared.SCIMUserDiscard(ctx, s.shared(), gate, tenantID, scimID)
}

// SCIMGroupCreate implements store.SCIMStore.
func (s *Store) SCIMGroupCreate(ctx context.Context, row store.SCIMGroupRow) error {
	return sqlshared.SCIMGroupCreate(ctx, s.shared(), row)
}

// SCIMGroupGet implements store.SCIMStore.
func (s *Store) SCIMGroupGet(ctx context.Context, gate, tenantID, scimID string) (store.SCIMGroupRow, error) {
	return sqlshared.SCIMGroupGet(ctx, s.shared(), gate, tenantID, scimID)
}

// SCIMGroupList implements store.SCIMStore.
func (s *Store) SCIMGroupList(ctx context.Context, gate, tenantID string, f store.SCIMFilter, offset, limit int) ([]store.SCIMGroupRow, int, error) {
	return sqlshared.SCIMGroupList(ctx, s.shared(), gate, tenantID, f, offset, limit)
}

// SCIMGroupUpdate implements store.SCIMStore.
func (s *Store) SCIMGroupUpdate(ctx context.Context, row store.SCIMGroupRow) error {
	return sqlshared.SCIMGroupUpdate(ctx, s.shared(), row)
}

// SCIMGroupDelete implements store.SCIMStore.
func (s *Store) SCIMGroupDelete(ctx context.Context, gate, tenantID, scimID string) (bool, error) {
	return sqlshared.SCIMGroupDelete(ctx, s.shared(), gate, tenantID, scimID)
}

// SCIMGroupMembers implements store.SCIMStore.
func (s *Store) SCIMGroupMembers(ctx context.Context, gate, tenantID, groupID string) ([]string, error) {
	return sqlshared.SCIMGroupMembers(ctx, s.shared(), gate, tenantID, groupID)
}

// SCIMGroupMembersAdd implements store.SCIMStore.
func (s *Store) SCIMGroupMembersAdd(ctx context.Context, gate, tenantID, groupID string, subjects []string) ([]string, error) {
	return sqlshared.SCIMGroupMembersAdd(ctx, s.shared(), gate, tenantID, groupID, subjects)
}

// SCIMGroupMembersRemove implements store.SCIMStore.
func (s *Store) SCIMGroupMembersRemove(ctx context.Context, gate, tenantID, groupID string, subjects []string) ([]string, error) {
	return sqlshared.SCIMGroupMembersRemove(ctx, s.shared(), gate, tenantID, groupID, subjects)
}

// SCIMGroupsOfSubject implements store.SCIMStore.
func (s *Store) SCIMGroupsOfSubject(ctx context.Context, gate, tenantID, subject string) ([]store.SCIMGroupRow, error) {
	return sqlshared.SCIMGroupsOfSubject(ctx, s.shared(), gate, tenantID, subject)
}

// SCIMRemovalDisown implements store.SCIMStore.
func (s *Store) SCIMRemovalDisown(ctx context.Context, tenantID, subject string) (bool, error) {
	return sqlshared.SCIMRemovalDisown(ctx, s.shared(), tenantID, subject)
}

// SCIMRemovalBase implements store.SCIMStore.
func (s *Store) SCIMRemovalBase(ctx context.Context, gate, tenantID string, before time.Time) (int, error) {
	return sqlshared.SCIMRemovalBase(ctx, s.shared(), gate, tenantID, before)
}

// SCIMUsersCreatedSince implements store.SCIMStore.
func (s *Store) SCIMUsersCreatedSince(ctx context.Context, gate, tenantID string, since time.Time) (int, error) {
	return sqlshared.SCIMUsersCreatedSince(ctx, s.shared(), gate, tenantID, since)
}

// SCIMUsersGetMany implements store.SCIMStore.
func (s *Store) SCIMUsersGetMany(ctx context.Context, gate, tenantID string, ids []string) (map[string]store.SCIMUserRow, error) {
	return sqlshared.SCIMUsersGetMany(ctx, s.shared(), gate, tenantID, ids)
}
