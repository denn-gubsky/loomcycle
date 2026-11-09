package sqlmem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// sqlite.go — the sqlite tier of SQL Memory: one .db file per scope under a
// root directory (and a .doc.db beside it for the Document tool's tables, see
// sqlite_planes.go), with an LRU of open handles. This is the Phase-1 backend;
// it backs a sqlite deployment (StorageConfig.Backend != "postgres"). The
// postgres tier (schema-per-scope) lives in postgres.go. Both satisfy the
// package-internal `backend` interface the Manager facade delegates to.

// maxOpenHandles caps the number of live *sql.DB handles the sqlite backend
// keeps open. On overflow the least-recently-used handle is closed. 64 is a
// generous ceiling for the per-scope-file model: each scope opens its own
// file, and a busy multi-tenant runtime touches far fewer than 64 distinct
// scopes within any short window. Closing an LRU handle is safe — the next
// access reopens it.
const maxOpenHandles = 64

// sqliteBackend owns the per-scope sqlite databases. Each scope (agent/user/
// run, keyed by tenant) maps to one .db file under root, plus a .doc.db file for
// its document plane once the Document tool uses it; it opens at most
// maxOpenHandles handles at a time, closing the least-recently-used on
// overflow. Safe for concurrent use.
//
// The backend is deliberately SEPARATE from the main loomcycle store: an
// agent's arbitrary SQL can only ever reach its own scope file, never the
// runtime's operational database.
type sqliteBackend struct {
	cfg Config

	mu    sync.Mutex
	open  map[string]*openHandle // keyed by resolved file path
	clock uint64                 // monotonic LRU stamp

	// adoptLocks serialises the move of one scope's document tables into its
	// document file (sqlite_planes.go), keyed by the scope's agent file.
	adoptLocks map[string]*sync.Mutex

	// adoptStep, when set, is called at each step of a scope's move into its
	// document file and stops the move on an error: tests use it to leave the move
	// where a crash at that step would.
	adoptStep func(step string) error
}

// The steps of a move that adoptStep sees.
const (
	adoptStepCopied = "copied" // rows copied, split recorded, not yet committed
	adoptStepMarked = "marked" // committed; the agent file's tables not yet dropped
	// adoptStepHeld: the tables to move are known, the scope's files not yet pinned.
	adoptStepHeld = "held"
	// adoptStepRestored: a restore's document rows and mark written, not committed.
	adoptStepRestored = "restored"
)

// openHandle is one live scope database plus its LRU stamp and an in-flight
// reference count. inUse > 0 means a query/exec is currently running against
// this handle, so eviction must NOT close it (a close mid-query, or between an
// exec's quota-check and its write, would surface as sql.ErrConnDone).
type openHandle struct {
	db    *sql.DB
	used  uint64
	inUse int
}

// newSQLiteBackend constructs the sqlite backend and ensures Root exists.
func newSQLiteBackend(cfg Config) (*sqliteBackend, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("sqlmem: empty Root")
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("sqlmem: mkdir root %q: %w", cfg.Root, err)
	}
	// Hold the root as its real path. WalkDir does not descend a root that
	// is itself a symlink, so the scope listing (and the snapshot, retention
	// and erasure built on it) and the sweeps saw nothing. Resolving here,
	// once, keeps every path in one form: the walks, keyPath, the open-handle
	// keys the sweeps check for in-use scopes, and the removal fences.
	real, err := filepath.EvalSymlinks(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: resolve root %q: %w", cfg.Root, err)
	}
	cfg.Root = real
	return &sqliteBackend{
		cfg:  cfg,
		open: make(map[string]*openHandle),
	}, nil
}

