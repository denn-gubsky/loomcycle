package sqlmem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// sqlite.go — the sqlite tier of SQL Memory: one .db file per scope under a
// root directory, with an LRU of open handles. This is the Phase-1 backend;
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
// run, keyed by tenant) maps to ONE .db file under root; it opens at most
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
}

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

// keyPath resolves the on-disk .db path for a ScopeKey under root.
//
//	durable:   <root>/<sanitize(tenant)>/<scope>/<sanitize(scope_id)>.db
//	ephemeral: <root>/run/<sanitize(scope_id)>.db   (scope == run)
//
// The shared tenant "" sanitizes via the empty-check error for durable
// scopes; callers resolve a non-empty tenant (RunIdentity stamps "default"
// or a real id). A run scope is NOT tenant-keyed — run ids are globally
// unique — which is also what lets DropRunScope target a single subtree.
func (k ScopeKey) keyPath(root string) (string, error) {
	id, err := sanitize(k.ScopeID)
	if err != nil {
		return "", err
	}
	if k.Scope == runScope {
		return filepath.Join(root, runScope, id+".db"), nil
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
	return filepath.Join(root, tenant, scope, id+".db"), nil
}

// dsnPragmas mirrors driver_default.go openDB: WAL + foreign-key enforcement
// + a 5-second busy timeout. SQL Memory uses the SAME pragma posture as the
// main store so a scope file behaves identically under contention.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"

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
				log.Printf("sqlmem: close LRU handle %q: %v", lruPath, err)
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
			log.Printf("sqlmem: close handle %q before drop: %v", path, err)
		}
		delete(b.open, path)
	}
}

// query runs a (already-validated, read-only) statement against the scope
// database. Rows beyond MaxRows are drained and Truncated is set.
func (b *sqliteBackend) query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
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

	rows, err := db.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectRows(rows, b.cfg.MaxRows)
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

	if quota := effectiveQuota(b.cfg, quotaOverride); quota > 0 {
		size, err := scopeSizeBytes(ctx, db)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
	}

	ectx, cancel := withTimeout(b.cfg, ctx)
	defer cancel()

	r, err := db.ExecContext(ectx, statement, args...)
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

// scopeSizeBytes returns the on-disk size of the scope database as
// page_count * page_size. Runs on either the scope handle (auto-commit) or an
// open transaction (rowQueryer). Both pragmas are engine-internal (read-only
// here); they never touch agent data.
func scopeSizeBytes(ctx context.Context, q rowQueryer) (int64, error) {
	var pageCount, pageSize int64
	if err := q.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page_count: %w", err)
	}
	if err := q.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// beginTx pins the scope handle (so it is not evicted while the txn is open)
// and opens a transaction on it. release drops the pin.
func (b *sqliteBackend) beginTx(ctx context.Context, key ScopeKey) (*sql.Tx, func(), error) {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return nil, nil, err
	}
	db, err := b.acquire(path)
	if err != nil {
		return nil, nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.release(path)
		return nil, nil, err
	}
	return tx, func() { b.release(path) }, nil
}

// txnSizeBytes measures the scope's size on the open transaction.
func (b *sqliteBackend) txnSizeBytes(ctx context.Context, tx *sql.Tx, key ScopeKey) (int64, error) {
	return scopeSizeBytes(ctx, tx)
}

// vectorsEnabled is always false for the sqlite tier (Phase 3c is postgres-only;
// sqlite-vec is deferred — see RFC AA).
func (b *sqliteBackend) vectorsEnabled() bool { return false }

