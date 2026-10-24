package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// password_gate.go — RFC FA P2 password-gate state. Sibling of
// internal/store/postgres/password_gate.go; both keep the contract documented
// on store.AuthIdentityStore / store.PasswordGateStore and asserted by
// internal/store/storetest/password_gate.go.
//
// Every transaction here opens with a write. A deferred SQLite transaction
// that reads first and writes later cannot upgrade its snapshot once another
// connection has committed (SQLITE_BUSY, which busy_timeout does not wait
// out); writing first takes the write lock up front, and that lock is what
// serialises concurrent failures, redemptions and code uses.

func gateUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// gateNanos is t as unix nanos, or NULL for the zero time.
func gateNanos(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// gateTime reads a nullable unix-nano column; NULL is the zero time.
func gateTime(ns sql.NullInt64) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return time.Unix(0, ns.Int64).UTC()
}

// gateStamp is t, or the current time when t is zero.
func gateStamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// gateLaterLock is the lock to keep: the existing one when it already ends
// after candidate, else candidate. A lock is never moved earlier.
func gateLaterLock(existing, candidate time.Time) time.Time {
	if existing.After(candidate) {
		return existing
	}
	return candidate
}

// gateActiveAt is t when it is still in the future at now, else the zero
// time, so a caller tests a lock with IsZero and never sees a stale one.
func gateActiveAt(t, now time.Time) time.Time {
	if t.After(now) {
		return t
	}
	return time.Time{}
}

func gateBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gateExecer is the write surface shared by *sql.DB and *sql.Tx.
type gateExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ---- identities ------------------------------------------------------------

const identityCols = `tenant_id, subject, gate, issuer, external_subject, email, email_verified, created_at, last_login_at`

type gateScanner interface {
	Scan(dest ...any) error
}

func scanIdentity(row gateScanner) (store.UserIdentityRow, error) {
	var r store.UserIdentityRow
	var created int64
	var lastLogin sql.NullInt64
	if err := row.Scan(&r.TenantID, &r.Subject, &r.Gate, &r.Issuer, &r.ExternalSubject,
		&r.Email, &r.EmailVerified, &created, &lastLogin); err != nil {
		return store.UserIdentityRow{}, err
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.LastLoginAt = gateTime(lastLogin)
	return r, nil
}

func insertIdentity(ctx context.Context, q gateExecer, row store.UserIdentityRow) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_identities (`+identityCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TenantID, row.Subject, row.Gate, row.Issuer, row.ExternalSubject,
		row.Email, gateBool(row.EmailVerified), gateStamp(row.CreatedAt).UnixNano(), gateNanos(row.LastLoginAt))
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "user_identity", ID: row.ExternalSubject}
		}
		return fmt.Errorf("user identity create: %w", err)
	}
	return nil
}

// UserIdentityCreate implements store.AuthIdentityStore.
func (s *Store) UserIdentityCreate(ctx context.Context, row store.UserIdentityRow) error {
	return insertIdentity(ctx, s.db, row)
}

// UserIdentityGet implements store.AuthIdentityStore.
func (s *Store) UserIdentityGet(ctx context.Context, gate, issuer, externalSubject string) (store.UserIdentityRow, error) {
	r, err := scanIdentity(s.db.QueryRowContext(ctx, `
		SELECT `+identityCols+` FROM user_identities
		WHERE gate = ? AND issuer = ? AND external_subject = ?`, gate, issuer, externalSubject))
	if errors.Is(err, sql.ErrNoRows) {
		return store.UserIdentityRow{}, &store.ErrNotFound{Kind: "user_identity", ID: externalSubject}
	}
	if err != nil {
		return store.UserIdentityRow{}, fmt.Errorf("user identity get: %w", err)
	}
	return r, nil
}

// UserIdentityListSubject implements store.AuthIdentityStore.
func (s *Store) UserIdentityListSubject(ctx context.Context, tenantID, subject string) ([]store.UserIdentityRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+identityCols+` FROM user_identities
		WHERE tenant_id = ? AND subject = ?
		ORDER BY gate, issuer`, tenantID, subject)
	if err != nil {
		return nil, fmt.Errorf("user identity list: %w", err)
	}
	defer rows.Close()
	out := []store.UserIdentityRow{}
	for rows.Next() {
		r, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("user identity list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserIdentityRename implements store.AuthIdentityStore.
func (s *Store) UserIdentityRename(ctx context.Context, tenantID, subject, gate, newExternalSubject string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_identities SET external_subject = ?
		WHERE tenant_id = ? AND subject = ? AND gate = ? AND issuer = ''`,
		newExternalSubject, tenantID, subject, gate)
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "user_identity", ID: newExternalSubject}
		}
		return fmt.Errorf("user identity rename: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "user_identity", ID: subject}
	}
	return nil
}

