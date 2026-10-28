package sqlshared

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FL app sign-ins: authorization codes, refresh-token families and their
// refresh tokens (store.OAuthStore). Every single-use step is one UPDATE whose
// own WHERE decides the winner, so two concurrent presentations of one code or
// one refresh token never both pass.

const oauthCodeCols = `code_hash, client_id, redirect_uri, code_challenge, tenant_id, subject, gate, amr,
	session_hash, scope_cap, restrictions, created_at, expires_at, consumed_at, family_id`

const oauthFamilyCols = `family_id, client_id, tenant_id, subject, gate, amr, scope_cap, restrictions,
	created_at, last_used_at, idle_expires_at, absolute_expires_at, revoked_at, revoke_reason`

func jsonList(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func scanList(s string, dst *[]string) error {
	if s == "" {
		*dst = nil
		return nil
	}
	return json.Unmarshal([]byte(s), dst)
}

// OAuthCodeCreate inserts an authorization code.
func OAuthCodeCreate(ctx context.Context, db DB, row store.OAuthCodeRow) error {
	if row.CodeHash == "" || row.ClientID == "" || row.RedirectURI == "" || row.CodeChallenge == "" ||
		row.TenantID == "" || row.Subject == "" || row.Gate == "" || row.ExpiresAt.IsZero() {
		return errors.New("oauth_code: code_hash, client_id, redirect_uri, code_challenge, tenant_id, subject, gate and expires_at are required")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	amr, err := jsonList(row.AMR)
	if err != nil {
		return err
	}
	caps, err := jsonList(row.ScopeCap)
	if err != nil {
		return err
	}
	d := db.Dialect()
	_, err = db.Exec(ctx, `INSERT INTO oauth_codes (`+oauthCodeCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')`,
		row.CodeHash, row.ClientID, row.RedirectURI, row.CodeChallenge, row.TenantID, row.Subject, row.Gate, amr,
		row.SessionHash, caps, row.RestrictionsJSON, d.TimeArg(row.CreatedAt), d.TimeArg(row.ExpiresAt))
	if IsUniqueViolation(err) {
		return &store.ErrConflict{Kind: "oauth_code", ID: "<hash>"}
	}
	if err != nil {
		return fmt.Errorf("oauth_code insert: %w", err)
	}
	return nil
}

func scanOAuthCode(d Dialect, r Row) (store.OAuthCodeRow, error) {
	var (
		out       store.OAuthCodeRow
		amr, caps string
	)
	if err := r.Scan(&out.CodeHash, &out.ClientID, &out.RedirectURI, &out.CodeChallenge, &out.TenantID, &out.Subject,
		&out.Gate, &amr, &out.SessionHash, &caps, &out.RestrictionsJSON, d.TimeDest(&out.CreatedAt),
		d.TimeDest(&out.ExpiresAt), &nullTimeDest{&out.ConsumedAt}, &out.FamilyID); err != nil {
		return store.OAuthCodeRow{}, err
	}
	if err := scanList(amr, &out.AMR); err != nil {
		return store.OAuthCodeRow{}, fmt.Errorf("oauth_code: amr: %w", err)
	}
	if err := scanList(caps, &out.ScopeCap); err != nil {
		return store.OAuthCodeRow{}, fmt.Errorf("oauth_code: scope_cap: %w", err)
	}
	return out, nil
}

// OAuthCodeConsume marks the code used and stamps familyID on it. The UPDATE's
// WHERE (consumed_at IS NULL) picks the one exchange that wins; every other
// reads the row back as already used, carrying the winner's family.
func OAuthCodeConsume(ctx context.Context, db DB, codeHash, familyID string, now time.Time) (store.OAuthCodeRow, bool, error) {
	var (
		out   store.OAuthCodeRow
		first bool
	)
	err := db.InTx(ctx, func(tx Querier) error {
		d := tx.Dialect()
		n, err := tx.Exec(ctx, `UPDATE oauth_codes SET consumed_at = ?, family_id = ?
			WHERE code_hash = ? AND consumed_at IS NULL`, d.TimeArg(now), familyID, codeHash)
		if err != nil {
			return err
		}
		first = n == 1
		out, err = scanOAuthCode(d, tx.QueryRow(ctx, `SELECT `+oauthCodeCols+` FROM oauth_codes WHERE code_hash = ?`, codeHash))
		return NotFound(err, "oauth_code", "<hash>")
	})
	if err != nil {
		return store.OAuthCodeRow{}, false, err
	}
	return out, first, nil
}

// OAuthCodeInvalidateClient marks every unused code of clientID used.
func OAuthCodeInvalidateClient(ctx context.Context, db DB, clientID string, at time.Time) (int, error) {
	n, err := db.Exec(ctx, `UPDATE oauth_codes SET consumed_at = ? WHERE client_id = ? AND consumed_at IS NULL`,
		db.Dialect().TimeArg(at), clientID)
	return int(n), err
}

// OAuthFamilyCreate inserts a family; a row of that id (a tombstone included)
// is a conflict.
func OAuthFamilyCreate(ctx context.Context, db DB, row store.OAuthFamilyRow) error {
	if row.FamilyID == "" || row.ClientID == "" || row.TenantID == "" || row.Subject == "" ||
		row.IdleExpiresAt.IsZero() || row.AbsoluteExpiresAt.IsZero() {
		return errors.New("oauth_family: family_id, client_id, tenant_id, subject and both expiry times are required")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.LastUsedAt.IsZero() {
		row.LastUsedAt = row.CreatedAt
	}
	amr, err := jsonList(row.AMR)
	if err != nil {
		return err
	}
	caps, err := jsonList(row.ScopeCap)
	if err != nil {
		return err
	}
	d := db.Dialect()
	_, err = db.Exec(ctx, `INSERT INTO oauth_refresh_families (`+oauthFamilyCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')`,
		row.FamilyID, row.ClientID, row.TenantID, row.Subject, row.Gate, amr, caps, row.RestrictionsJSON,
		d.TimeArg(row.CreatedAt), d.TimeArg(row.LastUsedAt), d.TimeArg(row.IdleExpiresAt), d.TimeArg(row.AbsoluteExpiresAt))
	if IsUniqueViolation(err) {
		return &store.ErrConflict{Kind: "oauth_family", ID: row.FamilyID}
	}
	if err != nil {
		return fmt.Errorf("oauth_family insert: %w", err)
	}
	return nil
}

// OAuthFamilyGet returns one family.
func OAuthFamilyGet(ctx context.Context, db DB, familyID string) (store.OAuthFamilyRow, error) {
	d := db.Dialect()
	var (
		out       store.OAuthFamilyRow
		amr, caps string
	)
	err := db.QueryRow(ctx, `SELECT `+oauthFamilyCols+` FROM oauth_refresh_families WHERE family_id = ?`, familyID).Scan(
		&out.FamilyID, &out.ClientID, &out.TenantID, &out.Subject, &out.Gate, &amr, &caps, &out.RestrictionsJSON,
		d.TimeDest(&out.CreatedAt), d.TimeDest(&out.LastUsedAt), d.TimeDest(&out.IdleExpiresAt),
		d.TimeDest(&out.AbsoluteExpiresAt), &nullTimeDest{&out.RevokedAt}, &out.RevokeReason)
	if err != nil {
		return store.OAuthFamilyRow{}, NotFound(err, "oauth_family", familyID)
	}
	if err := scanList(amr, &out.AMR); err != nil {
		return store.OAuthFamilyRow{}, fmt.Errorf("oauth_family: amr: %w", err)
	}
	if err := scanList(caps, &out.ScopeCap); err != nil {
		return store.OAuthFamilyRow{}, fmt.Errorf("oauth_family: scope_cap: %w", err)
	}
	return out, nil
}

// OAuthFamilyTouch records a refresh on a live family.
func OAuthFamilyTouch(ctx context.Context, db DB, familyID string, lastUsed, idleExpires time.Time) error {
	d := db.Dialect()
	_, err := db.Exec(ctx, `UPDATE oauth_refresh_families SET last_used_at = ?, idle_expires_at = ?
		WHERE family_id = ? AND revoked_at IS NULL`, d.TimeArg(lastUsed), d.TimeArg(idleExpires), familyID)
	return err
}

// OAuthFamilyRevoke revokes a family, writing a revoked tombstone when it does
// not exist yet: a code replayed while its first exchange is still running
// revokes a family that exchange has not created, and the tombstone makes that
// create fail rather than leave the family live.
func OAuthFamilyRevoke(ctx context.Context, db DB, familyID, reason string, at time.Time) (bool, error) {
	if familyID == "" {
		return false, errors.New("oauth_family revoke: family_id required")
	}
	var revoked bool
	err := db.InTx(ctx, func(tx Querier) error {
		d := tx.Dialect()
		t := d.TimeArg(at)
		n, err := tx.Exec(ctx, `UPDATE oauth_refresh_families SET revoked_at = ?, revoke_reason = ?
			WHERE family_id = ? AND revoked_at IS NULL`, t, reason, familyID)
		if err != nil {
			return err
		}
		if n == 1 {
			revoked = true
			return nil
		}
		n, err = tx.Exec(ctx, `INSERT INTO oauth_refresh_families (family_id, created_at, last_used_at, idle_expires_at,
			absolute_expires_at, revoked_at, revoke_reason) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (family_id) DO NOTHING`, familyID, t, t, t, t, t, reason)
		revoked = n == 1
		return err
	})
	return revoked, err
}

// OAuthFamilyRevokeClient revokes every live family of clientID.
func OAuthFamilyRevokeClient(ctx context.Context, db DB, clientID, reason string, at time.Time) ([]string, error) {
	var ids []string
	err := db.InTx(ctx, func(tx Querier) error {
		rows, err := tx.Query(ctx, `SELECT family_id FROM oauth_refresh_families WHERE client_id = ? AND revoked_at IS NULL`, clientID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_refresh_families SET revoked_at = ?, revoke_reason = ?
			WHERE client_id = ? AND revoked_at IS NULL`, tx.Dialect().TimeArg(at), reason, clientID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// OAuthRefreshCreate inserts a refresh token's hash.
func OAuthRefreshCreate(ctx context.Context, db DB, row store.OAuthRefreshRow) error {
	if row.TokenHash == "" || row.FamilyID == "" {
		return errors.New("oauth_refresh: token_hash and family_id are required")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	_, err := db.Exec(ctx, `INSERT INTO oauth_refresh_tokens (token_hash, family_id, created_at, used_at) VALUES (?, ?, ?, NULL)`,
		row.TokenHash, row.FamilyID, db.Dialect().TimeArg(row.CreatedAt))
	if IsUniqueViolation(err) {
		return &store.ErrConflict{Kind: "oauth_refresh", ID: "<hash>"}
	}
	if err != nil {
		return fmt.Errorf("oauth_refresh insert: %w", err)
	}
	return nil
}

// OAuthRefreshUse marks a refresh token used, reporting whether this call was
// its first use.
func OAuthRefreshUse(ctx context.Context, db DB, tokenHash string, now time.Time) (store.OAuthRefreshRow, bool, error) {
	var (
		out   store.OAuthRefreshRow
		first bool
	)
	err := db.InTx(ctx, func(tx Querier) error {
		d := tx.Dialect()
		n, err := tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
			d.TimeArg(now), tokenHash)
		if err != nil {
			return err
		}
		first = n == 1
		err = tx.QueryRow(ctx, `SELECT token_hash, family_id, created_at, used_at FROM oauth_refresh_tokens WHERE token_hash = ?`,
			tokenHash).Scan(&out.TokenHash, &out.FamilyID, d.TimeDest(&out.CreatedAt), &nullTimeDest{&out.UsedAt})
		return NotFound(err, "oauth_refresh", "<hash>")
	})
	if err != nil {
		return store.OAuthRefreshRow{}, false, err
	}
	return out, first, nil
}

// OAuthRefreshGet returns a refresh token without using it.
func OAuthRefreshGet(ctx context.Context, db DB, tokenHash string) (store.OAuthRefreshRow, error) {
	var out store.OAuthRefreshRow
	d := db.Dialect()
	err := db.QueryRow(ctx, `SELECT token_hash, family_id, created_at, used_at FROM oauth_refresh_tokens WHERE token_hash = ?`,
		tokenHash).Scan(&out.TokenHash, &out.FamilyID, d.TimeDest(&out.CreatedAt), &nullTimeDest{&out.UsedAt})
	if err != nil {
		return store.OAuthRefreshRow{}, NotFound(err, "oauth_refresh", "<hash>")
	}
	return out, nil
}

// OAuthTokenRetire retires the live access tokens of a client (of one family
// when familyID is set).
func OAuthTokenRetire(ctx context.Context, db DB, clientID, familyID string, at time.Time) (int, error) {
	if clientID == "" {
		return 0, errors.New("oauth token retire: client_id required")
	}
	d := db.Dialect()
	t := d.TimeArg(at)
	q := `UPDATE operator_token_defs SET retired_at = ? WHERE oauth_client = ? AND (retired_at IS NULL OR retired_at > ?)`
	args := []any{t, clientID, t}
	if familyID != "" {
		q += ` AND oauth_family = ?`
		args = append(args, familyID)
	}
	n, err := db.Exec(ctx, q, args...)
	return int(n), err
}

// OAuthDeleteExpired removes what can no longer authenticate: codes expired
// before `before`, families revoked or past their absolute lifetime before it,
// and those families' refresh tokens.
func OAuthDeleteExpired(ctx context.Context, db DB, before time.Time) (int, error) {
	var total int
	err := db.InTx(ctx, func(tx Querier) error {
		t := tx.Dialect().TimeArg(before)
		n, err := tx.Exec(ctx, `DELETE FROM oauth_codes WHERE expires_at <= ?`, t)
		if err != nil {
			return err
		}
		total += int(n)
		dead := `SELECT family_id FROM oauth_refresh_families WHERE absolute_expires_at <= ? OR revoked_at <= ?`
		n, err = tx.Exec(ctx, `DELETE FROM oauth_refresh_tokens WHERE family_id IN (`+dead+`)`, t, t)
		if err != nil {
			return err
		}
		total += int(n)
		n, err = tx.Exec(ctx, `DELETE FROM oauth_refresh_families WHERE absolute_expires_at <= ? OR revoked_at <= ?`, t, t)
		total += int(n)
		return err
	})
	return total, err
}