// sanitizeRe matches the characters allowed VERBATIM in a path segment.
// Anything outside it is replaced (see sanitize), so no separator or dot
// run can survive into the on-disk path.
var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitize maps an identifier to a single safe path segment. The inputs
// (tenant id, agent name, user id, run id) are charset-validated upstream,
// but the backend sanitizes defensively — it is the last line before a
// caller-influenced string becomes a filesystem path.
//
// Rules:
//   - empty → error (an empty segment would collapse the path).
//   - allowed charset [A-Za-z0-9_-] passes verbatim.
//   - any other character is replaced with '_', AND a short hash of the
//     ORIGINAL is appended — so two distinct ids that sanitize to the same
//     replaced form ("a/b" and "a_b") never collide on the same file.
//
// Because every disallowed character (including '/', '.', and the '..'
// sequence) is replaced, the result can never contain a path separator or
// a parent-directory token: a malicious scope id like "../../etc/x" becomes
// a single inert segment, so the derived path stays under Root.
func sanitize(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("sqlmem: empty scope identifier")
	}
	clean := sanitizeRe.ReplaceAllString(s, "_")
	if clean != s {
		// Disambiguate: append a hash of the original so distinct inputs that
		// map to the same replaced form land on distinct files.
		sum := sha256.Sum256([]byte(s))
		clean = clean + "-" + hex.EncodeToString(sum[:6])
	}
	return clean, nil
}

// keyPath resolves the on-disk path of the key's PLANE of a scope under root.
//
//	durable:   <root>/<sanitize(tenant)>/<scope>/<sanitize(scope_id)>.db
//	ephemeral: <root>/run/<sanitize(scope_id)>.db   (scope == run)
//
// The document plane is the same path with docFileSuffix in place of
// scopeFileSuffix: <sanitize(scope_id)>.doc.db. sanitize never lets a '.' through,
// so no scope id can end in ".doc" and the two names never meet.
//
// The shared tenant "" sanitizes via the empty-check error for durable
// scopes; callers resolve a non-empty tenant (RunIdentity stamps "default"
// or a real id). A run scope is NOT tenant-keyed — run ids are globally
// unique — which is also what lets DropRunScope target a single subtree.
func (k ScopeKey) keyPath(root string) (string, error) {
	// Belt-and-braces alongside the postgres tier's check. sanitize() already makes
	// two distinct ids land on distinct FILES (it appends a hash of the original), so
	// this tier cannot collide the way pgScopeNames could — but validating here too
	// means a future Manager entry point that forgets the door check still cannot
	// derive storage from an ambiguous key.
	if err := k.validate(); err != nil {
		return "", err
	}
	id, err := sanitize(k.ScopeID)
	if err != nil {
		return "", err
	}
	if k.Scope == runScope {
		if strings.Contains(id, "..") {
			return "", errDotDotSegment
		}
		return filepath.Join(root, runScope, id+k.fileSuffix()), nil
	}
	tenant, err := sanitize(k.Tenant)
	if err != nil {
		return "", err
	}
	// Scope is a closed enum resolved server-side, but sanitize it too so a
	// future caller can never inject a separator through it.
	scope, err := sanitize(k.Scope)
	if err != nil {
		return "", err
	}
	// One plain check per segment, so static analysis sees the guard on each.
	if strings.Contains(tenant, "..") {
		return "", errDotDotSegment
	}
	if strings.Contains(scope, "..") {
		return "", errDotDotSegment
	}
	if strings.Contains(id, "..") {
		return "", errDotDotSegment
	}
	return filepath.Join(root, tenant, scope, id+k.fileSuffix()), nil
}

// The two files of a scope. The agent plane's keeps the name every scope had
// before the planes split, so a scope from then is opened where it lies.
const (
	scopeFileSuffix = ".db"
	docFileSuffix   = ".doc.db"
)

// fileSuffix is the file name ending of the key's plane.
func (k ScopeKey) fileSuffix() string {
	if k.isDoc() {
		return docFileSuffix
	}
	return scopeFileSuffix
}

// planeFiles returns both files of the key's scope, the agent file first.
func (k ScopeKey) planeFiles(root string) (agent, doc string, err error) {
	if agent, err = k.wholeScope().keyPath(root); err != nil {
		return "", "", err
	}
	dk := k
	dk.plane = planeDoc
	if doc, err = dk.keyPath(root); err != nil {
		return "", "", err
	}
	return agent, doc, nil
}

// scopeFileBase strips a scope file's plane suffix: both files of one scope map to
// the same base. ok is false for anything that is not a scope file: -wal, -shm,
// and any name whose id part holds a '.', which sanitize never produces — a
// quarantine file (<id>.leftover-<n>.db) is one.
func scopeFileBase(path string) (base string, doc, ok bool) {
	switch {
	case strings.HasSuffix(path, docFileSuffix):
		base, doc = strings.TrimSuffix(path, docFileSuffix), true
	case strings.HasSuffix(path, scopeFileSuffix):
		base = strings.TrimSuffix(path, scopeFileSuffix)
	default:
		return "", false, false
	}
	if strings.Contains(filepath.Base(base), ".") {
		return "", false, false
	}
	return base, doc, true
}

