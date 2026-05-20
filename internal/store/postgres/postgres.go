// Package postgres implements store.Store backed by Postgres 14+ via
// jackc/pgx/v5. SQLite remains the default backend for compact installs;
// Postgres is the production path that unblocks horizontal scaling and
// per-tenant fairness work in v1.0.
//
// The schema and behaviour intentionally mirror the SQLite adapter — both
// run against the shared internal/store/storetest contract suite in CI so
// they can't drift silently. Differences are at the type level only
// (TIMESTAMPTZ vs unix-nano INTEGER, BIGINT vs INTEGER, BYTEA vs BLOB,
// BIGSERIAL vs AUTOINCREMENT). Consumer-visible Store interface behaviour
// is identical.
package postgres

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config carries the operator-supplied connection settings. DSN is
// required. The pool sizing fields are optional; defaults match what a
// single-replica jobs-search-agent deployment hitting hundreds of
// concurrent agents needs.
type Config struct {
	// DSN is a Postgres connection string in libpq format.
	// Example: postgres://user:pass@host:5432/loomcycle?sslmode=require
	DSN string
	// MaxOpenConns caps the pool size. Default 32.
	MaxOpenConns int32
	// MinIdleConns is the floor of warm idle connections. Default 4.
	MinIdleConns int32
	// AutoMigrate, when true, runs MigrateUp() during Open(). When
	// false, Open() calls VerifySchemaCurrent() and refuses to start
	// if the embedded migration set is ahead of the database.
	AutoMigrate bool
	// PingTimeout caps the initial connection check. Default 5s. The
	// pool is otherwise lazy — connections open on first query.
	PingTimeout time.Duration
}

// Store is the Postgres implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool

	// closeOnce guards the Close() idempotency contract.
	closeOnce sync.Once
}

// Open dials Postgres, applies migrations (or verifies schema currency
// when AutoMigrate is false), and returns a ready Store. Caller must
// defer Close().
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("postgres: DSN is required")
	}
	// The embedded migrations driver (golang-migrate/pgx5) registers
	// itself for URL-form DSNs only — pgx5://... — so a libpq
	// keyword=value DSN ("host=foo user=bar password=...") would
	// otherwise produce a confusing "unknown driver" error from deep
	// inside golang-migrate. Refuse upfront with a pointed message.
	if !strings.Contains(cfg.DSN, "://") {
		return nil, errors.New("postgres: DSN must be URL form (postgres://user:pass@host:port/db?...) — keyword=value DSNs are not supported by the embedded migrations driver")
	}
	if cfg.MaxOpenConns <= 0 {
		cfg.MaxOpenConns = 32
	}
	if cfg.MinIdleConns < 0 {
		cfg.MinIdleConns = 0
	}
	if cfg.MinIdleConns > cfg.MaxOpenConns {
		cfg.MinIdleConns = cfg.MaxOpenConns / 4
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 5 * time.Second
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxOpenConns
	poolCfg.MinConns = cfg.MinIdleConns
	// Burst-tolerant idle window. Connections older than this get
	// recycled even if idle, which avoids slow connection leaks under
	// long-running deployments.
	poolCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("dial postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if cfg.AutoMigrate {
		if err := MigrateUp(cfg.DSN); err != nil {
			pool.Close()
			return nil, err
		}
	} else {
		if err := VerifySchemaCurrent(cfg.DSN); err != nil {
			pool.Close()
			return nil, err
		}
	}

	return &Store{pool: pool}, nil
}

// CreateSession inserts a new session with a generated ID. userID may be
// empty; the column accepts NULL via the pointer cast below so empty
// stores as NULL (matters for the partial index on user_id IS NOT NULL).
func (s *Store) CreateSession(ctx context.Context, tenantID, agent, userID string) (store.Session, error) {
	id := newID("s_")
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id, tenant_id, agent, user_id, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, tenantID, agent, nullableText(userID), now,
	); err != nil {
		return store.Session{}, fmt.Errorf("create session: %w", err)
	}
	return store.Session{
		ID:        id,
		TenantID:  tenantID,
		Agent:     agent,
		UserID:    userID,
		CreatedAt: now,
	}, nil
}

// GetSession returns the row by ID or *store.ErrNotFound.
func (s *Store) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	var (
		out       store.Session
		userID    *string
		createdAt time.Time
	)
	row := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, agent, user_id, created_at
		 FROM sessions WHERE id = $1`, sessionID,
	)
	if err := row.Scan(&out.ID, &out.TenantID, &out.Agent, &userID, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
		}
		return store.Session{}, fmt.Errorf("get session: %w", err)
	}
	if userID != nil {
		out.UserID = *userID
	}
	out.CreatedAt = createdAt
	return out, nil
}

// CreateRun inserts a new run row in status="running". Validates the
// referenced session exists with a pre-check so we surface ErrNotFound
// instead of a foreign-key constraint violation.
func (s *Store) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	// Pre-check: surfacing FK violation as ErrNotFound is a contract
	// requirement (the SQLite adapter does the same, and the storetest
	// suite asserts the wrapped error type).
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = $1)`, sessionID).Scan(&exists); err != nil {
		return store.Run{}, fmt.Errorf("check session: %w", err)
	}
	if !exists {
		return store.Run{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
	}

	id := newID("r_")
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO runs (
			id, session_id, status, started_at,
			agent_id, parent_agent_id, parent_run_id, user_id, user_tier, agent_def_id, model
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, sessionID, string(store.RunRunning), now,
		nullableText(identity.AgentID),
		nullableText(identity.ParentAgentID),
		nullableText(identity.ParentRunID),
		nullableText(identity.UserID),
		nullableText(identity.UserTier),
		nullableText(identity.AgentDefID),
		nullableText(identity.Model),
	); err != nil {
		return store.Run{}, fmt.Errorf("create run: %w", err)
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
		AgentDefID:    identity.AgentDefID,
		Model:         identity.Model,
	}, nil
}

// AppendEvent inserts one event for a run. We pre-check the run row so a
// missing FK surfaces as ErrNotFound{Kind:"run"} (matches SQLite adapter).
//
// session_id is denormalised onto the event row because GetTranscript
// queries on (session_id, seq) and a JOIN-per-event would dominate
// transcript-replay cost. We look it up once on first append per run via
// the run row.
func (s *Store) AppendEvent(ctx context.Context, runID string, eventType string, payload []byte) error {
	var sessionID string
	if err := s.pool.QueryRow(ctx, `SELECT session_id FROM runs WHERE id = $1`, runID).Scan(&sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.ErrNotFound{Kind: "run", ID: runID}
		}
		return fmt.Errorf("lookup run: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO events (session_id, run_id, ts, type, payload)
		 VALUES ($1, $2, $3, $4, $5)`,
		sessionID, runID, time.Now().UTC(), eventType, payload,
	); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// FinishRun marks the run terminal. Idempotent: the WHERE status='running'
// guard prevents a late-arriving completed/failed write from overwriting
// a terminal cancellation. Returns nil whether the row was actually
// updated or not (matches SQLite adapter contract).
func (s *Store) FinishRun(ctx context.Context, runID string, status store.RunStatus, stopReason string, usage store.Usage, errMsg string) error {
	completed := time.Now().UTC()
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET
			status = $2,
			completed_at = $3,
			stop_reason = $4,
			input_tokens = $5,
			output_tokens = $6,
			cache_creation_tokens = $7,
			cache_read_tokens = $8,
			model = $9,
			error = $10
		 WHERE id = $1 AND status = $11`,
		runID, string(status), completed, nullableText(stopReason),
		usage.InputTokens, usage.OutputTokens,
		usage.CacheCreationTokens, usage.CacheReadTokens,
		nullableText(usage.Model), nullableText(errMsg),
		string(store.RunRunning),
	)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// GetTranscript returns every event for the session, ordered by seq.
// Empty slice (not error) when the session has no events yet.
func (s *Store) GetTranscript(ctx context.Context, sessionID string) ([]store.Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, session_id, run_id, ts, type, payload
		 FROM events WHERE session_id = $1 ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query transcript: %w", err)
	}
	defer rows.Close()

	out := []store.Event{}
	for rows.Next() {
		var (
			ev store.Event
			ts time.Time
		)
		if err := rows.Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		ev.Timestamp = ts
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter transcript: %w", err)
	}
	return out, nil
}

// GetLastEventForRun returns the latest event by seq for the given
// run. Index hint: events_by_run_seq (added in migration 0015).
func (s *Store) GetLastEventForRun(ctx context.Context, runID string) (store.Event, error) {
	var (
		ev store.Event
		ts time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT seq, session_id, run_id, ts, type, payload
		 FROM events WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
		runID,
	).Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, &store.ErrNotFound{Kind: "event", ID: runID}
	}
	if err != nil {
		return store.Event{}, fmt.Errorf("get last event for run: %w", err)
	}
	ev.Timestamp = ts
	return ev, nil
}

// ListEvents serves the v0.8.21 /v1/_events audit endpoint. Same
// filter semantics as the SQLite adapter; uses $N placeholders and
// numeric args. Index hint: events_by_ts / events_by_type_ts (added
// in migration 0014).
func (s *Store) ListEvents(ctx context.Context, filter store.EventFilter, limit, offset int) ([]store.Event, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var (
		conds []string
		args  []any
		i     = 1
	)
	if filter.Type != "" {
		conds = append(conds, fmt.Sprintf("type = $%d", i))
		args = append(args, filter.Type)
		i++
	}
	if !filter.From.IsZero() {
		conds = append(conds, fmt.Sprintf("ts >= $%d", i))
		args = append(args, filter.From)
		i++
	}
	if !filter.To.IsZero() {
		conds = append(conds, fmt.Sprintf("ts <= $%d", i))
		args = append(args, filter.To)
		i++
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM events "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}

	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx,
		"SELECT seq, session_id, run_id, ts, type, payload FROM events "+where+
			fmt.Sprintf(" ORDER BY ts DESC, seq DESC LIMIT $%d OFFSET $%d", i, i+1),
		args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	out := make([]store.Event, 0, limit)
	for rows.Next() {
		var (
			ev store.Event
			ts time.Time
		)
		if err := rows.Scan(&ev.Seq, &ev.SessionID, &ev.RunID, &ts, &ev.Type, &ev.Payload); err != nil {
			return nil, 0, fmt.Errorf("scan event: %w", err)
		}
		ev.Timestamp = ts
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iter events: %w", err)
	}
	return out, total, nil
}

// GetRunByAgentID returns the most recently started run with the given
// agent_id. Empty agentID short-circuits to ErrNotFound (callers don't
// have to pre-check, matching SQLite adapter behaviour).
func (s *Store) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	if agentID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	row := s.pool.QueryRow(ctx,
		`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
		        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		        r.model, r.error,
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
		        r.agent_def_id, r.pause_state,
		        s.agent
		 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
		 WHERE r.agent_id = $1 ORDER BY r.started_at DESC LIMIT 1`, agentID,
	)
	r, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
		}
		return store.Run{}, fmt.Errorf("get run by agent_id: %w", err)
	}
	return r, nil
}

