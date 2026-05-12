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
			agent_id, parent_agent_id, parent_run_id, user_id, user_tier, agent_def_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, sessionID, string(store.RunRunning), now,
		nullableText(identity.AgentID),
		nullableText(identity.ParentAgentID),
		nullableText(identity.ParentRunID),
		nullableText(identity.UserID),
		nullableText(identity.UserTier),
		nullableText(identity.AgentDefID),
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
		        r.agent_def_id,
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
		        r.agent_def_id,
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
			        r.agent_def_id,
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
			        r.agent_def_id,
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
		        r.agent_def_id,
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
		tag, err := tx.Exec(ctx,
			`DELETE FROM channel_messages
			 WHERE channel = $1 AND scope = $2 AND scope_id = $3
			   AND id != $5
			   AND id NOT IN (
			     SELECT id FROM channel_messages
			      WHERE channel = $1 AND scope = $2 AND scope_id = $3
			      ORDER BY id DESC
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
		&agentDefID,
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
