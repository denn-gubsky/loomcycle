package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// password_gate.go — RFC FA P2 password-gate state (migration 0111). Sibling
// of internal/store/sqlite/password_gate.go; both keep the contract documented
// on store.AuthIdentityStore / store.PasswordGateStore and asserted by
// internal/store/storetest/password_gate.go.

func gateUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// gateTime converts a scanned nullable timestamp to the zero-time convention.
func gateTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// gateStamp is t, or the current time when t is zero.
func gateStamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// gateLaterLock is the lock to keep: the existing one when it already ends after
// candidate, else candidate. A lock is never moved earlier.
func gateLaterLock(existing, candidate time.Time) time.Time {
	if existing.After(candidate) {
		return existing
	}
	return candidate
}

// gateActiveAt is t when it is still in the future at now, else the zero time, so
// a caller tests a lock with IsZero and never sees a stale one.
func gateActiveAt(t, now time.Time) time.Time {
	if t.After(now) {
		return t
	}
	return time.Time{}
}

// ---- identities ------------------------------------------------------------

const identityCols = `tenant_id, subject, gate, issuer, external_subject, email, email_verified, created_at, last_login_at`

func scanIdentity(row rowScanner) (store.UserIdentityRow, error) {
	var r store.UserIdentityRow
	var lastLogin *time.Time
	if err := row.Scan(&r.TenantID, &r.Subject, &r.Gate, &r.Issuer, &r.ExternalSubject,
		&r.Email, &r.EmailVerified, &r.CreatedAt, &lastLogin); err != nil {
		return store.UserIdentityRow{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.LastLoginAt = gateTime(lastLogin)
	return r, nil
}

func insertIdentity(ctx context.Context, q gateExecer, row store.UserIdentityRow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO user_identities (`+identityCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		row.TenantID, row.Subject, row.Gate, row.Issuer, row.ExternalSubject,
		row.Email, row.EmailVerified, gateStamp(row.CreatedAt), nullTime(row.LastLoginAt))
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "user_identity", ID: row.ExternalSubject}
		}
		return fmt.Errorf("user identity create: %w", err)
	}
	return nil
}

// gateExecer is the write surface shared by the pool and a transaction.
type gateExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// UserIdentityCreate implements store.AuthIdentityStore.
func (s *Store) UserIdentityCreate(ctx context.Context, row store.UserIdentityRow) error {
	return insertIdentity(ctx, s.pool, row)
}

// UserIdentityGet implements store.AuthIdentityStore.
func (s *Store) UserIdentityGet(ctx context.Context, gate, issuer, externalSubject string) (store.UserIdentityRow, error) {
	r, err := scanIdentity(s.pool.QueryRow(ctx, `
		SELECT `+identityCols+` FROM user_identities
		WHERE gate = $1 AND issuer = $2 AND external_subject = $3`, gate, issuer, externalSubject))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.UserIdentityRow{}, &store.ErrNotFound{Kind: "user_identity", ID: externalSubject}
	}
	if err != nil {
		return store.UserIdentityRow{}, fmt.Errorf("user identity get: %w", err)
	}
	return r, nil
}

// UserIdentityListSubject implements store.AuthIdentityStore.
func (s *Store) UserIdentityListSubject(ctx context.Context, tenantID, subject string) ([]store.UserIdentityRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+identityCols+` FROM user_identities
		WHERE tenant_id = $1 AND subject = $2
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
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_identities SET external_subject = $1
		WHERE tenant_id = $2 AND subject = $3 AND gate = $4 AND issuer = ''`,
		newExternalSubject, tenantID, subject, gate)
	if err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "user_identity", ID: newExternalSubject}
		}
		return fmt.Errorf("user identity rename: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "user_identity", ID: subject}
	}
	return nil
}

// UserIdentityTouchLogin implements store.AuthIdentityStore.
func (s *Store) UserIdentityTouchLogin(ctx context.Context, gate, issuer, externalSubject string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_identities SET last_login_at = $1
		WHERE gate = $2 AND issuer = $3 AND external_subject = $4`,
		gateStamp(at), gate, issuer, externalSubject)
	if err != nil {
		return fmt.Errorf("user identity touch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "user_identity", ID: externalSubject}
	}
	return nil
}

// ---- credentials -----------------------------------------------------------

// putCredential inserts or replaces a credential with its throttle reset.
func putCredential(ctx context.Context, q gateExecer, row store.UserCredentialRow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO user_credentials (tenant_id, subject, kind, phc, pepper_kid, updated_at, failed_count, locked_until)
		VALUES ($1, $2, $3, $4, $5, $6, 0, NULL)
		ON CONFLICT (tenant_id, subject, kind) DO UPDATE SET
			phc = EXCLUDED.phc, pepper_kid = EXCLUDED.pepper_kid, updated_at = EXCLUDED.updated_at,
			failed_count = 0, locked_until = NULL`,
		row.TenantID, row.Subject, row.Kind, row.PHC, row.PepperKID, gateStamp(row.UpdatedAt))
	if err != nil {
		return fmt.Errorf("user credential set: %w", err)
	}
	return nil
}

// UserCredentialGet implements store.PasswordGateStore.
func (s *Store) UserCredentialGet(ctx context.Context, tenantID, subject, kind string) (store.UserCredentialRow, error) {
	var r store.UserCredentialRow
	var locked *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, subject, kind, phc, pepper_kid, updated_at, failed_count, locked_until
		FROM user_credentials WHERE tenant_id = $1 AND subject = $2 AND kind = $3`,
		tenantID, subject, kind).Scan(
		&r.TenantID, &r.Subject, &r.Kind, &r.PHC, &r.PepperKID, &r.UpdatedAt, &r.FailedCount, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.UserCredentialRow{}, &store.ErrNotFound{Kind: "user_credential", ID: subject}
	}
	if err != nil {
		return store.UserCredentialRow{}, fmt.Errorf("user credential get: %w", err)
	}
	r.UpdatedAt = r.UpdatedAt.UTC()
	r.LockedUntil = gateTime(locked)
	return r, nil
}

// UserCredentialSet implements store.PasswordGateStore.
func (s *Store) UserCredentialSet(ctx context.Context, row store.UserCredentialRow) error {
	return putCredential(ctx, s.pool, row)
}

// UserCredentialRehash implements store.PasswordGateStore.
func (s *Store) UserCredentialRehash(ctx context.Context, tenantID, subject, kind, oldPHC, newPHC, pepperKID string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_credentials SET phc = $1, pepper_kid = $2, updated_at = $3
		WHERE tenant_id = $4 AND subject = $5 AND kind = $6 AND phc = $7`,
		newPHC, pepperKID, gateStamp(at), tenantID, subject, kind, oldPHC)
	if err != nil {
		return false, fmt.Errorf("user credential rehash: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UserCredentialRecordFailure implements store.PasswordGateStore. The
// increment's row lock serialises concurrent failures, so lockFor sees each
// count exactly once.
func (s *Store) UserCredentialRecordFailure(ctx context.Context, tenantID, subject, kind string, now time.Time, lockFor func(failures int) time.Duration) (int, time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var n int
	var locked *time.Time
	err = tx.QueryRow(ctx, `
		UPDATE user_credentials SET failed_count = failed_count + 1
		WHERE tenant_id = $1 AND subject = $2 AND kind = $3
		RETURNING failed_count, locked_until`, tenantID, subject, kind).Scan(&n, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, &store.ErrNotFound{Kind: "user_credential", ID: subject}
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("user credential record failure: %w", err)
	}
	lock := gateTime(locked)
	if d := lockFor(n); d > 0 {
		if next := gateLaterLock(lock, now.Add(d)); !next.Equal(lock) {
			if _, err := tx.Exec(ctx, `
				UPDATE user_credentials SET locked_until = $1
				WHERE tenant_id = $2 AND subject = $3 AND kind = $4`, next, tenantID, subject, kind); err != nil {
				return 0, time.Time{}, fmt.Errorf("user credential lock: %w", err)
			}
			lock = next
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, err
	}
	return n, gateActiveAt(lock, now), nil
}

// UserCredentialRecordSuccess implements store.PasswordGateStore. A missing
// credential is not an error: there is nothing to clear.
func (s *Store) UserCredentialRecordSuccess(ctx context.Context, tenantID, subject, kind string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE user_credentials SET failed_count = 0, locked_until = NULL
		WHERE tenant_id = $1 AND subject = $2 AND kind = $3`, tenantID, subject, kind); err != nil {
		return fmt.Errorf("user credential record success: %w", err)
	}
	return nil
}

// UserCredentialDelete implements store.PasswordGateStore.
func (s *Store) UserCredentialDelete(ctx context.Context, tenantID, subject, kind string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_credentials WHERE tenant_id = $1 AND subject = $2 AND kind = $3`,
		tenantID, subject, kind)
	if err != nil {
		return false, fmt.Errorf("user credential delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---- TOTP + recovery codes -------------------------------------------------

func putTOTP(ctx context.Context, q gateExecer, row store.UserTOTPRow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO user_totp (tenant_id, subject, sealed, created_at, confirmed_at, last_step)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, subject) DO UPDATE SET
			sealed = EXCLUDED.sealed, created_at = EXCLUDED.created_at,
			confirmed_at = EXCLUDED.confirmed_at, last_step = EXCLUDED.last_step`,
		row.TenantID, row.Subject, row.Sealed, gateStamp(row.CreatedAt), nullTime(row.ConfirmedAt), row.LastStep)
	if err != nil {
		return fmt.Errorf("user totp put: %w", err)
	}
	return nil
}

func replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, tenantID, subject string, hashes []string, at time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE tenant_id = $1 AND subject = $2`, tenantID, subject); err != nil {
		return fmt.Errorf("user recovery codes clear: %w", err)
	}
	at = gateStamp(at)
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_recovery_codes (tenant_id, subject, code_hash, created_at) VALUES ($1, $2, $3, $4)`,
			tenantID, subject, h, at); err != nil {
			return fmt.Errorf("user recovery codes insert: %w", err)
		}
	}
	return nil
}

// writeSecondFactor stores a redemption's or a reset's TOTP enrolment. The
// TOTP row is written under (tenantID, subject) — the row being enrolled —
// whatever the caller put in it, since the seal is bound to that pair.
func writeSecondFactor(ctx context.Context, tx pgx.Tx, tenantID, subject string, sf *store.AuthSecondFactor, at time.Time) error {
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
	var confirmed *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, subject, sealed, created_at, confirmed_at, last_step
		FROM user_totp WHERE tenant_id = $1 AND subject = $2`, tenantID, subject).Scan(
		&r.TenantID, &r.Subject, &r.Sealed, &r.CreatedAt, &confirmed, &r.LastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.UserTOTPRow{}, &store.ErrNotFound{Kind: "user_totp", ID: subject}
	}
	if err != nil {
		return store.UserTOTPRow{}, fmt.Errorf("user totp get: %w", err)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.ConfirmedAt = gateTime(confirmed)
	return r, nil
}

// UserTOTPPut implements store.PasswordGateStore.
func (s *Store) UserTOTPPut(ctx context.Context, row store.UserTOTPRow) error {
	return putTOTP(ctx, s.pool, row)
}

// UserTOTPUseStep implements store.PasswordGateStore. The step comparison is
// in the WHERE clause, so two logins presenting the same code race on one row
// update and exactly one wins.
func (s *Store) UserTOTPUseStep(ctx context.Context, tenantID, subject string, step int64, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_totp SET last_step = $1, confirmed_at = COALESCE(confirmed_at, $2)
		WHERE tenant_id = $3 AND subject = $4 AND last_step < $1`,
		step, gateStamp(at), tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("user totp use step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UserTOTPDelete implements store.PasswordGateStore. It reports whether any
// row (the TOTP row or a recovery code) was removed.
func (s *Store) UserTOTPDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t1, err := tx.Exec(ctx, `DELETE FROM user_totp WHERE tenant_id = $1 AND subject = $2`, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("user totp delete: %w", err)
	}
	t2, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE tenant_id = $1 AND subject = $2`, tenantID, subject)
	if err != nil {
		return false, fmt.Errorf("user recovery codes delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return t1.RowsAffected()+t2.RowsAffected() > 0, nil
}

// UserRecoveryCodesReplace implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodesReplace(ctx context.Context, tenantID, subject string, hashes []string, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := replaceRecoveryCodes(ctx, tx, tenantID, subject, hashes, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UserRecoveryCodeConsume implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodeConsume(ctx context.Context, tenantID, subject, hash string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_recovery_codes SET used_at = $1
		WHERE tenant_id = $2 AND subject = $3 AND code_hash = $4 AND used_at IS NULL`,
		gateStamp(at), tenantID, subject, hash)
	if err != nil {
		return false, fmt.Errorf("user recovery code consume: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UserRecoveryCodesRemaining implements store.PasswordGateStore.
func (s *Store) UserRecoveryCodesRemaining(ctx context.Context, tenantID, subject string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM user_recovery_codes
		WHERE tenant_id = $1 AND subject = $2 AND used_at IS NULL`, tenantID, subject).Scan(&n); err != nil {
		return 0, fmt.Errorf("user recovery codes remaining: %w", err)
	}
	return n, nil
}

// ---- invites ---------------------------------------------------------------

const inviteCols = `invite_id, code_hash, gate, tenant_id, subject, access_mode, groups, login, email, return_to,
	bootstrap, created_by, created_at, expires_at, redeemed_at, redeemed_subject, revoked_at`

func scanInvite(row rowScanner) (store.AuthInviteRow, error) {
	var r store.AuthInviteRow
	var groups string
	var redeemed, revoked *time.Time
	if err := row.Scan(&r.InviteID, &r.CodeHash, &r.Gate, &r.TenantID, &r.Subject, &r.AccessMode, &groups,
		&r.Login, &r.Email, &r.ReturnTo, &r.Bootstrap, &r.CreatedBy, &r.CreatedAt, &r.ExpiresAt,
		&redeemed, &r.RedeemedSubject, &revoked); err != nil {
		return store.AuthInviteRow{}, err
	}
	if err := json.Unmarshal([]byte(groups), &r.Groups); err != nil {
		return store.AuthInviteRow{}, fmt.Errorf("invite %s groups: %w", r.InviteID, err)
	}
	if len(r.Groups) == 0 {
		r.Groups = nil
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.ExpiresAt = r.ExpiresAt.UTC()
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
	_, err = s.pool.Exec(ctx, `
		INSERT INTO auth_invites (`+inviteCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		row.InviteID, row.CodeHash, row.Gate, row.TenantID, row.Subject, row.AccessMode, string(gj),
		row.Login, row.Email, row.ReturnTo, row.Bootstrap, row.CreatedBy, gateStamp(row.CreatedAt), row.ExpiresAt,
		nullTime(row.RedeemedAt), row.RedeemedSubject, nullTime(row.RevokedAt))
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
func (s *Store) authInviteGetWhere(ctx context.Context, column, arg, missID string) (store.AuthInviteRow, error) {
	r, err := scanInvite(s.pool.QueryRow(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE `+column+` = $1`, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AuthInviteRow{}, &store.ErrNotFound{Kind: "auth_invite", ID: missID}
	}
	if err != nil {
		return store.AuthInviteRow{}, fmt.Errorf("auth invite get: %w", err)
	}
	return r, nil
}

// AuthInviteGet implements store.PasswordGateStore.
func (s *Store) AuthInviteGet(ctx context.Context, inviteID string) (store.AuthInviteRow, error) {
	return s.authInviteGetWhere(ctx, "invite_id", inviteID, inviteID)
}

// AuthInviteGetByCode implements store.PasswordGateStore.
func (s *Store) AuthInviteGetByCode(ctx context.Context, codeHash string) (store.AuthInviteRow, error) {
	return s.authInviteGetWhere(ctx, "code_hash", codeHash, "")
}

// AuthInviteList implements store.PasswordGateStore. The tenant filter is
// unconditional: "" is the shared tenant, not a wildcard.
func (s *Store) AuthInviteList(ctx context.Context, gate, tenantID string) ([]store.AuthInviteRow, error) {
	return s.listInvites(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE gate = $1 AND tenant_id = $2
		ORDER BY created_at DESC, invite_id DESC`, gate, tenantID)
}

// AuthInviteListAllTenants implements store.PasswordGateStore.
func (s *Store) AuthInviteListAllTenants(ctx context.Context, gate string) ([]store.AuthInviteRow, error) {
	return s.listInvites(ctx, `SELECT `+inviteCols+` FROM auth_invites WHERE gate = $1
		ORDER BY created_at DESC, invite_id DESC`, gate)
}

func (s *Store) listInvites(ctx context.Context, q string, args ...any) ([]store.AuthInviteRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
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
	at = gateStamp(at)
	tag, err := s.pool.Exec(ctx, `
		UPDATE auth_invites SET revoked_at = $1
		WHERE invite_id = $2 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > $1`, at, inviteID)
	if err != nil {
		return false, fmt.Errorf("auth invite revoke: %w", err)
	}
	return tag.RowsAffected() == 1, nil
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
// and leaves the invite live.
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Consume first: the row lock this takes makes a concurrent redemption of
	// the same code wait, then find it no longer live.
	inv, err := scanInvite(tx.QueryRow(ctx, `
		UPDATE auth_invites SET redeemed_at = $1, redeemed_subject = $2
		WHERE code_hash = $3 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > $1
		RETURNING `+inviteCols, now, red.Identity.Subject, red.CodeHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.ErrNotFound{Kind: "auth_invite"}
	}
	if err != nil {
		return fmt.Errorf("auth invite redeem: consume: %w", err)
	}
	if err := checkRedemption(inv, red); err != nil {
		return err
	}

	if inv.Bootstrap {
		// Two bootstrap invites of one tenant redeemed at once must not both
		// see it empty. The transaction-scoped lock serialises them; the count
		// below runs after the lock is held, so under READ COMMITTED it sees
		// the user the other one committed.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('loomcycle/auth-bootstrap/' || $1::text, 0))`, inv.TenantID); err != nil {
			return fmt.Errorf("auth invite redeem: bootstrap lock: %w", err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE tenant_id = $1)
			+ (SELECT count(*) FROM tenant_bootstraps WHERE tenant_id = $1)`, inv.TenantID).Scan(&n); err != nil {
			return fmt.Errorf("auth invite redeem: count users: %w", err)
		}
		if n > 0 {
			return &store.ErrConflict{Kind: "tenant_not_empty", ID: inv.TenantID}
		}
	}

	if red.User != nil {
		u := *red.User
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_bootstraps (tenant_id, at, via) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id) DO NOTHING`, u.TenantID, now, "invite:"+inv.Gate); err != nil {
			return fmt.Errorf("auth invite redeem: bootstrap marker: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (tenant_id, subject, display_name, access_mode, status, created_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			u.TenantID, u.Subject, u.DisplayName, u.AccessMode, u.Status, gateStamp(u.CreatedAt), u.CreatedBy); err != nil {
			if gateUniqueViolation(err) {
				return &store.ErrConflict{Kind: "user", ID: u.Subject}
			}
			return fmt.Errorf("auth invite redeem: create user: %w", err)
		}
	} else {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE tenant_id = $1 AND subject = $2`,
			red.Identity.TenantID, red.Identity.Subject).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
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
	return tx.Commit(ctx)
}

// TenantBootstrapped implements store.PasswordGateStore.
func (s *Store) TenantBootstrapped(ctx context.Context, tenantID string) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_bootstraps WHERE tenant_id = $1)
		OR EXISTS (SELECT 1 FROM users WHERE tenant_id = $1)`, tenantID).Scan(&ok); err != nil {
		return false, fmt.Errorf("tenant bootstrapped: %w", err)
	}
	return ok, nil
}

// AuthInviteLiveBootstrap implements store.PasswordGateStore.
func (s *Store) AuthInviteLiveBootstrap(ctx context.Context, tenantID string, now time.Time) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auth_invites
			WHERE tenant_id = $1 AND bootstrap AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > $2)`,
		tenantID, now).Scan(&ok); err != nil {
		return false, fmt.Errorf("auth invite live bootstrap: %w", err)
	}
	return ok, nil
}

// AuthInviteDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthInviteDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM auth_invites WHERE expires_at < $1 OR redeemed_at < $1 OR revoked_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("auth invite delete expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- resets ----------------------------------------------------------------

const resetCols = `reset_id, code_hash, gate, tenant_id, subject, created_by, created_at, expires_at, used_at`

func scanReset(row rowScanner) (store.AuthResetRow, error) {
	var r store.AuthResetRow
	var used *time.Time
	if err := row.Scan(&r.ResetID, &r.CodeHash, &r.Gate, &r.TenantID, &r.Subject, &r.CreatedBy,
		&r.CreatedAt, &r.ExpiresAt, &used); err != nil {
		return store.AuthResetRow{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.ExpiresAt = r.ExpiresAt.UTC()
	r.UsedAt = gateTime(used)
	return r, nil
}

// supersedeResets marks the subject's live resets used at `at`.
func supersedeResets(ctx context.Context, tx pgx.Tx, tenantID, subject string, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE auth_resets SET used_at = $1
		WHERE tenant_id = $2 AND subject = $3 AND used_at IS NULL AND expires_at > $1`,
		at, tenantID, subject); err != nil {
		return fmt.Errorf("auth reset supersede: %w", err)
	}
	return nil
}