// GetRun returns one row by run_id (the primary key on runs).
func (s *Store) GetRun(ctx context.Context, runID string) (store.Run, error) {
	if runID == "" {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	row := s.pool.QueryRow(ctx,
		`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
		        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		        r.model, r.error,
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
		        r.agent_def_id, r.pause_state,
		        s.agent
		 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
		 WHERE r.id = $1`, runID,
	)
	r, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
		}
		return store.Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// ListUsers returns one row per distinct user_id with summary stats.
// Drives the v0.7.3 Web UI user picker. Mirrors the SQLite shape so
// behaviour is identical across backends.
func (s *Store) ListUsers(ctx context.Context) ([]store.UserSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			user_id,
			COUNT(*) FILTER (WHERE status = 'running') AS running_count,
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
		if err := rows.Scan(&u.UserID, &u.RunningCount, &u.TotalCount, &u.LastStartedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListActiveRunsByUser returns up to 100 runs for the user, ordered by
// started_at DESC. Empty status returns all statuses; non-empty filters
// to the exact status string. Empty userID short-circuits to no rows.
func (s *Store) ListActiveRunsByUser(ctx context.Context, userID string, status store.RunStatus) ([]store.Run, error) {
	if userID == "" {
		return nil, nil
	}
	const limit = 100
	var (
		rows pgx.Rows
		err  error
	)
	if status == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
			        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
			        r.model, r.error,
			        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
			        r.agent_def_id, r.pause_state,
			        s.agent
			 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
			 WHERE r.user_id = $1
			 ORDER BY r.started_at DESC LIMIT $2`, userID, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
			        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
			        r.model, r.error,
			        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
			        r.agent_def_id, r.pause_state,
			        s.agent
			 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
			 WHERE r.user_id = $1 AND r.status = $2
			 ORDER BY r.started_at DESC LIMIT $3`, userID, string(status), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer rows.Close()
	return scanRunRows(rows)
}

// ListRunsByParentAgentID returns the direct children of the given
// parent. Recursion to grandchildren is the caller's responsibility —
// keeping the SQL flat keeps the indexes simple.
func (s *Store) ListRunsByParentAgentID(ctx context.Context, parentAgentID string) ([]store.Run, error) {
	if parentAgentID == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
		        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		        r.model, r.error,
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
		        r.agent_def_id, r.pause_state,
		        s.agent
		 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
		 WHERE r.parent_agent_id = $1
		 ORDER BY r.started_at ASC`, parentAgentID,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs by parent: %w", err)
	}
	defer rows.Close()
	return scanRunRows(rows)
}

// UpdateHeartbeat advances last_heartbeat_at on a running run. The
// status='running' guard prevents a late heartbeat from un-finalising a
// terminal run (which would corrupt the sweeper's stale-row detection).
func (s *Store) UpdateHeartbeat(ctx context.Context, runID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET last_heartbeat_at = $2
		 WHERE id = $1 AND status = $3`,
		runID, time.Now().UTC(), string(store.RunRunning),
	)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}
	return nil
}

// SweepStaleRuns implements store.Store. Runs whose last_heartbeat_at
// is older than cutoff (or whose started_at is older than cutoff and
// who never heartbeated) are flipped to status="failed" with
// error="heartbeat timeout". Single atomic UPDATE so concurrent
// sweepers — including a future multi-replica deployment — race
// correctly.
func (s *Store) SweepStaleRuns(ctx context.Context, cutoff time.Time) (int, error) {
	cutoffUTC := cutoff.UTC()
	completed := time.Now().UTC()
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET
			status = $1,
			completed_at = $2,
			error = $3,
			stop_reason = $4
		 WHERE status = $5
		   AND (
			 (last_heartbeat_at IS NOT NULL AND last_heartbeat_at < $6)
			 OR (last_heartbeat_at IS NULL AND started_at < $6)
		   )`,
		string(store.RunFailed), completed,
		"heartbeat timeout", "heartbeat_timeout",
		string(store.RunRunning),
		cutoffUTC,
	)
	if err != nil {
		return 0, fmt.Errorf("sweep stale runs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SetRunPauseState implements store.Store. Writes runs.pause_state.
// Refuses unknown state strings — the v0.8.17 PauseManager always uses
// the PauseState* constants but a forward-compat guard at this layer
// prevents a future caller bug from inserting garbage. Returns
// *ErrNotFound when no row matches.
//
// Idempotent — writing the current value is a no-op (UPDATE still
// affects 1 row but the value doesn't change). Does NOT clear
// pause_state for terminal runs; the column on terminal runs records
// what state they were in when the loop exited.
func (s *Store) SetRunPauseState(ctx context.Context, runID, state string) error {
	switch state {
	case store.PauseStateRunning, store.PauseStatePausing, store.PauseStatePaused:
	default:
		return fmt.Errorf("set run pause_state: unknown state %q", state)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET pause_state = $1 WHERE id = $2`, state, runID)
	if err != nil {
		return fmt.Errorf("set run pause_state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "run", ID: runID}
	}
	return nil
}

// ListPausedRuns implements store.Store. Returns runs with
// pause_state = 'paused' (at-rest only, not 'pausing'), ordered by
// started_at ASC. Uses the partial index from 0012_runs_pause_state.
func (s *Store) ListPausedRuns(ctx context.Context) ([]store.Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
		        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		        r.model, r.error,
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at, r.user_tier,
		        r.agent_def_id, r.pause_state,
		        s.agent
		 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
		 WHERE r.pause_state = $1
		 ORDER BY r.started_at ASC`, store.PauseStatePaused)
	if err != nil {
		return nil, fmt.Errorf("list paused runs: %w", err)
	}
	defer rows.Close()
	return scanRunRows(rows)
}

// ---- v0.8.17 Pause/Resume/Snapshot — Snapshot storage (PR 2) ----

// SnapshotCreate inserts one snapshot row. Returns *store.ErrConflict
// on PK violation (id already exists).
func (s *Store) SnapshotCreate(ctx context.Context, row store.SnapshotRow) error {
	if row.ID == "" {
		return fmt.Errorf("snapshot create: id required")
	}
	if len(row.JSONContent) == 0 {
		return fmt.Errorf("snapshot create: json_content required")
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var label any
	if row.Label != "" {
		label = row.Label
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO snapshots(id, created_at, label, schema_version, byte_size, json_content)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		row.ID, createdAt, label, row.SchemaVersion, row.ByteSize, string(row.JSONContent),
	)
	if err != nil {
		// pgx returns *pgconn.PgError with Code 23505 (unique_violation)
		// on PK conflict. Match by code so a future column constraint
		// addition can't be silently caught as a "conflict."
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &store.ErrConflict{Kind: "snapshot", ID: row.ID}
		}
		return fmt.Errorf("snapshot create: %w", err)
	}
	return nil
}

// SnapshotGet returns the full snapshot row including the JSON payload.
func (s *Store) SnapshotGet(ctx context.Context, id string) (store.SnapshotRow, error) {
	if id == "" {
		return store.SnapshotRow{}, &store.ErrNotFound{Kind: "snapshot", ID: id}
	}
	var (
		row         store.SnapshotRow
		label       *string
		jsonContent []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, created_at, label, schema_version, byte_size, json_content
		 FROM snapshots WHERE id = $1`, id,
	).Scan(&row.ID, &row.CreatedAt, &label, &row.SchemaVersion, &row.ByteSize, &jsonContent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.SnapshotRow{}, &store.ErrNotFound{Kind: "snapshot", ID: id}
		}
		return store.SnapshotRow{}, fmt.Errorf("snapshot get: %w", err)
	}
	if label != nil {
		row.Label = *label
	}
	row.JSONContent = jsonContent
	return row, nil
}