// UserIdentityTouchLogin implements store.AuthIdentityStore.
func (s *Store) UserIdentityTouchLogin(ctx context.Context, gate, issuer, externalSubject string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_identities SET last_login_at = ?
		WHERE gate = ? AND issuer = ? AND external_subject = ?`,
		gateStamp(at).UnixNano(), gate, issuer, externalSubject)
	if err != nil {
		return fmt.Errorf("user identity touch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "user_identity", ID: externalSubject}
	}
	return nil
}

// ---- credentials -----------------------------------------------------------

// putCredential inserts or replaces a credential with its throttle reset.
func putCredential(ctx context.Context, q gateExecer, row store.UserCredentialRow) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_credentials (tenant_id, subject, kind, phc, pepper_kid, updated_at, failed_count, locked_until)
		VALUES (?, ?, ?, ?, ?, ?, 0, NULL)
		ON CONFLICT (tenant_id, subject, kind) DO UPDATE SET
			phc = excluded.phc, pepper_kid = excluded.pepper_kid, updated_at = excluded.updated_at,
			failed_count = 0, locked_until = NULL`,
		row.TenantID, row.Subject, row.Kind, row.PHC, row.PepperKID, gateStamp(row.UpdatedAt).UnixNano())
	if err != nil {
		return fmt.Errorf("user credential set: %w", err)
	}
	return nil
}

