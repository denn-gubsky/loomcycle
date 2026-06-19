package sqlmem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

// postgres.go — the postgres tier of SQL Memory: a SCHEMA per scope inside a
// SEPARATE aux database (LOOMCYCLE_SQLMEM_PG_DSN, distinct from the main store
// DSN), each reached through a per-scope NOLOGIN low-privilege role that has
// USAGE only on its own schema. The runtime connects as an operator-provisioned
// admin role and, per statement, opens a transaction that SET LOCAL ROLEs down
// to the scope role with search_path pinned to the scope schema and a
// statement_timeout set — so the agent's SQL runs with the privileges of a role
// that CANNOT reach another scope's schema, read host files, run programs, load
// extensions, or connect out (all engine-denied; see the RFC AA security note).
//
// Isolation is therefore engine-enforced least privilege; the parsed-statement
// validator (validate_postgres.go) is defense-in-depth. The aux database being
// a DIFFERENT database from the main loomcycle store means even a hypothetical
// escape can't reach the operational data.
//
// Multi-replica: schemas/roles are shared state in the aux DB and provisioning
// is idempotent, so any replica can serve any scope. The in-process
// `provisioned` set is a per-replica fast-path only; correctness does not
// depend on it (a cross-replica first-touch race is tolerated — the DDL is
// IF-NOT-EXISTS / DO-guarded and duplicate errors are swallowed).

// pgMaxConns bounds the aux pool. SQL Memory is not the hot path; a modest pool
// keeps aux-DB load predictable. (Fixed for Phase 2; promote to config if a
// deployment needs more.)
const pgMaxConns = 8

// postgresBackend implements `backend` over the aux database.
type postgresBackend struct {
	cfg  Config
	db   *sql.DB
	pg16 bool // server is >= 16: GRANT … WITH SET TRUE is required for SET ROLE

	mu          sync.Mutex
	provisioned map[string]bool // schema names provisioned in THIS process
}

