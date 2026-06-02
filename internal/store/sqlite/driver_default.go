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
//
// busy_timeout(5000) MUST be carried onto the in-memory DSN too. With
// SetMaxOpenConns(8) + cache=shared, multiple connections contend on
// one shared-cache lock; without a busy timeout the loser of a write
// race gets SQLITE_BUSY *immediately* (default timeout 0) instead of
// waiting. That surfaced as a CI-only flake in TestSchedulerBearerCompound:
// 64 concurrent fires raced their ScheduleRunStateRecordResult writes,
// the BUSY'd ones failed to advance next_run_at, and the still-due rows
// re-fired — double-firing the early phases. journal_mode(WAL) is a
// no-op on an in-memory DB (no file journal), so only the two
// connection-level pragmas carry over.
func openDB(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		id := memoryDBCounter.Add(1)
		dsn = "file:lc-mem-" + strconv.FormatUint(id, 10) +
			"?mode=memory&cache=shared&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	}
	return sql.Open("sqlite", dsn)
}