// AuthResetCreate implements store.PasswordGateStore.
func (s *Store) AuthResetCreate(ctx context.Context, row store.AuthResetRow) error {
	created := gateStamp(row.CreatedAt)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := supersedeResets(ctx, tx, row.TenantID, row.Subject, created); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_resets (`+resetCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		row.ResetID, row.CodeHash, row.Gate, row.TenantID, row.Subject, row.CreatedBy, created, row.ExpiresAt,
		nullTime(row.UsedAt)); err != nil {
		if gateUniqueViolation(err) {
			return &store.ErrConflict{Kind: "auth_reset", ID: row.ResetID}
		}
		return fmt.Errorf("auth reset create: %w", err)
	}
	return tx.Commit(ctx)
}

// AuthResetGetByCode implements store.PasswordGateStore.
func (s *Store) AuthResetGetByCode(ctx context.Context, codeHash string) (store.AuthResetRow, error) {
	r, err := scanReset(s.pool.QueryRow(ctx, `SELECT `+resetCols+` FROM auth_resets WHERE code_hash = $1`, codeHash))
	if errors.Is(err, pgx.ErrNoRows) {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := scanReset(tx.QueryRow(ctx, `
		UPDATE auth_resets SET used_at = $1
		WHERE code_hash = $2 AND used_at IS NULL AND expires_at > $1
		RETURNING `+resetCols, now, c.CodeHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.ErrNotFound{Kind: "auth_reset"}
	}
	if err != nil {
		return fmt.Errorf("auth reset complete: consume: %w", err)
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
	return tx.Commit(ctx)
}

// AuthResetDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthResetDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_resets WHERE expires_at < $1 OR used_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("auth reset delete expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- throttle --------------------------------------------------------------

// AuthThrottleGet implements store.PasswordGateStore. The failure count is
// zero once the window has ended, but a block that outlasts its window is
// still returned: a blocked key must not read as unblocked early.
func (s *Store) AuthThrottleGet(ctx context.Context, key string, now time.Time) (int, time.Time, error) {
	var n int
	var windowEnd time.Time
	var blocked *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT failures, window_end, blocked_until FROM auth_throttle WHERE throttle_key = $1`, key).
		Scan(&n, &windowEnd, &blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("auth throttle get: %w", err)
	}
	if !windowEnd.After(now) {
		n = 0
	}
	return n, gateActiveAt(gateTime(blocked), now), nil
}

