//go:build !sqlite_vec

package sqlite

import (
	"database/sql"
	"strconv"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// memoryDBCounter mints a unique identifier per `:memory:` open so
// every Store gets its own in-memory database. Plain
// `file::memory:?cache=shared` aliases ALL :memory: opens process-
// wide to one DB, which was load-bearing for one test class
// (within-test goroutine concurrency seeing each other's rows) but
// catastrophic for test isolation across files (one test's seeded
// rows leaked into another's assertions; failures showed up as
// "entries = 5, want 2"). The fix: name each in-memory DB so opens
// in one test never collide with opens in another. The
// `cache=shared` flag is preserved so multiple connections within
// one *sql.DB still see each other's writes immediately (otherwise
// modernc's default is per-connection isolation, which breaks the
// MemoryAtomicUpdate locking pattern).
var memoryDBCounter atomic.Uint64

// openDB opens the SQLite database using modernc.org/sqlite — pure Go,
// no CGO. This is the default build target; ships in the goreleaser
// tarballs.
//
// The DSN encodes pragmas as `?_pragma=...` clauses; modernc supports
// this URI syntax. WAL mode + foreign-key enforcement + a 5-second
// busy timeout absorb the SQLite single-writer contention.
//
// In-memory (":memory:") gets a unique named in-memory database per
// call via the `file:lc-mem-N:?mode=memory&cache=shared` form (where
// N is a process-wide atomic counter). Each Open() gets its own DB
// rooted at a distinct name; `cache=shared` preserves the
// within-handle multi-connection-sees-the-same-writes semantics
// needed by the MemoryAtomicUpdate locking pattern.
func openDB(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		id := memoryDBCounter.Add(1)
		dsn = "file:lc-mem-" + strconv.FormatUint(id, 10) + "?mode=memory&cache=shared"
	}
	return sql.Open("sqlite", dsn)
}
