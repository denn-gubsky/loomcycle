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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
			agent_id, parent_agent_id, parent_run_id, user_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, sessionID, string(store.RunRunning), now,
		nullableText(identity.AgentID),
		nullableText(identity.ParentAgentID),
		nullableText(identity.ParentRunID),
		nullableText(identity.UserID),
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
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
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
			        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
			        s.agent
			 FROM runs r LEFT JOIN sessions s ON r.session_id = s.id
			 WHERE r.user_id = $1
			 ORDER BY r.started_at DESC LIMIT $2`, userID, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT r.id, r.session_id, r.status, r.started_at, r.completed_at, r.stop_reason,
			        r.input_tokens, r.output_tokens, r.cache_creation_tokens, r.cache_read_tokens,
			        r.model, r.error,
			        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
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
		        r.agent_id, r.parent_agent_id, r.parent_run_id, r.user_id, r.last_heartbeat_at,
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
// add in a single transaction with `FOR UPDATE` row-locking; the
// existing-value parse is JSON-side because JSONB stores the value as
// the parsed shape, not as text.
func (s *Store) MemoryIncrement(ctx context.Context, scope store.MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("memory incr begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		valueText []byte
		expiresAt *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT value::text, expires_at FROM memory
		 WHERE scope = $1 AND scope_id = $2 AND key = $3
		 FOR UPDATE`,
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

		agentID, parentAgentID, parentRunID, userID *string
		lastHeartbeatAt                             *time.Time
		sessAgent                                   *string

		statusStr string
	)
	if err := r.Scan(
		&out.ID, &out.SessionID, &statusStr, &started, &completed, &stopReason,
		&out.InputTokens, &out.OutputTokens, &out.CacheCreationTokens, &out.CacheReadTokens,
		&model, &errMsg,
		&agentID, &parentAgentID, &parentRunID, &userID, &lastHeartbeatAt,
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
