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
	"sort"
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
		// v0.8.4 Channel tool — see internal/store/postgres/migrations/0004_channels.up.sql
		// for the full rationale. SQLite mirrors the shape: TEXT id (ULID-like prefix
		// "msg_<unixnano>_<rand>" — sortable by publish time), per-(channel, scope,
		// scope_id) composite PK so per-subscriber scans are index lookups. payload is
		// TEXT-encoded JSON because SQLite doesn't have a native JSONB type.
		`CREATE TABLE IF NOT EXISTS channel_messages (
			id           TEXT    NOT NULL,
			channel      TEXT    NOT NULL,
			scope        TEXT    NOT NULL,
			scope_id     TEXT    NOT NULL,
			payload      TEXT    NOT NULL,
			published_at INTEGER NOT NULL,
			expires_at   INTEGER,
			PRIMARY KEY (channel, scope, scope_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS channel_messages_by_expires_at ON channel_messages(expires_at) WHERE expires_at IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS channel_cursors (
			channel    TEXT    NOT NULL,
			scope      TEXT    NOT NULL,
			scope_id   TEXT    NOT NULL,
			cursor     TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (channel, scope, scope_id)
		)`,
		// v0.8.5 Self-Evolution Substrate — see
		// internal/store/postgres/migrations/0006_agent_defs.up.sql for
		// the full design rationale. SQLite mirrors the shape; INTEGER
		// boolean (0/1) instead of Postgres BOOLEAN, unix-nano INTEGER
		// timestamps instead of TIMESTAMPTZ, TEXT JSON instead of JSONB.
		`CREATE TABLE IF NOT EXISTS agent_defs (
			def_id                    TEXT    PRIMARY KEY,
			name                      TEXT    NOT NULL,
			version                   INTEGER NOT NULL,
			parent_def_id             TEXT    REFERENCES agent_defs(def_id),
			definition                TEXT    NOT NULL,
			description               TEXT,
			created_at                INTEGER NOT NULL,
			created_by_agent_id       TEXT,
			created_by_run_id         TEXT,
			retired                   INTEGER NOT NULL DEFAULT 0,
			bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
			UNIQUE(name, version)
		)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_name   ON agent_defs(name, version DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_parent ON agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS agent_defs_by_run    ON agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS agent_def_active (
			name                  TEXT    PRIMARY KEY,
			def_id                TEXT    NOT NULL REFERENCES agent_defs(def_id),
			promoted_at           INTEGER NOT NULL,
			promoted_by_agent_id  TEXT
		)`,
		// v0.8.5 evaluations table. emitter_role is server-derived in
		// the tool layer; the store stores the string verbatim. Score
		// is REAL (Go float64). Dimensions + Judgement are JSON-as-TEXT
		// (sqlite has no JSONB).
		//
		// NO foreign keys on run_id or def_id: evaluations are an
		// immutable audit log and must survive any future run/def
		// pruning. Referential integrity is enforced at the
		// application layer. A RESTRICT FK would block legitimate
		// admin pruning workflows; CASCADE would silently delete
		// audit data. Mirrors the postgres migration 0008.
		`CREATE TABLE IF NOT EXISTS evaluations (
			eval_id            TEXT    PRIMARY KEY,
			run_id             TEXT    NOT NULL,
			def_id             TEXT,
			score              REAL    NOT NULL,
			dimensions         TEXT,
			judgement          TEXT,
			rationale          TEXT,
			emitter_role       TEXT    NOT NULL,
			emitter_agent_id   TEXT,
			emitter_run_id     TEXT,
			created_at         INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_run     ON evaluations(run_id)`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_def     ON evaluations(def_id) WHERE def_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS evaluations_by_emitter ON evaluations(emitter_agent_id) WHERE emitter_agent_id IS NOT NULL`,
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
		// v0.8.2: user_tier marker (PR #52). Nullable on legacy rows;
		// new rows carry the name of the user_tier policy applied at
		// run creation. Compliance + cost-retro queries facet on this.
		`ALTER TABLE runs ADD COLUMN user_tier TEXT`,
		// v0.8.5: agent_def_id audit column. NULL = the run resolved
		// against the static cfg.Agents fallback (no DB-versioned def).
		// Non-NULL = the run targeted a specific (name, version) row
		// in agent_defs. Distinguishes static-resolved from DB-resolved
		// runs without a separate flag.
		`ALTER TABLE runs ADD COLUMN agent_def_id TEXT`,
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
		// v0.8.5: facets cost retros + experiment audits by which
		// agent_def_id the run actually ran against. Partial index
		// keeps it small — only DB-resolved runs have a non-NULL value.
		`CREATE INDEX IF NOT EXISTS runs_by_agent_def       ON runs(agent_def_id)    WHERE agent_def_id IS NOT NULL`,
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
		`INSERT INTO runs(id, session_id, status, started_at, agent_id, parent_agent_id, parent_run_id, user_id, user_tier)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, store.RunRunning, now.UnixNano(),
		nilIfEmpty(identity.AgentID),
		nilIfEmpty(identity.ParentAgentID),
		nilIfEmpty(identity.ParentRunID),
		nilIfEmpty(identity.UserID),
		nilIfEmpty(identity.UserTier),
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
		UserTier:      identity.UserTier,
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
	var agentID, parentAgentID, parentRunID, userID, userTier sql.NullString
	var sessAgent sql.NullString
	var status string
	if err := scanner.Scan(
		&r.ID, &r.SessionID, &status, &startedNs, &completedNs,
		&stopReason,
		&r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens,
		&model, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHbNs,
		&userTier,
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
	if userTier.Valid {
		r.UserTier = userTier.String
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
		r.user_tier,
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

// GetRun returns one row by run_id (the primary key on runs).
func (s *Store) GetRun(ctx context.Context, runID string) (store.Run, error) {
	if runID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM `+runFromTable+` WHERE r.id = ?`,
		runID,
	)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
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
//
// modernc/sqlite's database/sql driver does NOT translate
// `sql.LevelSerializable` to `BEGIN IMMEDIATE` — it only honors
// `_txlock=immediate` in the DSN (which would affect every
// transaction, including read paths where DEFERRED is preferred).
// We therefore pin a connection from the pool and issue
// `BEGIN IMMEDIATE` / `COMMIT` raw, scoping the lock-on-BEGIN
// behaviour to this one operation. Verified by a 100-goroutine
// regression test in storetest (counter must hit exactly 100).
func (s *Store) MemoryIncrement(ctx context.Context, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var (
		valueText sql.NullString
		expiresAt sql.NullInt64
	)
	err = conn.QueryRowContext(ctx,
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

	_, err = conn.ExecContext(ctx,
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
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true
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

// ---- v0.8.4 Channel tool ----
//
// All five methods are single-table operations against
// channel_messages / channel_cursors. Per-(channel, scope, scope_id)
// reads use the composite primary key directly — no extra index
// needed. The expires_at filter happens at read time AND in the
// sweeper; readers never see expired rows even if the sweeper has
// lagged.

// ChannelPublish appends one message and trims the channel down to
// maxMessages inside the same txn (oldest rows go first). maxMessages
// <= 0 disables the trim. Returns the assigned id and the trim count.
func (s *Store) ChannelPublish(ctx context.Context, msg store.ChannelMessage, maxMessages int) (string, int, error) {
	now := time.Now()
	msg.ID = store.MintChannelMessageID(now)
	msg.PublishedAt = now

	var expiresAt any
	if !msg.ExpiresAt.IsZero() {
		expiresAt = msg.ExpiresAt.UnixNano()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_messages(id, channel, scope, scope_id, payload, published_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.Channel, string(msg.Scope), msg.ScopeID, string(msg.Payload), now.UnixNano(), expiresAt,
	); err != nil {
		return "", 0, err
	}

	dropped := 0
	if maxMessages > 0 {
		// Trim by deleting the oldest rows beyond maxMessages. We
		// ORDER BY id (= publish time) so the trim is deterministic.
		// The subquery selects the surviving "keep" set; everything
		// older is dropped. This is the lossy-on-overflow shape from
		// the v0.8.4 RFC — publisher never blocks.
		//
		// The `id != ?` clause protects against the Postgres
		// READ-COMMITTED race where two concurrent publishers to the
		// same (channel, scope, scope_id) can each see the other's
		// committed row inside their own trim subquery. Without this
		// guard, A's INSERT X + concurrent B's commit of Y > X means
		// A's trim subquery picks Y as top-N (excluding X by lex
		// order) and A's DELETE removes its own just-inserted X.
		// A then commits and reports success to its caller, but X
		// is gone. With the guard, the just-inserted row is never
		// in the DELETE candidate set under any race.
		//
		// SQLite is single-writer (WAL) so the race doesn't occur,
		// but the guard adds no cost and keeps the two backends'
		// SQL identical.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM channel_messages
			 WHERE channel = ? AND scope = ? AND scope_id = ?
			   AND id != ?
			   AND id NOT IN (
			     SELECT id FROM channel_messages
			      WHERE channel = ? AND scope = ? AND scope_id = ?
			      ORDER BY id DESC
			      LIMIT ?
			   )`,
			msg.Channel, string(msg.Scope), msg.ScopeID, msg.ID,
			msg.Channel, string(msg.Scope), msg.ScopeID,
			maxMessages,
		)
		if err != nil {
			return "", 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			dropped = int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return msg.ID, dropped, nil
}

// ChannelSubscribe reads up to `limit` messages newer than fromCursor.
// fromCursor == "" || "cur_0" → from the oldest non-expired row.
// Returns the batch + the id of the LAST message as nextCursor.
func (s *Store) ChannelSubscribe(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	return s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
}

// ChannelPeek is identical to ChannelSubscribe (non-consuming — the
// cursor table is never touched on either path). The semantic
// difference lives entirely in the tool layer: Subscribe optionally
// commits the returned cursor on the next call, Peek never does.
func (s *Store) ChannelPeek(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, error) {
	msgs, _, err := s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
	return msgs, err
}

// channelRead is the shared read body for Subscribe + Peek. expired-
// at-read-time filter applies on both paths.
func (s *Store) channelRead(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	if limit <= 0 {
		limit = 10
	}
	if fromCursor == "cur_0" {
		fromCursor = ""
	}
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, payload, published_at, expires_at
		 FROM channel_messages
		 WHERE channel = ? AND scope = ? AND scope_id = ?
		   AND id > ?
		   AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY id ASC
		 LIMIT ?`,
		channel, string(scope), scopeID, fromCursor, now, limit,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var msgs []store.ChannelMessage
	var lastID string
	for rows.Next() {
		var (
			id          string
			payload     string
			publishedAt int64
			expiresAt   sql.NullInt64
		)
		if err := rows.Scan(&id, &payload, &publishedAt, &expiresAt); err != nil {
			return nil, "", err
		}
		msg := store.ChannelMessage{
			ID:          id,
			Channel:     channel,
			Scope:       scope,
			ScopeID:     scopeID,
			Payload:     json.RawMessage(payload),
			PublishedAt: time.Unix(0, publishedAt),
		}
		if expiresAt.Valid {
			msg.ExpiresAt = time.Unix(0, expiresAt.Int64)
		}
		msgs = append(msgs, msg)
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return msgs, lastID, nil
}

// ChannelAck commits cursor to the per-subscriber row. Rejects cursor
// values older than the currently committed one (ULID lexicographic
// order). Idempotent re-ack of the SAME cursor is a no-op.
func (s *Store) ChannelAck(ctx context.Context, channel string, scope store.MemoryScope, scopeID, cursor string) error {
	if cursor == "" || cursor == "cur_0" {
		return nil // nothing to commit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = ? AND scope = ? AND scope_id = ?`,
		channel, string(scope), scopeID,
	).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if existing != "" && cursor < existing {
		return store.ErrChannelCursorRegression
	}
	if existing == cursor {
		return tx.Commit() // idempotent
	}

	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_cursors(channel, scope, scope_id, cursor, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(channel, scope, scope_id) DO UPDATE SET
		    cursor = excluded.cursor,
		    updated_at = excluded.updated_at`,
		channel, string(scope), scopeID, cursor, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ChannelCommittedCursor returns the last cursor ack'd for a
// subscriber, or empty string when none.
func (s *Store) ChannelCommittedCursor(ctx context.Context, channel string, scope store.MemoryScope, scopeID string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = ? AND scope = ? AND scope_id = ?`,
		channel, string(scope), scopeID,
	).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cursor, nil
}

// ChannelSweepExpired deletes every expired row. Mirror of MemorySweep.
func (s *Store) ChannelSweepExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_messages WHERE expires_at IS NOT NULL AND expires_at <= ?`,
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

// ---- v0.8.5 Self-Evolution Substrate ----
//
// AgentDef methods. Append-only. Version is allocated under a
// per-name lock (BEGIN IMMEDIATE in sqlite — coarse but correct for
// single-writer WAL, mirrors MemoryIncrement's pattern).

// AgentDefCreate allocates the next version for row.Name under a
// per-name lock and inserts. The caller supplies row.DefID (UUID/
// ULID-ish opaque string). Validates parent_def_id when set.
//
// SQLite concurrency: uses the same BEGIN IMMEDIATE + pinned-conn
// pattern as MemoryIncrement — pinning the connection scopes the
// write lock to this one transaction, and IMMEDIATE means concurrent
// writers see SQLITE_BUSY at BEGIN time (database/sql retries) rather
// than upgrade-deadlocking mid-tx. Without this, two concurrent
// AgentDefCreate calls against the same name both start a DEFERRED
// tx, both SELECT MAX(version) (returning the same value), then both
// try to INSERT with the same version — one succeeds, one fails on
// the UNIQUE(name, version) constraint.
func (s *Store) AgentDefCreate(ctx context.Context, row store.AgentDefRow) (store.AgentDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.AgentDefRow{}, fmt.Errorf("agent_def: def_id + name required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return store.AgentDefRow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if row.ParentDefID != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_defs WHERE def_id = ?`, row.ParentDefID).Scan(&n); err != nil {
			return store.AgentDefRow{}, err
		}
		if n == 0 {
			return store.AgentDefRow{}, store.ErrAgentDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version) FROM agent_defs WHERE name = ?`, row.Name,
	).Scan(&maxVer); err != nil {
		return store.AgentDefRow{}, err
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now()

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO agent_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.DefID, row.Name, row.Version, nilIfEmpty(row.ParentDefID),
		string(row.Definition), nilIfEmpty(row.Description),
		row.CreatedAt.UnixNano(),
		nilIfEmpty(row.CreatedByAgentID), nilIfEmpty(row.CreatedByRunID),
		boolToInt(row.Retired), boolToInt(row.BootstrappedFromStatic),
	); err != nil {
		return store.AgentDefRow{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return store.AgentDefRow{}, err
	}
	committed = true
	return row, nil
}

// AgentDefGet returns one row by def_id.
func (s *Store) AgentDefGet(ctx context.Context, defID string) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.db.QueryRowContext(ctx, agentDefSelect+` WHERE def_id = ?`, defID))
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return row, err
}

// AgentDefGetByNameVersion returns one row by (name, version).
func (s *Store) AgentDefGetByNameVersion(ctx context.Context, name string, version int) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.db.QueryRowContext(ctx, agentDefSelect+` WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