// quarantineFilesOf lists the quarantine files (sqlite_planes.go) beside a scope
// whose files share base. Read from the directory rather than globbed, so a root
// holding glob metacharacters cannot widen the match.
func quarantineFilesOf(base string) []string {
	dir, prefix := filepath.Dir(base), filepath.Base(base)+quarantineInfix
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, scopeFileSuffix) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// errDotDotSegment refuses a path segment holding "..". sanitize() keeps only
// A-Z a-z 0-9 _ - (anything else is replaced and the original hashed), so a
// sanitized segment can never hold one: this never refuses a real key. The
// checks are written in the form static analysis recognises as a traversal
// guard, so the guarantee is visible at the join rather than only inside
// sanitize.
var errDotDotSegment = errors.New(`sqlmem: scope path segment contains ".."`)

// dsnPragmas mirrors driver_default.go openDB: WAL + foreign-key enforcement
// + a 5-second busy timeout. SQL Memory uses the SAME pragma posture as the
// main store so a scope file behaves identically under contention.
//
// _defensive=1 turns on SQLITE_DBCONFIG_DEFENSIVE before anything else runs on
// the connection. An agent's SQL runs on the agent file's connection: without it
// a write to sqlite_dbpage rewrites any page of the file and writable_schema edits
// the schema, neither naming a table the Memory tool's guard could refuse. A scope
// from before the planes split still holds its document tables in that file until
// the Document tool moves them (sqlite_planes.go), and the document file gets the
// same posture. Reads of sqlite_dbpage are not stopped by it; the guard refuses
// those by name.
const dsnPragmas = "?_defensive=1&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"