// UserCredentialGet implements store.PasswordGateStore.
func (s *Store) UserCredentialGet(ctx context.Context, tenantID, subject, kind string) (store.UserCredentialRow, error) {
	var r store.UserCredentialRow
	var updated int64
	var locked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, subject, kind, phc, pepper_kid, updated_at, failed_count, locked_until
		FROM user_credentials WHERE tenant_id = ? AND subject = ? AND kind = ?`,
		tenantID, subject, kind).Scan(
		&r.TenantID, &r.Subject, &r.Kind, &r.PHC, &r.PepperKID, &updated, &r.FailedCount, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return store.UserCredentialRow{}, &store.ErrNotFound{Kind: "user_credential", ID: subject}
	}
	if err != nil {
		return store.UserCredentialRow{}, fmt.Errorf("user credential get: %w", err)
	}
	r.UpdatedAt = time.Unix(0, updated).UTC()
	r.LockedUntil = gateTime(locked)
	return r, nil
}

// UserCredentialSet implements store.PasswordGateStore.
func (s *Store) UserCredentialSet(ctx context.Context, row store.UserCredentialRow) error {
	return putCredential(ctx, s.db, row)
}

// UserCredentialRehash implements store.PasswordGateStore.
func (s *Store) UserCredentialRehash(ctx context.Context, tenantID, subject, kind, oldPHC, newPHC, pepperKID string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_credentials SET phc = ?, pepper_kid = ?, updated_at = ?
		WHERE tenant_id = ? AND subject = ? AND kind = ? AND phc = ?`,
		newPHC, pepperKID, gateStamp(at).UnixNano(), tenantID, subject, kind, oldPHC)
	if err != nil {
		return false, fmt.Errorf("user credential rehash: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UserCredentialRecordFailure implements store.PasswordGateStore.
func (s *Store) UserCredentialRecordFailure(ctx context.Context, tenantID, subject, kind string, now time.Time, lockFor func(failures int) time.Duration) (int, time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE user_credentials SET failed_count = failed_count + 1
		WHERE tenant_id = ? AND subject = ? AND kind = ?`, tenantID, subject, kind)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("user credential record failure: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, time.Time{}, err
	} else if n == 0 {
		return 0, time.Time{}, &store.ErrNotFound{Kind: "user_credential", ID: subject}
	}
	var count int
	var locked sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT failed_count, locked_until FROM user_credentials
		WHERE tenant_id = ? AND subject = ? AND kind = ?`, tenantID, subject, kind).Scan(&count, &locked); err != nil {
		return 0, time.Time{}, fmt.Errorf("user credential record failure: read: %w", err)
	}
	lock := gateTime(locked)
	if d := lockFor(count); d > 0 {
		if next := gateLaterLock(lock, now.Add(d)); !next.Equal(lock) {
			if _, err := tx.ExecContext(ctx, `
				UPDATE user_credentials SET locked_until = ?
				WHERE tenant_id = ? AND subject = ? AND kind = ?`, next.UnixNano(), tenantID, subject, kind); err != nil {
				return 0, time.Time{}, fmt.Errorf("user credential lock: %w", err)
			}
			lock = next
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, time.Time{}, err
	}
	return count, gateActiveAt(lock, now), nil
}

// UserCredentialRecordSuccess implements store.PasswordGateStore. A missing
// credential is not an error: there is nothing to clear.
func (s *Store) UserCredentialRecordSuccess(ctx context.Context, tenantID, subject, kind string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE user_credentials SET failed_count = 0, locked_until = NULL
		WHERE tenant_id = ? AND subject = ? AND kind = ?`, tenantID, subject, kind); err != nil {
		return fmt.Errorf("user credential record success: %w", err)
	}
	return nil
}

// UserCredentialDelete implements store.PasswordGateStore.
func (s *Store) UserCredentialDelete(ctx context.Context, tenantID, subject, kind string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_credentials WHERE tenant_id = ? AND subject = ? AND kind = ?`,
		tenantID, subject, kind)
	if err != nil {
		return false, fmt.Errorf("user credential delete: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---- TOTP + recovery codes -------------------------------------------------

func putTOTP(ctx context.Context, q gateExecer, row store.UserTOTPRow) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO user_totp (tenant_id, subject, sealed, created_at, confirmed_at, last_step)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, subject) DO UPDATE SET
			sealed = excluded.sealed, created_at = excluded.created_at,
			confirmed_at = excluded.confirmed_at, last_step = excluded.last_step`,
		row.TenantID, row.Subject, row.Sealed, gateStamp(row.CreatedAt).UnixNano(), gateNanos(row.ConfirmedAt), row.LastStep)
	if err != nil {
		return fmt.Errorf("user totp put: %w", err)
	}
	return nil
}

func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, tenantID, subject string, hashes []string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE tenant_id = ? AND subject = ?`, tenantID, subject); err != nil {
		return fmt.Errorf("user recovery codes clear: %w", err)
	}
	ns := gateStamp(at).UnixNano()
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_recovery_codes (tenant_id, subject, code_hash, created_at) VALUES (?, ?, ?, ?)`,
			tenantID, subject, h, ns); err != nil {
			return fmt.Errorf("user recovery codes insert: %w", err)
		}
	}
	return nil
}

// writeSecondFactor stores a redemption's or a reset's TOTP enrolment for
// (tenantID, subject), the row being enrolled; the seal is bound to that pair.
func writeSecondFactor(ctx context.Context, tx *sql.Tx, tenantID, subject string, sf *store.AuthSecondFactor, at time.Time) error {
	if sf == nil {
		return nil
	}
	if sf.TOTP.TenantID != tenantID || sf.TOTP.Subject != subject {
		return fmt.Errorf("second factor: totp row is for %q/%q, not %q/%q", sf.TOTP.TenantID, sf.TOTP.Subject, tenantID, subject)
	}
	if err := putTOTP(ctx, tx, sf.TOTP); err != nil {
		return err
	}
	return replaceRecoveryCodes(ctx, tx, tenantID, subject, sf.RecoveryHashes, at)
}

// UserTOTPGet implements store.PasswordGateStore.
func (s *Store) UserTOTPGet(ctx context.Context, tenantID, subject string) (store.UserTOTPRow, error) {
	var r store.UserTOTPRow
	var created int64
	var confirmed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, subject, sealed, created_at, confirmed_at, last_step
		FROM user_totp WHERE tenant_id = ? AND subject = ?`, tenantID, subject).Scan(
		&r.TenantID, &r.Subject, &r.Sealed, &created, &confirmed, &r.LastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return store.UserTOTPRow{}, &store.ErrNotFound{Kind: "user_totp", ID: subject}
	}
	if err != nil {
		return store.UserTOTPRow{}, fmt.Errorf("user totp get: %w", err)
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.ConfirmedAt = gateTime(confirmed)
	return r, nil
}

// UserTOTPPut implements store.PasswordGateStore.
func (s *Store) UserTOTPPut(ctx context.Context, row store.UserTOTPRow) error {
	return putTOTP(ctx, s.db, row)
}

// UserTOTPUseStep implements store.PasswordGateStore. The step comparison is
// in the WHERE clause, so two logins presenting the same code race on one row
// update and exactly one wins.
func (s *Store) UserTOTPUseStep(ctx context.Context, tenantID, subject string, step int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_totp SET last_step = ?, confirmed_at = COALESCE(confirmed_at, ?)
		WHERE tenant_id = ? AND subject = ? AND last_step < ?`,
		step, gateStamp(at).UnixNano(), tenantID, subject, step)
	if err != nil {
		return false, fmt.Errorf("user totp use step: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UserTOTPDelete implements store.PasswordGateStore. It reports whether any
// row (the TOTP row or a recovery code) was removed.
func (s *Store) UserTOTPDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var total int64
	for _, q := range []string{
		`DELETE FROM user_totp WHERE tenant_id = ? AND subject = ?`,
		`DELETE FROM user_recovery_codes WHERE tenant_id = ? AND subject = ?`,
	} {
		res, err := tx.ExecContext(ctx, q, tenantID, subject)
		if err != nil {
			return false, fmt.Errorf("user totp delete: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return total > 0, nil
}

// UserRecoveryCodesReplace implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodesReplace(ctx context.Context, tenantID, subject string, hashes []string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceRecoveryCodes(ctx, tx, tenantID, subject, hashes, at); err != nil {
		return err
	}
	return tx.Commit()
}

// UserRecoveryCodeConsume implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodeConsume(ctx context.Context, tenantID, subject, hash string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_recovery_codes SET used_at = ?
		WHERE tenant_id = ? AND subject = ? AND code_hash = ? AND used_at IS NULL`,
		gateStamp(at).UnixNano(), tenantID, subject, hash)
	if err != nil {
		return false, fmt.Errorf("user recovery code consume: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UserRecoveryCodesRemaining implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodesRemaining(ctx context.Context, tenantID, subject string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_recovery_codes
		WHERE tenant_id = ? AND subject = ? AND used_at IS NULL`, tenantID, subject).Scan(&n); err != nil {
		return 0, fmt.Errorf("user recovery codes remaining: %w", err)
	}
	return n, nil
}

// ---- invites ---------------------------------------------------------------

const inviteCols = `invite_id, code_hash, gate, tenant_id, subject, access_mode, groups, login, email, return_to,
	bootstrap, created_by, created_at, expires_at, redeemed_at, redeemed_subject, revoked_at`

func scanInvite(row gateScanner) (store.AuthInviteRow, error) {
	var r store.AuthInviteRow
	var groups string
	var created, expires int64
	var redeemed, revoked sql.NullInt64
	if err := row.Scan(&r.InviteID, &r.CodeHash, &r.Gate, &r.TenantID, &r.Subject, &r.AccessMode, &groups,
		&r.Login, &r.Email, &r.ReturnTo, &r.Bootstrap, &r.CreatedBy, &created, &expires,
		&redeemed, &r.RedeemedSubject, &revoked); err != nil {
		return store.AuthInviteRow{}, err
	}
	if err := json.Unmarshal([]byte(groups), &r.Groups); err != nil {
		return store.AuthInviteRow{}, fmt.Errorf("invite %s groups: %w", r.InviteID, err)
	}
	if len(r.Groups) == 0 {
		r.Groups = nil
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.ExpiresAt = time.Unix(0, expires).UTC()
	r.RedeemedAt = gateTime(redeemed)
	r.RevokedAt = gateTime(revoked)
	return r, nil
}

// AuthInviteCreate implements store.PasswordGateStore.
func (s *Store) AuthInviteCreate(ctx context.Context, row store.AuthInviteRow) error {
	groups := row.Groups
	if groups == nil {
		groups = []string{}
	}
	gj, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("auth invite create: groups: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO auth_invites (`+inviteCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.InviteID, row.CodeHash, row.Gate, row.TenantID, row.Subject, row.AccessMode, string(gj),
		row.Login, row.Email, row.ReturnTo, gateBool(row.Bootstrap), row.CreatedBy,
		gateStamp(row.CreatedAt).UnixNano(), row.ExpiresAt.UnixNano(),
		gateNanos(row.RedeemedAt), row.RedeemedSubject, gateNanos(row.RevokedAt))
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "auth_invite", ID: row.InviteID}
		}
		return fmt.Errorf("auth invite create: %w", err)
	}
	return nil
}