// AgentDefListByName returns rows for one name, version DESC.
func (s *Store) AgentDefListByName(ctx context.Context, name string) ([]store.AgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, agentDefSelect+` WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListChildren returns immediate children (parent_def_id == arg).
func (s *Store) AgentDefListChildren(ctx context.Context, parentDefID string) ([]store.AgentDefRow, error) {
	rows, err := s.db.QueryContext(ctx, agentDefSelect+` WHERE parent_def_id = ? ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListNames returns one summary row per distinct name. Joins
// agent_def_active to surface the active def_id when one exists.
func (s *Store) AgentDefListNames(ctx context.Context) ([]store.AgentDefNameSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.name,
			COUNT(*)                  AS version_count,
			MAX(d.version)            AS latest_version,
			MAX(d.created_at)         AS last_updated,
			COALESCE(a.def_id, '')    AS active_def_id
		FROM agent_defs d
		LEFT JOIN agent_def_active a ON a.name = d.name
		GROUP BY d.name, a.def_id
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.AgentDefNameSummary
	for rows.Next() {
		var s store.AgentDefNameSummary
		var updatedAt int64
		if err := rows.Scan(&s.Name, &s.VersionCount, &s.LatestVersion, &updatedAt, &s.ActiveDefID); err != nil {
			return nil, err
		}
		s.LastUpdated = time.Unix(0, updatedAt)
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentDefSetActive UPSERTs the agent_def_active pointer for name.
func (s *Store) AgentDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	// Validate def_id exists + matches name (defence-in-depth; the
	// FK isn't enforced without foreign_keys PRAGMA).
	var rowName string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM agent_defs WHERE def_id = ?`, defID).Scan(&rowName)
	if err == sql.ErrNoRows {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if err != nil {
		return err
	}
	if rowName != name {
		return fmt.Errorf("agent_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    def_id               = excluded.def_id,
		    promoted_at          = excluded.promoted_at,
		    promoted_by_agent_id = excluded.promoted_by_agent_id`,
		name, defID, time.Now().UnixNano(), nilIfEmpty(promotedByAgentID),
	)
	return err
}

// AgentDefGetActive returns the active row for name. *ErrNotFound
// when no pointer exists — caller falls through to cfg.Agents.
func (s *Store) AgentDefGetActive(ctx context.Context, name string) (store.AgentDefRow, error) {
	var defID string
	err := s.db.QueryRowContext(ctx, `SELECT def_id FROM agent_def_active WHERE name = ?`, name).Scan(&defID)
	if err == sql.ErrNoRows {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def_active", ID: name}
	}
	if err != nil {
		return store.AgentDefRow{}, err
	}
	return s.AgentDefGet(ctx, defID)
}

// AgentDefSetRetired flips the `retired` flag on one row. The row
// stays visible in lineage queries.
func (s *Store) AgentDefSetRetired(ctx context.Context, defID string, retired bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_defs SET retired = ? WHERE def_id = ?`,
		boolToInt(retired), defID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return nil
}

// agentDefSelect is the column list shared by every read. Kept in
// one place so column additions (a future tenant_id, similarity_score,
// ...) need only a single touch-point.
const agentDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static
FROM agent_defs`

func (s *Store) scanAgentDef(row *sql.Row) (store.AgentDefRow, error) {
	var (
		out        store.AgentDefRow
		definition string
		createdAt  int64
		retired    int
		bootstrap  int
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&createdAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&retired, &bootstrap,
	)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	out.CreatedAt = time.Unix(0, createdAt)
	out.Retired = retired != 0
	out.BootstrappedFromStatic = bootstrap != 0
	return out, nil
}

func (s *Store) scanAgentDefRows(rows *sql.Rows) ([]store.AgentDefRow, error) {
	var out []store.AgentDefRow
	for rows.Next() {
		var (
			r          store.AgentDefRow
			definition string
			createdAt  int64
			retired    int
			bootstrap  int
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&createdAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&retired, &bootstrap,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		r.CreatedAt = time.Unix(0, createdAt)
		r.Retired = retired != 0
		r.BootstrappedFromStatic = bootstrap != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Evaluation (pure-insert, no concurrency lock) ----

// EvaluationSubmit inserts one evaluation row. CreatedAt set by store.
func (s *Store) EvaluationSubmit(ctx context.Context, row store.EvaluationRow) (store.EvaluationRow, error) {
	if row.EvalID == "" || row.RunID == "" || row.EmitterRole == "" {
		return store.EvaluationRow{}, fmt.Errorf("evaluation: eval_id, run_id, emitter_role required")
	}
	row.CreatedAt = time.Now()
	var dimsJSON, judgementJSON sql.NullString
	if len(row.Dimensions) > 0 {
		b, err := json.Marshal(row.Dimensions)
		if err != nil {
			return store.EvaluationRow{}, fmt.Errorf("evaluation: marshal dimensions: %w", err)
		}
		dimsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(row.Judgement) > 0 {
		judgementJSON = sql.NullString{String: string(row.Judgement), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (
			eval_id, run_id, def_id, score, dimensions, judgement, rationale,
			emitter_role, emitter_agent_id, emitter_run_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.EvalID, row.RunID, nilIfEmpty(row.DefID), row.Score,
		dimsJSON, judgementJSON, nilIfEmpty(row.Rationale),
		row.EmitterRole, nilIfEmpty(row.EmitterAgentID), nilIfEmpty(row.EmitterRunID),
		row.CreatedAt.UnixNano(),
	)
	if err != nil {
		return store.EvaluationRow{}, err
	}
	return row, nil
}

// EvaluationGet returns one row by eval_id.
func (s *Store) EvaluationGet(ctx context.Context, evalID string) (store.EvaluationRow, error) {
	row, err := s.scanEvaluation(s.db.QueryRowContext(ctx, evaluationSelect+` WHERE eval_id = ?`, evalID))
	if err == sql.ErrNoRows {
		return store.EvaluationRow{}, &store.ErrNotFound{Kind: "evaluation", ID: evalID}
	}
	return row, err
}

// EvaluationListForRun returns evals targeting one run.
func (s *Store) EvaluationListForRun(ctx context.Context, runID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, evaluationSelect+` WHERE run_id = ? ORDER BY created_at DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

// EvaluationListForDef returns evals targeting one def.
func (s *Store) EvaluationListForDef(ctx context.Context, defID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, evaluationSelect+` WHERE def_id = ? ORDER BY created_at DESC LIMIT ?`, defID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

// EvaluationAggregate computes the score+dimension+by-role aggregates
// for a def_id. When opts.IncludeLineage is true, walks parent_def_id
// chain depth-first and includes ancestors.
func (s *Store) EvaluationAggregate(ctx context.Context, defID string, opts store.AggregateOpts) (store.AggregateResult, error) {
	defIDs := []string{defID}
	if opts.IncludeLineage {
		ancestors, err := s.walkAncestors(ctx, defID)
		if err != nil {
			return store.AggregateResult{}, err
		}
		defIDs = append(defIDs, ancestors...)
	}

	// Build the IN list. Limit defensively at 1000 ancestors so a
	// pathological lineage can't blow query parser limits — the
	// aggregator caller is responsible for not building megacycles.
	if len(defIDs) > 1000 {
		defIDs = defIDs[:1000]
	}
	placeholders := make([]string, len(defIDs))
	args := make([]any, len(defIDs))
	for i, id := range defIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := evaluationSelect + ` WHERE def_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return store.AggregateResult{}, err
	}
	defer rows.Close()
	evals, err := s.scanEvaluationRows(rows)
	if err != nil {
		return store.AggregateResult{}, err
	}
	return computeAggregate(defID, evals, opts.IncludeLineage), nil
}

// walkAncestors returns the parent_def_id chain for defID (NOT
// including defID itself). Depth-first, bounded at 100 hops to
// protect against the (impossible-by-construction-but-let's-be-safe)
// case of a cycle. Empty when defID has no parent.
func (s *Store) walkAncestors(ctx context.Context, defID string) ([]string, error) {
	var ancestors []string
	seen := map[string]bool{defID: true}
	cur := defID
	for i := 0; i < 100; i++ {
		var parent sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT parent_def_id FROM agent_defs WHERE def_id = ?`, cur).Scan(&parent)
		if err == sql.ErrNoRows || !parent.Valid || parent.String == "" {
			return ancestors, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[parent.String] {
			return ancestors, nil // cycle guard
		}
		seen[parent.String] = true
		ancestors = append(ancestors, parent.String)
		cur = parent.String
	}
	return ancestors, nil
}

const evaluationSelect = `SELECT
	eval_id, run_id,
	COALESCE(def_id, ''),
	score,
	COALESCE(dimensions, ''),
	COALESCE(judgement, ''),
	COALESCE(rationale, ''),
	emitter_role,
	COALESCE(emitter_agent_id, ''),
	COALESCE(emitter_run_id, ''),
	created_at
FROM evaluations`

func (s *Store) scanEvaluation(row *sql.Row) (store.EvaluationRow, error) {
	var (
		out                   store.EvaluationRow
		dimensions, judgement string
		createdAt             int64
	)
	if err := row.Scan(
		&out.EvalID, &out.RunID, &out.DefID, &out.Score,
		&dimensions, &judgement, &out.Rationale,
		&out.EmitterRole, &out.EmitterAgentID, &out.EmitterRunID,
		&createdAt,
	); err != nil {
		return store.EvaluationRow{}, err
	}
	if dimensions != "" {
		_ = json.Unmarshal([]byte(dimensions), &out.Dimensions)
	}
	if judgement != "" {
		out.Judgement = json.RawMessage(judgement)
	}
	out.CreatedAt = time.Unix(0, createdAt)
	return out, nil
}

func (s *Store) scanEvaluationRows(rows *sql.Rows) ([]store.EvaluationRow, error) {
	var out []store.EvaluationRow
	for rows.Next() {
		var (
			r                     store.EvaluationRow
			dimensions, judgement string
			createdAt             int64
		)
		if err := rows.Scan(
			&r.EvalID, &r.RunID, &r.DefID, &r.Score,
			&dimensions, &judgement, &r.Rationale,
			&r.EmitterRole, &r.EmitterAgentID, &r.EmitterRunID,
			&createdAt,
		); err != nil {
			return nil, err
		}
		if dimensions != "" {
			_ = json.Unmarshal([]byte(dimensions), &r.Dimensions)
		}
		if judgement != "" {
			r.Judgement = json.RawMessage(judgement)
		}
		r.CreatedAt = time.Unix(0, createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// computeAggregate is the pure-Go aggregation kernel — shared by
// sqlite + postgres adapters. Given a flat list of evaluations,
// produce summary stats. Empty input → zero-valued AggregateResult
// with Count=0 (well-defined "no evaluations yet").
func computeAggregate(defID string, evals []store.EvaluationRow, lineageIncluded bool) store.AggregateResult {
	out := store.AggregateResult{
		DefID:           defID,
		Count:           len(evals),
		LineageIncluded: lineageIncluded,
	}
	if len(evals) == 0 {
		return out
	}

	scores := make([]float64, len(evals))
	dimAcc := map[string][]float64{}
	roleAcc := map[string][]float64{}
	for i, e := range evals {
		scores[i] = e.Score
		for k, v := range e.Dimensions {
			dimAcc[k] = append(dimAcc[k], v)
		}
		roleAcc[e.EmitterRole] = append(roleAcc[e.EmitterRole], e.Score)
	}
	out.Score = statsOf(scores)
	out.Score.Latest = evals[len(evals)-1].Score // evals is ASC by created_at

	if len(dimAcc) > 0 {
		out.Dimensions = make(map[string]store.ScoreStats, len(dimAcc))
		for k, v := range dimAcc {
			out.Dimensions[k] = statsOf(v)
		}
	}
	if len(roleAcc) > 0 {
		out.ByEmitterRole = make(map[string]store.ScoreStats, len(roleAcc))
		for k, v := range roleAcc {
			out.ByEmitterRole[k] = statsOf(v)
		}
	}
	return out
}

// statsOf computes Mean/Median/Min/Max/Count for a non-empty slice.
// Latest is set here as vals[len-1]: callers MUST append in
// created_at ASC order so the last element is the newest. For the
// top-level Score axis the caller currently overwrites Latest after
// returning (the input slice for that axis is built differently); for
// the Dimensions and ByEmitterRole axes the value set here stands.
func statsOf(vals []float64) store.ScoreStats {
	if len(vals) == 0 {
		return store.ScoreStats{}
	}
	out := store.ScoreStats{Count: len(vals), Min: vals[0], Max: vals[0]}
	sum := 0.0
	for _, v := range vals {
		sum += v
		if v < out.Min {
			out.Min = v
		}
		if v > out.Max {
			out.Max = v
		}
	}
	out.Mean = sum / float64(len(vals))
	out.Latest = vals[len(vals)-1] // overwritten by caller for the top-level Score; ok for dim/role

	// Median — sort a copy to avoid mutating caller's slice.
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		out.Median = sorted[mid]
	} else {
		out.Median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