// openScopeDB opens (creating the parent dir) the scope database at path
// with the hardened pragma set. SetMaxOpenConns(1): one writer per scope
// file. Each scope is a private, single-agent keyspace, so there is no
// within-scope concurrency to exploit — a single connection sidesteps
// SQLITE_BUSY entirely and keeps the WAL simple. Extension loading is NEVER
// enabled (the DSN carries no extension pragma, and the Go-layer validator
// already refuses load_extension).
func openScopeDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sqlmem: mkdir %q: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("sqlmem: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

// acquire returns the (cached or freshly opened) *sql.DB for path and pins it
// (inUse++) so a concurrent eviction cannot close it mid-use. The caller MUST
// call release(path) when done — pair them with `defer`. One acquisition spans
// a whole query, or a whole exec (quota-check + write), so neither half ever
// races eviction.
func (b *sqliteBackend) acquire(path string) (*sql.DB, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquireLocked(path)
}

// acquireLocked is acquire with b.mu held.
func (b *sqliteBackend) acquireLocked(path string) (*sql.DB, error) {
	b.clock++
	h, ok := b.open[path]
	if !ok {
		db, err := openScopeDB(path)
		if err != nil {
			return nil, err
		}
		h = &openHandle{db: db}
		b.open[path] = h
	}
	h.used = b.clock
	h.inUse++
	b.evictLocked()
	return h.db, nil
}

// release drops one in-flight reference taken by acquire.
func (b *sqliteBackend) release(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h, ok := b.open[path]; ok && h.inUse > 0 {
		h.inUse--
	}
}

// evictLocked closes the least-recently-used IDLE handle while the open set
// exceeds the cap. Caller must hold b.mu. An in-use handle (inUse > 0) is never
// a victim, so the set may briefly exceed the cap under heavy concurrency —
// that is correct (those handles are actively serving). Closing is best-effort.
func (b *sqliteBackend) evictLocked() {
	for len(b.open) > maxOpenHandles {
		var lruPath string
		var lruUsed uint64
		for p, h := range b.open {
			if h.inUse > 0 {
				continue // never evict an in-flight handle
			}
			if lruPath == "" || h.used < lruUsed {
				lruPath, lruUsed = p, h.used
			}
		}
		if lruPath == "" {
			return // every handle is in use — nothing evictable right now
		}
		if h, ok := b.open[lruPath]; ok {
			if err := h.db.Close(); err != nil {
				slog.Warn("sqlmem: close LRU handle failed", "path", lruPath, "err", err)
			}
			delete(b.open, lruPath)
		}
	}
}

// evictPathLocked closes and forgets a single open handle (used by
// dropRunScope before deleting the file). Caller must hold b.mu.
func (b *sqliteBackend) evictPathLocked(path string) {
	if h, ok := b.open[path]; ok {
		if err := h.db.Close(); err != nil {
			slog.Warn("sqlmem: close handle before drop failed", "path", path, "err", err)
		}
		delete(b.open, path)
	}
}

// sqliteLimitLength is SQLITE_LIMIT_LENGTH, the sqlite3_limit category that
// bounds the length of one string or BLOB value, and of a row.
const sqliteLimitLength = 0

// opConn takes the scope file's connection for one operation. When limited
// it runs with SQLITE_LIMIT_LENGTH lowered to valueLengthLimit, so a value an
// agent's statement builds (zeroblob, randomblob, printf, a concatenation) is
// refused by the engine as too big before it is allocated, instead of being
// built, copied into the Go heap and only then refused by the result cap.
//
// The limit is the connection's, and the connection is shared with the
// runtime's own statements on the same file (the Document tool's reads of an
// image chunk's bytes, larger than an agent may build), so release puts the
// previous limit back before the connection returns to the pool. A connection
// whose limit cannot be put back is discarded rather than reused. A limit that
// cannot be set fails the operation: the agent's statement never runs
// without it.
func (b *sqliteBackend) opConn(ctx context.Context, db *sql.DB, limited bool) (*sql.Conn, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !limited {
		return conn, func() { _ = conn.Close() }, nil
	}
	prev, err := sqlite.Limit(conn, sqliteLimitLength, valueLengthLimit(b.cfg))
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sqlmem: set the value length limit: %w", err)
	}
	return conn, func() {
		if _, err := sqlite.Limit(conn, sqliteLimitLength, prev); err != nil {
			slog.Warn("sqlmem: restore the value length limit failed; discarding the connection", "err", err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

// query runs a (already-validated, read-only) statement against the scope
// database. Rows beyond MaxRows, or past maxBytes (0 = none), are drained and
// Truncated is set. A capped read is an agent's statement, and runs under the
// value length limit (opConn).
func (b *sqliteBackend) query(ctx context.Context, key ScopeKey, statement string, args []any, maxBytes int) (*QueryResult, error) {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return nil, err
	}

	db, err := b.acquire(path)
	if err != nil {
		return nil, err
	}
	defer b.release(path)

	qctx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	conn, done, err := b.opConn(qctx, db, maxBytes > 0)
	if err != nil {
		return nil, err
	}
	defer done()

	rows, err := conn.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectRows(rows, b.cfg.MaxRows, maxBytes)
}

// exec runs a (already-validated, read-write) DDL/DML statement against the
// scope database. The quota is checked BEFORE the write (page_count *
// page_size): an approximate bound — a single statement can push the file
// past the quota, but the NEXT write then refuses, so growth is bounded to
// one statement's worth of overshoot.
func (b *sqliteBackend) exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return nil, err
	}

	// One acquisition spans the quota check AND the write, so eviction can't
	// close the handle between them.
	db, err := b.acquire(path)
	if err != nil {
		return nil, err
	}
	defer b.release(path)

	// Every write to the agent file is an agent's statement; the document
	// file's writes are the Document tool's own.
	conn, done, err := b.opConn(ctx, db, !key.isDoc())
	if err != nil {
		return nil, err
	}
	defer done()

	if quota := effectiveQuota(b.cfg, quotaOverride); quota > 0 {
		size, err := scopeSizeBytes(ctx, conn)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		other, err := b.otherPlaneBytes(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		size += other
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
	}

	ectx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	r, err := conn.ExecContext(ectx, statement, args...)
	if err != nil {
		return nil, err
	}
	out := &ExecResult{}
	if n, err := r.RowsAffected(); err == nil {
		out.RowsAffected = n
	}
	// LastInsertId is meaningless for non-INSERT statements; ignore its error.
	if id, err := r.LastInsertId(); err == nil {
		out.LastInsertID = id
	}
	return out, nil
}

// scopeSizeBytes returns what the scope database holds, as (page_count -
// freelist_count) * page_size: free pages are space the file keeps, not data the
// scope stores, and a scope whose documents moved out of its agent file must not
// be charged for the pages they left. Runs on either the scope handle (auto-commit)
// or an open transaction (rowQueryer). The pragmas are engine-internal (read-only
// here); they never touch agent data.
func scopeSizeBytes(ctx context.Context, q rowQueryer) (int64, error) {
	var pageCount, freePages, pageSize int64
	if err := q.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page_count: %w", err)
	}
	if err := q.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		return 0, fmt.Errorf("freelist_count: %w", err)
	}
	if err := q.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page_size: %w", err)
	}
	return (pageCount - freePages) * pageSize, nil
}