// touchScope sets the durable scope's .db mtime to now, so a read-only-but-live
// scope also counts as used (writes already bump mtime). A not-yet-created file
// is a no-op (the next write creates it with a fresh mtime).
func (b *sqliteBackend) touchScope(key ScopeKey) error {
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sweepStale walks the durable scope tree (everything EXCEPT <root>/run) and
// fenced-removes each .db whose mtime is before cutoff. The run subtree is
// skipped — run scopes drop at run-end, never via GC.
func (b *sqliteBackend) sweepStale(cutoff time.Time) (int, error) {
	runDir := filepath.Join(b.cfg.Root, runScope)
	var dropped int
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
		if !strings.HasSuffix(path, ".db") {
			return nil // skip -wal/-shm + anything else
		}
		info, ierr := d.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			return nil
		}
		// Skip a scope that is actively in use — an in-flight Query/Exec OR an
		// open explicit transaction pins the handle (inUse>0). Never evict/remove
		// a pinned handle (it would break the live op, exactly what the inUse
		// refcount exists to prevent); the next sweep catches it once idle.
		b.mu.Lock()
		if h, ok := b.open[path]; ok && h.inUse > 0 {
			b.mu.Unlock()
			return nil
		}
		b.evictPathLocked(path)
		b.mu.Unlock()
		// Fence under the scope's OWN directory (tighter than <root>) so a
		// symlinked .db can't widen the delete to another tenant's subtree.
		if removed, rerr := fencedRemoveDB(filepath.Dir(path), path); rerr == nil && removed {
			dropped++
		}
		return nil
	})
	return dropped, err
}

// sweepBudget drops the LARGEST idle durable scopes until the aggregate durable
// footprint is at or under budget (Phase 3f size-based GC). Size is the true
// on-disk footprint — the .db file PLUS its -wal/-shm sidecars (in WAL mode
// un-checkpointed pages live in -wal, so the .db alone undercounts an active
// scope). In-use scopes (inUse>0) are skipped — the next sweep catches them once
// idle — and the run subtree is never counted or dropped.
func (b *sqliteBackend) sweepBudget(budget int64) (int, error) {
	runDir := filepath.Join(b.cfg.Root, runScope)
	type scopeFile struct {
		path string
		size int64
	}
	var files []scopeFile
	var total int64
	err := filepath.WalkDir(b.cfg.Root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if path == runDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".db") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		size := info.Size()
		for _, suffix := range []string{"-wal", "-shm"} {
			if si, serr := os.Stat(path + suffix); serr == nil {
				size += si.Size()
			}
		}
		files = append(files, scopeFile{path: path, size: size})
		total += size
		return nil
	})
	if err != nil {
		return 0, err
	}
	if total <= budget {
		return 0, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	var dropped int
	for _, f := range files {
		if total <= budget {
			break
		}
		b.mu.Lock()
		if h, ok := b.open[f.path]; ok && h.inUse > 0 {
			b.mu.Unlock()
			continue // pinned by a live op / open txn — skip, catch it next sweep
		}
		b.evictPathLocked(f.path)
		b.mu.Unlock()
		if removed, rerr := fencedRemoveDB(filepath.Dir(f.path), f.path); rerr == nil && removed {
			dropped++
			total -= f.size
		}
	}
	return dropped, nil
}

// dropRunScope closes+evicts the handle for the run/<runID>.db file and
// removes it (plus -wal/-shm sidecars) behind a fence: the resolved target
// must sit STRICTLY inside <Root>/run and not equal <Root>/run itself, so a
// malformed run id can never widen the delete. Best-effort; mirrors the
// VolumeDef ephemeral-purge fence rooted at <Root>/run. removed reports
// whether a file was actually deleted (false when it was already gone).
func (b *sqliteBackend) dropRunScope(runID string) (removed bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("sqlmem: empty run id")
	}
	key := ScopeKey{Scope: runScope, ScopeID: runID}
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		return false, err
	}

	b.mu.Lock()
	b.evictPathLocked(path)
	b.mu.Unlock()

	runRoot := filepath.Join(b.cfg.Root, runScope)
	return fencedRemoveDB(runRoot, path)
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
		log.Printf("sqlmem: DropRunScope REFUSED: %q does not resolve strictly inside %q", path, rootResolved)
		return false, fmt.Errorf("sqlmem: refusing to delete %q — not inside the run-scope root", resolved)
	}
	if resolved == rootResolved {
		log.Printf("sqlmem: DropRunScope REFUSED: resolved path is the run-scope root itself")
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
