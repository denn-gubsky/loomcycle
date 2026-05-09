// Package sqlite implements store.Store backed by SQLite via modernc.org/sqlite
// (pure Go, no cgo). Single-file database; WAL journal mode for concurrent
// readers during a write.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Store is the SQLite implementation of store.Store.
type Store struct {
	db *sql.DB

	// closeOnce guards the Close() idempotency contract.
	closeOnce sync.Once
}

// Open opens (or creates) a SQLite database at path and applies the schema.
// path may be an OS path or ":memory:" for an ephemeral test DB.
func Open(path string) (*Store, error) {
	// modernc accepts query params on the DSN; WAL mode + foreign keys.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		// memdb shared cache so concurrent goroutines see the same DB
		// (default :memory: is per-connection).
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer. Cap the connection pool to avoid
	// SQLITE_BUSY storms; one writer + a few readers is plenty for v0.3.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates the schema if needed. Idempotent. v0.3 schema is fixed; if
// we add columns post-1.0 we'll add a versioned migration table.
//
// The two phases below are separated because:
//   - Phase 1 (CREATE) is unconditionally idempotent (IF NOT EXISTS).
//   - Phase 2 (ALTER ADD COLUMN) is NOT idempotent in SQLite — re-running
//     the same ADD on an existing column returns "duplicate column name".
//     We swallow exactly that error so a second startup is a no-op without
//     introducing a versioned migrations table for v0.4.
func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			tenant_id  TEXT NOT NULL,
			agent      TEXT NOT NULL,
			created_at INTEGER NOT NULL  -- unix nano
		)`,
		`CREATE TABLE IF NOT EXISTS runs (
			id                       TEXT PRIMARY KEY,
			session_id               TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			status                   TEXT NOT NULL,
			started_at               INTEGER NOT NULL,
			completed_at             INTEGER,
			stop_reason              TEXT,
			input_tokens             INTEGER NOT NULL DEFAULT 0,
			output_tokens            INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
			model                    TEXT,
			error                    TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS runs_by_session ON runs(session_id)`,
		`CREATE TABLE IF NOT EXISTS events (
			seq        INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			ts         INTEGER NOT NULL,
			type       TEXT NOT NULL,
			payload    BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS events_by_session ON events(session_id, seq)`,
		// v0.8 Memory tool. PRIMARY KEY (scope, scope_id, key) gives
		// the natural lookup index; the partial expires_at index keeps
		// the sweeper's DELETE cheap (no full-table scan).
		`CREATE TABLE IF NOT EXISTS memory (
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL,
			key         TEXT NOT NULL,
			value       TEXT NOT NULL,
			expires_at  INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (scope, scope_id, key)
		)`,
		`CREATE INDEX IF NOT EXISTS memory_by_expires_at ON memory(expires_at) WHERE expires_at IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// v0.4 additive columns + indexes for tracking + cancel.
	//
	// Order matters only in that ALTER must precede the partial indexes
	// that reference the new columns.
	addColumns := []string{
		`ALTER TABLE sessions ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_agent_id TEXT`,
		`ALTER TABLE runs ADD COLUMN parent_run_id TEXT`,
		`ALTER TABLE runs ADD COLUMN user_id TEXT`,
		`ALTER TABLE runs ADD COLUMN last_heartbeat_at INTEGER`,
	}
	for _, q := range addColumns {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// SQLite returns errors of the form "duplicate column name: X"
			// when the column already exists. Match on substring rather
			// than introspecting the schema with PRAGMA table_info — the
			// substring check is well-defined for modernc/sqlite and
			// cheaper.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("migrate add column: %w", err)
		}
	}

	addIndexes := []string{
		// Drives the hot lookup paths for the cancel/get endpoints.
		// Partial indexes (WHERE ... IS NOT NULL) keep the index small —
		// the vast majority of historical rows have no agent_id.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_id        ON runs(agent_id)        WHERE agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runs_by_user_active     ON runs(user_id, status) WHERE user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS sessions_by_user        ON sessions(user_id)     WHERE user_id IS NOT NULL`,
	}
	for _, q := range addIndexes {
		// Note the asymmetry vs addColumns above: indexes use
		// `CREATE INDEX IF NOT EXISTS` which is unconditionally
		// idempotent, so we don't need to swallow "duplicate"
		// errors. If you ADD a non-IF-NOT-EXISTS statement here for
		// some reason, do NOT copy the column-loop's substring guard —
		// you'd silently suppress real schema errors. Keep the
		// idempotent shape consistent across all index DDL.
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate add index: %w", err)
		}
	}
	return nil
}