// beginTx pins the scope handle (so it is not evicted while the txn is open)
// and opens a transaction on it. release drops the pin. A transaction on the
// agent file is an agent's (sql_begin), and holds the value length limit for
// its life (opConn).
func (b *sqliteBackend) beginTx(ctx context.Context, key ScopeKey) (*sql.Tx, func(), error) {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return nil, nil, err
	}
	db, err := b.acquire(path)
	if err != nil {
		return nil, nil, err
	}
	conn, done, err := b.opConn(ctx, db, !key.isDoc())
	if err != nil {
		b.release(path)
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		done()
		b.release(path)
		return nil, nil, err
	}
	return tx, func() { done(); b.release(path) }, nil
}

// txnSizeBytes measures the scope's size on the open transaction, with the
// scope's other file as it stands on disk.
func (b *sqliteBackend) txnSizeBytes(ctx context.Context, tx *sql.Tx, key ScopeKey) (int64, error) {
	size, err := scopeSizeBytes(ctx, tx)
	if err != nil {
		return 0, err
	}
	other, err := b.otherPlaneBytes(ctx, key)
	if err != nil {
		return 0, err
	}
	return size + other, nil
}

// otherPlaneBytes is the size of what the scope's OTHER file holds, measured as
// scopeSizeBytes measures this one. The quota is the scope's, wherever its tables
// live, as on postgres (pgScopeSizeBytes): before the planes split, documents and
// the agent's tables shared one file and one quota, and moving the documents out
// must not hand the agent their share.
//
// It reads the other file on a short-lived connection of its own, not through the
// pool: each scope file has one pooled connection, and a transaction holding it
// (an agent's sql_begin, the Document tool's withSqlTxn) would block this check
// until it ended. A WAL reader never waits on a writer. Only a write under a quota
// pays for it.
func (b *sqliteBackend) otherPlaneBytes(ctx context.Context, key ScopeKey) (int64, error) {
	other := key
	other.plane = planeDoc
	if key.isDoc() {
		other.plane = planeAgent
	}
	path, err := other.keyPath(b.cfg.Root)
	if err != nil {
		return 0, err
	}
	if there, err := statExists(path); err != nil || !there {
		return 0, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	return scopeSizeBytes(ctx, db)
}

// fileFootprint is a scope file's size on disk with its -wal/-shm sidecars (in
// WAL mode un-checkpointed pages live in -wal, so the file alone undercounts an
// active scope). An absent file counts zero.
func fileFootprint(path string) int64 {
	var size int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			size += fi.Size()
		}
	}
	return size
}

// vectorsEnabled is always false for the sqlite tier (Phase 3c is postgres-only;
// sqlite-vec is deferred — see RFC AA).
func (b *sqliteBackend) vectorsEnabled() bool { return false }

