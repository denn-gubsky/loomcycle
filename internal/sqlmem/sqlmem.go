// Package sqlmem implements RFC AA SQL Memory: a runtime-hosted, per-scope
// SQL database that AUTHORIZED agents run arbitrary SQL against, isolated
// from the main loomcycle store.
//
// The security model is NOT injection defence (agents are authorized to run
// SQL) — it is preventing an agent from ESCAPING its per-scope database
// (reading/writing arbitrary host files, running arbitrary code, or reaching
// another scope's data) and from exhausting resources.
//
// Two tiers behind one Manager facade (it FOLLOWS the main store's backend):
//
//   - sqlite (sqlite.go) — one .db file per scope under a root directory, an
//     LRU of open handles, file isolation as the backstop. The DEFAULT sqlite
//     driver loomcycle links — modernc.org/sqlite (pure-Go) — exposes NO
//     sqlite3_set_authorizer, so the PRIMARY, driver-agnostic defence is the
//     Go-layer parsed statement validator (validate.go) plus per-scope files.
//   - postgres (postgres.go) — a schema per scope in a SEPARATE aux database,
//     reached through a per-scope low-privilege NOLOGIN role (USAGE only on its
//     own schema) that the runtime SET LOCAL ROLEs into per statement. Here the
//     PRIMARY defence is engine-enforced least privilege; the validator is
//     defense-in-depth.
//
// The Manager validates every statement BEFORE it reaches a driver, then
// delegates the DB work to the configured backend.
package sqlmem

import (
	"context"
	"database/sql"
	"time"
)

// Config configures the SQL Memory Manager. All bounds default to "off"
// when zero EXCEPT the ones with sensible runtime defaults the caller is
// expected to supply (see cmd/loomcycle wiring); the Manager itself treats
// a zero StatementTimeoutMS / MaxRows / QuotaBytes as "no bound".
type Config struct {
	// Root is the parent directory under which every per-scope .db file
	// lives (<DataDir>/sqlmem by convention). Required for the sqlite tier.
	Root string
	// QuotaBytes caps a single scope's on-disk size. 0 = no quota. Checked
	// BEFORE a write; an approximate bound that can overshoot by at most one
	// statement (see each backend's exec).
	QuotaBytes int
	// StatementTimeoutMS bounds a single Query/Exec. 0 = no timeout.
	StatementTimeoutMS int
	// MaxRows caps the rows a Query returns; the remainder is drained and
	// Truncated is set. 0 = no cap.
	MaxRows int
}

// backend is the per-tier DB engine the Manager delegates to AFTER the
// statement has passed the security validator. Implemented by sqliteBackend
// (sqlite.go) and postgresBackend (postgres.go).
type backend interface {
	query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error)
	exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error)
	dropRunScope(runID string) (removed bool, err error)
	close() error
}

// Manager is the public SQL Memory facade. It owns the statement validator
// (the choke point every SQL op passes through) and delegates the DB work to
// the configured backend. Safe for concurrent use (the backends are).
//
// The Manager is deliberately SEPARATE from the main loomcycle store: an
// agent's arbitrary SQL can only ever reach its own scope, never the
// runtime's operational database.
type Manager struct {
	dialect dialect
	backend backend
}

// ScopeKey identifies one scope database. Tenant/Scope/ScopeID are all
// resolved server-side (never model-supplied raw) but are sanitized /
// name-hashed defensively by each backend before they touch the filesystem
// (sqlite) or become a postgres identifier (postgres).
type ScopeKey struct {
	Tenant  string
	Scope   string // "agent" | "user" | "run"
	ScopeID string
}

// runScope is the scope name for ephemeral run databases. It is NOT keyed by
// tenant (run ids are globally unique) and is the only scope DropRunScope can
// target.
const runScope = "run"

// QueryResult is the shape returned by Query.
type QueryResult struct {
	Columns   []string
	Rows      [][]any
	Truncated bool // true when MaxRows was hit and more rows existed
}

// ExecResult is the shape returned by Exec. LastInsertID is sqlite-only
// (postgres has no implicit last-insert id; use RETURNING in the statement).
type ExecResult struct {
	RowsAffected int64
	LastInsertID int64
}

// New constructs a Manager backed by the sqlite tier. The postgres tier is
// selected via the ctx-taking constructor wired in cmd/loomcycle (see
// postgres.go); this signature stays sqlite-only for the file-backed default.
func New(cfg Config) (*Manager, error) {
	b, err := newSQLiteBackend(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{dialect: dialectSQLite, backend: b}, nil
}

// Query runs a read-only statement against the scope database. The statement
// is validated by the Go-layer security floor FIRST (read-only dialect);
// only then does it reach the backend. A *ErrStatement or a driver error is
// returned as a plain error — the caller surfaces it as is_error.
func (m *Manager) Query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
	if err := validateStatementForDialect(statement, true, m.dialect); err != nil {
		return nil, err
	}
	return m.backend.query(ctx, key, statement, args)
}

// Exec runs a DDL/DML statement against the scope database. The statement is
// validated by the Go-layer floor FIRST (read-write dialect), then the backend
// enforces the quota + timeout.
func (m *Manager) Exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	if err := validateStatementForDialect(statement, false, m.dialect); err != nil {
		return nil, err
	}
	return m.backend.exec(ctx, key, statement, args, quotaOverride)
}

// DropRunScope removes the ephemeral run scope (sqlite: the .db file behind a
// path fence; postgres: the scope schema + role). removed reports whether the
// scope existed.
func (m *Manager) DropRunScope(runID string) (removed bool, err error) {
	return m.backend.dropRunScope(runID)
}

// Close releases the backend (sqlite: every open scope handle; postgres: the
// aux connection pool).
func (m *Manager) Close() error {
	return m.backend.close()
}

// collectRows scans up to maxRows rows from a *sql.Rows into a QueryResult,
// normalizing []byte columns to string so the JSON encoder emits text rather
// than base64 (both sqlite-under-modernc and postgres-under-pgx hand TEXT
// columns back as []byte). When maxRows is hit, Truncated is set and the
// caller's deferred rows.Close drains the rest. Shared by both backends.
func collectRows(rows *sql.Rows, maxRows int) (*QueryResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Columns: cols, Rows: make([][]any, 0)}
	for rows.Next() {
		if maxRows > 0 && len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		scan := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range scan {
			if b, ok := v.([]byte); ok {
				scan[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, scan)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// effectiveQuota returns the quota to enforce for a call: the per-call
// override when > 0, else the Manager default.
func effectiveQuota(cfg Config, override int) int {
	if override > 0 {
		return override
	}
	return cfg.QuotaBytes
}

// withTimeout derives a context bounded by StatementTimeoutMS. A zero timeout
// returns the parent ctx with a no-op cancel. (The postgres backend ALSO sets
// SET LOCAL statement_timeout; this ctx deadline is the shared backstop.)
func withTimeout(cfg Config, ctx context.Context) (context.Context, context.CancelFunc) {
	if cfg.StatementTimeoutMS <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(cfg.StatementTimeoutMS)*time.Millisecond)
}
