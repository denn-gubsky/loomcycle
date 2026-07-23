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
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	// PgDSN is the SEPARATE aux-database DSN for the postgres tier (distinct
	// from the main store DSN). Required by NewPostgres; ignored by New.
	PgDSN string
	// QuotaBytes caps a single scope's on-disk size. 0 = no quota. Checked
	// BEFORE a write; an approximate bound that can overshoot by at most one
	// statement (see each backend's exec).
	QuotaBytes int
	// StatementTimeoutMS bounds a single Query/Exec. 0 = no timeout.
	StatementTimeoutMS int
	// MaxRows caps the rows a Query returns; the remainder is drained and
	// Truncated is set. 0 = no cap.
	MaxRows int
	// TxnTimeoutMS bounds how long an explicit transaction (sql_begin) may stay
	// open before the reaper rolls it back. 0 = no reaper (txns end only on
	// commit/rollback/run-end).
	TxnTimeoutMS int
	// MaxOpenTxns caps concurrent open explicit transactions process-wide (each
	// pins a scope connection). 0 = unbounded.
	MaxOpenTxns int
	// MaxTxnDepth caps the SAVEPOINT nesting depth of one explicit transaction
	// (Phase 3b): a nested sql_begin past this errors, bounding the in-memory
	// savepoint stack. 0 = unbounded.
	MaxTxnDepth int
	// ScopeTTLMS turns on durable-scope GC: a durable (agent/user) scope idle
	// longer than this is dropped by the sweeper. 0 = GC OFF (the default — GC
	// discards data, so it is opt-in). Run scopes are never GC'd (dropped at
	// run-end).
	ScopeTTLMS int
	// GCIntervalMS is how often the GC sweeper runs. 0 → a sensible default
	// (one hour). Meaningful when ScopeTTLMS > 0 or TotalMaxBytes > 0.
	GCIntervalMS int
	// TotalMaxBytes turns on size-based GC (Phase 3f): when the aggregate on-disk
	// size of all durable scopes exceeds this, the sweeper evicts the largest idle
	// scopes until back under budget. 0 = OFF. Complements ScopeTTLMS.
	TotalMaxBytes int64
	// SharedSchemas lists operator-curated READ-ONLY shared schemas (Phase 3g,
	// postgres tier only): each is baked onto every scope role's search_path so
	// agents can SELECT the operator's reference tables. Read-only is engine-
	// enforced (the operator grants SELECT only). Invalid/missing names are
	// dropped at construction with a warning. Ignored by the sqlite tier.
	SharedSchemas []string
}

// backend is the per-tier DB engine the Manager delegates to AFTER the
// statement has passed the security validator. Implemented by sqliteBackend
// (sqlite.go) and postgresBackend (postgres.go).
type backend interface {
	query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error)
	exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error)
	dropRunScope(runID string) (removed bool, err error)
	// dropScope drops one DURABLE (agent/user) scope: retire its pool, drop its
	// schema+role (postgres) / .db file (sqlite), remove its GC meta. Mirrors the
	// per-scope drop the GC sweeper does, exposed for RFC BM retention (reclaim a
	// fully-retired agent's SQL-Memory scope). removed reflects an actual drop
	// (false when the scope was already gone). MUST NOT be called for the run
	// scope — use dropRunScope.
	dropScope(key ScopeKey) (removed bool, err error)
	// beginTx pins the scope connection (so it is not evicted while the txn is
	// open) and opens a transaction on it. release drops the pin; the caller
	// MUST call it after Commit/Rollback. The *sql.Tx is backend-agnostic, so
	// the Manager runs validated statements on it directly.
	beginTx(ctx context.Context, key ScopeKey) (tx *sql.Tx, release func(), err error)
	// txnSizeBytes returns the scope's current on-disk size measured ON the open
	// transaction (sqlite: page_count*page_size; postgres: pg_total_relation_size
	// over the scope schema) — for the per-write quota check inside a txn.
	txnSizeBytes(ctx context.Context, tx *sql.Tx, key ScopeKey) (int64, error)
	// vectorsEnabled reports whether the tier can serve vector columns (Phase 3c
	// — postgres with pgvector installed; sqlite is always false for now).
	vectorsEnabled() bool
	// touchScope records that a DURABLE scope was just used (sqlite: the .db
	// mtime; postgres: the meta table), so the GC sweeper can find idle ones.
	touchScope(key ScopeKey) error
	// sweepStale drops every DURABLE (agent/user) scope last used before cutoff
	// and returns how many were dropped. Never touches the run scope.
	sweepStale(cutoff time.Time) (dropped int, err error)
	// sweepBudget drops the LARGEST idle DURABLE scopes until the aggregate
	// durable footprint is at or under budget (Phase 3f size-based GC). Skips
	// in-use scopes (inUse>0) and the run scope; returns how many were dropped.
	sweepBudget(budget int64) (dropped int, err error)
	// listScopes enumerates every DURABLE scope for snapshot capture (RFC AA
	// Phase 3e); exportScope/restoreScope move one scope's logical dump in/out.
	listScopes(ctx context.Context) ([]ScopeKey, error)
	exportScope(ctx context.Context, key ScopeKey) (*ScopeDump, error)
	restoreScope(ctx context.Context, key ScopeKey, dump *ScopeDump) error
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
	dialect    dialect
	cfg        Config
	backend    backend
	txns       *txnRegistry
	reaperStop chan struct{}
	reaperDone chan struct{} // closed by the reaper goroutine on exit (nil if none)

	// Durable-scope GC (Phase 3d). touch debounce + the sweeper goroutine.
	touchMu   sync.Mutex
	lastTouch map[string]time.Time
	gcStop    chan struct{}
	gcDone    chan struct{} // closed by the GC sweeper on exit (nil if none)
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
	return newManager(dialectSQLite, cfg, b), nil
}

