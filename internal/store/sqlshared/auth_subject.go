package sqlshared

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// authSubjectFamily is one table of a subject's sign-in rows. where takes
// the tenant as its first placeholder and the subject as every later one.
type authSubjectFamily struct {
	name, table, where string
}

// authSubjectFamilies lists every sign-in row a subject owns, in delete
// order (refresh tokens before the families that name them). One list
// serves the user delete, the erasure and the erasure's count, so the three
// can never disagree about what a subject's sign-in is.
//
// auth_removals is not here: the user delete removes it (subjectRemovalRecord)
// and an erasure keeps it, as the removal policy's own record of the user.
func authSubjectFamilies() []authSubjectFamily {
	return []authSubjectFamily{
		{store.AuthFamilyIdentities, "user_identities", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyPasswords, "user_credentials", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyTOTP, "user_totp", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyRecoveryCodes, "user_recovery_codes", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyPasskeys, "user_passkeys", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyPasskeyCeremony, "webauthn_ceremonies", "tenant_id = ? AND subject = ?"},
		// Invites go when they attach to the subject or were redeemed by it,
		// so no row names the subject afterwards.
		{store.AuthFamilyInvites, "auth_invites", "tenant_id = ? AND (subject = ? OR redeemed_subject = ?)"},
		{store.AuthFamilyResets, "auth_resets", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyLinkLogins, "auth_login_states", "link_tenant = ? AND link_subject = ?"},
		{store.AuthFamilySessions, "ui_sessions", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyDeviceLogins, "device_authorizations", "decided_tenant = ? AND decided_subject = ?"},
		{store.AuthFamilyAppCodes, "oauth_codes", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilyAppRefreshTokens, "oauth_refresh_tokens",
			"family_id IN (SELECT family_id FROM oauth_refresh_families WHERE tenant_id = ? AND subject = ?)"},
		{store.AuthFamilyAppFamilies, "oauth_refresh_families", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilySCIMMemberships, "scim_group_members", "tenant_id = ? AND subject = ?"},
		{store.AuthFamilySCIMUsers, "scim_users", "tenant_id = ? AND subject = ?"},
	}
}

// args binds f's placeholders: the tenant first, the subject for the rest.
func (f authSubjectFamily) args(tenantID, subject string) []any {
	out := []any{tenantID}
	for i := 1; i < strings.Count(f.where, "?"); i++ {
		out = append(out, subject)
	}
	return out
}

// subjectRemovalRecord is the one row the user delete removes and an erasure
// keeps.
const subjectRemovalRecord = `DELETE FROM auth_removals WHERE tenant_id = ? AND subject = ?`

// AuthSubjectDelete removes every sign-in row of a subject and its removal
// record, in one transaction.
func AuthSubjectDelete(ctx context.Context, db DB, tenantID, subject string) error {
	err := db.InTx(ctx, func(tx Querier) error {
		if _, err := deleteSubjectFamilies(ctx, tx, tenantID, subject); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, subjectRemovalRecord, tenantID, subject)
		return err
	})
	if err != nil {
		return fmt.Errorf("auth subject delete: %w", err)
	}
	return nil
}

func deleteSubjectFamilies(ctx context.Context, tx Querier, tenantID, subject string) (map[string]int, error) {
	out := map[string]int{}
	for _, f := range authSubjectFamilies() {
		n, err := tx.Exec(ctx, `DELETE FROM `+f.table+` WHERE `+f.where, f.args(tenantID, subject)...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.table, err)
		}
		out[f.name] = int(n)
	}
	return out, nil
}

// AuthSubjectCount counts what UserErase would remove.
func AuthSubjectCount(ctx context.Context, db DB, tenantID, subject string) (map[string]int, error) {
	out := map[string]int{}
	for _, f := range authSubjectFamilies() {
		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM `+f.table+` WHERE `+f.where, f.args(tenantID, subject)...).Scan(&n); err != nil {
			return nil, fmt.Errorf("auth subject count: %s: %w", f.table, err)
		}
		out[f.name] = n
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ? AND subject = ? AND erased_at IS NULL`,
		tenantID, subject).Scan(&n); err != nil {
		return nil, fmt.Errorf("auth subject count: users: %w", err)
	}
	out[store.AuthFamilyAccount] = n
	return out, nil
}

// UserErase tombstones the users row and deletes the sign-in rows in one
// transaction, the tombstone first: the moment the transaction commits there
// is neither a way in nor an active user to let in.
func UserErase(ctx context.Context, db DB, tenantID, subject string, at time.Time) (map[string]int, error) {
	var out map[string]int
	err := db.InTx(ctx, func(tx Querier) error {
		n, err := tx.Exec(ctx, `UPDATE users SET display_name = '', status = 'disabled', erased_at = ?
			WHERE tenant_id = ? AND subject = ? AND erased_at IS NULL`, tx.Dialect().TimeArg(at), tenantID, subject)
		if err != nil {
			return fmt.Errorf("users: %w", err)
		}
		if out, err = deleteSubjectFamilies(ctx, tx, tenantID, subject); err != nil {
			return err
		}
		out[store.AuthFamilyAccount] = int(n)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("user erase: %w", err)
	}
	return out, nil
}

// UserErased reports whether (tenantID, subject) is an erasure's tombstone.
// A store write that found no row to change asks it to tell "erased" from
// "not found".
func UserErased(ctx context.Context, q Querier, tenantID, subject string) (bool, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ? AND subject = ? AND erased_at IS NOT NULL`,
		tenantID, subject).Scan(&n); err != nil {
		return false, fmt.Errorf("user erased: %w", err)
	}
	return n > 0, nil
}

// UserUpdateMiss is the error for a users update that changed no row: the
// user is an erasure's tombstone (store.ErrUserErased), or there is none.
func UserUpdateMiss(ctx context.Context, q Querier, tenantID, subject string) error {
	erased, err := UserErased(ctx, q, tenantID, subject)
	if err != nil {
		return fmt.Errorf("user update: %w", err)
	}
	if erased {
		return store.ErrUserErased
	}
	return &store.ErrNotFound{Kind: "user", ID: subject}
}
