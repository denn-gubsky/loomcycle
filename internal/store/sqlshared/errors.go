package sqlshared

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// IsNoRows reports whether err says a single-row read found nothing. pgx's
// ErrNoRows matches sql.ErrNoRows under errors.Is, so one check covers both
// drivers.
func IsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// NotFound turns a no-rows err into the store's typed *store.ErrNotFound for
// kind and id; any other err, nil included, is returned as is.
func NotFound(err error, kind, id string) error {
	if IsNoRows(err) {
		return &store.ErrNotFound{Kind: kind, ID: id}
	}
	return err
}

// IsUniqueViolation reports whether err is a PRIMARY KEY or UNIQUE constraint
// violation. Postgres reports SQLSTATE 23505 on a *pgconn.PgError. Both SQLite
// drivers (modernc and mattn) word it "UNIQUE constraint failed", for a
// primary key too; they share no error type, so the text is the check, as it
// has been in the SQLite store.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
