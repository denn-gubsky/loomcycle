// Package store defines the persistence interface for sessions, runs, and
// the event transcript. v0.3 ships a SQLite implementation as the default.
//
// Three concepts:
//
//   - Session: a logical conversation thread the consumer addresses by ID.
//     Persists across HTTP calls so a chat-style consumer can continue
//     where it left off.
//   - Run:     one POST /v1/runs invocation. May iterate through multiple
//     model→tool→model cycles inside the same run.
//   - Event:   one streamed datum from the loop (text, tool_call,
//     tool_result, usage, ...). Append-only.
//
// Sessions have many runs. Runs have many events. The full transcript of
// a session is its events in seq order across all its runs.
package store

import (
	"context"
	"time"
)

// Session is a logical conversation thread.
type Session struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
}

// RunStatus is the terminal state of a run, or "running" while it's still in
// flight. Transitions: running → (completed | failed | cancelled).
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Run is one execution within a session.
type Run struct {
	ID                  string    `json:"id"`
	SessionID           string    `json:"session_id"`
	Status              RunStatus `json:"status"`
	StartedAt           time.Time `json:"started_at"`
	CompletedAt         time.Time `json:"completed_at,omitempty"`
	StopReason          string    `json:"stop_reason,omitempty"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheCreationTokens int       `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int       `json:"cache_read_input_tokens,omitempty"`
	Model               string    `json:"model,omitempty"`
	ErrorMsg            string    `json:"error,omitempty"`
}

// Event is one streamed datum, persisted append-only. Payload is the JSON
// representation of the loop's providers.Event so we never lose typed
// fields when reading back; the API package re-decodes it on replay.
type Event struct {
	Seq       int64     `json:"seq"`
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Payload   []byte    `json:"-"` // raw JSON; emit via custom marshalling at the API edge
}

// Usage is one run's aggregated token accounting, computed by the loop and
// passed to FinishRun.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	Model               string
}

// Store is the persistence backend. SQLite is the default; Postgres / Redis
// adapters slot in behind this interface in v0.4.
//
// All methods take ctx. Implementations must honour ctx cancellation for
// long-running queries (transcript replay especially).
type Store interface {
	// CreateSession creates a new session with a generated ID.
	CreateSession(ctx context.Context, tenantID, agent string) (Session, error)

	// GetSession returns the session metadata. Returns ErrNotFound if
	// the ID doesn't exist.
	GetSession(ctx context.Context, sessionID string) (Session, error)

	// CreateRun starts a new run within an existing session, in
	// status "running". Returns ErrNotFound if sessionID doesn't exist.
	CreateRun(ctx context.Context, sessionID string) (Run, error)

	// AppendEvent persists one event for a run. Implementations should be
	// safe to call from the loop's goroutine on the hot path; bulk-insert
	// is not required (a run typically emits 10–100 events).
	AppendEvent(ctx context.Context, runID string, eventType string, payload []byte) error

	// FinishRun marks the run terminal and stores the aggregated usage and
	// stop reason (or error message, when status is "failed"). Idempotent:
	// calling on an already-finished run is a no-op.
	FinishRun(ctx context.Context, runID string, status RunStatus, stopReason string, usage Usage, errMsg string) error

	// GetTranscript returns all events for a session, ordered by Seq.
	// Returns an empty slice (not error) for a session with no runs yet.
	GetTranscript(ctx context.Context, sessionID string) ([]Event, error)

	// Close releases backend resources. Idempotent.
	Close() error
}

// ErrNotFound is returned when a session or run ID isn't in the store.
type ErrNotFound struct {
	Kind string // "session" | "run"
	ID   string
}

func (e *ErrNotFound) Error() string { return e.Kind + " not found: " + e.ID }
