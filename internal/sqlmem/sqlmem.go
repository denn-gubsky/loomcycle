package sqlmem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Config configures the SQL Memory Manager. All bounds default to "off"
// when zero EXCEPT the ones with sensible runtime defaults the caller is
// expected to supply (see cmd/loomcycle wiring); the Manager itself treats
// a zero StatementTimeoutMS / MaxRows / QuotaBytes as "no bound".
type Config struct {
	// Root is the parent directory under which every per-scope .db file
	// lives (<DataDir>/sqlmem by convention). Required.
	Root string
	// QuotaBytes caps a single scope file's on-disk size. 0 = no quota.
	// Checked BEFORE a write (PRAGMA page_count * page_size); an approximate
	// bound that can overshoot by at most one statement (see Exec).
	QuotaBytes int
	// StatementTimeoutMS bounds a single Query/Exec via context.WithTimeout.
	// 0 = no timeout.
	StatementTimeoutMS int
	// MaxRows caps the rows a Query returns; the remainder is drained and
	// Truncated is set. 0 = no cap.
	MaxRows int
}

// maxOpenHandles caps the number of live *sql.DB handles the Manager keeps
// open. On overflow the least-recently-used handle is closed. 64 is a
// generous ceiling for the per-scope-file model: each scope opens its own
// file, and a busy multi-tenant runtime touches far fewer than 64 distinct
// scopes within any short window. Closing an LRU handle is safe — the next
// access reopens it.
const maxOpenHandles = 64

// Manager owns the per-scope sqlite databases for RFC AA SQL Memory. Each
// scope (agent/user/run, keyed by tenant) maps to ONE .db file under Root;
// the Manager opens at most maxOpenHandles handles at a time, closing the
// least-recently-used on overflow. Safe for concurrent use.
//
// The Manager is deliberately SEPARATE from the main loomcycle store: an
// agent's arbitrary SQL can only ever reach its own scope file, never the
// runtime's operational database.
type Manager struct {
	cfg Config

	mu    sync.Mutex
	open  map[string]*openHandle // keyed by resolved file path
	clock uint64                 // monotonic LRU stamp
}

// openHandle is one live scope database plus its LRU stamp and an in-flight
// reference count. inUse > 0 means a Query/Exec is currently running against
// this handle, so eviction must NOT close it (a close mid-query, or between an
// Exec's quota-check and its write, would surface as sql.ErrConnDone).
type openHandle struct {
	db    *sql.DB
	used  uint64
	inUse int
}

// ScopeKey identifies one scope database. Tenant/Scope/ScopeID are all
// resolved server-side (never model-supplied raw) but are sanitized
// defensively here before they touch the filesystem.
type ScopeKey struct {
	Tenant  string
	Scope   string // "agent" | "user" | "run"
	ScopeID string
}

// runScope is the scope name for ephemeral run databases. It is stored
// under <Root>/run/<run_id>.db (NOT keyed by tenant — run ids are globally
// unique), and is the only scope DropRunScope can target.
const runScope = "run"

// QueryResult is the shape returned by Query.
type QueryResult struct {
	Columns   []string
	Rows      [][]any
	Truncated bool // true when MaxRows was hit and more rows existed
}

// ExecResult is the shape returned by Exec.
type ExecResult struct {
	RowsAffected int64
	LastInsertID int64
}

// New constructs a Manager and ensures Root exists.
func New(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("sqlmem: empty Root")
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("sqlmem: mkdir root %q: %w", cfg.Root, err)
	}
	return &Manager{
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
// but the Manager sanitizes defensively — it is the last line before a
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
// a whole Query, or a whole Exec (quota-check + write), so neither half ever
// races eviction.
func (m *Manager) acquire(path string) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock++
	h, ok := m.open[path]
	if !ok {
		db, err := openScopeDB(path)
		if err != nil {
			return nil, err
		}
		h = &openHandle{db: db}
		m.open[path] = h
	}
	h.used = m.clock
	h.inUse++
	m.evictLocked()
	return h.db, nil
}

// release drops one in-flight reference taken by acquire.
func (m *Manager) release(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.open[path]; ok && h.inUse > 0 {
		h.inUse--
	}
}

