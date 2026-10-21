package sqlshared

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestIsNoRows_MatchesBothDriversWrappedOrNot(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":               {nil, false},
		"database/sql":      {sql.ErrNoRows, true},
		"pgx":               {pgx.ErrNoRows, true},
		"wrapped pgx":       {fmt.Errorf("get: %w", pgx.ErrNoRows), true},
		"other":             {errors.New("no rows in result set, but not the sentinel"), false},
		"unique violation":  {&pgconn.PgError{Code: "23505"}, false},
		"wrapped sql":       {fmt.Errorf("get: %w", sql.ErrNoRows), true},
		"double-wrapped":    {fmt.Errorf("a: %w", fmt.Errorf("b: %w", pgx.ErrNoRows)), true},
		"string mention":    {fmt.Errorf("get: %v", sql.ErrNoRows), false},
		"joined with other": {errors.Join(errors.New("x"), sql.ErrNoRows), true},
	}
	for name, c := range cases {
		if got := IsNoRows(c.err); got != c.want {
			t.Errorf("%s: IsNoRows(%v) = %v, want %v", name, c.err, got, c.want)
		}
	}
}

func TestNotFound_MapsNoRowsToTheStoreErrorAndPassesOthersThrough(t *testing.T) {
	err := NotFound(pgx.ErrNoRows, "dirent", "/docs/a")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) || nf.Kind != "dirent" || nf.ID != "/docs/a" {
		t.Fatalf("NotFound(no rows) = %#v, want *store.ErrNotFound{dirent, /docs/a}", err)
	}
	other := errors.New("connection reset")
	if got := NotFound(other, "dirent", "x"); got != other {
		t.Errorf("NotFound(other) = %v, want it unchanged", got)
	}
	if got := NotFound(nil, "dirent", "x"); got != nil {
		t.Errorf("NotFound(nil) = %v, want nil", got)
	}
}

func TestIsUniqueViolation_RecognisesBothEngines(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                     {nil, false},
		"postgres 23505":          {&pgconn.PgError{Code: "23505"}, true},
		"wrapped postgres 23505":  {fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), true},
		"postgres fk 23503":       {&pgconn.PgError{Code: "23503"}, false},
		"postgres not null 23502": {&pgconn.PgError{Code: "23502"}, false},
		// A Postgres error whose text happens to read like SQLite's is judged
		// by its SQLSTATE, not its message.
		"postgres with sqlite-like text": {&pgconn.PgError{Code: "P0001", Message: "UNIQUE constraint failed"}, false},
		"sqlite unique":                  {errors.New("constraint failed: UNIQUE constraint failed: users.id (1555)"), true},
		"wrapped sqlite unique":          {fmt.Errorf("create: %w", errors.New("UNIQUE constraint failed: t.a")), true},
		"sqlite not null":                {errors.New("NOT NULL constraint failed: t.a"), false},
		"no rows":                        {sql.ErrNoRows, false},
	}
	for name, c := range cases {
		if got := IsUniqueViolation(c.err); got != c.want {
			t.Errorf("%s: IsUniqueViolation(%v) = %v, want %v", name, c.err, got, c.want)
		}
	}
}