// NewPostgres constructs a Manager backed by the postgres tier. It opens the
// aux pool, verifies connectivity, and detects the server version (the
// role-grant syntax differs at PG16). The DSN must point at a SEPARATE database
// from the main loomcycle store, reached by a non-superuser admin role with
// CREATE on that database and CREATEROLE (see docs/SQL_MEMORY.md for the
// operator provisioning recipe).
func NewPostgres(ctx context.Context, cfg Config) (*Manager, error) {
	b, err := newPostgresBackend(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{dialect: dialectPostgres, backend: b}, nil
}

func newPostgresBackend(ctx context.Context, cfg Config) (*postgresBackend, error) {
	if strings.TrimSpace(cfg.PgDSN) == "" {
		return nil, fmt.Errorf("sqlmem: postgres tier requires a non-empty aux DSN (LOOMCYCLE_SQLMEM_PG_DSN)")
	}
	db, err := sql.Open("pgx", cfg.PgDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: open aux postgres: %w", err)
	}
	db.SetMaxOpenConns(pgMaxConns)
	db.SetMaxIdleConns(pgMaxConns / 2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var verNum int
	if err := db.QueryRowContext(pingCtx, "SELECT current_setting('server_version_num')::int").Scan(&verNum); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlmem: connect aux postgres: %w", err)
	}
	return &postgresBackend{
		cfg:         cfg,
		db:          db,
		pg16:        verNum >= 160000,
		provisioned: make(map[string]bool),
	}, nil
}

// pgIdentRe is the shape EVERY interpolated identifier must match before it is
// ever placed into DDL (pgScopeNames produces exactly this). Hex-only names are
// injection-proof by construction; the assertion + q() quoting are belt-and-
// braces (never interpolate an un-vetted identifier).
var pgIdentRe = regexp.MustCompile(`^sqlmem_[sr]_[0-9a-f]{32}$`)

// pgScopeNames derives the (schema, role) identifier pair for a ScopeKey by
// hashing a canonical key string. Durable scopes hash (tenant, scope, scope_id)
// — matching the sqlite tier's tenant-keyed durable files; the ephemeral run
// scope hashes (run, run_id) WITHOUT tenant (run ids are globally unique), so
// DropRunScope can target it without a tenant. The 128-bit hash makes
// collisions negligible and the names fit Postgres's 63-char identifier limit.
func pgScopeNames(key ScopeKey) (schema, role string, err error) {
	var canon string
	if key.Scope == runScope {
		if strings.TrimSpace(key.ScopeID) == "" {
			return "", "", fmt.Errorf("sqlmem: empty run id")
		}
		canon = "run\x1f" + key.ScopeID
	} else {
		if key.Tenant == "" || key.Scope == "" || key.ScopeID == "" {
			return "", "", fmt.Errorf("sqlmem: empty scope key component")
		}
		canon = key.Tenant + "\x1f" + key.Scope + "\x1f" + key.ScopeID
	}
	sum := sha256.Sum256([]byte(canon))
	h := hex.EncodeToString(sum[:16]) // 32 hex chars = 128 bits
	schema = "sqlmem_s_" + h
	role = "sqlmem_r_" + h
	if !pgIdentRe.MatchString(schema) || !pgIdentRe.MatchString(role) {
		// Unreachable (hex by construction) — a hard stop before any interpolation.
		return "", "", fmt.Errorf("sqlmem: derived identifier failed validation")
	}
	return schema, role, nil
}

// q double-quotes a postgres identifier. The names passed here are validated
// hex (pgScopeNames / pgIdentRe), so no embedded quote can occur; the escaping
// is belt-and-braces.
func q(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// lit renders a single-quoted string literal (for a role name compared inside
// a DO block). Same validated-hex inputs; escaping is belt-and-braces.
func lit(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// provision lazily creates the scope schema + its low-privilege role and the
// grants that let the admin SET LOCAL ROLE into it. Idempotent and tolerant of
// the cross-replica race (duplicate-object errors are swallowed). The DDL runs
// in autocommit (NOT inside a caller's read-only txn). The package mutex is held
// across the DDL so concurrent first-touches of the SAME scope in this process
// serialize — provisioning is once-per-scope-lifetime, so the cost is paid once.
func (b *postgresBackend) provision(ctx context.Context, schema, role string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.provisioned[schema] {
		return nil
	}

	grant := fmt.Sprintf(`GRANT %s TO CURRENT_USER`, q(role))
	if b.pg16 {
		// PG16 split membership from the right to SET ROLE — the admin must be
		// granted the role WITH SET TRUE to SET LOCAL ROLE into it.
		grant = fmt.Sprintf(`GRANT %s TO CURRENT_USER WITH SET TRUE, INHERIT FALSE`, q(role))
	}

	stmts := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, q(schema)),
		// CREATE ROLE has no IF NOT EXISTS — guard it.
		fmt.Sprintf(`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname=%s) THEN CREATE ROLE %s NOLOGIN NOINHERIT; END IF; END $$`, lit(role), q(role)),
		// Default-deny: PUBLIC gets nothing; only the scope role gets in.
		fmt.Sprintf(`REVOKE ALL ON SCHEMA %s FROM PUBLIC`, q(schema)),
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, q(schema), q(role)),
		grant,
	}
	for _, s := range stmts {
		if _, err := b.db.ExecContext(ctx, s); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("sqlmem: provision scope: %w", err)
		}
	}
	b.provisioned[schema] = true
	return nil
}

// enterScope pins the per-statement guards on an open transaction, dropping the
// session from the admin role down to the per-scope role LAST (so the runtime
// sets statement_timeout + search_path before the agent's statement runs, and
// the agent — which the validator stops from issuing SET — cannot change them).
func (b *postgresBackend) enterScope(ctx context.Context, tx *sql.Tx, schema, role string) error {
	if b.cfg.StatementTimeoutMS > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, b.cfg.StatementTimeoutMS)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO %s`, q(schema))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL ROLE %s`, q(role))); err != nil {
		return err
	}
	return nil
}

