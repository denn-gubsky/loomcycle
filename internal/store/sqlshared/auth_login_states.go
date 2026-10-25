package sqlshared

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P1: started redirect logins (auth_login_states) and the insert-only
// group create an identity provider's group sync uses.

const loginStateCols = `state_hash, gate, binding_hash, purpose, return_to, reauth, link_tenant, link_subject,
	link_session_hash, created_at, expires_at, consumed_at`

// AuthLoginStateCreate inserts a login state.
func AuthLoginStateCreate(ctx context.Context, db DB, row store.AuthLoginStateRow) error {
	if row.StateHash == "" || row.Gate == "" || row.BindingHash == "" || row.ExpiresAt.IsZero() {
		return errors.New("auth_login_state: state_hash + gate + binding_hash + expires_at required")
	}
	if row.Purpose == "" {
		row.Purpose = store.LoginPurposeLogin
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	d := db.Dialect()
	_, err := db.Exec(ctx, `INSERT INTO auth_login_states (`+loginStateCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		row.StateHash, row.Gate, row.BindingHash, row.Purpose, row.ReturnTo, row.Reauth, row.LinkTenant,
		row.LinkSubject, row.LinkSessionHash, d.TimeArg(row.CreatedAt), d.TimeArg(row.ExpiresAt))
	if IsUniqueViolation(err) {
		return &store.ErrConflict{Kind: "auth_login_state", ID: "<hash>"}
	}
	if err != nil {
		return fmt.Errorf("auth_login_state insert: %w", err)
	}
	return nil
}

// AuthLoginStateConsume marks a live state used and returns it. The UPDATE's
// own WHERE decides who wins: of two concurrent callbacks with the same state,
// exactly one changes the row, and only that one reads it back.
func AuthLoginStateConsume(ctx context.Context, db DB, stateHash string, now time.Time) (store.AuthLoginStateRow, error) {
	var out store.AuthLoginStateRow
	err := db.InTx(ctx, func(tx Querier) error {
		d := tx.Dialect()
		n, err := tx.Exec(ctx, `UPDATE auth_login_states SET consumed_at = ?
			WHERE state_hash = ? AND consumed_at IS NULL AND expires_at > ?`,
			d.TimeArg(now), stateHash, d.TimeArg(now))
		if err != nil {
			return err
		}
		if n != 1 {
			return &store.ErrNotFound{Kind: "auth_login_state", ID: "<hash>"}
		}
		// consumed_at was set just above, so it scans as a NOT NULL time.
		return tx.QueryRow(ctx, `SELECT `+loginStateCols+` FROM auth_login_states WHERE state_hash = ?`, stateHash).Scan(
			&out.StateHash, &out.Gate, &out.BindingHash, &out.Purpose, &out.ReturnTo, &out.Reauth,
			&out.LinkTenant, &out.LinkSubject, &out.LinkSessionHash, d.TimeDest(&out.CreatedAt),
			d.TimeDest(&out.ExpiresAt), d.TimeDest(&out.ConsumedAt))
	})
	if err != nil {
		return store.AuthLoginStateRow{}, err
	}
	return out, nil
}

// AuthLoginStateDeleteExpired removes states that expired or were used before
// `before`.
func AuthLoginStateDeleteExpired(ctx context.Context, db DB, before time.Time) (int, error) {
	t := db.Dialect().TimeArg(before)
	n, err := db.Exec(ctx, `DELETE FROM auth_login_states WHERE expires_at <= ? OR consumed_at <= ?`, t, t)
	return int(n), err
}

// GroupCreate inserts a group unless one of that name exists in the tenant.
func GroupCreate(ctx context.Context, db DB, row store.GroupRow) (bool, error) {
	if !authz.ValidName(row.Name) {
		return false, fmt.Errorf("group create: %q is not a valid name", row.Name)
	}
	if len(row.Description) > authz.MaxDescriptionBytes {
		return false, fmt.Errorf("group create: description is longer than %d bytes", authz.MaxDescriptionBytes)
	}
	var created bool
	err := db.InTx(ctx, func(tx Querier) error {
		now := tx.Dialect().TimeArg(time.Now().UTC())
		n, err := tx.Exec(ctx, `INSERT INTO user_groups (tenant_id, name, description, external_id, created_at, updated_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, name) DO NOTHING`,
			row.TenantID, row.Name, row.Description, row.ExternalID, now, now, row.CreatedBy)
		if err != nil || n == 0 {
			return err
		}
		created = true
		return bumpAclVersion(ctx, tx, row.TenantID)
	})
	return created, err
}
