package sqlmem

import (
	"context"
	"crypto/hmac"
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// postgres.go — the postgres tier of SQL Memory: a SCHEMA per scope inside a
// SEPARATE aux database (LOOMCYCLE_SQLMEM_PG_DSN, distinct from the main-store
// DSN). Isolation is engine-enforced least privilege.
//
// THE ISOLATION MODEL (and why an earlier `SET LOCAL ROLE`-from-a-shared-admin
// design was abandoned). Each scope gets its OWN postgres LOGIN role and the
// runtime runs the agent's SQL on a DEDICATED connection authenticated AS that
// role — so the agent's `session_user` IS the scope role. That is the load-
// bearing property: a scope role is a member of NOTHING, so every postgres
// role-switch primitive (`SET ROLE`, a function's `SET role` attribute,
// `set_config('role',…)`, `RESET ROLE`) is checked against `session_user` (the
// scope role) and fails to reach any OTHER scope — there is no shared admin in
// the agent's session to pivot through. A prior design that connected as one
// shared admin and `SET LOCAL ROLE`d down to the scope role was BROKEN: `SET
// LOCAL ROLE` changes `current_user` but NOT `session_user`, and the admin had
// to be a `WITH SET` member of every scope role, so an agent could pivot into
// any scope via a `SET role` function clause. With session_user = scope role
// that whole class is gone.
//
// The scope role is a non-superuser LOGIN role (a derived password only this
// runtime can compute; the agent has no network path to the aux DB),
// NOCREATEDB/NOCREATEROLE/NOINHERIT, with USAGE only on its own schema (PUBLIC
// revoked). It therefore cannot reach another
// scope's schema, read host files, run programs, load extensions, or connect out
// (engine-denied). search_path + statement_timeout are baked onto the role.
// sql_query runs in a READ-ONLY transaction (the write backstop). The
// validate_postgres.go denies are defense-in-depth.
//
// The operator-provisioned ADMIN role (CREATEROLE + CREATE on the aux DB) is
// used ONLY to provision/drop scopes and never to run agent SQL — so the
// admin's authority is never reachable from an agent statement. Per-scope role
// passwords are derived HMAC(aux-admin-password, role-name) so every replica
// computes the same value without coordination (multi-replica), and the agent
// has no network path to the aux DB regardless.

// pgAdminMaxConns bounds the admin pool (provisioning + drop only — not the hot
// path). pgScopeConnLRU bounds how many per-scope connection pools stay open at
// once; the least-recently-used idle one is closed on overflow (mirrors the
// sqlite handle LRU). Each is a separate database, so a busy runtime touches far
// fewer distinct scopes than the cap within any short window.
const (
	pgAdminMaxConns = 4
	pgScopeConnLRU  = 32
)

// scopeConn is one live per-scope connection pool (authenticated as the scope
// role) plus its LRU stamp, in-flight refcount, and the stdlib registration key
// to unregister on close.
type scopeConn struct {
	db      *sql.DB
	connStr string
	used    uint64
	inUse   int
}

// postgresBackend implements `backend` over the aux database.
type postgresBackend struct {
	cfg     Config
	admin   *sql.DB         // admin pool — provisioning/drop ONLY, never agent SQL
	baseCfg *pgx.ConnConfig // parsed admin DSN; template for per-scope DSNs
	secret  []byte          // HMAC key for per-scope role passwords (= admin password)
	pg16    bool

	mu          sync.Mutex
	provisioned map[string]bool        // schema names provisioned in THIS process
	provLocks   map[string]*sync.Mutex // per-schema provisioning lock (parallelize distinct scopes)
	scopes      map[string]*scopeConn  // per-scope connection pools, keyed by role name
	clock       uint64
}

// NewPostgres constructs a Manager backed by the postgres tier. It opens the
// admin pool, verifies connectivity, and detects the server version. The DSN
// must point at a SEPARATE database from the main loomcycle store, reached by a
// non-superuser admin role with CREATE on that database and CREATEROLE, and it
// must use PASSWORD authentication (the password keys per-scope role
// credentials). See docs/SQL_MEMORY.md for the provisioning recipe.
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
	baseCfg, err := pgx.ParseConfig(cfg.PgDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: parse aux DSN: %w", err)
	}
	if baseCfg.Password == "" {
		return nil, fmt.Errorf("sqlmem: postgres tier requires the aux DSN to use password authentication (the admin password keys per-scope role credentials)")
	}

	admin := stdlib.OpenDB(*baseCfg)
	admin.SetMaxOpenConns(pgAdminMaxConns)
	admin.SetMaxIdleConns(pgAdminMaxConns / 2)
	admin.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var verNum int
	if err := admin.QueryRowContext(pingCtx, "SELECT current_setting('server_version_num')::int").Scan(&verNum); err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("sqlmem: connect aux postgres: %w", err)
	}
	return &postgresBackend{
		cfg:         cfg,
		admin:       admin,
		baseCfg:     baseCfg,
		secret:      []byte(baseCfg.Password),
		pg16:        verNum >= 160000,
		provisioned: make(map[string]bool),
		provLocks:   make(map[string]*sync.Mutex),
		scopes:      make(map[string]*scopeConn),
	}, nil
}