// CreateSession inserts a new session with a generated ID and returns it.
// userID may be empty (e.g. legacy callers); the column accepts NULL via the
// pointer-conversion below so empty doesn't shadow as "" on read.
func (s *Store) CreateSession(ctx context.Context, tenantID, agent, userID string) (store.Session, error) {
	id := newID("s_")
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, tenant_id, agent, created_at, user_id) VALUES (?, ?, ?, ?, ?)`,
		id, tenantID, agent, now.UnixNano(), nilIfEmpty(userID),
	)
	if err != nil {
		return store.Session{}, err
	}
	return store.Session{ID: id, TenantID: tenantID, Agent: agent, CreatedAt: now, UserID: userID}, nil
}

// GetSession returns session metadata or *store.ErrNotFound.
func (s *Store) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	var sess store.Session
	var createdNs int64
	var userID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, agent, created_at, user_id FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sess.ID, &sess.TenantID, &sess.Agent, &createdNs, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	if err != nil {
		return store.Session{}, err
	}
	sess.CreatedAt = time.Unix(0, createdNs)
	if userID.Valid {
		sess.UserID = userID.String
	}
	return sess, nil
}

// CreateRun starts a new run inside an existing session. The caller may
// supply identity fields (agent_id, parent linkage, denormalised user_id)
// for v0.4+ tracking; an empty RunIdentity behaves as v0.3 did.
func (s *Store) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	// Verify the session exists so a missing ID surfaces as ErrNotFound,
	// not a foreign-key error.
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return store.Run{}, err
	}
	id := newID("r_")
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, session_id, status, started_at, agent_id, parent_agent_id, parent_run_id, user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, store.RunRunning, now.UnixNano(),
		nilIfEmpty(identity.AgentID),
		nilIfEmpty(identity.ParentAgentID),
		nilIfEmpty(identity.ParentRunID),
		nilIfEmpty(identity.UserID),
	)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{
		ID:            id,
		SessionID:     sessionID,
		Status:        store.RunRunning,
		StartedAt:     now,
		AgentID:       identity.AgentID,
		ParentAgentID: identity.ParentAgentID,
		ParentRunID:   identity.ParentRunID,
		UserID:        identity.UserID,
	}, nil
}

// AppendEvent persists one event. We look up session_id from the run row
// rather than threading it through callers.
func (s *Store) AppendEvent(ctx context.Context, runID string, eventType string, payload []byte) error {
	var sessionID string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM runs WHERE id = ?`, runID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO events(session_id, run_id, ts, type, payload) VALUES (?, ?, ?, ?, ?)`,
		sessionID, runID, time.Now().UnixNano(), eventType, payload,
	)
	return err
}

// FinishRun marks a run terminal. Idempotent — if the run is already
// finished, the row's status is unchanged. (We use status='running' as a
// guard so a slow-to-finish goroutine can't overwrite a cancellation.)
func (s *Store) FinishRun(ctx context.Context, runID string, status store.RunStatus, stopReason string, usage store.Usage, errMsg string) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
			status                = ?,
			completed_at          = ?,
			stop_reason           = ?,
			input_tokens          = ?,
			output_tokens         = ?,
			cache_creation_tokens = ?,
			cache_read_tokens     = ?,
			model                 = ?,
			error                 = ?
		WHERE id = ? AND status = ?`,
		string(status), now, stopReason,
		usage.InputTokens, usage.OutputTokens,
		usage.CacheCreationTokens, usage.CacheReadTokens,
		usage.Model, errMsg,
		runID, string(store.RunRunning),
	)
	return err
}