// newManager wires the facade + the explicit-transaction registry and starts
// the abandoned-txn reaper. Shared by New (sqlite) and NewPostgres.
func newManager(d dialect, cfg Config, b backend) *Manager {
	m := &Manager{dialect: d, cfg: cfg, backend: b, txns: newTxnRegistry(), lastTouch: make(map[string]time.Time)}
	m.startReaper()
	m.startGC()
	return m
}

// Rebind converts `?` positional placeholders to the active dialect's form:
// `$1, $2, …` for postgres (which the pgx backend requires — it does NOT
// accept `?`), and unchanged for sqlite. This lets a tool author write ONE
// portable statement with `?`. It is a purely positional replacement and does
// NOT skip a `?` inside a string literal — call it only on statements you
// construct yourself (with bound values), never on raw model/user SQL.
func (m *Manager) Rebind(sql string) string {
	if m.dialect != dialectPostgres {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql) + 8)
	n := 0
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(sql[i])
	}
	return b.String()
}

// Query runs a read-only statement against the scope database. The statement
// is validated by the Go-layer security floor FIRST (read-only dialect);
// only then does it reach the backend. A *ErrStatement or a driver error is
// returned as a plain error — the caller surfaces it as is_error.
func (m *Manager) Query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
	if err := validateStatementForDialect(statement, true, m.dialect); err != nil {
		return nil, err
	}
	defer m.touch(key)
	return m.backend.query(ctx, key, statement, args)
}

// Exec runs a DDL/DML statement against the scope database. The statement is
// validated by the Go-layer floor FIRST (read-write dialect), then the backend
// enforces the quota + timeout.
func (m *Manager) Exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	if err := validateStatementForDialect(statement, false, m.dialect); err != nil {
		return nil, err
	}
	defer m.touch(key)
	return m.backend.exec(ctx, key, statement, args, quotaOverride)
}

// DropRunScope removes the ephemeral run scope (sqlite: the .db file behind a
// path fence; postgres: the scope schema + role). removed reports whether the
// scope existed.
func (m *Manager) DropRunScope(runID string) (removed bool, err error) {
	return m.backend.dropRunScope(runID)
}

// DropScope removes one DURABLE (agent/user) scope (sqlite: the .db file behind
// a path fence; postgres: the scope schema + role + GC meta). removed reports
// whether the scope existed. Rejects the run scope (use DropRunScope) and an
// empty ScopeID. Used by the RFC BM retention sweeper to reclaim a fully-retired
// agent's SQL-Memory scope (its SQL tables + document-structure rows). Idempotent
// — a never-provisioned scope returns removed=false, nil.
func (m *Manager) DropScope(ctx context.Context, key ScopeKey) (removed bool, err error) {
	if key.Scope == runScope {
		return false, fmt.Errorf("sqlmem: DropScope is for durable scopes; use DropRunScope for the run scope")
	}
	if key.Scope == "" || strings.TrimSpace(key.ScopeID) == "" {
		return false, fmt.Errorf("sqlmem: DropScope requires a non-empty scope and scope_id")
	}
	return m.backend.dropScope(key)
}

// VectorsEnabled reports whether this Manager's tier can serve vector columns
// (Phase 3c). True only on the postgres tier with pgvector installed in the
// sqlmem_ext schema.
func (m *Manager) VectorsEnabled() bool { return m.backend.vectorsEnabled() }

// Close stops the reaper, rolls back every still-open transaction (releasing
// their scope pins), and releases the backend (sqlite: every open scope handle;
// postgres: the aux connection pool).
func (m *Manager) Close() error {
	m.stopReaper()
	m.stopGC()
	m.txns.rollbackAll()
	return m.backend.close()
}

// rowQueryer is satisfied by both *sql.DB and *sql.Tx, so the per-scope size
// check (the quota) can run either on the scope handle (auto-commit exec) or on
// an open transaction (ExecTxn).
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
