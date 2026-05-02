// Package sqlite implements store.Store backed by SQLite via modernc.org/sqlite
// (pure Go, no cgo). Single-file database; WAL journal mode for concurrent
// readers during a write.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// CreateSession inserts a new session with a generated ID and returns it.
func (s *Store) CreateSession(ctx context.Context, tenantID, agent string) (store.Session, error) {
	id := newID("s_")
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, tenant_id, agent, created_at) VALUES (?, ?, ?, ?)`,
		id, tenantID, agent, now.UnixNano(),
	)
	if err != nil {
		return store.Session{}, err
	}
	return store.Session{ID: id, TenantID: tenantID, Agent: agent, CreatedAt: now}, nil
}

// GetSession returns session metadata or *store.ErrNotFound.
func (s *Store) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	var sess store.Session
	var createdNs int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, agent, created_at FROM sessions WHERE id = ?`, sessionID,
	).Scan(&sess.ID, &sess.TenantID, &sess.Agent, &createdNs)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	if err != nil {
		return store.Session{}, err
	}
	sess.CreatedAt = time.Unix(0, createdNs)
	return sess, nil
}

// CreateRun starts a new run inside an existing session.
func (s *Store) CreateRun(ctx context.Context, sessionID string) (store.Run, error) {
	// Verify the session exists so a missing ID surfaces as ErrNotFound,
	// not a foreign-key error.
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return store.Run{}, err
	}
	id := newID("r_")
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, session_id, status, started_at) VALUES (?, ?, ?, ?)`,
		id, sessionID, store.RunRunning, now.UnixNano(),
	)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{
		ID:        id,
		SessionID: sessionID,
		Status:    store.RunRunning,
		StartedAt: now,
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