// pgIdentRe is the shape EVERY interpolated identifier must match before it is
// placed into DDL (pgScopeNames produces exactly this). Hex-only names are
// injection-proof by construction; the assertion + q() quoting are belt-and-
// braces.
var pgIdentRe = regexp.MustCompile(`^sqlmem_[sr]_[0-9a-f]{32}$`)

// pgScopeNames derives the (schema, role) identifier pair for a ScopeKey by
// hashing a canonical key string. Durable scopes hash (tenant, scope, scope_id)
// — matching the sqlite tier's tenant-keyed durable files; the ephemeral run
// scope hashes (run, run_id) WITHOUT tenant (run ids are globally unique). The
// 128-bit hash makes collisions negligible and the names fit Postgres's 63-char
// identifier limit. Components cannot contain the 0x1f separator (tenant/scope/
// scope_id are charset-validated upstream), so the join is unambiguous.
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
		return "", "", fmt.Errorf("sqlmem: derived identifier failed validation")
	}
	return schema, role, nil
}

// q double-quotes a postgres identifier. Scope identifiers are validated hex
// (pgScopeNames); the base DB name is operator-trusted; the escaping is
// belt-and-braces.
func q(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// lit renders a single-quoted string literal (a role name in a DO block, or the
// derived password). Inputs are validated hex; escaping is belt-and-braces.
func lit(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// derivePassword computes the per-scope role password as
// HMAC-SHA256(admin-password, role-name). Deterministic across replicas (no
// coordination) and unknown without the admin password.
func (b *postgresBackend) derivePassword(role string) string {
	m := hmac.New(sha256.New, b.secret)
	m.Write([]byte(role))
	return hex.EncodeToString(m.Sum(nil))
}

// provLock returns the per-schema provisioning mutex (created on first use), so
// concurrent first-touches of the SAME scope serialize while DISTINCT scopes
// provision in parallel.
func (b *postgresBackend) provLock(schema string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.provLocks[schema]
	if !ok {
		l = &sync.Mutex{}
		b.provLocks[schema] = l
	}
	return l
}

// provision lazily creates the scope schema + its dedicated LOGIN role (USAGE on
// its own schema only; search_path + statement_timeout baked in; password set to
// the derived value so the runtime can authenticate as it). Idempotent and
// tolerant of the cross-replica race; cached per process. The DDL runs on the
// admin pool in autocommit (NOT inside a caller's transaction).
func (b *postgresBackend) provision(ctx context.Context, schema, role string) error {
	b.mu.Lock()
	done := b.provisioned[schema]
	b.mu.Unlock()
	if done {
		return nil
	}

	// Serialize provisioning of THIS schema only (distinct scopes parallelize).
	lk := b.provLock(schema)
	lk.Lock()
	defer lk.Unlock()
	b.mu.Lock()
	done = b.provisioned[schema]
	b.mu.Unlock()
	if done {
		return nil
	}

	if err := b.runProvisionDDL(ctx, schema, role); err != nil {
		return err // NOT cached → the next op retries (self-heal)
	}
	b.mu.Lock()
	b.provisioned[schema] = true
	b.mu.Unlock()
	return nil
}

// runProvisionDDL executes the provisioning statements with a small bounded
// retry on transient serialization errors (concurrent cross-replica first-touch
// can deadlock on catalog rows). Duplicate-object errors are swallowed.
func (b *postgresBackend) runProvisionDDL(ctx context.Context, schema, role string) error {
	pw := b.derivePassword(role)
	createRole := fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname=%s) THEN CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION; END IF; END $$`,
		lit(role), q(role), lit(pw),
	)
	stmts := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, q(schema)),
		createRole,
		// Keep the role password in sync with the current derivation (idempotent).
		fmt.Sprintf(`ALTER ROLE %s WITH PASSWORD %s`, q(role), lit(pw)),
		fmt.Sprintf(`ALTER ROLE %s SET search_path TO %s`, q(role), q(schema)),
		fmt.Sprintf(`REVOKE ALL ON SCHEMA %s FROM PUBLIC`, q(schema)),
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, q(schema), q(role)),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, q(b.baseCfg.Database), q(role)),
	}
	if b.cfg.StatementTimeoutMS > 0 {
		stmts = append(stmts, fmt.Sprintf(`ALTER ROLE %s SET statement_timeout TO '%dms'`, q(role), b.cfg.StatementTimeoutMS))
	}

	for attempt := 0; ; attempt++ {
		err := b.execProvisionStmts(ctx, stmts)
		if err == nil {
			return nil
		}
		if isTransientRace(err) && attempt < 2 {
			continue // another replica is concurrently provisioning; converge
		}
		return fmt.Errorf("sqlmem: provision scope: %w", err)
	}
}

func (b *postgresBackend) execProvisionStmts(ctx context.Context, stmts []string) error {
	for _, s := range stmts {
		if _, err := b.admin.ExecContext(ctx, s); err != nil && !isAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// openScopeDB registers a per-scope pgx config (admin DSN with the user/password
// swapped to the scope role) and opens a small pool against it. Returns the
// stdlib registration key so close() can unregister it.
func (b *postgresBackend) openScopeDB(role string) (*sql.DB, string, error) {
	scfg := b.baseCfg.Copy()
	scfg.User = role
	scfg.Password = b.derivePassword(role)
	connStr := stdlib.RegisterConnConfig(scfg)
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		stdlib.UnregisterConnConfig(connStr)
		return nil, "", fmt.Errorf("sqlmem: open scope conn: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, connStr, nil
}

// acquireScope returns the (cached or freshly opened) per-scope *sql.DB and pins
// it (inUse++) so eviction can't close it mid-op. Pair with releaseScope.
func (b *postgresBackend) acquireScope(role string) (*sql.DB, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clock++
	sc, ok := b.scopes[role]
	if !ok {
		db, connStr, err := b.openScopeDB(role)
		if err != nil {
			return nil, err
		}
		sc = &scopeConn{db: db, connStr: connStr}
		b.scopes[role] = sc
	}
	sc.used = b.clock
	sc.inUse++
	b.evictScopesLocked()
	return sc.db, nil
}

func (b *postgresBackend) releaseScope(role string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sc, ok := b.scopes[role]; ok && sc.inUse > 0 {
		sc.inUse--
	}
}

// evictScopesLocked closes the least-recently-used IDLE scope pool while the set
// exceeds the cap. Caller holds b.mu. An in-use pool is never evicted.
func (b *postgresBackend) evictScopesLocked() {
	for len(b.scopes) > pgScopeConnLRU {
		var lru string
		var lruUsed uint64
		for role, sc := range b.scopes {
			if sc.inUse > 0 {
				continue
			}
			if lru == "" || sc.used < lruUsed {
				lru, lruUsed = role, sc.used
			}
		}
		if lru == "" {
			return // every pool is in use
		}
		b.closeScopeLocked(lru)
	}
}

// closeScopeLocked closes + forgets one scope pool and unregisters its config.
// Caller holds b.mu.
func (b *postgresBackend) closeScopeLocked(role string) {
	sc, ok := b.scopes[role]
	if !ok {
		return
	}
	if err := sc.db.Close(); err != nil {
		log.Printf("sqlmem: close scope pool %s: %v", role, err)
	}
	stdlib.UnregisterConnConfig(sc.connStr)
	delete(b.scopes, role)
}

// invalidate forgets a scope's provisioned flag and closes its pool, so the next
// op re-provisions + reconnects. Used to self-heal an auth failure (e.g. a role
// whose password drifted from the current derivation).
func (b *postgresBackend) invalidate(schema, role string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.provisioned, schema)
	b.closeScopeLocked(role)
}

// query runs a (already-validated, read-only) statement on the scope connection
// in a READ-ONLY transaction (the write backstop). No SET LOCAL ROLE: the
// connection's session_user IS the scope role.
func (b *postgresBackend) query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return nil, err
	}
	if err := b.provision(ctx, schema, role); err != nil {
		return nil, err
	}
	db, err := b.acquireScope(role)
	if err != nil {
		return nil, err
	}
	defer b.releaseScope(role)

	qctx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	tx, err := db.BeginTx(qctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, b.healAuth(schema, role, err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows, b.cfg.MaxRows)
}

// exec runs a (already-validated, read-write) statement on the scope connection.
// The quota is checked first (the scope role can read its own schema's sizes).
func (b *postgresBackend) exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	schema, role, err := pgScopeNames(key)
	if err != nil {
		return nil, err
	}
	if err := b.provision(ctx, schema, role); err != nil {
		return nil, err
	}
	db, err := b.acquireScope(role)
	if err != nil {
		return nil, err
	}
	defer b.releaseScope(role)

	ectx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	tx, err := db.BeginTx(ectx, nil)
	if err != nil {
		return nil, b.healAuth(schema, role, err)
	}
	defer func() { _ = tx.Rollback() }()

	if quota := effectiveQuota(b.cfg, quotaOverride); quota > 0 {
		size, err := pgSchemaSizeBytes(ectx, tx, schema)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
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

// healAuth self-heals a wedged scope on an authentication failure (a role whose
// password drifted from the derivation, e.g. after a partial provision): it
// invalidates the cache so the next op re-provisions, and returns the error.
func (b *postgresBackend) healAuth(schema, role string, err error) error {
	if isAuthError(err) {
		b.invalidate(schema, role)
	}
	return err
}

// pgSchemaSizeBytes sums pg_total_relation_size over every table in the scope
// schema. schema is bound as a value ($1), not interpolated. Runs as the scope
// role (it can read its own relations' sizes).
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

// dropRunScope removes an ephemeral run scope: close the scope pool, then (as
// admin) DROP SCHEMA … CASCADE and DROP ROLE. removed reflects the actual schema
// drop (3F000 undefined_schema => already gone). Run scopes are single-replica
// (the owning replica's run-completion path calls this), so the role has no
// other live sessions.
func (b *postgresBackend) dropRunScope(runID string) (removed bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("sqlmem: empty run id")
	}
	schema, role, err := pgScopeNames(ScopeKey{Scope: runScope, ScopeID: runID})
	if err != nil {
		return false, err
	}

	// Close this scope's pool so the role has no live sessions before DROP ROLE.
	b.mu.Lock()
	b.closeScopeLocked(role)
	delete(b.provisioned, schema)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	removed = true
	if _, err := b.admin.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA %s CASCADE`, q(schema))); err != nil {
		if isUndefinedSchema(err) {
			removed = false // already gone
		} else {
			return false, fmt.Errorf("sqlmem: drop run schema: %w", err)
		}
	}
	// Best-effort role drop (schema CASCADE already removed the data). Terminate
	// any lingering backend first so DROP ROLE doesn't fail on a live session,
	// and REVOKE the database CONNECT grant so the role has no remaining
	// dependency (else DROP ROLE fails 2BP01 dependent_objects_still_exist).
	_, _ = b.admin.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, role)
	dropRole := fmt.Sprintf(
		`DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname=%s) THEN REVOKE ALL ON DATABASE %s FROM %s; DROP ROLE %s; END IF; END $$`,
		lit(role), q(b.baseCfg.Database), q(role), q(role),
	)
	if _, err := b.admin.ExecContext(ctx, dropRole); err != nil {
		log.Printf("sqlmem: drop run-scope role (schema already dropped): %v", err)
	}
	return removed, nil
}