// AuthThrottleRecordFailure implements store.PasswordGateStore. The upsert
// takes the row lock, so concurrent failures of one key count one at a time.
func (s *Store) AuthThrottleRecordFailure(ctx context.Context, key string, now time.Time, window time.Duration, blockFor func(failures int) time.Duration) (int, time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var n int
	var blocked *time.Time
	// Every SET expression reads the row as it was before the update, so the
	// window test is the same in each.
	err = tx.QueryRow(ctx, `
		INSERT INTO auth_throttle (throttle_key, failures, window_start, window_end, blocked_until)
		VALUES ($1, 1, $2, $3, NULL)
		ON CONFLICT (throttle_key) DO UPDATE SET
			failures     = CASE WHEN auth_throttle.window_end <= $2 THEN 1  ELSE auth_throttle.failures + 1 END,
			window_start = CASE WHEN auth_throttle.window_end <= $2 THEN $2 ELSE auth_throttle.window_start END,
			window_end   = CASE WHEN auth_throttle.window_end <= $2 THEN $3 ELSE auth_throttle.window_end END
		RETURNING failures, blocked_until`, key, now, now.Add(window)).Scan(&n, &blocked)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("auth throttle record failure: %w", err)
	}
	block := gateTime(blocked)
	if d := blockFor(n); d > 0 {
		if next := gateLaterLock(block, now.Add(d)); !next.Equal(block) {
			if _, err := tx.Exec(ctx, `UPDATE auth_throttle SET blocked_until = $1 WHERE throttle_key = $2`, next, key); err != nil {
				return 0, time.Time{}, fmt.Errorf("auth throttle block: %w", err)
			}
			block = next
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, err
	}
	return n, gateActiveAt(block, now), nil
}