// evictLocked closes the least-recently-used IDLE handle while the open set
// exceeds the cap. Caller must hold m.mu. An in-use handle (inUse > 0) is never
// a victim, so the set may briefly exceed the cap under heavy concurrency —
// that is correct (those handles are actively serving). Closing is best-effort.
func (m *Manager) evictLocked() {
	for len(m.open) > maxOpenHandles {
		var lruPath string
		var lruUsed uint64
		for p, h := range m.open {
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
		if h, ok := m.open[lruPath]; ok {
			if err := h.db.Close(); err != nil {
				log.Printf("sqlmem: close LRU handle %q: %v", lruPath, err)
			}
			delete(m.open, lruPath)
		}
	}
}

// evictPathLocked closes and forgets a single open handle (used by
// DropRunScope before deleting the file). Caller must hold m.mu.
func (m *Manager) evictPathLocked(path string) {
	if h, ok := m.open[path]; ok {
		if err := h.db.Close(); err != nil {
			log.Printf("sqlmem: close handle %q before drop: %v", path, err)
		}
		delete(m.open, path)
	}
}

// effectiveQuota returns the quota to enforce for this call: the per-call
// override when > 0, else the Manager default.
func (m *Manager) effectiveQuota(override int) int {
	if override > 0 {
		return override
	}
	return m.cfg.QuotaBytes
}

// withTimeout derives a context bounded by StatementTimeoutMS. A zero
// timeout returns the parent ctx with a no-op cancel.
func (m *Manager) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.cfg.StatementTimeoutMS <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(m.cfg.StatementTimeoutMS)*time.Millisecond)
}

// Query runs a read-only statement against the scope database. The
// statement is validated by the Go-layer security floor FIRST
// (validateStatement(read-only)); only then does it reach the driver.
// Rows beyond MaxRows are drained and Truncated is set. A *ErrStatement or
// a driver error is returned as a plain error — the caller surfaces it as
// is_error.
func (m *Manager) Query(ctx context.Context, key ScopeKey, statement string, args []any) (*QueryResult, error) {
	if err := validateStatement(statement, true); err != nil {
		return nil, err
	}
	path, err := key.keyPath(m.cfg.Root)
	if err != nil {
		return nil, err
	}

	db, err := m.acquire(path)
	if err != nil {
		return nil, err
	}
	defer m.release(path)

	qctx, cancel := m.withTimeout(ctx)
	defer cancel()

	rows, err := db.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	res := &QueryResult{Columns: cols, Rows: make([][]any, 0)}
	for rows.Next() {
		if m.cfg.MaxRows > 0 && len(res.Rows) >= m.cfg.MaxRows {
			// Stop scanning but mark truncated. The deferred rows.Close drains
			// the rest; we do not scan them.
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
		// Normalize []byte to string so the JSON encoder emits text, not
		// base64 — sqlite hands TEXT columns back as []byte under modernc.
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

// Exec runs a DDL/DML statement against the scope database. The statement
// is validated by the Go-layer floor FIRST (validateStatement(read-write)).
// The quota is checked BEFORE the write (page_count * page_size): an
// approximate bound — a single statement can push the file past the quota,
// but the NEXT write then refuses, so growth is bounded to one statement's
// worth of overshoot.
func (m *Manager) Exec(ctx context.Context, key ScopeKey, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	if err := validateStatement(statement, false); err != nil {
		return nil, err
	}
	path, err := key.keyPath(m.cfg.Root)
	if err != nil {
		return nil, err
	}

	// One acquisition spans the quota check AND the write, so eviction can't
	// close the handle between them.
	db, err := m.acquire(path)
	if err != nil {
		return nil, err
	}
	defer m.release(path)

	if quota := m.effectiveQuota(quotaOverride); quota > 0 {
		size, err := scopeSizeBytes(ctx, db)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
	}

	ectx, cancel := m.withTimeout(ctx)
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
// page_count * page_size. Both pragmas are engine-internal (read-only
// here); they never touch agent data.
func scopeSizeBytes(ctx context.Context, db *sql.DB) (int64, error) {
	var pageCount, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page_count: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page_size: %w", err)
	}
	return pageCount * pageSize, nil
}

// DropRunScope closes+evicts the handle for the run/<runID>.db file and
// removes it (plus -wal/-shm sidecars) behind a fence: the resolved target
// must sit STRICTLY inside <Root>/run and not equal <Root>/run itself, so a
// malformed run id can never widen the delete. Best-effort; mirrors the
// VolumeDef ephemeral-purge fence rooted at <Root>/run. removed reports
// whether a file was actually deleted (false when it was already gone).
func (m *Manager) DropRunScope(runID string) (removed bool, err error) {
	if strings.TrimSpace(runID) == "" {
		return false, fmt.Errorf("sqlmem: empty run id")
	}
	key := ScopeKey{Scope: runScope, ScopeID: runID}
	path, err := key.keyPath(m.cfg.Root)
	if err != nil {
		return false, err
	}

	m.mu.Lock()
	m.evictPathLocked(path)
	m.mu.Unlock()

	runRoot := filepath.Join(m.cfg.Root, runScope)
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

// Close closes every open scope handle. Best-effort: the first close error
// is returned but every handle is still attempted.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for path, h := range m.open {
		if err := h.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.open, path)
	}
	return firstErr
}
