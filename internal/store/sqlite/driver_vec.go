//go:build sqlite_vec

package sqlite

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// openDB opens the SQLite database using github.com/mattn/go-sqlite3
// (CGO) and loads the sqlite-vec extension from the path declared by
// the LOOMCYCLE_SQLITE_VEC_PATH env var. Built only when
// `-tags=sqlite_vec` is set.
//
// The extension exposes the `vec0` virtual-table API that
// memory_embeddings_vec.go's MemoryEmbed* implementations use.
// Without the extension loaded the virtual-table CREATE would fail
// with "no such module: vec0" — so we load it eagerly at every
// connection-open via a custom driver ConnectHook.
//
// LOOMCYCLE_SQLITE_VEC_PATH MUST be set when this build is used.
// Refusing to start with a clear error is the right call: a CGO
// binary built with -tags=sqlite_vec but no path configured is
// always a misconfiguration; failing silently would surface as
// confusing virtual-table errors at first MemoryEmbedSet call.
//
// The driver is registered once per process under the name
// `sqlite3_loomcycle_vec`. Re-registration is guarded by sync.Once
// so test-binary repeated Opens don't panic on
// "sql: Register called twice".
func openDB(path string) (*sql.DB, error) {
	extPath := os.Getenv("LOOMCYCLE_SQLITE_VEC_PATH")
	if extPath == "" {
		return nil, fmt.Errorf(
			"sqlite_vec build requires LOOMCYCLE_SQLITE_VEC_PATH " +
				"pointing at the sqlite-vec extension shared library " +
				"(e.g. /usr/local/lib/vec0 on Linux, " +
				"$(brew --prefix sqlite-vec)/lib/vec0 on macOS) — " +
				"set the var or rebuild without the sqlite_vec tag",
		)
	}

	registerVecDriverOnce(extPath)
	// Operator-visible boot signal that the sqlite_vec build is
	// active. SupportsVectors() still returns false in v0.10.2
	// (MemoryEmbed* methods stubbed pending v0.10.3 schema design),
	// so this log line is the only confirmation the operator's
	// build choice took effect.
	logVecLoadOnce(extPath)

	// mattn DSN uses `_pragma_name=value` query params (different from
	// modernc's `_pragma=name(value)` shape). WAL + foreign keys +
	// busy timeout mirror the default-build defaults.
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	if path == ":memory:" {
		// mattn supports shared in-memory dbs the same way: cache=shared
		// with the file::memory: URI form.
		dsn = "file::memory:?cache=shared&_journal_mode=WAL"
	}
	return sql.Open("sqlite3_loomcycle_vec", dsn)
}

var (
	vecDriverOnce sync.Once
	vecLogOnce    sync.Once
)

// registerVecDriverOnce registers a mattn driver instance with a
// ConnectHook that calls LoadExtension on every new connection. The
// hook fires before any user-facing query, so virtual-table CREATEs
// resolve `vec0` reliably.
//
// Once-guarded because sql.Register panics on duplicate-name
// registration. The extPath is captured by value (function-parameter
// closure) at the first Once.Do call; subsequent Opens reuse the same
// driver regardless of any new path passed in. In practice nothing in
// loomcycle calls Open() with a different extension path in the same
// process — but test binaries that reset os.Setenv across cases get
// the first-seen path. Documented here so a future bug-hunter doesn't
// get confused.
func registerVecDriverOnce(extPath string) {
	vecDriverOnce.Do(func() {
		sql.Register("sqlite3_loomcycle_vec", &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				if err := c.LoadExtension(extPath, ""); err != nil {
					return fmt.Errorf("sqlite-vec load %s: %w", extPath, err)
				}
				return nil
			},
		})
	})
}

// logVecLoadOnce emits the operator-visible "sqlite_vec build active"
// confirmation. Once-guarded so test suites that Open() many times
// don't spam the log.
func logVecLoadOnce(extPath string) {
	vecLogOnce.Do(func() {
		log.Printf("sqlite: sqlite_vec build active — extension path=%s (MemoryEmbed* implementation lands in v0.10.3; SupportsVectors() still false until then)", extPath)
	})
}