// GetTranscript returns all events for a session, ordered by seq ascending.
func (s *Store) GetTranscript(ctx context.Context, sessionID string) ([]store.Event, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, run_id, ts, type, payload FROM events WHERE session_id = ? ORDER BY seq ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Event
	for rows.Next() {
		var ev store.Event
		var ts int64
		if err := rows.Scan(&ev.Seq, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, err
		}
		ev.SessionID = sessionID
		ev.Timestamp = time.Unix(0, ts)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// scanRun decodes one row from a runs SELECT into a store.Run. The
// SELECT column list MUST match the order in runColumns below.
//
// The trailing `agent` column comes from a LEFT JOIN onto sessions
// — sessions.agent is the YAML-declared agent name. NULL when the
// session row is missing (the JOIN drops the agent name silently
// rather than failing the read; the rest of the run row is still
// useful).
func scanRun(scanner interface{ Scan(...any) error }) (store.Run, error) {
	var r store.Run
	var startedNs, completedNs sql.NullInt64
	var lastHbNs sql.NullInt64
	var stopReason, model, errMsg sql.NullString
	var agentID, parentAgentID, parentRunID, userID sql.NullString
	var sessAgent sql.NullString
	var status string
	if err := scanner.Scan(
		&r.ID, &r.SessionID, &status, &startedNs, &completedNs,
		&stopReason,
		&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
		&model, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHbNs,
		&sessAgent,
	); err != nil {
		return store.Run{}, err
	}
	r.Status = store.RunStatus(status)
	if startedNs.Valid {
		r.StartedAt = time.Unix(0, startedNs.Int64)
	}
	if completedNs.Valid {
		r.CompletedAt = time.Unix(0, completedNs.Int64)
	}
	if lastHbNs.Valid {
		r.LastHeartbeatAt = time.Unix(0, lastHbNs.Int64)
	}
	if stopReason.Valid {
		r.StopReason = stopReason.String
	}
	if model.Valid {
		r.Model = model.String
	}
	if errMsg.Valid {
		r.ErrorMsg = errMsg.String
	}
	if agentID.Valid {
		r.AgentID = agentID.String
	}
	if parentAgentID.Valid {
		r.ParentAgentID = parentAgentID.String
	}
	if parentRunID.Valid {
		r.ParentRunID = parentRunID.String
	}
	if userID.Valid {
		r.UserID = userID.String
	}
	if sessAgent.Valid {
		r.Agent = sessAgent.String
	}
	return r, nil
}

// runColumns is the canonical SELECT column list paired with scanRun.
// Centralised so a future column addition is a one-line change.
//
// The `r.` / `s.` qualifiers + the trailing JOIN clause are required
// because of the sessions.agent column (denormalised onto Run.Agent
// at read time so callers don't have to fetch the session row
// separately). All callers MUST use `runFromTable` to reference the
// table (with its alias) so the qualifiers resolve.
const runColumns = `r.id, r.session_id, r.status, r.started_at, r.completed_at,
		r.stop_reason,
		r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		r.model, r.error,
		r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
		s.agent`

// runFromTable is the canonical FROM clause paired with runColumns.
// Provides the `r` and `s` aliases that the column list references.
const runFromTable = `runs r LEFT JOIN sessions s ON r.session_id = s.id`

// GetRunByAgentID returns the most recently started run carrying the
// given agent_id, or *store.ErrNotFound. Multiple historical runs may
// share an agent_id (a caller reused it after the first terminated);
// we surface the latest, which is the one any cancel/status caller
// would mean.
func (s *Store) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	if agentID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.agent_id = ? ORDER BY r.started_at DESC LIMIT 1`,
		agentID,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return r, err
}

// ListUsers returns one row per distinct user_id with summary stats.
// Drives the v0.7.3 Web UI user picker.
//
// SQLite COUNT(CASE WHEN ...) is the conventional shape for grouped
// counts by category; both backends produce identical row sets.
func (s *Store) ListUsers(ctx context.Context) ([]store.UserSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			user_id,
			COUNT(CASE WHEN status = 'running' THEN 1 END) AS running_count,
			COUNT(*) AS total_count,
			MAX(started_at) AS last_started_at
		FROM runs
		WHERE user_id IS NOT NULL AND user_id != ''
		GROUP BY user_id
		ORDER BY last_started_at DESC
		LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.UserSummary
	for rows.Next() {
		var u store.UserSummary
		var lastNanos int64
		if err := rows.Scan(&u.UserID, &u.RunningCount, &u.TotalCount, &lastNanos); err != nil {
			return nil, err
		}
		u.LastStartedAt = time.Unix(0, lastNanos).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListActiveRunsByUser returns runs for userID whose status matches the
// supplied filter. An empty status returns ALL statuses. Capped at 100
// rows ordered by started_at DESC.
func (s *Store) ListActiveRunsByUser(ctx context.Context, userID string, status store.RunStatus) ([]store.Run, error) {
	if userID == "" {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.user_id = ? ORDER BY r.started_at DESC LIMIT 100`,
			userID,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.user_id = ? AND r.status = ? ORDER BY r.started_at DESC LIMIT 100`,
			userID, string(status),
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsByParentAgentID returns the runs whose parent_agent_id
// matches. Drives cascade-cancel discovery (every direct child of a
// parent agent_id). Recursion (grandchildren) is the caller's job —
// keeps this query simple and lets the cancel handler walk the tree
// however it wants.
func (s *Store) ListRunsByParentAgentID(ctx context.Context, parentAgentID string) ([]store.Run, error) {
	if parentAgentID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.parent_agent_id = ? ORDER BY r.started_at ASC`,
		parentAgentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateHeartbeat sets last_heartbeat_at to the current time. No-op for
// runs that aren't currently running (the WHERE guard prevents a slow
// hb update from un-finishing a terminal run that just got cancelled).
func (s *Store) UpdateHeartbeat(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET last_heartbeat_at = ? WHERE id = ? AND status = ?`,
		time.Now().UnixNano(), runID, string(store.RunRunning),
	)
	return err
}

// SweepStaleRuns implements store.Store. Runs whose last_heartbeat_at
// is older than cutoff (or whose started_at is older than cutoff and
// who never heartbeated) are flipped to status="failed" with
// error="heartbeat timeout". Single atomic UPDATE so concurrent
// sweepers race correctly.
func (s *Store) SweepStaleRuns(ctx context.Context, cutoff time.Time) (int, error) {
	cutoffNs := cutoff.UnixNano()
	completedNs := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
			status = ?,
			completed_at = ?,
			error = ?,
			stop_reason = ?
		 WHERE status = ?
		   AND (
			 (last_heartbeat_at IS NOT NULL AND last_heartbeat_at < ?)
			 OR (last_heartbeat_at IS NULL AND started_at < ?)
		   )`,
		string(store.RunFailed), completedNs,
		"heartbeat timeout", "heartbeat_timeout",
		string(store.RunRunning),
		cutoffNs, cutoffNs,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't report RowsAffected; this isn't fatal —
		// the UPDATE landed, we just don't know the count. Return 0.
		return 0, nil
	}
	return int(n), nil
}

// MemorySet upserts a Memory row. ttl > 0 sets expires_at = now+ttl;
// ttl <= 0 clears the column to NULL (no expiry).
//
// Stored as JSON text in a TEXT column — SQLite has no native JSON
// type beyond what JSON1 functions consume; the tool layer is the
// source of truth for shape validation. (We also use the textual
// representation for the JSON-number parse in MemoryIncrement.)
func (s *Store) MemorySet(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error {
	now := time.Now().UnixNano()
	var expiresAt any
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at`,
		string(scope), scopeID, key, string(value), expiresAt, now, now,
	)
	return err
}

// MemoryGet returns the entry or *ErrNotFound. Expired rows are
// surfaced as ErrNotFound regardless of whether the sweeper has
// reaped them yet.
func (s *Store) MemoryGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	var (
		valueText string
		expiresAt sql.NullInt64
		createdAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at, created_at, updated_at
		 FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	}
	if err != nil {
		return store.MemoryEntry{}, err
	}
	if expiresAt.Valid && time.Now().UnixNano() > expiresAt.Int64 {
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	}
	out := store.MemoryEntry{
		Key:       key,
		Value:     json.RawMessage(valueText),
		CreatedAt: time.Unix(0, createdAt),
		UpdatedAt: time.Unix(0, updatedAt),
	}
	if expiresAt.Valid {
		out.ExpiresAt = time.Unix(0, expiresAt.Int64)
	}
	return out, nil
}

// MemoryDelete removes a row. Returns whether a row was actually
// deleted; both outcomes are non-error.
func (s *Store) MemoryDelete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// MemoryList enumerates entries for a (scope, scopeID), filtered by
// prefix and capped at limit rows. Expired rows are filtered in the
// WHERE clause so callers never see them. truncated == true when the
// underlying query found at least limit+1 rows.
func (s *Store) MemoryList(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	nowNs := time.Now().UnixNano()
	// Fetch limit+1 to detect truncation without a separate COUNT(*).
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, expires_at, created_at, updated_at
		 FROM memory
		 WHERE scope = ? AND scope_id = ? AND key LIKE ? ESCAPE '\'
		   AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY key ASC
		 LIMIT ?`,
		string(scope), scopeID, escapeLikePrefix(prefix)+"%", nowNs, limit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []store.MemoryEntry
	for rows.Next() {
		var (
			key       string
			valueText string
			expiresAt sql.NullInt64
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&key, &valueText, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, false, err
		}
		entry := store.MemoryEntry{
			Key:       key,
			Value:     json.RawMessage(valueText),
			CreatedAt: time.Unix(0, createdAt),
			UpdatedAt: time.Unix(0, updatedAt),
		}
		if expiresAt.Valid {
			entry.ExpiresAt = time.Unix(0, expiresAt.Int64)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// MemoryIncrement is the atomic counter primitive. SQLite has no
// native compare-and-set on JSON values, so we wrap the read +
// arithmetic + write in an IMMEDIATE transaction (a write lock at
// the BEGIN). Concurrent increments serialise on the lock; the
// loop is contention-free in the absence of writes.
func (s *Store) MemoryIncrement(ctx context.Context, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		valueText sql.NullString
		expiresAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT value, expires_at FROM memory WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt)
	now := time.Now()
	nowNs := now.UnixNano()

	var current int64
	rowExists := !errors.Is(err, sql.ErrNoRows)
	if rowExists && err != nil {
		return 0, err
	}
	// Treat expired rows as missing — increment from zero rather than
	// the stale value.
	if rowExists && expiresAt.Valid && nowNs > expiresAt.Int64 {
		rowExists = false
	}
	if rowExists {
		text := strings.TrimSpace(valueText.String)
		n, parseErr := strconv.ParseInt(text, 10, 64)
		if parseErr != nil {
			// Fall back to JSON parse: covers floats expressed as
			// integers ("3.0") which strconv rejects but JSON allows.
			var f float64
			if jsonErr := json.Unmarshal([]byte(text), &f); jsonErr != nil {
				return 0, store.ErrMemoryWrongType
			}
			if f != float64(int64(f)) {
				return 0, store.ErrMemoryWrongType
			}
			n = int64(f)
		}
		current = n
	}
	next := current + delta
	nextText := strconv.FormatInt(next, 10)

	var newExpires any
	switch {
	case ttl > 0:
		newExpires = now.Add(ttl).UnixNano()
	case rowExists && expiresAt.Valid:
		newExpires = expiresAt.Int64 // preserve existing expiry
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, scope_id, key) DO UPDATE SET
		    value = excluded.value,
		    expires_at = excluded.expires_at,
		    updated_at = excluded.updated_at`,
		string(scope), scopeID, key, nextText, newExpires, nowNs, nowNs,
	)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

// MemoryListScopeIDs returns distinct scope_ids under scope with
// summary stats. Excludes expired rows so operators see live state
// only. Capped at 200 rows ordered by updated_at DESC.
func (s *Store) MemoryListScopeIDs(ctx context.Context, scope store.MemoryScope) ([]store.MemoryScopeIDSummary, error) {
	nowNs := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			scope_id,
			COUNT(*)                                              AS key_count,
			COALESCE(SUM(LENGTH(key) + LENGTH(value)), 0)          AS bytes,
			MAX(updated_at)                                        AS updated_at
		FROM memory
		WHERE scope = ? AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY scope_id
		ORDER BY updated_at DESC
		LIMIT 200`,
		string(scope), nowNs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MemoryScopeIDSummary
	for rows.Next() {
		var (
			summary   store.MemoryScopeIDSummary
			updatedNs int64
		)
		if err := rows.Scan(&summary.ScopeID, &summary.KeyCount, &summary.Bytes, &updatedNs); err != nil {
			return nil, err
		}
		summary.UpdatedAt = time.Unix(0, updatedNs).UTC()
		out = append(out, summary)
	}
	return out, rows.Err()
}

// MemorySweep deletes every Memory row whose expires_at has passed.
func (s *Store) MemorySweep(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().UnixNano(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// escapeLikePrefix escapes the LIKE wildcards in `prefix` so an agent
// passing "events_2026" doesn't get treated as "events" + any-char +
// "2026". The ESCAPE clause in the LIKE statement uses backslash.
func escapeLikePrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(prefix)
}

// nilIfEmpty returns nil when s is empty so the SQL driver writes NULL
// rather than an empty string. Callers should prefer NULL for "no
// value" so that COUNT(column) and IS NULL queries behave correctly.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Close closes the underlying *sql.DB. Idempotent.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.db.Close() })
	return err
}

// newID generates a short opaque ID with a prefix. 16 hex chars = 64 bits of
// entropy — plenty for v0.3 single-tenant scale; can swap for ULID/UUID later.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
