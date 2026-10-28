package sqlshared

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// UserPasskey* and WebAuthnCeremony* implement store.PasskeyStore for both
// stores (RFC FA P6; postgres migration 0126). Binary values are stored as
// base64url text (the AAGUID as hex), so both engines hold the same strings.

var b64 = base64.RawURLEncoding

const passkeyCols = `credential_id, tenant_id, subject, user_handle, public_key, attestation_type, attestation_format,
	transports, flags, aaguid, sign_count, attachment, discoverable, name, created_at, last_used_at, flagged_at, flag_reason`

func scanPasskey(d Dialect, sc Row) (store.UserPasskeyRow, error) {
	var (
		r                       store.UserPasskeyRow
		id, handle, pub, aaguid string
		transports              string
		flags, count            int64
	)
	if err := sc.Scan(&id, &r.TenantID, &r.Subject, &handle, &pub, &r.AttestationType, &r.AttestationFormat,
		&transports, &flags, &aaguid, &count, &r.Attachment, &r.Discoverable, &r.Name,
		d.TimeDest(&r.CreatedAt), &nullTimeDest{&r.LastUsedAt}, &nullTimeDest{&r.FlaggedAt}, &r.FlagReason); err != nil {
		return store.UserPasskeyRow{}, err
	}
	var err error
	if r.CredentialID, err = b64.DecodeString(id); err == nil {
		if r.UserHandle, err = b64.DecodeString(handle); err == nil {
			if r.PublicKey, err = b64.DecodeString(pub); err == nil {
				r.AAGUID, err = hex.DecodeString(aaguid)
			}
		}
	}
	if err != nil {
		return store.UserPasskeyRow{}, fmt.Errorf("user passkey: decode: %w", err)
	}
	if err := json.Unmarshal([]byte(transports), &r.Transports); err != nil {
		return store.UserPasskeyRow{}, fmt.Errorf("user passkey: decode transports: %w", err)
	}
	if flags < 0 || flags > 0xff || count < 0 || count > 0xffffffff {
		return store.UserPasskeyRow{}, fmt.Errorf("user passkey: flags %d or sign count %d out of range", flags, count)
	}
	r.Flags, r.SignCount = uint8(flags), uint32(count)
	return r, nil
}

// UserPasskeyCreate writes a passkey. *ErrConflict{Kind:"user_passkey"} on a
// credential id already registered.
func UserPasskeyCreate(ctx context.Context, q Querier, row store.UserPasskeyRow) error {
	if len(row.CredentialID) == 0 || row.Subject == "" || len(row.UserHandle) == 0 || len(row.PublicKey) == 0 {
		return fmt.Errorf("user passkey: credential id, subject, user handle and public key are required")
	}
	transports := row.Transports
	if transports == nil {
		transports = []string{}
	}
	tj, err := json.Marshal(transports)
	if err != nil {
		return err
	}
	d := q.Dialect()
	created := row.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if _, err := q.Exec(ctx, `INSERT INTO user_passkeys (`+passkeyCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b64.EncodeToString(row.CredentialID), row.TenantID, row.Subject, b64.EncodeToString(row.UserHandle),
		b64.EncodeToString(row.PublicKey), row.AttestationType, row.AttestationFormat, string(tj), int64(row.Flags),
		hex.EncodeToString(row.AAGUID), int64(row.SignCount), row.Attachment, row.Discoverable, row.Name,
		d.TimeArg(created), nullTimeArg(d, row.LastUsedAt), nullTimeArg(d, row.FlaggedAt), row.FlagReason); err != nil {
		if IsUniqueViolation(err) {
			return &store.ErrConflict{Kind: "user_passkey", ID: b64.EncodeToString(row.CredentialID)}
		}
		return fmt.Errorf("user passkey insert: %w", err)
	}
	return nil
}