// SnapshotList returns the metadata-only projections, optionally
// filtered by case-insensitive label substring and capped at limit.
func (s *Store) SnapshotList(ctx context.Context, labelContains string, limit int) ([]store.SnapshotListEntry, error) {
	var (
		rows pgx.Rows
		err  error
	)
	query := `SELECT id, created_at, label, schema_version, byte_size
	          FROM snapshots `
	args := []any{}
	if labelContains != "" {
		query += `WHERE COALESCE(label, '') ILIKE $1 `
		args = append(args, "%"+labelContains+"%")
	}
	query += `ORDER BY created_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err = s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("snapshot list: %w", err)
	}
	defer rows.Close()
	var out []store.SnapshotListEntry
	for rows.Next() {
		var (
			e     store.SnapshotListEntry
			label *string
		)
		if err := rows.Scan(&e.ID, &e.CreatedAt, &label, &e.SchemaVersion, &e.ByteSize); err != nil {
			return nil, fmt.Errorf("snapshot list scan: %w", err)
		}
		if label != nil {
			e.Label = *label
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotDelete removes one snapshot row. Idempotent — returns
// (false, nil) when nothing matched.
func (s *Store) SnapshotDelete(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM snapshots WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("snapshot delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ---- v0.8.17 Snapshot capture — bulk readers (PR 2.3a) ----

// SnapshotReadAgentDefs implements store.Store.
func (s *Store) SnapshotReadAgentDefs(ctx context.Context) ([]store.AgentDefRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT def_id, name, version, parent_def_id, definition::text, description,
		        created_at, created_by_agent_id, created_by_run_id,
		        retired, bootstrapped_from_static
		 FROM agent_defs
		 ORDER BY name ASC, version ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("snapshot read agent_defs: %w", err)
	}
	defer rows.Close()
	var out []store.AgentDefRow
	for rows.Next() {
		var (
			r           store.AgentDefRow
			parentDefID *string
			description *string
			createdBy   *string
			createdRun  *string
			definition  string
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version, &parentDefID,
			&definition, &description,
			&r.CreatedAt, &createdBy, &createdRun,
			&r.Retired, &r.BootstrappedFromStatic,
		); err != nil {
			return nil, fmt.Errorf("scan agent_def: %w", err)
		}
		r.Definition = json.RawMessage(definition)
		if parentDefID != nil {
			r.ParentDefID = *parentDefID
		}
		if description != nil {
			r.Description = *description
		}
		if createdBy != nil {
			r.CreatedByAgentID = *createdBy
		}
		if createdRun != nil {
			r.CreatedByRunID = *createdRun
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotReadAgentDefActive implements store.Store.
func (s *Store) SnapshotReadAgentDefActive(ctx context.Context) ([]store.AgentDefActiveEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, def_id, promoted_at, promoted_by_agent_id
		 FROM agent_def_active
		 ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read agent_def_active: %w", err)
	}
	defer rows.Close()
	var out []store.AgentDefActiveEntry
	for rows.Next() {
		var (
			e        store.AgentDefActiveEntry
			promoter *string
		)
		if err := rows.Scan(&e.Name, &e.DefID, &e.PromotedAt, &promoter); err != nil {
			return nil, fmt.Errorf("scan agent_def_active: %w", err)
		}
		if promoter != nil {
			e.PromotedByAgentID = *promoter
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadMemory implements store.Store. Filters expired rows.
func (s *Store) SnapshotReadMemory(ctx context.Context) ([]store.MemorySnapshotEntry, error) {
	now := time.Now().UTC()
	rows, err := s.pool.Query(ctx,
		`SELECT scope, scope_id, key, value::text, expires_at, created_at, updated_at
		 FROM memory
		 WHERE expires_at IS NULL OR expires_at > $1
		 ORDER BY scope ASC, scope_id ASC, key ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("snapshot read memory: %w", err)
	}
	defer rows.Close()
	var out []store.MemorySnapshotEntry
	for rows.Next() {
		var (
			e         store.MemorySnapshotEntry
			scopeStr  string
			value     string
			expiresAt *time.Time
		)
		if err := rows.Scan(&scopeStr, &e.ScopeID, &e.Key, &value, &expiresAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		e.Scope = store.MemoryScope(scopeStr)
		e.Value = json.RawMessage(value)
		if expiresAt != nil {
			e.ExpiresAt = *expiresAt
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SnapshotReadChannelMessages implements store.Store. Filters expired
// rows.
func (s *Store) SnapshotReadChannelMessages(ctx context.Context) ([]store.ChannelMessage, error) {
	now := time.Now().UTC()
	rows, err := s.pool.Query(ctx,
		`SELECT id, channel, scope, scope_id, payload::text, published_at, expires_at, visible_at, published_by_user_id
		 FROM channel_messages
		 WHERE expires_at IS NULL OR expires_at > $1
		 ORDER BY channel ASC, scope ASC, scope_id ASC, visible_at ASC, id ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("snapshot read channel_messages: %w", err)
	}
	defer rows.Close()
	var out []store.ChannelMessage
	for rows.Next() {
		var (
			m           store.ChannelMessage
			scopeStr    string
			payload     string
			expiresAt   *time.Time
			visibleAt   *time.Time
			publishedBy *string
		)
		if err := rows.Scan(&m.ID, &m.Channel, &scopeStr, &m.ScopeID, &payload, &m.PublishedAt, &expiresAt, &visibleAt, &publishedBy); err != nil {
			return nil, fmt.Errorf("scan channel_message: %w", err)
		}
		m.Scope = store.MemoryScope(scopeStr)
		m.Payload = json.RawMessage(payload)
		if expiresAt != nil {
			m.ExpiresAt = *expiresAt
		}
		if visibleAt != nil {
			m.VisibleAt = *visibleAt
		}
		if publishedBy != nil {
			m.PublishedByUserID = *publishedBy
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SnapshotReadChannelCursors implements store.Store.
func (s *Store) SnapshotReadChannelCursors(ctx context.Context) ([]store.ChannelCursorEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT channel, scope, scope_id, cursor, updated_at
		 FROM channel_cursors
		 ORDER BY channel ASC, scope ASC, scope_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read channel_cursors: %w", err)
	}
	defer rows.Close()
	var out []store.ChannelCursorEntry
	for rows.Next() {
		var (
			c        store.ChannelCursorEntry
			scopeStr string
		)
		if err := rows.Scan(&c.Channel, &scopeStr, &c.ScopeID, &c.Cursor, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan channel_cursor: %w", err)
		}
		c.Scope = store.MemoryScope(scopeStr)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SnapshotReadEvaluations implements store.Store. Ordered by
// created_at ASC so the envelope preserves submission order.
func (s *Store) SnapshotReadEvaluations(ctx context.Context) ([]store.EvaluationRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT eval_id, run_id, def_id, score, dimensions::text, judgement::text, rationale,
		        emitter_role, emitter_agent_id, emitter_run_id, created_at
		 FROM evaluations
		 ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("snapshot read evaluations: %w", err)
	}
	defer rows.Close()
	var out []store.EvaluationRow
	for rows.Next() {
		var (
			r              store.EvaluationRow
			defID          *string
			dimensions     *string
			judgement      *string
			rationale      *string
			emitterAgentID *string
			emitterRunID   *string
		)
		if err := rows.Scan(
			&r.EvalID, &r.RunID, &defID, &r.Score,
			&dimensions, &judgement, &rationale,
			&r.EmitterRole, &emitterAgentID, &emitterRunID,
			&r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan evaluation: %w", err)
		}
		if defID != nil {
			r.DefID = *defID
		}
		if dimensions != nil && *dimensions != "" {
			var dim map[string]float64
			if err := json.Unmarshal([]byte(*dimensions), &dim); err == nil {
				r.Dimensions = dim
			}
		}
		if judgement != nil && *judgement != "" {
			r.Judgement = json.RawMessage(*judgement)
		}
		if rationale != nil {
			r.Rationale = *rationale
		}
		if emitterAgentID != nil {
			r.EmitterAgentID = *emitterAgentID
		}
		if emitterRunID != nil {
			r.EmitterRunID = *emitterRunID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- v0.8.17 Snapshot restore — idempotent raw inserts (PR 3.2a) ----

// SnapshotRestoreSession implements store.Store.
func (s *Store) SnapshotRestoreSession(ctx context.Context, sess store.Session) (bool, error) {
	if sess.ID == "" {
		return false, fmt.Errorf("snapshot restore session: id required")
	}
	createdAt := sess.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var userID any
	if sess.UserID != "" {
		userID = sess.UserID
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO sessions(id, tenant_id, agent, created_at, user_id) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO NOTHING`,
		sess.ID, sess.TenantID, sess.Agent, createdAt, userID,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore session: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreRun implements store.Store.
func (s *Store) SnapshotRestoreRun(ctx context.Context, r store.Run) (bool, error) {
	if r.ID == "" || r.SessionID == "" {
		return false, fmt.Errorf("snapshot restore run: id and session_id required")
	}
	startedAt := r.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	var completedAt, lastHbAt *time.Time
	if !r.CompletedAt.IsZero() {
		t := r.CompletedAt
		completedAt = &t
	}
	if !r.LastHeartbeatAt.IsZero() {
		t := r.LastHeartbeatAt
		lastHbAt = &t
	}
	status := string(r.Status)
	if status == "" {
		status = string(store.RunRunning)
	}
	pauseState := r.PauseState
	if pauseState == "" {
		pauseState = store.PauseStateRunning
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO runs(
			id, session_id, status, started_at, completed_at, stop_reason,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			model, error,
			agent_id, parent_agent_id, parent_run_id, user_id, last_heartbeat_at,
			user_tier, agent_def_id, pause_state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		 ON CONFLICT (id) DO NOTHING`,
		r.ID, r.SessionID, status, startedAt, completedAt, nullIfEmpty(r.StopReason),
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		nullIfEmpty(r.Model), nullIfEmpty(r.ErrorMsg),
		nullIfEmpty(r.AgentID), nullIfEmpty(r.ParentAgentID), nullIfEmpty(r.ParentRunID),
		nullIfEmpty(r.UserID), lastHbAt,
		nullIfEmpty(r.UserTier), nullIfEmpty(r.AgentDefID), pauseState,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore run: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreEvent implements store.Store.
func (s *Store) SnapshotRestoreEvent(ctx context.Context, e store.Event) (bool, error) {
	if e.RunID == "" || e.SessionID == "" {
		return false, fmt.Errorf("snapshot restore event: run_id and session_id required")
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	if e.Seq != 0 {
		tag, err := s.pool.Exec(ctx,
			`INSERT INTO events(seq, session_id, run_id, ts, type, payload) VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (seq) DO NOTHING`,
			e.Seq, e.SessionID, e.RunID, ts, e.Type, e.Payload,
		)
		if err != nil {
			return false, fmt.Errorf("snapshot restore event: %w", err)
		}
		return tag.RowsAffected() > 0, nil
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO events(session_id, run_id, ts, type, payload) VALUES ($1, $2, $3, $4, $5)`,
		e.SessionID, e.RunID, ts, e.Type, e.Payload,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore event (auto-seq): %w", err)
	}
	return true, nil
}

// SnapshotRestoreAgentDef implements store.Store.
func (s *Store) SnapshotRestoreAgentDef(ctx context.Context, r store.AgentDefRow) (bool, error) {
	if r.DefID == "" || r.Name == "" {
		return false, fmt.Errorf("snapshot restore agent_def: def_id and name required")
	}
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO agent_defs(
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (def_id) DO NOTHING`,
		r.DefID, r.Name, r.Version, nullIfEmpty(r.ParentDefID),
		string(r.Definition), nullIfEmpty(r.Description),
		createdAt, nullIfEmpty(r.CreatedByAgentID), nullIfEmpty(r.CreatedByRunID),
		r.Retired, r.BootstrappedFromStatic,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore agent_def: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreAgentDefActive implements store.Store. ON CONFLICT
// DO NOTHING — first restore writes the snapshot's promoted_at +
// def_id; subsequent restores leave the existing row alone so the
// (bool, error) return reads as "not inserted" and the caller's
// counter stays honest on a re-restore.
func (s *Store) SnapshotRestoreAgentDefActive(ctx context.Context, e store.AgentDefActiveEntry) (bool, error) {
	if e.Name == "" || e.DefID == "" {
		return false, fmt.Errorf("snapshot restore agent_def_active: name and def_id required")
	}
	promotedAt := e.PromotedAt
	if promotedAt.IsZero() {
		promotedAt = time.Now().UTC()
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO agent_def_active(name, def_id, promoted_at, promoted_by_agent_id) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name) DO NOTHING`,
		e.Name, e.DefID, promotedAt, nullIfEmpty(e.PromotedByAgentID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore agent_def_active: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreMemory implements store.Store.
func (s *Store) SnapshotRestoreMemory(ctx context.Context, e store.MemorySnapshotEntry) (bool, error) {
	if e.Scope == "" || e.Key == "" {
		return false, fmt.Errorf("snapshot restore memory: scope and key required")
	}
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := e.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	var expiresAt *time.Time
	if !e.ExpiresAt.IsZero() {
		t := e.ExpiresAt
		expiresAt = &t
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO memory(scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		 ON CONFLICT (scope, scope_id, key) DO NOTHING`,
		string(e.Scope), e.ScopeID, e.Key, string(e.Value),
		expiresAt, createdAt, updatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore memory: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreChannelMessage implements store.Store.
func (s *Store) SnapshotRestoreChannelMessage(ctx context.Context, m store.ChannelMessage) (bool, error) {
	if m.ID == "" || m.Channel == "" {
		return false, fmt.Errorf("snapshot restore channel_message: id and channel required")
	}
	publishedAt := m.PublishedAt
	if publishedAt.IsZero() {
		publishedAt = time.Now().UTC()
	}
	var expiresAt *time.Time
	if !m.ExpiresAt.IsZero() {
		t := m.ExpiresAt
		expiresAt = &t
	}
	visibleAt := m.VisibleAt
	if visibleAt.IsZero() {
		visibleAt = publishedAt
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO channel_messages(id, channel, scope, scope_id, payload, published_at, expires_at, visible_at, published_by_user_id)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)
		 ON CONFLICT (id) DO NOTHING`,
		m.ID, m.Channel, string(m.Scope), m.ScopeID, string(m.Payload),
		publishedAt, expiresAt, visibleAt, nullIfEmpty(m.PublishedByUserID),
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore channel_message: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreChannelCursor implements store.Store. ON CONFLICT
// DO NOTHING — first restore writes the snapshot's cursor; subsequent
// restores leave an evolved live cursor alone so the (bool, error)
// return reads as "not inserted" on a re-restore.
func (s *Store) SnapshotRestoreChannelCursor(ctx context.Context, c store.ChannelCursorEntry) (bool, error) {
	if c.Channel == "" || c.Cursor == "" {
		return false, fmt.Errorf("snapshot restore channel_cursor: channel and cursor required")
	}
	updatedAt := c.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO channel_cursors(channel, scope, scope_id, cursor, updated_at) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (channel, scope, scope_id) DO NOTHING`,
		c.Channel, string(c.Scope), c.ScopeID, c.Cursor, updatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore channel_cursor: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SnapshotRestoreEvaluation implements store.Store.
func (s *Store) SnapshotRestoreEvaluation(ctx context.Context, r store.EvaluationRow) (bool, error) {
	if r.EvalID == "" || r.RunID == "" {
		return false, fmt.Errorf("snapshot restore evaluation: eval_id and run_id required")
	}
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var dimensions, judgement any
	if len(r.Dimensions) > 0 {
		b, err := json.Marshal(r.Dimensions)
		if err == nil {
			dimensions = string(b)
		}
	}
	if len(r.Judgement) > 0 {
		judgement = string(r.Judgement)
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO evaluations(
			eval_id, run_id, def_id, score, dimensions, judgement, rationale,
			emitter_role, emitter_agent_id, emitter_run_id, created_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9, $10, $11)
		 ON CONFLICT (eval_id) DO NOTHING`,
		r.EvalID, r.RunID, nullIfEmpty(r.DefID), r.Score,
		dimensions, judgement, nullIfEmpty(r.Rationale),
		r.EmitterRole, nullIfEmpty(r.EmitterAgentID), nullIfEmpty(r.EmitterRunID),
		createdAt,
	)
	if err != nil {
		return false, fmt.Errorf("snapshot restore evaluation: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// nullIfEmpty converts an empty string to a *string nil so pgx writes
// SQL NULL into nullable text columns rather than "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- v0.8.x Process-resource metrics sampler ----

// MetricsWriteSample inserts one process_samples row.
func (s *Store) MetricsWriteSample(ctx context.Context, sample store.ProcessSample) error {
	var sysCPU, sysMemUsed, sysMemAvail any
	if sample.SystemCPUPctX100 != nil {
		sysCPU = *sample.SystemCPUPctX100
	}
	if sample.SystemMemUsedMB != nil {
		sysMemUsed = *sample.SystemMemUsedMB
	}
	if sample.SystemMemAvailableMB != nil {
		sysMemAvail = *sample.SystemMemAvailableMB
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO process_samples(
		sample_id, sampled_at, active_runs, queued_runs,
		loomcycle_rss_bytes, loomcycle_heap_alloc_bytes, loomcycle_heap_inuse_bytes,
		loomcycle_num_goroutines, loomcycle_cpu_pct_x100,
		system_cpu_pct_x100, system_mem_used_mb, system_mem_available_mb
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		sample.SampleID, sample.SampledAt, sample.ActiveRuns, sample.QueuedRuns,
		sample.LoomcycleRSSBytes, sample.LoomcycleHeapAlloc, sample.LoomcycleHeapInuse,
		sample.LoomcycleGoroutines, sample.LoomcycleCPUPctX100,
		sysCPU, sysMemUsed, sysMemAvail,
	)
	if err != nil {
		return fmt.Errorf("metrics: write sample: %w", err)
	}
	return nil
}

// MetricsSampleWindow returns samples in [since, until] ordered by
// sampled_at ASC then sample_id ASC. Pagination via the last
// sample_id seen.
func (s *Store) MetricsSampleWindow(ctx context.Context, since, until time.Time, limit int, cursor string) ([]store.ProcessSample, string, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT sample_id, sampled_at, active_runs, queued_runs,
	             loomcycle_rss_bytes, loomcycle_heap_alloc_bytes, loomcycle_heap_inuse_bytes,
	             loomcycle_num_goroutines, loomcycle_cpu_pct_x100,
	             system_cpu_pct_x100, system_mem_used_mb, system_mem_available_mb
	      FROM process_samples
	      WHERE sampled_at BETWEEN $1 AND $2`
	args := []any{since, until}
	if cursor != "" {
		q += ` AND sample_id > $3`
		args = append(args, cursor)
	}
	q += ` ORDER BY sampled_at ASC, sample_id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("metrics: query window: %w", err)
	}
	defer rows.Close()
	out := make([]store.ProcessSample, 0, limit)
	for rows.Next() {
		var (
			rec                          store.ProcessSample
			sysCPU, sysMemU, sysMemAvail *int
		)
		if err := rows.Scan(
			&rec.SampleID, &rec.SampledAt, &rec.ActiveRuns, &rec.QueuedRuns,
			&rec.LoomcycleRSSBytes, &rec.LoomcycleHeapAlloc, &rec.LoomcycleHeapInuse,
			&rec.LoomcycleGoroutines, &rec.LoomcycleCPUPctX100,
			&sysCPU, &sysMemU, &sysMemAvail,
		); err != nil {
			return nil, "", fmt.Errorf("metrics: scan sample: %w", err)
		}
		rec.SystemCPUPctX100 = sysCPU
		rec.SystemMemUsedMB = sysMemU
		rec.SystemMemAvailableMB = sysMemAvail
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("metrics: iterate samples: %w", err)
	}
	nextCursor := ""
	if len(out) > limit {
		out = out[:limit]
		nextCursor = out[len(out)-1].SampleID
	}
	return out, nextCursor, nil
}

// MetricsRunSummary aggregates samples overlapping the run's window.
func (s *Store) MetricsRunSummary(ctx context.Context, runID string) (store.MetricsRunWindow, error) {
	var (
		startedAt   time.Time
		completedAt *time.Time
	)
	row := s.pool.QueryRow(ctx, `SELECT started_at, completed_at FROM runs WHERE id = $1`, runID)
	if err := row.Scan(&startedAt, &completedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.MetricsRunWindow{}, &store.ErrNotFound{Kind: "run", ID: runID}
		}
		return store.MetricsRunWindow{}, fmt.Errorf("metrics: read run %s: %w", runID, err)
	}
	upper := time.Now().UTC()
	if completedAt != nil {
		upper = *completedAt
	}
	var (
		sampleCount   int
		peakRSS       *int64
		meanRSS       *float64
		maxCPUPctX100 *int
	)
	row = s.pool.QueryRow(ctx, `SELECT
		COUNT(*),
		MAX(loomcycle_rss_bytes),
		AVG(loomcycle_rss_bytes),
		MAX(loomcycle_cpu_pct_x100)
	FROM process_samples
	WHERE sampled_at BETWEEN $1 AND $2`, startedAt, upper)
	if err := row.Scan(&sampleCount, &peakRSS, &meanRSS, &maxCPUPctX100); err != nil {
		return store.MetricsRunWindow{}, fmt.Errorf("metrics: aggregate run %s: %w", runID, err)
	}
	out := store.MetricsRunWindow{
		RunID:       runID,
		StartedAt:   startedAt,
		SampleCount: sampleCount,
	}
	if completedAt != nil {
		out.CompletedAt = *completedAt
	}
	if peakRSS != nil {
		out.PeakRSSBytes = *peakRSS
	}
	if meanRSS != nil {
		out.MeanRSSBytes = int64(*meanRSS)
	}
	if maxCPUPctX100 != nil {
		out.MaxCPUPctX100 = *maxCPUPctX100
	}
	return out, nil
}

// MetricsSweep deletes samples with sampled_at < cutoff.
func (s *Store) MetricsSweep(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM process_samples WHERE sampled_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("metrics: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// --- v0.8.15 dynamic_agents (LoomCycle MCP runtime registration) ---

func (s *Store) DynamicAgentUpsert(ctx context.Context, a store.DynamicAgent) error {
	if a.Name == "" {
		return fmt.Errorf("dynamic_agents: name required")
	}
	if len(a.Definition) == 0 {
		return fmt.Errorf("dynamic_agents: definition required")
	}
	createdAt := a.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	// Postgres expires_at column defaults to 'epoch' for "no expiry".
	expiresAt := a.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Unix(0, 0).UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dynamic_agents (name, definition, created_at, expires_at, description)
		VALUES ($1, $2::jsonb, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET
			definition  = EXCLUDED.definition,
			created_at  = EXCLUDED.created_at,
			expires_at  = EXCLUDED.expires_at,
			description = EXCLUDED.description
	`, a.Name, string(a.Definition), createdAt, expiresAt, a.Description)
	if err != nil {
		return fmt.Errorf("dynamic_agents: upsert: %w", err)
	}
	return nil
}

func (s *Store) DynamicAgentGet(ctx context.Context, name string) (store.DynamicAgent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT name, definition::text, created_at, expires_at, COALESCE(description, '')
		FROM dynamic_agents
		WHERE name = $1 AND (expires_at = 'epoch' OR expires_at > $2)
	`, name, time.Now())

	var a store.DynamicAgent
	var defStr string
	if err := row.Scan(&a.Name, &defStr, &a.CreatedAt, &a.ExpiresAt, &a.Description); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.DynamicAgent{}, &store.ErrNotFound{Kind: "dynamic_agent", ID: name}
		}
		return store.DynamicAgent{}, fmt.Errorf("dynamic_agents: get: %w", err)
	}
	a.Definition = []byte(defStr)
	// Normalise the "no expiry" marker back to zero-value Time so
	// callers (and the connector layer) see a consistent shape across
	// SQLite (0 unix-ns) and Postgres ('epoch').
	if a.ExpiresAt.Unix() == 0 {
		a.ExpiresAt = time.Time{}
	}
	return a, nil
}

func (s *Store) DynamicAgentList(ctx context.Context) ([]store.DynamicAgent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, definition::text, created_at, expires_at, COALESCE(description, '')
		FROM dynamic_agents
		WHERE expires_at = 'epoch' OR expires_at > $1
		ORDER BY created_at DESC
		LIMIT 200
	`, time.Now())
	if err != nil {
		return nil, fmt.Errorf("dynamic_agents: list: %w", err)
	}
	defer rows.Close()

	out := []store.DynamicAgent{}
	for rows.Next() {
		var a store.DynamicAgent
		var defStr string
		if err := rows.Scan(&a.Name, &defStr, &a.CreatedAt, &a.ExpiresAt, &a.Description); err != nil {
			return nil, fmt.Errorf("dynamic_agents: list scan: %w", err)
		}
		a.Definition = []byte(defStr)
		if a.ExpiresAt.Unix() == 0 {
			a.ExpiresAt = time.Time{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DynamicAgentDelete(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dynamic_agents WHERE name = $1`, name)
	if err != nil {
		return false, fmt.Errorf("dynamic_agents: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) DynamicAgentSweep(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM dynamic_agents
		WHERE expires_at > 'epoch' AND expires_at < $1
	`, time.Now())
	if err != nil {
		return 0, fmt.Errorf("dynamic_agents: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- Interruption (v0.8.16) ----------------------------------------

// nullableTimePtr returns a *time.Time pointing at t when non-zero,
// nil otherwise. Postgres expires_at / resolved_at are nullable
// TIMESTAMPTZ; pgx writes SQL NULL on nil pointers.
func nullableTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullableStringArg returns the string when non-empty, nil otherwise
// (writes SQL NULL via pgx).
func nullableStringArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableJSONArg returns the raw JSON as a string when non-empty,
// nil otherwise. Postgres column type is JSONB; pgx-side we send TEXT
// + cast via the ::jsonb in the INSERT/UPDATE query.
func nullableJSONArg(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func (s *Store) InterruptCreate(ctx context.Context, r store.InterruptRow) (string, error) {
	if r.InterruptID == "" {
		return "", fmt.Errorf("interrupts: interrupt_id required")
	}
	if r.RunID == "" {
		return "", fmt.Errorf("interrupts: run_id required")
	}
	if r.Kind == "" {
		r.Kind = store.InterruptKindQuestion
	}
	if r.Status == "" {
		r.Status = store.InterruptStatusPending
	}
	if r.Priority == "" {
		r.Priority = store.InterruptPriorityNormal
	}
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO interrupts (
			interrupt_id, run_id, kind, status,
			question, options, context_data, priority,
			answer, answer_meta,
			created_at, expires_at, resolved_at, resolved_by,
			user_id, agent_id, agent_name
		) VALUES (
			$1, $2, $3, $4,
			$5, $6::jsonb, $7, $8,
			$9, $10::jsonb,
			$11, $12, NULL, $13,
			$14, $15, $16
		)
	`,
		r.InterruptID, r.RunID, r.Kind, r.Status,
		nullableStringArg(r.Question), nullableJSONArg(r.Options), nullableStringArg(r.ContextData), r.Priority,
		nullableStringArg(r.Answer), nullableJSONArg(r.AnswerMeta),
		createdAt, nullableTimePtr(r.ExpiresAt), nullableStringArg(r.ResolvedBy),
		nullableStringArg(r.UserID), nullableStringArg(r.AgentID), nullableStringArg(r.AgentName),
	)
	if err != nil {
		return "", fmt.Errorf("interrupts: create: %w", err)
	}
	return r.InterruptID, nil
}

func (s *Store) InterruptGet(ctx context.Context, interruptID string) (store.InterruptRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT interrupt_id, run_id, kind, status,
		       question, options::text, context_data, priority,
		       answer, answer_meta::text,
		       created_at, expires_at, resolved_at, resolved_by,
		       user_id, agent_id, agent_name
		FROM interrupts
		WHERE interrupt_id = $1
	`, interruptID)
	r, err := s.scanInterruptRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.InterruptRow{}, &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
	}
	if err != nil {
		return store.InterruptRow{}, fmt.Errorf("interrupts: get: %w", err)
	}
	return r, nil
}

// pgRowScanner abstracts pgx.Row / pgx.Rows for scanInterruptRow.
type pgRowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanInterruptRow(row pgRowScanner) (store.InterruptRow, error) {
	var r store.InterruptRow
	var question, options, contextData, answer, answerMeta, resolvedBy, userID, agentID, agentName *string
	var expiresAt, resolvedAt *time.Time
	if err := row.Scan(
		&r.InterruptID, &r.RunID, &r.Kind, &r.Status,
		&question, &options, &contextData, &r.Priority,
		&answer, &answerMeta,
		&r.CreatedAt, &expiresAt, &resolvedAt, &resolvedBy,
		&userID, &agentID, &agentName,
	); err != nil {
		return store.InterruptRow{}, err
	}
	if question != nil {
		r.Question = *question
	}
	if contextData != nil {
		r.ContextData = *contextData
	}
	if answer != nil {
		r.Answer = *answer
	}
	if resolvedBy != nil {
		r.ResolvedBy = *resolvedBy
	}
	if userID != nil {
		r.UserID = *userID
	}
	if agentID != nil {
		r.AgentID = *agentID
	}
	if agentName != nil {
		r.AgentName = *agentName
	}
	if options != nil && *options != "" {
		r.Options = json.RawMessage(*options)
	}
	if answerMeta != nil && *answerMeta != "" {
		r.AnswerMeta = json.RawMessage(*answerMeta)
	}
	if expiresAt != nil {
		r.ExpiresAt = *expiresAt
	}
	if resolvedAt != nil {
		r.ResolvedAt = *resolvedAt
	}
	return r, nil
}

func (s *Store) InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error {
	now := time.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE interrupts
		SET status      = $1,
		    answer      = $2,
		    answer_meta = $3::jsonb,
		    resolved_at = $4,
		    resolved_by = $5
		WHERE interrupt_id = $6
		  AND status = $7
		  AND (expires_at IS NULL OR expires_at > $8)
	`,
		store.InterruptStatusResolved,
		answer, nullableJSONArg(answerMeta),
		now, resolvedBy,
		interruptID, store.InterruptStatusPending, now,
	)
	if err != nil {
		return fmt.Errorf("interrupts: resolve: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existing string
		err := s.pool.QueryRow(ctx, `SELECT status FROM interrupts WHERE interrupt_id = $1`, interruptID).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
		}
		if err != nil {
			return fmt.Errorf("interrupts: resolve probe: %w", err)
		}
		return store.ErrInterruptAlreadyTerminal
	}
	return nil
}

func (s *Store) InterruptFinish(ctx context.Context, interruptID, status, resolvedBy string) error {
	switch status {
	case store.InterruptStatusTimedOut, store.InterruptStatusCancelled:
		// ok
	default:
		return fmt.Errorf("interrupts: finish: invalid terminal status %q", status)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE interrupts
		SET status      = $1,
		    resolved_at = $2,
		    resolved_by = $3
		WHERE interrupt_id = $4 AND status = $5
	`,
		status,
		time.Now(), resolvedBy,
		interruptID, store.InterruptStatusPending,
	)
	if err != nil {
		return fmt.Errorf("interrupts: finish: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existing string
		err := s.pool.QueryRow(ctx, `SELECT status FROM interrupts WHERE interrupt_id = $1`, interruptID).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.ErrNotFound{Kind: "interrupt", ID: interruptID}
		}
		if err != nil {
			return fmt.Errorf("interrupts: finish probe: %w", err)
		}
		return store.ErrInterruptAlreadyTerminal
	}
	return nil
}

func (s *Store) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	return s.interruptList(ctx, "run_id", runID, statusFilter)
}

func (s *Store) InterruptListByUser(ctx context.Context, userID, statusFilter string) ([]store.InterruptRow, error) {
	return s.interruptList(ctx, "user_id", userID, statusFilter)
}

func (s *Store) interruptList(ctx context.Context, col, val, statusFilter string) ([]store.InterruptRow, error) {
	if col != "run_id" && col != "user_id" {
		return nil, fmt.Errorf("interrupts: list: unknown filter column %q", col)
	}
	q := `
		SELECT interrupt_id, run_id, kind, status,
		       question, options::text, context_data, priority,
		       answer, answer_meta::text,
		       created_at, expires_at, resolved_at, resolved_by,
		       user_id, agent_id, agent_name
		FROM interrupts
		WHERE ` + col + ` = $1`
	args := []any{val}
	if statusFilter != "" {
		q += ` AND status = $2`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interrupts: list: %w", err)
	}
	defer rows.Close()

	out := []store.InterruptRow{}
	for rows.Next() {
		r, err := s.scanInterruptRow(rows)
		if err != nil {
			return nil, fmt.Errorf("interrupts: list scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InterruptCountPendingByRun(ctx context.Context, runID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM interrupts WHERE run_id = $1 AND status = $2
	`, runID, store.InterruptStatusPending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("interrupts: count pending: %w", err)
	}
	return n, nil
}

func (s *Store) InterruptSweepExpired(ctx context.Context) (int, error) {
	now := time.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE interrupts
		SET status      = $1,
		    resolved_at = $2,
		    resolved_by = $3
		WHERE status = $4 AND expires_at IS NOT NULL AND expires_at < $5
	`,
		store.InterruptStatusTimedOut,
		now, store.InterruptResolvedByTimeout,
		store.InterruptStatusPending, now,
	)
	if err != nil {
		return 0, fmt.Errorf("interrupts: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Close releases the connection pool. Idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.pool.Close()
	})
	return nil
}

// Pool exposes the underlying pgxpool for the migrate subcommands and the
// future SQLite-to-Postgres data migration tool. Not part of the Store
// interface — this is package-internal access for the runtime layer.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// MemorySet upserts a Memory row. The value column is JSONB; we cast
// the input bytes to ::jsonb in the query so the database validates
// the JSON shape (an invalid payload surfaces as a SQL error rather
// than a silently-stored bad row).
func (s *Store) MemorySet(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error {
	now := time.Now().UTC()
	var expiresAt any
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO memory (scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		 ON CONFLICT (scope, scope_id, key) DO UPDATE SET
		    value = EXCLUDED.value,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = EXCLUDED.updated_at`,
		string(scope), scopeID, key, string(value), expiresAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("memory set: %w", err)
	}
	return nil
}

// MemoryGet returns one entry. Expired rows are surfaced as
// ErrNotFound (the WHERE clause filters them out so the caller never
// sees a stale value, even if the sweeper is behind).
func (s *Store) MemoryGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	var (
		valueText []byte
		expiresAt *time.Time
		createdAt time.Time
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT value::text, expires_at, created_at, updated_at
		 FROM memory
		 WHERE scope = $1 AND scope_id = $2 AND key = $3
		   AND (expires_at IS NULL OR expires_at > NOW())`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	}
	if err != nil {
		return store.MemoryEntry{}, fmt.Errorf("memory get: %w", err)
	}
	out := store.MemoryEntry{
		Key:       key,
		Value:     json.RawMessage(valueText),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if expiresAt != nil {
		out.ExpiresAt = *expiresAt
	}
	return out, nil
}

// MemoryDelete removes a row. The boolean reports whether a row was
// actually present. Both branches are non-error.
func (s *Store) MemoryDelete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM memory WHERE scope = $1 AND scope_id = $2 AND key = $3`,
		string(scope), scopeID, key,
	)
	if err != nil {
		return false, fmt.Errorf("memory delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MemoryList enumerates entries for a (scope, scopeID), filtered by
// prefix and capped at limit. The query fetches limit+1 rows so we
// can report truncated == true without a separate COUNT(*).
func (s *Store) MemoryList(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	pattern := escapeLikePrefix(prefix) + "%"
	rows, err := s.pool.Query(ctx,
		`SELECT key, value::text, expires_at, created_at, updated_at
		 FROM memory
		 WHERE scope = $1 AND scope_id = $2 AND key LIKE $3 ESCAPE '\'
		   AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY key ASC
		 LIMIT $4`,
		string(scope), scopeID, pattern, limit+1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("memory list: %w", err)
	}
	defer rows.Close()
	var out []store.MemoryEntry
	for rows.Next() {
		var (
			key       string
			valueText []byte
			expiresAt *time.Time
			createdAt time.Time
			updatedAt time.Time
		)
		if err := rows.Scan(&key, &valueText, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, false, fmt.Errorf("memory list scan: %w", err)
		}
		entry := store.MemoryEntry{
			Key:       key,
			Value:     json.RawMessage(valueText),
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		}
		if expiresAt != nil {
			entry.ExpiresAt = *expiresAt
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("memory list iter: %w", err)
	}
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
}

// MemoryIncrement is the atomic counter primitive. We do the parse +
// add in a single transaction. `SELECT ... FOR UPDATE` correctly
// serialises increments on an EXISTING row, but does NOT block when
// the row is absent (no row to lock) — two concurrent first-
// increments of the same key would both see ErrNoRows, both compute
// `delta`, and both INSERT. The unique constraint serialises the
// writes (one INSERT wins, the other falls into ON CONFLICT DO
// UPDATE), but EXCLUDED.value is the SECOND transaction's `delta`
// rather than `first_result + delta`, losing the first's contribution.
//
// Fix: take a transaction-scoped advisory lock keyed by the
// (scope, scope_id, key) hash before SELECT-ing. This serialises
// every increment on the same key — the FIRST winner does its
// SELECT (NoRows → INSERT delta), commits, releases the advisory
// lock; the SECOND now does its SELECT (sees value=delta → INSERT
// 2*delta via ON CONFLICT DO UPDATE). Different keys hash to
// different lock IDs and don't contend. Verified by a 100-goroutine
// regression test in storetest (all 100 increments must land).
func (s *Store) MemoryIncrement(ctx context.Context, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("memory incr begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		string(scope)+":"+scopeID+":"+key,
	); err != nil {
		return 0, fmt.Errorf("memory incr lock: %w", err)
	}

	var (
		valueText []byte
		expiresAt *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT value::text, expires_at FROM memory
		 WHERE scope = $1 AND scope_id = $2 AND key = $3`,
		string(scope), scopeID, key,
	).Scan(&valueText, &expiresAt)

	now := time.Now().UTC()
	rowExists := !errors.Is(err, pgx.ErrNoRows)
	if rowExists && err != nil {
		return 0, fmt.Errorf("memory incr select: %w", err)
	}
	if rowExists && expiresAt != nil && now.After(*expiresAt) {
		// Treat expired as missing — increment from zero rather than
		// the stale value.
		rowExists = false
	}

	var current int64
	if rowExists {
		text := strings.TrimSpace(string(valueText))
		n, parseErr := strconv.ParseInt(text, 10, 64)
		if parseErr != nil {
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
		newExpires = now.Add(ttl)
	case rowExists && expiresAt != nil:
		newExpires = *expiresAt // preserve existing expiry
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO memory (scope, scope_id, key, value, expires_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		 ON CONFLICT (scope, scope_id, key) DO UPDATE SET
		    value = EXCLUDED.value,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = EXCLUDED.updated_at`,
		string(scope), scopeID, key, nextText, newExpires, now, now,
	); err != nil {
		return 0, fmt.Errorf("memory incr write: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("memory incr commit: %w", err)
	}
	return next, nil
}

// MemoryListScopeIDs returns distinct scope_ids under scope with
// summary stats. octet_length(value::text) is used for the bytes
// estimate — JSONB has no LENGTH() in the SQLite sense; the textual
// representation is what an operator cares about anyway. Capped at
// 200 rows ordered by updated_at DESC.
func (s *Store) MemoryListScopeIDs(ctx context.Context, scope store.MemoryScope) ([]store.MemoryScopeIDSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			scope_id,
			COUNT(*)                                                          AS key_count,
			COALESCE(SUM(octet_length(key) + octet_length(value::text)), 0)   AS bytes,
			MAX(updated_at)                                                   AS updated_at
		FROM memory
		WHERE scope = $1 AND (expires_at IS NULL OR expires_at > NOW())
		GROUP BY scope_id
		ORDER BY updated_at DESC
		LIMIT 200`,
		string(scope),
	)
	if err != nil {
		return nil, fmt.Errorf("memory list scope ids: %w", err)
	}
	defer rows.Close()
	var out []store.MemoryScopeIDSummary
	for rows.Next() {
		var summary store.MemoryScopeIDSummary
		if err := rows.Scan(&summary.ScopeID, &summary.KeyCount, &summary.Bytes, &summary.UpdatedAt); err != nil {
			return nil, fmt.Errorf("memory list scope ids scan: %w", err)
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// MemorySweep deletes every Memory row whose expires_at has passed.
// Single atomic DELETE so concurrent sweepers race correctly.
func (s *Store) MemorySweep(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM memory WHERE expires_at IS NOT NULL AND expires_at <= NOW()`,
	)
	if err != nil {
		return 0, fmt.Errorf("memory sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- v0.8.4 Channel tool ----
//
// Postgres mirror of the SQLite implementation. Reads filter expired
// rows at the WHERE clause; the sweeper is best-effort cleanup, not a
// correctness anchor. payload is JSONB so future filter primitives
// can use @> / -> without a migration.

// ChannelPublish appends one message and (if maxMessages > 0) trims
// the channel down to maxMessages oldest-first inside the same txn.
// Returns the new id + the count of rows trimmed (lossy-on-overflow).
//
// v0.8.6: visible_at + published_by_user_id are honoured. Deferred
// publishes (VisibleAt > now) land in storage immediately but are
// hidden from reads until visible_at <= now; the tool layer schedules
// a Bus.Notify(channel) at visible_at so long-poll subscribers wake.
func (s *Store) ChannelPublish(ctx context.Context, msg store.ChannelMessage, maxMessages int) (string, int, error) {
	now := time.Now().UTC()
	msg.ID = store.MintChannelMessageID(now)
	msg.PublishedAt = now
	if msg.VisibleAt.IsZero() || msg.VisibleAt.Before(now) {
		msg.VisibleAt = now
	} else {
		msg.VisibleAt = msg.VisibleAt.UTC()
	}

	var expiresAt any
	if !msg.ExpiresAt.IsZero() {
		expiresAt = msg.ExpiresAt.UTC()
	}
	var publishedByUserID any
	if msg.PublishedByUserID != "" {
		publishedByUserID = msg.PublishedByUserID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("channel publish begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_messages (id, channel, scope, scope_id, payload, published_at, expires_at, visible_at, published_by_user_id)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)`,
		msg.ID, msg.Channel, string(msg.Scope), msg.ScopeID, string(msg.Payload),
		now, expiresAt, msg.VisibleAt, publishedByUserID,
	); err != nil {
		return "", 0, fmt.Errorf("channel publish insert: %w", err)
	}

	dropped := 0
	if maxMessages > 0 {
		// The `id != $5` clause is the race guard: under READ
		// COMMITTED, two concurrent publishers to the same
		// (channel, scope, scope_id) can each see the other's
		// committed row inside their own trim subquery. Without the
		// guard, A's INSERT X + concurrent B's commit of Y > X means
		// A's trim picks Y as top-N (excluding X by lex order) and
		// A's DELETE removes its own just-inserted X. A then
		// commits and reports success to its caller, but X is gone.
		// With the guard, the just-inserted row is never in the
		// DELETE candidate set under any race.
		//
		// Trade-off: under sustained concurrent overflow, the table
		// may briefly exceed maxMessages by k (one extra row per
		// concurrent publisher whose trim races). The next trim
		// converges. This is the right safety property: no message
		// that was reported as published is ever silently lost.
		// v0.8.6: ORDER BY (visible_at, id) DESC to match the read
		// path's delivery order — see the sqlite adapter for the
		// full rationale. With pure id DESC, a deferred message
		// published earlier but with a future visible_at would sort
		// BEFORE a later immediate publish; the trim would silently
		// drop the deferred row before it became deliverable.
		tag, err := tx.Exec(ctx,
			`DELETE FROM channel_messages
			 WHERE channel = $1 AND scope = $2 AND scope_id = $3
			   AND id != $5
			   AND id NOT IN (
			     SELECT id FROM channel_messages
			      WHERE channel = $1 AND scope = $2 AND scope_id = $3
			      ORDER BY visible_at DESC, id DESC
			      LIMIT $4
			   )`,
			msg.Channel, string(msg.Scope), msg.ScopeID, maxMessages, msg.ID,
		)
		if err != nil {
			return "", 0, fmt.Errorf("channel publish trim: %w", err)
		}
		dropped = int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("channel publish commit: %w", err)
	}
	return msg.ID, dropped, nil
}

// ChannelSubscribe reads up to `limit` messages newer than fromCursor.
// fromCursor == "" || "cur_0" → from the oldest non-expired row.
func (s *Store) ChannelSubscribe(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	return s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
}

// ChannelPeek is identical to Subscribe at the storage layer — the
// difference is purely semantic (whether the tool layer commits the
// returned cursor on the next call).
func (s *Store) ChannelPeek(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, error) {
	msgs, _, err := s.channelRead(ctx, channel, scope, scopeID, fromCursor, limit)
	return msgs, err
}

// channelRead is the shared read body. Filters expired + invisible
// rows at WHERE; orders by (visible_at, id) tuple so deferred
// messages don't get skipped by subscribers that already progressed
// past their publish-time id (v0.8.6).
func (s *Store) channelRead(ctx context.Context, channel string, scope store.MemoryScope, scopeID, fromCursor string, limit int) ([]store.ChannelMessage, string, error) {
	if limit <= 0 {
		limit = 10
	}
	cursorVisibleAt, cursorMsgID, fromOldest, err := store.DecodeChannelCursor(fromCursor)
	if err != nil {
		return nil, "", err
	}

	var rows pgx.Rows
	var qErr error
	if fromOldest {
		rows, qErr = s.pool.Query(ctx,
			`SELECT id, payload::text, published_at, expires_at, visible_at, published_by_user_id
			 FROM channel_messages
			 WHERE channel = $1 AND scope = $2 AND scope_id = $3
			   AND visible_at <= NOW()
			   AND (expires_at IS NULL OR expires_at > NOW())
			 ORDER BY visible_at ASC, id ASC
			 LIMIT $4`,
			channel, string(scope), scopeID, limit)
	} else {
		rows, qErr = s.pool.Query(ctx,
			`SELECT id, payload::text, published_at, expires_at, visible_at, published_by_user_id
			 FROM channel_messages
			 WHERE channel = $1 AND scope = $2 AND scope_id = $3
			   AND visible_at <= NOW()
			   AND (expires_at IS NULL OR expires_at > NOW())
			   AND (visible_at > $4 OR (visible_at = $4 AND id > $5))
			 ORDER BY visible_at ASC, id ASC
			 LIMIT $6`,
			channel, string(scope), scopeID,
			cursorVisibleAt.UTC(), cursorMsgID, limit)
	}
	if qErr != nil {
		return nil, "", fmt.Errorf("channel read: %w", qErr)
	}
	defer rows.Close()

	var msgs []store.ChannelMessage
	var lastVisibleAt time.Time
	var lastID string
	for rows.Next() {
		var (
			id                string
			payloadText       []byte
			publishedAt       time.Time
			expiresAt         *time.Time
			visibleAt         time.Time
			publishedByUserID *string
		)
		if err := rows.Scan(&id, &payloadText, &publishedAt, &expiresAt, &visibleAt, &publishedByUserID); err != nil {
			return nil, "", fmt.Errorf("channel read scan: %w", err)
		}
		msg := store.ChannelMessage{
			ID:          id,
			Channel:     channel,
			Scope:       scope,
			ScopeID:     scopeID,
			Payload:     json.RawMessage(payloadText),
			PublishedAt: publishedAt,
			VisibleAt:   visibleAt,
		}
		if expiresAt != nil {
			msg.ExpiresAt = *expiresAt
		}
		if publishedByUserID != nil {
			msg.PublishedByUserID = *publishedByUserID
		}
		msgs = append(msgs, msg)
		lastVisibleAt = visibleAt
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("channel read rows: %w", err)
	}
	var nextCursor string
	if lastID != "" {
		nextCursor = store.EncodeChannelCursor(lastVisibleAt, lastID)
	}
	return msgs, nextCursor, nil
}

// ChannelAck advances the per-subscriber committed cursor. Rejects
// cursor values older than the currently committed one (lexicographic
// order matches tuple order because the v0.8.6 cursor format encodes
// visible_at as a fixed-width hex prefix). Idempotent on re-ack.
func (s *Store) ChannelAck(ctx context.Context, channel string, scope store.MemoryScope, scopeID, cursor string) error {
	if cursor == "" || cursor == "cur_0" {
		return nil
	}
	// Validate format — reject legacy `msg_<hex>` cursors and garbage
	// rather than silently storing them.
	if _, _, _, err := store.DecodeChannelCursor(cursor); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("channel ack begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existing string
	err = tx.QueryRow(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = $1 AND scope = $2 AND scope_id = $3`,
		channel, string(scope), scopeID,
	).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("channel ack lookup: %w", err)
	}
	if existing != "" && cursor < existing {
		return store.ErrChannelCursorRegression
	}
	if existing == cursor {
		return tx.Commit(ctx) // idempotent
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_cursors (channel, scope, scope_id, cursor, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (channel, scope, scope_id) DO UPDATE SET
		    cursor = EXCLUDED.cursor,
		    updated_at = EXCLUDED.updated_at`,
		channel, string(scope), scopeID, cursor, now,
	); err != nil {
		return fmt.Errorf("channel ack upsert: %w", err)
	}
	return tx.Commit(ctx)
}

// ChannelCommittedCursor returns the most recent ack'd cursor, or "".
func (s *Store) ChannelCommittedCursor(ctx context.Context, channel string, scope store.MemoryScope, scopeID string) (string, error) {
	var cursor string
	err := s.pool.QueryRow(ctx,
		`SELECT cursor FROM channel_cursors WHERE channel = $1 AND scope = $2 AND scope_id = $3`,
		channel, string(scope), scopeID,
	).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("channel committed: %w", err)
	}
	return cursor, nil
}

// ChannelSweepExpired drops every expired channel_messages row.
func (s *Store) ChannelSweepExpired(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM channel_messages WHERE expires_at IS NOT NULL AND expires_at <= NOW()`,
	)
	if err != nil {
		return 0, fmt.Errorf("channel sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- v0.8.5 Self-Evolution Substrate ----
//
// Postgres mirror of the SQLite implementation. Version-allocation
// lock uses pg_advisory_xact_lock(hashtextextended('agent_def:' || name, 0))
// inside the tx so concurrent forks against the same name serialise
// without locking the whole table. The aggregation kernel
// (computeAggregate / statsOf) is shared with sqlite.

// AgentDefCreate allocates the next version for row.Name under a
// per-name advisory lock and inserts. Validates parent_def_id.
func (s *Store) AgentDefCreate(ctx context.Context, row store.AgentDefRow) (store.AgentDefRow, error) {
	if row.DefID == "" || row.Name == "" {
		return store.AgentDefRow{}, fmt.Errorf("agent_def: def_id + name required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def create begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Per-name advisory lock — serialises version allocation against
	// concurrent forks of the SAME name. Different names proceed in
	// parallel. The hash is deterministic so the same name always
	// hashes to the same lock key.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"agent_def:"+row.Name,
	); err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def create lock: %w", err)
	}

	if row.ParentDefID != "" {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_defs WHERE def_id = $1`, row.ParentDefID).Scan(&n); err != nil {
			return store.AgentDefRow{}, fmt.Errorf("agent_def create parent check: %w", err)
		}
		if n == 0 {
			return store.AgentDefRow{}, store.ErrAgentDefParentNotFound
		}
	}

	var maxVer sql.NullInt64
	if err := tx.QueryRow(ctx, `SELECT MAX(version) FROM agent_defs WHERE name = $1`, row.Name).Scan(&maxVer); err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def create max version: %w", err)
	}
	row.Version = 1
	if maxVer.Valid {
		row.Version = int(maxVer.Int64) + 1
	}
	row.CreatedAt = time.Now().UTC()

	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_defs (
			def_id, name, version, parent_def_id, definition, description,
			created_at, created_by_agent_id, created_by_run_id,
			retired, bootstrapped_from_static
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11)`,
		row.DefID, row.Name, row.Version, nullableString(row.ParentDefID),
		string(row.Definition), nullableString(row.Description),
		row.CreatedAt,
		nullableString(row.CreatedByAgentID), nullableString(row.CreatedByRunID),
		row.Retired, row.BootstrappedFromStatic,
	); err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def commit: %w", err)
	}
	return row, nil
}

// AgentDefGet returns one row by def_id.
func (s *Store) AgentDefGet(ctx context.Context, defID string) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.pool.QueryRow(ctx, agentDefSelect+` WHERE def_id = $1`, defID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return row, err
}

// AgentDefGetByNameVersion returns one row by (name, version).
func (s *Store) AgentDefGetByNameVersion(ctx context.Context, name string, version int) (store.AgentDefRow, error) {
	row, err := s.scanAgentDef(s.pool.QueryRow(ctx, agentDefSelect+` WHERE name = $1 AND version = $2`, name, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: fmt.Sprintf("%s@v%d", name, version)}
	}
	return row, err
}

// AgentDefListByName returns rows for one name, version DESC.
func (s *Store) AgentDefListByName(ctx context.Context, name string) ([]store.AgentDefRow, error) {
	rows, err := s.pool.Query(ctx, agentDefSelect+` WHERE name = $1 ORDER BY version DESC`, name)
	if err != nil {
		return nil, fmt.Errorf("agent_def list by name: %w", err)
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListChildren returns immediate children.
func (s *Store) AgentDefListChildren(ctx context.Context, parentDefID string) ([]store.AgentDefRow, error) {
	rows, err := s.pool.Query(ctx, agentDefSelect+` WHERE parent_def_id = $1 ORDER BY version DESC`, parentDefID)
	if err != nil {
		return nil, fmt.Errorf("agent_def list children: %w", err)
	}
	defer rows.Close()
	return s.scanAgentDefRows(rows)
}

// AgentDefListNames returns one summary per distinct name.
func (s *Store) AgentDefListNames(ctx context.Context) ([]store.AgentDefNameSummary, error) {
	rows, err := s.pool.Query(ctx, `
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
		return nil, fmt.Errorf("agent_def list names: %w", err)
	}
	defer rows.Close()

	var out []store.AgentDefNameSummary
	for rows.Next() {
		var s store.AgentDefNameSummary
		if err := rows.Scan(&s.Name, &s.VersionCount, &s.LatestVersion, &s.LastUpdated, &s.ActiveDefID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentDefSetActive UPSERTs the agent_def_active pointer for name.
func (s *Store) AgentDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error {
	var rowName string
	err := s.pool.QueryRow(ctx, `SELECT name FROM agent_defs WHERE def_id = $1`, defID).Scan(&rowName)
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if err != nil {
		return fmt.Errorf("agent_def_active check: %w", err)
	}
	if rowName != name {
		return fmt.Errorf("agent_def_active: def_id %q has name %q, refusing to promote under name %q", defID, rowName, name)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO agent_def_active (name, def_id, promoted_at, promoted_by_agent_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
		    def_id               = EXCLUDED.def_id,
		    promoted_at          = EXCLUDED.promoted_at,
		    promoted_by_agent_id = EXCLUDED.promoted_by_agent_id`,
		name, defID, time.Now().UTC(), nullableString(promotedByAgentID),
	)
	if err != nil {
		return fmt.Errorf("agent_def_active upsert: %w", err)
	}
	return nil
}

// AgentDefGetActive returns the active row for name. *ErrNotFound
// when no pointer exists.
func (s *Store) AgentDefGetActive(ctx context.Context, name string) (store.AgentDefRow, error) {
	var defID string
	err := s.pool.QueryRow(ctx, `SELECT def_id FROM agent_def_active WHERE name = $1`, name).Scan(&defID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def_active", ID: name}
	}
	if err != nil {
		return store.AgentDefRow{}, fmt.Errorf("agent_def_active lookup: %w", err)
	}
	return s.AgentDefGet(ctx, defID)
}

// AgentDefSetRetired flips the retired flag.
func (s *Store) AgentDefSetRetired(ctx context.Context, defID string, retired bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_defs SET retired = $1 WHERE def_id = $2`, retired, defID)
	if err != nil {
		return fmt.Errorf("agent_def set retired: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	return nil
}

const agentDefSelect = `SELECT
	def_id, name, version,
	COALESCE(parent_def_id, ''),
	definition::text,
	COALESCE(description, ''),
	created_at,
	COALESCE(created_by_agent_id, ''),
	COALESCE(created_by_run_id, ''),
	retired,
	bootstrapped_from_static
FROM agent_defs`

func (s *Store) scanAgentDef(row pgx.Row) (store.AgentDefRow, error) {
	var (
		out        store.AgentDefRow
		definition string
	)
	err := row.Scan(
		&out.DefID, &out.Name, &out.Version,
		&out.ParentDefID,
		&definition,
		&out.Description,
		&out.CreatedAt,
		&out.CreatedByAgentID, &out.CreatedByRunID,
		&out.Retired, &out.BootstrappedFromStatic,
	)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	out.Definition = json.RawMessage(definition)
	return out, nil
}

func (s *Store) scanAgentDefRows(rows pgx.Rows) ([]store.AgentDefRow, error) {
	var out []store.AgentDefRow
	for rows.Next() {
		var (
			r          store.AgentDefRow
			definition string
		)
		if err := rows.Scan(
			&r.DefID, &r.Name, &r.Version,
			&r.ParentDefID,
			&definition,
			&r.Description,
			&r.CreatedAt,
			&r.CreatedByAgentID, &r.CreatedByRunID,
			&r.Retired, &r.BootstrappedFromStatic,
		); err != nil {
			return nil, err
		}
		r.Definition = json.RawMessage(definition)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Evaluation ----

func (s *Store) EvaluationSubmit(ctx context.Context, row store.EvaluationRow) (store.EvaluationRow, error) {
	if row.EvalID == "" || row.RunID == "" || row.EmitterRole == "" {
		return store.EvaluationRow{}, fmt.Errorf("evaluation: eval_id, run_id, emitter_role required")
	}
	row.CreatedAt = time.Now().UTC()
	var dimsJSON, judgementJSON any
	if len(row.Dimensions) > 0 {
		b, err := json.Marshal(row.Dimensions)
		if err != nil {
			return store.EvaluationRow{}, fmt.Errorf("evaluation: marshal dimensions: %w", err)
		}
		dimsJSON = string(b)
	}
	if len(row.Judgement) > 0 {
		judgementJSON = string(row.Judgement)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO evaluations (
			eval_id, run_id, def_id, score, dimensions, judgement, rationale,
			emitter_role, emitter_agent_id, emitter_run_id, created_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9, $10, $11)`,
		row.EvalID, row.RunID, nullableString(row.DefID), row.Score,
		dimsJSON, judgementJSON, nullableString(row.Rationale),
		row.EmitterRole, nullableString(row.EmitterAgentID), nullableString(row.EmitterRunID),
		row.CreatedAt,
	); err != nil {
		return store.EvaluationRow{}, fmt.Errorf("evaluation submit: %w", err)
	}
	return row, nil
}

func (s *Store) EvaluationGet(ctx context.Context, evalID string) (store.EvaluationRow, error) {
	row, err := s.scanEvaluation(s.pool.QueryRow(ctx, evaluationSelect+` WHERE eval_id = $1`, evalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.EvaluationRow{}, &store.ErrNotFound{Kind: "evaluation", ID: evalID}
	}
	return row, err
}

func (s *Store) EvaluationListForRun(ctx context.Context, runID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, evaluationSelect+` WHERE run_id = $1 ORDER BY created_at DESC LIMIT $2`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("evaluation list for run: %w", err)
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

func (s *Store) EvaluationListForDef(ctx context.Context, defID string, limit int) ([]store.EvaluationRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, evaluationSelect+` WHERE def_id = $1 ORDER BY created_at DESC LIMIT $2`, defID, limit)
	if err != nil {
		return nil, fmt.Errorf("evaluation list for def: %w", err)
	}
	defer rows.Close()
	return s.scanEvaluationRows(rows)
}

func (s *Store) EvaluationAggregate(ctx context.Context, defID string, opts store.AggregateOpts) (store.AggregateResult, error) {
	defIDs := []string{defID}
	if opts.IncludeLineage {
		ancestors, err := s.walkAncestors(ctx, defID)
		if err != nil {
			return store.AggregateResult{}, err
		}
		defIDs = append(defIDs, ancestors...)
	}
	if len(defIDs) > 1000 {
		defIDs = defIDs[:1000]
	}
	rows, err := s.pool.Query(ctx, evaluationSelect+` WHERE def_id = ANY($1) ORDER BY created_at ASC`, defIDs)
	if err != nil {
		return store.AggregateResult{}, fmt.Errorf("evaluation aggregate: %w", err)
	}
	defer rows.Close()
	evals, err := s.scanEvaluationRows(rows)
	if err != nil {
		return store.AggregateResult{}, err
	}
	return computeAggregate(defID, evals, opts.IncludeLineage), nil
}

func (s *Store) walkAncestors(ctx context.Context, defID string) ([]string, error) {
	var ancestors []string
	seen := map[string]bool{defID: true}
	cur := defID
	for i := 0; i < 100; i++ {
		var parent sql.NullString
		err := s.pool.QueryRow(ctx, `SELECT parent_def_id FROM agent_defs WHERE def_id = $1`, cur).Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) || !parent.Valid || parent.String == "" {
			return ancestors, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[parent.String] {
			return ancestors, nil
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
	COALESCE(dimensions::text, ''),
	COALESCE(judgement::text, ''),
	COALESCE(rationale, ''),
	emitter_role,
	COALESCE(emitter_agent_id, ''),
	COALESCE(emitter_run_id, ''),
	created_at
FROM evaluations`

func (s *Store) scanEvaluation(row pgx.Row) (store.EvaluationRow, error) {
	var (
		out                   store.EvaluationRow
		dimensions, judgement string
	)
	if err := row.Scan(
		&out.EvalID, &out.RunID, &out.DefID, &out.Score,
		&dimensions, &judgement, &out.Rationale,
		&out.EmitterRole, &out.EmitterAgentID, &out.EmitterRunID,
		&out.CreatedAt,
	); err != nil {
		return store.EvaluationRow{}, err
	}
	if dimensions != "" {
		_ = json.Unmarshal([]byte(dimensions), &out.Dimensions)
	}
	if judgement != "" {
		out.Judgement = json.RawMessage(judgement)
	}
	return out, nil
}

func (s *Store) scanEvaluationRows(rows pgx.Rows) ([]store.EvaluationRow, error) {
	var out []store.EvaluationRow
	for rows.Next() {
		var (
			r                     store.EvaluationRow
			dimensions, judgement string
		)
		if err := rows.Scan(
			&r.EvalID, &r.RunID, &r.DefID, &r.Score,
			&dimensions, &judgement, &r.Rationale,
			&r.EmitterRole, &r.EmitterAgentID, &r.EmitterRunID,
			&r.CreatedAt,
		); err != nil {
			return nil, err
		}
		if dimensions != "" {
			_ = json.Unmarshal([]byte(dimensions), &r.Dimensions)
		}
		if judgement != "" {
			r.Judgement = json.RawMessage(judgement)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullableString returns sql.NullString-equivalent for pgx Exec args.
// pgx accepts `nil` for NULL; empty strings stay as empty NOT NULL —
// but our columns are TEXT NULL so we pass nil to mean NULL.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// computeAggregate + statsOf are defined in the sqlite adapter and
// live in package store_sqlite. To avoid duplicating the aggregation
// kernel, we redeclare it here as well — the two packages don't
// share a parent except `store`, and putting the kernel in `store`
// would force `store` to know about ScoreStats math. Leaving these
// here for now; if a third backend lands we'll extract a helper
// package.
//
// (Below copies are intentionally identical to sqlite's bodies.)

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
	out.Score.Latest = evals[len(evals)-1].Score
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
// Mirrors the SQLite implementation in sqlite.go — duplicated
// intentionally; extract to a shared package if a third backend lands.
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
	out.Latest = vals[len(vals)-1]
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

// escapeLikePrefix neutralises the LIKE wildcards in `prefix` so an
// agent searching for "events_2026" doesn't get treated as
// "events" + any-char + "2026". Backslash is the ESCAPE clause.
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

// ---- helpers ----

// rowScanner is the subset of pgx.Row + pgx.Rows we need for the shared
// scanRun() — both single-row QueryRow and multi-row Rows.Scan return a
// type that satisfies this.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRun reads one run row from pgx into a store.Run, converting
// nullable columns through pointer-string scratch variables.
//
// Trailing column is `s.agent` from the LEFT JOIN onto sessions —
// surfaces the human-readable agent name (yaml-declared, e.g.
// "qa-agent") on every Run without a separate session lookup.
func scanRun(r rowScanner) (store.Run, error) {
	var (
		out store.Run

		started    time.Time
		completed  *time.Time
		stopReason *string
		model      *string
		errMsg     *string

		agentID, parentAgentID, parentRunID, userID, userTier *string
		agentDefID                                            *string
		pauseState                                            *string
		lastHeartbeatAt                                       *time.Time
		sessAgent                                             *string

		statusStr string
	)
	if err := r.Scan(
		&out.ID, &out.SessionID, &statusStr, &started, &completed, &stopReason,
		&out.InputTokens, &out.OutputTokens, &out.CacheCreationTokens, &out.CacheReadTokens,
		&model, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHeartbeatAt,
		&userTier,
		&agentDefID, &pauseState,
		&sessAgent,
	); err != nil {
		return store.Run{}, err
	}
	out.Status = store.RunStatus(statusStr)
	out.StartedAt = started
	if completed != nil {
		out.CompletedAt = *completed
	}
	if stopReason != nil {
		out.StopReason = *stopReason
	}
	if model != nil {
		out.Model = *model
	}
	if errMsg != nil {
		out.ErrorMsg = *errMsg
	}
	if agentID != nil {
		out.AgentID = *agentID
	}
	if parentAgentID != nil {
		out.ParentAgentID = *parentAgentID
	}
	if parentRunID != nil {
		out.ParentRunID = *parentRunID
	}
	if userID != nil {
		out.UserID = *userID
	}
	if lastHeartbeatAt != nil {
		out.LastHeartbeatAt = *lastHeartbeatAt
	}
	if userTier != nil {
		out.UserTier = *userTier
	}
	if agentDefID != nil {
		out.AgentDefID = *agentDefID
	}
	if pauseState != nil {
		out.PauseState = *pauseState
	}
	if sessAgent != nil {
		out.Agent = *sessAgent
	}
	return out, nil
}

func scanRunRows(rows pgx.Rows) ([]store.Run, error) {
	out := []store.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter runs: %w", err)
	}
	return out, nil
}

// nullableText returns nil for an empty string so the column writes NULL
// instead of "". Matters for the partial indexes on `... IS NOT NULL` —
// empty-string rows would otherwise fall into the index and bloat it.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// newID generates a 16-byte hex ID with the given prefix. Same shape as
// the SQLite adapter's IDs so adapter-agnostic test fixtures look the
// same regardless of backend.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable — panic so tests
		// surface the issue instead of inserting predictable IDs.
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}
