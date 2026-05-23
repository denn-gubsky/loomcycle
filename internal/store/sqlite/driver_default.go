//go:build !sqlite_vec

package sqlite

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// openDB opens the SQLite database using modernc.org/sqlite — pure Go,
// no CGO. This is the default build target; ships in the goreleaser
// tarballs.
//
// The DSN encodes pragmas as `?_pragma=...` clauses; modernc supports
// this URI syntax. WAL mode + foreign-key enforcement + a 5-second
// busy timeout absorb the SQLite single-writer contention.
//
// In-memory (":memory:") uses the `file::memory:?cache=shared` form
// so concurrent goroutines in tests see the same DB (default
// :memory: opens per-connection in modernc).
func openDB(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	return sql.Open("sqlite", dsn)
}