// query runs a (already-validated, read-only) statement in a READ-ONLY
// transaction as the scope role. The read-only transaction is the write
// backstop: any write the validator missed (e.g. SELECT … INTO) fails at the
// engine.
func (b *postgresBackend) query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return nil, err
	}
	if err := b.provision(ctx, schema, role); err != nil {
		return nil, err
	}

	qctx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	tx, err := b.db.BeginTx(qctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // read-only — nothing to commit

	if err := b.enterScope(qctx, tx, schema, role); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows, b.cfg.MaxRows)
}

// exec runs a (already-validated, read-write) statement as the scope role.
// The quota is checked as the ADMIN (before dropping to the scope role) so the
// catalog read is unaffected by the role's restricted search_path; like the
// sqlite tier it is an approximate before-the-write bound.
func (b *postgresBackend) exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return nil, err
	}
	if err := b.provision(ctx, schema, role); err != nil {
		return nil, err
	}

	ectx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	tx, err := b.db.BeginTx(ectx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	if quota := effectiveQuota(b.cfg, quotaOverride); quota > 0 {
		size, err := pgSchemaSizeBytes(ectx, tx, schema)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
	}

	if err := b.enterScope(ectx, tx, schema, role); err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ectx, statement, args...)
	if err != nil {
		return nil, err
	}
	out := &ExecResult{}
	if n, err := r.RowsAffected(); err == nil {
		out.RowsAffected = n
	}
	// Postgres has no implicit last-insert id (use RETURNING); leave 0.
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// pgSchemaSizeBytes sums pg_total_relation_size over every table in the scope
// schema. schema is bound as a value ($1), not interpolated.
func pgSchemaSizeBytes(ctx context.Context, tx *sql.Tx, schema string) (int64, error) {
	var size sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(pg_total_relation_size(c.oid)), 0)
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind IN ('r','m','p')`, schema).Scan(&size)
	if err != nil {
		return 0, err
	}
	return size.Int64, nil
}

// dropRunScope removes an ephemeral run scope: DROP SCHEMA … CASCADE drops the
// scope's tables, then the role is dropped (DROP OWNED first clears any residual
// grants). Best-effort on the role (an orphaned empty role is harmless); the
// schema drop is the data removal. removed reports whether the schema existed.
func (b *postgresBackend) dropRunScope(runID string) (removed bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("sqlmem: empty run id")
	}
	schema, role, err := pgScopeNames(ScopeKey{Scope: runScope, ScopeID: runID})
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var existed bool
	_ = b.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name=$1)`, schema).Scan(&existed)

	// DROP SCHEMA … CASCADE removes everything the role owns (it has CREATE
	// only on its own schema), so a plain DROP ROLE then succeeds — no
	// DROP OWNED needed (and a CREATEROLE non-superuser admin cannot DROP
	// OWNED anyway).
	if _, err := b.db.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, q(schema))); err != nil {
		return false, fmt.Errorf("sqlmem: drop run schema: %w", err)
	}
	dropRole := fmt.Sprintf(`DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname=%s) THEN DROP ROLE %s; END IF; END $$`, lit(role), q(role))
	if _, err := b.db.ExecContext(ctx, dropRole); err != nil {
		log.Printf("sqlmem: drop run-scope role (schema already dropped): %v", err)
	}

	b.mu.Lock()
	delete(b.provisioned, schema)
	b.mu.Unlock()
	return existed, nil
}

// close closes the aux connection pool.
func (b *postgresBackend) close() error { return b.db.Close() }

// isAlreadyExists reports whether err is a postgres duplicate-object error —
// tolerated during the idempotent / cross-replica provisioning DDL.
func isAlreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P06", // duplicate_schema
			"42710", // duplicate_object (role)
			"42P07", // duplicate_table
			"23505": // unique_violation (concurrent catalog insert)
			return true
		}
	}
	return false
}