// authInviteGetWhere reads one invite by column = arg. missID is the id a miss
// reports; a lookup by code hash passes "" so no error string carries it.
func authInviteGetWhere(ctx context.Context, q *sql.DB, column, arg, missID string) (store.AuthInviteRow, error) {
	r, err := scanInvite(q.QueryRowContext(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE `+column+` = ?`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthInviteRow{}, &store.ErrNotFound{Kind: "auth_invite", ID: missID}
	}
	if err != nil {
		return store.AuthInviteRow{}, fmt.Errorf("auth invite get: %w", err)
	}
	return r, nil
}

// AuthInviteGet implements store.PasswordGateStore.
func (s *Store) AuthInviteGet(ctx context.Context, inviteID string) (store.AuthInviteRow, error) {
	return authInviteGetWhere(ctx, s.db, "invite_id", inviteID, inviteID)
}

// AuthInviteGetByCode implements store.PasswordGateStore.
func (s *Store) AuthInviteGetByCode(ctx context.Context, codeHash string) (store.AuthInviteRow, error) {
	return authInviteGetWhere(ctx, s.db, "code_hash", codeHash, "")
}

// AuthInviteList implements store.PasswordGateStore. The tenant filter is
// unconditional: "" is the shared tenant, not a wildcard.
func (s *Store) AuthInviteList(ctx context.Context, gate, tenantID string) ([]store.AuthInviteRow, error) {
	return s.listInvites(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE gate = ? AND tenant_id = ?
		ORDER BY created_at DESC, invite_id DESC`, gate, tenantID)
}

// AuthInviteListAllTenants implements store.PasswordGateStore.
func (s *Store) AuthInviteListAllTenants(ctx context.Context, gate string) ([]store.AuthInviteRow, error) {
	return s.listInvites(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE gate = ?
		ORDER BY created_at DESC, invite_id DESC`, gate)
}

func (s *Store) listInvites(ctx context.Context, q string, args ...any) ([]store.AuthInviteRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("auth invite list: %w", err)
	}
	defer rows.Close()
	out := []store.AuthInviteRow{}
	for rows.Next() {
		r, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("auth invite list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuthInviteRevoke implements store.PasswordGateStore.
func (s *Store) AuthInviteRevoke(ctx context.Context, inviteID string, at time.Time) (bool, error) {
	ns := gateStamp(at).UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth_invites SET revoked_at = ?
		WHERE invite_id = ? AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`, ns, inviteID, ns)
	if err != nil {
		return false, fmt.Errorf("auth invite revoke: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// checkRedemption refuses a redemption whose rows do not belong to the invite
// it consumes. The caller builds them from the invite, so a mismatch is a bug;
// failing closed here keeps a bug from writing a login into another tenant or
// attaching a password to a user the invite did not name.
func checkRedemption(inv store.AuthInviteRow, red store.AuthInviteRedemption) error {
	id := red.Identity
	if id.TenantID != inv.TenantID || id.Gate != inv.Gate {
		return fmt.Errorf("auth invite redeem: identity is for %q at gate %q, invite is for %q at gate %q", id.TenantID, id.Gate, inv.TenantID, inv.Gate)
	}
	if red.Credential.TenantID != id.TenantID || red.Credential.Subject != id.Subject {
		return fmt.Errorf("auth invite redeem: credential and identity name different users")
	}
	if inv.Subject != "" {
		if red.User != nil || id.Subject != inv.Subject {
			return fmt.Errorf("auth invite redeem: the invite attaches to an existing user; the redemption must name it and create none")
		}
		return nil
	}
	if red.User == nil {
		return fmt.Errorf("auth invite redeem: the invite creates a user; the redemption has none")
	}
	if red.User.TenantID != id.TenantID || red.User.Subject != id.Subject {
		return fmt.Errorf("auth invite redeem: user and identity name different users")
	}
	return nil
}

// AuthInviteRedeem implements store.PasswordGateStore. Every step runs in one
// transaction; any error rolls it back, so a failed redemption writes nothing
// and leaves the invite live. The consuming UPDATE comes first and takes
// SQLite's write lock, which serialises redemptions as a whole — including two
// bootstrap invites of one tenant, so the second one's user count sees the
// first one's user.
func (s *Store) AuthInviteRedeem(ctx context.Context, red store.AuthInviteRedemption) error {
	if red.User != nil {
		if !store.ValidUserAccessMode(red.User.AccessMode) {
			return fmt.Errorf("auth invite redeem: access_mode must be one of tenant|isolated, got %q", red.User.AccessMode)
		}
		if !store.ValidUserStatus(red.User.Status) {
			return fmt.Errorf("auth invite redeem: status must be one of active|disabled, got %q", red.User.Status)
		}
	}
	now := gateStamp(red.Now)
	ns := now.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE auth_invites SET redeemed_at = ?, redeemed_subject = ?
		WHERE code_hash = ? AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		ns, red.Identity.Subject, red.CodeHash, ns)
	if err != nil {
		return fmt.Errorf("auth invite redeem: consume: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return &store.ErrNotFound{Kind: "auth_invite"}
	}
	inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE code_hash = ?`, red.CodeHash))
	if err != nil {
		return fmt.Errorf("auth invite redeem: read: %w", err)
	}
	if err := checkRedemption(inv, red); err != nil {
		return err
	}

	if inv.Bootstrap {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM users WHERE tenant_id = ?)
			+ (SELECT COUNT(*) FROM tenant_bootstraps WHERE tenant_id = ?)`, inv.TenantID, inv.TenantID).Scan(&n); err != nil {
			return fmt.Errorf("auth invite redeem: count users: %w", err)
		}
		if n > 0 {
			return &store.ErrConflict{Kind: "tenant_not_empty", ID: inv.TenantID}
		}
	}

	if red.User != nil {
		u := *red.User
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tenant_bootstraps (tenant_id, at, via) VALUES (?, ?, ?)`,
			u.TenantID, now.UnixNano(), "invite:"+inv.Gate); err != nil {
			return fmt.Errorf("auth invite redeem: bootstrap marker: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (tenant_id, subject, display_name, access_mode, status, created_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.TenantID, u.Subject, u.DisplayName, u.AccessMode, u.Status, gateStamp(u.CreatedAt).UnixNano(), u.CreatedBy); err != nil {
			if gateUniqueViolation(err) {
				return &store.ErrConflict{Kind: "user", ID: u.Subject}
			}
			return fmt.Errorf("auth invite redeem: create user: %w", err)
		}
	} else {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE tenant_id = ? AND subject = ?`,
			red.Identity.TenantID, red.Identity.Subject).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.ErrNotFound{Kind: "user", ID: red.Identity.Subject}
		}
		if err != nil {
			return fmt.Errorf("auth invite redeem: user lookup: %w", err)
		}
	}

	if err := insertIdentity(ctx, tx, red.Identity); err != nil {
		return err
	}
	if err := putCredential(ctx, tx, red.Credential); err != nil {
		return err
	}
	if err := writeSecondFactor(ctx, tx, red.Identity.TenantID, red.Identity.Subject, red.SecondFactor, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TenantBootstrapped implements store.PasswordGateStore.
func (s *Store) TenantBootstrapped(ctx context.Context, tenantID string) (bool, error) {
	var ok bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_bootstraps WHERE tenant_id = ?)
		OR EXISTS (SELECT 1 FROM users WHERE tenant_id = ?)`, tenantID, tenantID).Scan(&ok); err != nil {
		return false, fmt.Errorf("tenant bootstrapped: %w", err)
	}
	return ok, nil
}

// AuthInviteLiveBootstrap implements store.PasswordGateStore.
func (s *Store) AuthInviteLiveBootstrap(ctx context.Context, tenantID string, now time.Time) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM auth_invites
		WHERE tenant_id = ? AND bootstrap = 1 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		tenantID, now.UnixNano()).Scan(&n); err != nil {
		return false, fmt.Errorf("auth invite live bootstrap: %w", err)
	}
	return n > 0, nil
}

// AuthInviteDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthInviteDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	ns := before.UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM auth_invites WHERE expires_at < ? OR redeemed_at < ? OR revoked_at < ?`, ns, ns, ns)
	if err != nil {
		return 0, fmt.Errorf("auth invite delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- resets ----------------------------------------------------------------

const resetCols = `reset_id, code_hash, gate, tenant_id, subject, created_by, created_at, expires_at, used_at`

func scanReset(row gateScanner) (store.AuthResetRow, error) {
	var r store.AuthResetRow
	var created, expires int64
	var used sql.NullInt64
	if err := row.Scan(&r.ResetID, &r.CodeHash, &r.Gate, &r.TenantID, &r.Subject, &r.CreatedBy,
		&created, &expires, &used); err != nil {
		return store.AuthResetRow{}, err
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.ExpiresAt = time.Unix(0, expires).UTC()
	r.UsedAt = gateTime(used)
	return r, nil
}

// supersedeResets marks the subject's live resets used at `at`.
func supersedeResets(ctx context.Context, tx *sql.Tx, tenantID, subject string, at time.Time) error {
	ns := at.UnixNano()
	if _, err := tx.ExecContext(ctx, `
		UPDATE auth_resets SET used_at = ?
		WHERE tenant_id = ? AND subject = ? AND used_at IS NULL AND expires_at > ?`,
		ns, tenantID, subject, ns); err != nil {
		return fmt.Errorf("auth reset supersede: %w", err)
	}
	return nil
}

// AuthResetCreate implements store.PasswordGateStore.
func (s *Store) AuthResetCreate(ctx context.Context, row store.AuthResetRow) error {
	created := gateStamp(row.CreatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := supersedeResets(ctx, tx, row.TenantID, row.Subject, created); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_resets (`+resetCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ResetID, row.CodeHash, row.Gate, row.TenantID, row.Subject, row.CreatedBy,
		created.UnixNano(), row.ExpiresAt.UnixNano(), gateNanos(row.UsedAt)); err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "auth_reset", ID: row.ResetID}
		}
		return fmt.Errorf("auth reset create: %w", err)
	}
	return tx.Commit()
}

// AuthResetGetByCode implements store.PasswordGateStore.
func (s *Store) AuthResetGetByCode(ctx context.Context, codeHash string) (store.AuthResetRow, error) {
	r, err := scanReset(s.db.QueryRowContext(ctx, `SELECT `+resetCols+` FROM auth_resets WHERE code_hash = ?`, codeHash))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthResetRow{}, &store.ErrNotFound{Kind: "auth_reset"}
	}
	if err != nil {
		return store.AuthResetRow{}, fmt.Errorf("auth reset get: %w", err)
	}
	return r, nil
}

// AuthResetComplete implements store.PasswordGateStore.
func (s *Store) AuthResetComplete(ctx context.Context, c store.AuthResetCompletion) error {
	now := gateStamp(c.Now)
	ns := now.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE auth_resets SET used_at = ?
		WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`, ns, c.CodeHash, ns)
	if err != nil {
		return fmt.Errorf("auth reset complete: consume: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return &store.ErrNotFound{Kind: "auth_reset"}
	}
	r, err := scanReset(tx.QueryRowContext(ctx, `SELECT `+resetCols+` FROM auth_resets WHERE code_hash = ?`, c.CodeHash))
	if err != nil {
		return fmt.Errorf("auth reset complete: read: %w", err)
	}
	// The credential must be the reset subject's: a mismatch is a caller bug
	// that would set another user's password.
	if c.Credential.TenantID != r.TenantID || c.Credential.Subject != r.Subject {
		return fmt.Errorf("auth reset complete: credential is not for the reset's user")
	}
	if err := supersedeResets(ctx, tx, r.TenantID, r.Subject, now); err != nil {
		return err
	}
	if err := putCredential(ctx, tx, c.Credential); err != nil {
		return err
	}
	if err := writeSecondFactor(ctx, tx, r.TenantID, r.Subject, c.SecondFactor, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AuthResetDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthResetDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	ns := before.UnixNano()
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_resets WHERE expires_at < ? OR used_at < ?`, ns, ns)
	if err != nil {
		return 0, fmt.Errorf("auth reset delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- throttle --------------------------------------------------------------

// AuthThrottleGet implements store.PasswordGateStore. The failure count is
// zero once the window has ended, but a block that outlasts its window is
// still returned: a blocked key must not read as unblocked early.
func (s *Store) AuthThrottleGet(ctx context.Context, key string, now time.Time) (int, time.Time, error) {
	var n int
	var windowEnd int64
	var blocked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT failures, window_end, blocked_until FROM auth_throttle WHERE throttle_key = ?`, key).
		Scan(&n, &windowEnd, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("auth throttle get: %w", err)
	}
	if windowEnd <= now.UnixNano() {
		n = 0
	}
	return n, gateActiveAt(gateTime(blocked), now), nil
}

// AuthThrottleRecordFailure implements store.PasswordGateStore.
func (s *Store) AuthThrottleRecordFailure(ctx context.Context, key string, now time.Time, window time.Duration, blockFor func(failures int) time.Duration) (int, time.Time, error) {
	ns := now.UnixNano()
	end := now.Add(window).UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Every SET expression reads the row as it was before the update, so the
	// window test is the same in each.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_throttle (throttle_key, failures, window_start, window_end, blocked_until)
		VALUES (?1, 1, ?2, ?3, NULL)
		ON CONFLICT (throttle_key) DO UPDATE SET
			failures     = CASE WHEN auth_throttle.window_end <= ?2 THEN 1  ELSE auth_throttle.failures + 1 END,
			window_start = CASE WHEN auth_throttle.window_end <= ?2 THEN ?2 ELSE auth_throttle.window_start END,
			window_end   = CASE WHEN auth_throttle.window_end <= ?2 THEN ?3 ELSE auth_throttle.window_end END`,
		key, ns, end); err != nil {
		return 0, time.Time{}, fmt.Errorf("auth throttle record failure: %w", err)
	}
	var n int
	var blocked sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT failures, blocked_until FROM auth_throttle WHERE throttle_key = ?`, key).
		Scan(&n, &blocked); err != nil {
		return 0, time.Time{}, fmt.Errorf("auth throttle record failure: read: %w", err)
	}
	block := gateTime(blocked)
	if d := blockFor(n); d > 0 {
		if next := gateLaterLock(block, now.Add(d)); !next.Equal(block) {
			if _, err := tx.ExecContext(ctx, `UPDATE auth_throttle SET blocked_until = ? WHERE throttle_key = ?`, next.UnixNano(), key); err != nil {
				return 0, time.Time{}, fmt.Errorf("auth throttle block: %w", err)
			}
			block = next
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, time.Time{}, err
	}
	return n, gateActiveAt(block, now), nil
}

// AuthThrottleClear implements store.PasswordGateStore.
func (s *Store) AuthThrottleClear(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_throttle WHERE throttle_key = ?`, key); err != nil {
		return fmt.Errorf("auth throttle clear: %w", err)
	}
	return nil
}

// AuthThrottleDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthThrottleDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	ns := before.UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM auth_throttle
		WHERE window_end < ? AND (blocked_until IS NULL OR blocked_until < ?)`, ns, ns)
	if err != nil {
		return 0, fmt.Errorf("auth throttle delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- subject deletion ------------------------------------------------------

// AuthSubjectDelete implements store.PasswordGateStore. Invites go when they
// attach to the subject or were redeemed by it, so no row names the deleted
// user afterwards.
func (s *Store) AuthSubjectDelete(ctx context.Context, tenantID, subject string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM user_identities WHERE tenant_id = ?1 AND subject = ?2`,
		`DELETE FROM user_credentials WHERE tenant_id = ?1 AND subject = ?2`,
		`DELETE FROM user_totp WHERE tenant_id = ?1 AND subject = ?2`,
		`DELETE FROM user_recovery_codes WHERE tenant_id = ?1 AND subject = ?2`,
		`DELETE FROM auth_invites WHERE tenant_id = ?1 AND (subject = ?2 OR redeemed_subject = ?2)`,
		`DELETE FROM auth_resets WHERE tenant_id = ?1 AND subject = ?2`,
	} {
		if _, err := tx.ExecContext(ctx, q, tenantID, subject); err != nil {
			return fmt.Errorf("auth subject delete: %w", err)
		}
	}
	return tx.Commit()
}