// AuthThrottleClear implements store.PasswordGateStore.
func (s *Store) AuthThrottleClear(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_throttle WHERE throttle_key = $1`, key); err != nil {
		return fmt.Errorf("auth throttle clear: %w", err)
	}
	return nil
}

// AuthThrottleDeleteExpired implements store.PasswordGateStore.
func (s *Store) AuthThrottleDeleteExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM auth_throttle
		WHERE window_end < $1 AND (blocked_until IS NULL OR blocked_until < $1)`, before)
	if err != nil {
		return 0, fmt.Errorf("auth throttle delete expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- subject deletion ------------------------------------------------------

// AuthSubjectDelete implements store.PasswordGateStore. Invites go when they
// attach to the subject or were redeemed by it, so no row names the deleted
// user afterwards.
func (s *Store) AuthSubjectDelete(ctx context.Context, tenantID, subject string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM user_identities WHERE tenant_id = $1 AND subject = $2`,
		`DELETE FROM user_credentials WHERE tenant_id = $1 AND subject = $2`,
		`DELETE FROM user_totp WHERE tenant_id = $1 AND subject = $2`,
		`DELETE FROM user_recovery_codes WHERE tenant_id = $1 AND subject = $2`,
		`DELETE FROM auth_invites WHERE tenant_id = $1 AND (subject = $2 OR redeemed_subject = $2)`,
		`DELETE FROM auth_resets WHERE tenant_id = $1 AND subject = $2`,
	} {
		if _, err := tx.Exec(ctx, q, tenantID, subject); err != nil {
			return fmt.Errorf("auth subject delete: %w", err)
		}
	}
	return tx.Commit(ctx)
}