func UserPasskeyGet(ctx context.Context, q Querier, credentialID []byte) (store.UserPasskeyRow, error) {
	r, err := scanPasskey(q.Dialect(), q.QueryRow(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE credential_id = ?`,
		b64.EncodeToString(credentialID)))
	if err != nil {
		return store.UserPasskeyRow{}, NotFound(err, "user_passkey", b64.EncodeToString(credentialID))
	}
	return r, nil
}

func UserPasskeyList(ctx context.Context, q Querier, tenantID, subject string) ([]store.UserPasskeyRow, error) {
	rows, err := q.Query(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE tenant_id = ? AND subject = ?
		ORDER BY created_at, credential_id`, tenantID, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.UserPasskeyRow{}
	for rows.Next() {
		r, err := scanPasskey(q.Dialect(), rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserPasskeyRecordUse is a compare-and-set on the counter, in one statement,
// so two replicas racing on assertions of one credential cannot both move it
// and a counter can never go backwards in the store.
func UserPasskeyRecordUse(ctx context.Context, q Querier, credentialID []byte, signCount uint32, flags uint8, at time.Time) (bool, error) {
	d := q.Dialect()
	n, err := q.Exec(ctx, `UPDATE user_passkeys SET sign_count = ?, flags = ?, last_used_at = ?
		WHERE credential_id = ? AND flagged_at IS NULL AND (sign_count < ? OR (sign_count = 0 AND ? = 0))`,
		int64(signCount), int64(flags), d.TimeArg(at), b64.EncodeToString(credentialID), int64(signCount), int64(signCount))
	return n == 1, err
}

func UserPasskeyFlag(ctx context.Context, q Querier, credentialID []byte, reason string, at time.Time) (bool, error) {
	n, err := q.Exec(ctx, `UPDATE user_passkeys SET flagged_at = ?, flag_reason = ? WHERE credential_id = ? AND flagged_at IS NULL`,
		q.Dialect().TimeArg(at), reason, b64.EncodeToString(credentialID))
	return n == 1, err
}

func UserPasskeyRename(ctx context.Context, q Querier, tenantID, subject string, credentialID []byte, name string) (bool, error) {
	n, err := q.Exec(ctx, `UPDATE user_passkeys SET name = ? WHERE tenant_id = ? AND subject = ? AND credential_id = ?`,
		name, tenantID, subject, b64.EncodeToString(credentialID))
	return n == 1, err
}

func UserPasskeyDelete(ctx context.Context, q Querier, tenantID, subject string, credentialID []byte) (bool, error) {
	n, err := q.Exec(ctx, `DELETE FROM user_passkeys WHERE tenant_id = ? AND subject = ? AND credential_id = ?`,
		tenantID, subject, b64.EncodeToString(credentialID))
	return n == 1, err
}

func UserPasskeyDeleteSubject(ctx context.Context, q Querier, tenantID, subject string) (int, error) {
	n, err := q.Exec(ctx, `DELETE FROM user_passkeys WHERE tenant_id = ? AND subject = ?`, tenantID, subject)
	return int(n), err
}

// ---- ceremonies ----

const ceremonyCols = `ceremony_hash, kind, gate, tenant_id, subject, binding, session_data, created_at, expires_at`

func WebAuthnCeremonyCreate(ctx context.Context, q Querier, row store.WebAuthnCeremonyRow) error {
	if row.CeremonyHash == "" || row.Kind == "" || row.SessionData == "" || row.ExpiresAt.IsZero() {
		return fmt.Errorf("webauthn ceremony: hash, kind, session data and expiry are required")
	}
	d := q.Dialect()
	created := row.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := q.Exec(ctx, `INSERT INTO webauthn_ceremonies (`+ceremonyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.CeremonyHash, row.Kind, row.Gate, row.TenantID, row.Subject, row.Binding, row.SessionData,
		d.TimeArg(created), d.TimeArg(row.ExpiresAt))
	return err
}

// WebAuthnCeremonyTake deletes and returns the ceremony in one statement:
// DELETE … RETURNING lets exactly one caller have it.
func WebAuthnCeremonyTake(ctx context.Context, q Querier, ceremonyHash string, now time.Time) (store.WebAuthnCeremonyRow, error) {
	d := q.Dialect()
	var r store.WebAuthnCeremonyRow
	err := q.QueryRow(ctx, `DELETE FROM webauthn_ceremonies WHERE ceremony_hash = ? AND expires_at > ? RETURNING `+ceremonyCols,
		ceremonyHash, d.TimeArg(now)).Scan(&r.CeremonyHash, &r.Kind, &r.Gate, &r.TenantID, &r.Subject, &r.Binding,
		&r.SessionData, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.ExpiresAt))
	if err != nil {
		return store.WebAuthnCeremonyRow{}, NotFound(err, "webauthn_ceremony", "<hash>")
	}
	return r, nil
}

func WebAuthnCeremonyDeleteExpired(ctx context.Context, q Querier, before time.Time) (int, error) {
	n, err := q.Exec(ctx, `DELETE FROM webauthn_ceremonies WHERE expires_at < ?`, q.Dialect().TimeArg(before))
	return int(n), err
}