// close closes every scope pool (unregistering its config) and the admin pool.
func (b *postgresBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for role := range b.scopes {
		b.closeScopeLocked(role)
	}
	return b.admin.Close()
}

// isAlreadyExists reports a postgres duplicate-object error — tolerated during
// idempotent / cross-replica provisioning.
func isAlreadyExists(err error) bool {
	return pgCodeIn(err, "42P06" /*dup schema*/, "42710" /*dup object/role*/, "42P07" /*dup table*/, "23505" /*unique_violation*/)
}

// isTransientRace reports a transient serialization error worth retrying during
// concurrent cross-replica provisioning (deadlock / lock-not-available).
func isTransientRace(err error) bool {
	return pgCodeIn(err, "40P01" /*deadlock*/, "55P03" /*lock_not_available*/, "40001" /*serialization_failure*/)
}

// isAuthError reports an authentication / authorization failure (a scope role
// whose password drifted from the derivation).
func isAuthError(err error) bool {
	return pgCodeIn(err, "28P01" /*invalid_password*/, "28000" /*invalid_authorization*/)
}

// isUndefinedSchema reports the "schema does not exist" error.
func isUndefinedSchema(err error) bool { return pgCodeIn(err, "3F000") }

func pgCodeIn(err error, codes ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return true
		}
	}
	return false
}