// touchScope sets the mtime of the durable scope's files to now, so a
// read-only-but-live scope also counts as used (writes already bump mtime). Both
// files: the GC judges a scope by the newer of them, and a file that does not
// exist yet is skipped (the next write creates it with a fresh mtime).
func (b *sqliteBackend) touchScope(key ScopeKey) error {
	agent, doc, err := key.planeFiles(b.cfg.Root)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, path := range []string{agent, doc} {
		if err := os.Chtimes(path, now, now); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// durableScope is one durable scope as a sweep finds it on disk: its files (the
// agent file, the document file, or both), the newer of their mtimes and their
// summed footprint. The sweeps act on a scope, never on one of its files: GC that
// removed a scope's document file because only its agent file was in use would
// lose the documents of a live scope.
type durableScope struct {
	files  []string
	newest time.Time
	size   int64
}

// durableScopes walks the durable scope tree (everything EXCEPT <root>/run) and
// groups the scope files by scope, in a stable order.
func (b *sqliteBackend) durableScopes() ([]*durableScope, error) {
	runDir := filepath.Join(b.cfg.Root, runScope)
	byBase := map[string]*durableScope{}
	var order []string
	err := filepath.WalkDir(b.cfg.Root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if path == runDir {
				return filepath.SkipDir
			}
			return nil
		}
		base, _, ok := scopeFileBase(path)
		if !ok {
			return nil // skip -wal/-shm + anything else
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		sc := byBase[base]
		if sc == nil {
			sc = &durableScope{}
			byBase[base] = sc
			order = append(order, base)
		}
		sc.files = append(sc.files, path)
		if info.ModTime().After(sc.newest) {
			sc.newest = info.ModTime()
		}
		sc.size += fileFootprint(path)
		return nil
	})
	sort.Strings(order)
	out := make([]*durableScope, 0, len(order))
	for _, base := range order {
		out = append(out, byBase[base])
	}
	return out, err
}

// removeIfIdle evicts and fenced-removes every one of a scope's files, unless any
// of them is pinned by an in-flight op, an open transaction or a move (inUse>0):
// then it touches nothing and reports idle=false. Never evict a pinned handle — it
// would break the live op, exactly what the inUse refcount exists to prevent.
//
// The check and the removal are one step under b.mu, the lock a move pins the
// scope under (pinScopeFiles): a move either sees the files gone or pins them
// before they are removed, so it never writes into a scope being erased or
// recreates one.
//
// Each file is fenced under its OWN directory (tighter than <root>) so a
// symlinked file can't widen the delete to another tenant's subtree. removed
// reports whether any file was deleted.
func (b *sqliteBackend) removeIfIdle(files []string) (idle, removed bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, path := range files {
		if h, ok := b.open[path]; ok && h.inUse > 0 {
			return false, false, nil
		}
	}
	for _, path := range files {
		b.evictPathLocked(path)
	}
	for _, path := range files {
		r, rerr := fencedRemoveDB(filepath.Dir(path), path)
		if rerr != nil && err == nil {
			err = rerr
		}
		removed = removed || r
	}
	// The scope's quarantine files go with it: they hold its rows too.
	seen := map[string]bool{}
	for _, path := range files {
		base, _, ok := scopeFileBase(path)
		if !ok || seen[base] {
			continue
		}
		seen[base] = true
		for _, q := range quarantineFilesOf(base) {
			if _, rerr := fencedRemoveDB(filepath.Dir(q), q); rerr != nil && err == nil {
				err = rerr
			}
			_ = os.Remove(q + "-journal")
		}
	}
	return true, removed, err
}

// sweepStale fenced-removes each durable scope whose files were last used before
// cutoff. The run subtree is skipped — run scopes drop at run-end, never via GC.
func (b *sqliteBackend) sweepStale(cutoff time.Time) (int, error) {
	scopes, err := b.durableScopes()
	var dropped int
	for _, sc := range scopes {
		if !sc.newest.Before(cutoff) {
			continue
		}
		// An in-use scope is skipped; the next sweep catches it once idle.
		if _, removed, rerr := b.removeIfIdle(sc.files); rerr == nil && removed {
			dropped++
		}
	}
	return dropped, err
}

// sweepBudget drops the LARGEST idle durable scopes until the aggregate durable
// footprint is at or under budget (Phase 3f size-based GC). A scope's size is the
// true on-disk footprint of both its files with their sidecars. In-use scopes are
// skipped — the next sweep catches them once idle — and the run subtree is never
// counted or dropped.
func (b *sqliteBackend) sweepBudget(budget int64) (int, error) {
	scopes, err := b.durableScopes()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, sc := range scopes {
		total += sc.size
	}
	if total <= budget {
		return 0, nil
	}
	sort.SliceStable(scopes, func(i, j int) bool { return scopes[i].size > scopes[j].size })
	var dropped int
	for _, sc := range scopes {
		if total <= budget {
			break
		}
		// A pinned scope (live op / open txn / move) is skipped; the next sweep
		// catches it once idle.
		if _, removed, rerr := b.removeIfIdle(sc.files); rerr == nil && removed {
			dropped++
			total -= sc.size
		}
	}
	return dropped, nil
}

// dropRunScope closes+evicts the handles of the run/<runID> files and removes
// them (plus -wal/-shm sidecars) behind a fence: the resolved target must sit
// STRICTLY inside <Root>/run and not equal <Root>/run itself, so a malformed run
// id can never widen the delete. Best-effort; mirrors the VolumeDef
// ephemeral-purge fence rooted at <Root>/run. removed reports whether a file was
// actually deleted (false when it was already gone).
func (b *sqliteBackend) dropRunScope(runID string) (removed bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("sqlmem: empty run id")
	}
	key := ScopeKey{Scope: runScope, ScopeID: runID}
	agent, doc, err := key.planeFiles(b.cfg.Root)
	if err != nil {
		return false, err
	}

	b.mu.Lock()
	b.evictPathLocked(agent)
	b.evictPathLocked(doc)
	b.mu.Unlock()

	runRoot := filepath.Join(b.cfg.Root, runScope)
	for _, path := range []string{agent, doc} {
		r, rerr := fencedRemoveDB(runRoot, path)
		if rerr != nil && err == nil {
			err = rerr
		}
		removed = removed || r
	}
	return removed, err
}

// dropScope removes one DURABLE (agent/user) scope's files (and their -wal/-shm
// sidecars) behind a path fence. The paths are RE-DERIVED from (Root, tenant,
// scope, sanitize(scope_id)) — never a stored path — and fenced under the scope's
// OWN directory (mirrors sweepStale) so a symlinked file can't widen the delete to
// another tenant's subtree. An in-use handle (inUse>0) on either file is REFUSED
// with an error (not silently skipped) so the RFC BM caller retries next tick
// rather than assuming the scope is gone and dropping its sibling memory/dirents.
func (b *sqliteBackend) dropScope(key ScopeKey) (removed bool, err error) {
	if key.Scope == runScope {
		return false, fmt.Errorf("sqlmem: dropScope is not for the run scope")
	}
	agent, doc, err := key.planeFiles(b.cfg.Root)
	if err != nil {
		return false, err
	}
	idle, removed, err := b.removeIfIdle([]string{agent, doc})
	if !idle {
		return false, fmt.Errorf("sqlmem: scope %s/%s in use", key.Scope, key.ScopeID)
	}
	return removed, err
}

// fencedRemoveDB removes a single scope .db file (and its -wal/-shm
// sidecars) only after proving the resolved path is strictly inside
// expectedRoot. It NEVER trusts a stored path — path is re-derived by the
// caller from (Root, run, sanitize(run_id)). A non-existent target is a
// no-op success (removed=false). On a fence failure it logs and returns an
// error WITHOUT deleting.
func fencedRemoveDB(expectedRoot, path string) (removed bool, err error) {
	rootResolved, err := filepath.EvalSymlinks(expectedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			// The run-scope root was never created — nothing to drop.
			return false, nil
		}
		return false, fmt.Errorf("sqlmem: resolve run root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // already gone
		}
		return false, fmt.Errorf("sqlmem: resolve target: %w", err)
	}
	// Strictly inside expectedRoot and not equal to it.
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Error("sqlmem: DropRunScope REFUSED: path does not resolve strictly inside the run-scope root", "path", path, "root", rootResolved)
		return false, fmt.Errorf("sqlmem: refusing to delete %q — not inside the run-scope root", resolved)
	}
	if resolved == rootResolved {
		slog.Error("sqlmem: DropRunScope REFUSED: resolved path is the run-scope root itself")
		return false, fmt.Errorf("sqlmem: refusing to delete the run-scope root")
	}
	// Remove the file and its WAL sidecars. RemoveAll tolerates absent
	// sidecars; we OR the primary file's removal into `removed`.
	if err := os.Remove(resolved); err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("sqlmem: remove %q: %w", resolved, err)
		}
	} else {
		removed = true
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		// Sidecars sit next to the resolved primary file; deriving them from
		// `resolved` keeps them inside the same fenced directory.
		_ = os.Remove(resolved + suffix)
	}
	return removed, nil
}

// close closes every open scope handle. Best-effort: the first close error
// is returned but every handle is still attempted.
func (b *sqliteBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var firstErr error
	for path, h := range b.open {
		if err := h.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(b.open, path)
	}
	return firstErr
}
