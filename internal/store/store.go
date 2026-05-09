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
	"encoding/json"
	"time"
)

// Session is a logical conversation thread.
type Session struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
	// UserID binds the session to a user (v0.4+). Empty for legacy
	// rows that pre-date the column. Caller-supplied at session
	// creation; sub-agent sessions inherit the parent's value.
	UserID string `json:"user_id,omitempty"`
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
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	// Agent is the YAML-declared agent name (e.g. "qa-agent",
	// "company-researcher"). Read from the parent session via SQL JOIN
	// at read time — NOT a column on the runs table. Empty when the
	// JOIN can't resolve (e.g. a session row was manually pruned).
	Agent               string    `json:"agent,omitempty"`
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

	// v0.4 tracking + cancel fields. All optional/nullable for
	// back-compat with rows created before the columns landed.

	// AgentID is the caller-supplied (or loomcycle-generated)
	// tracking handle. Distinct from SessionID — agent_id is
	// per-run, session_id is per-conversation-thread. Used as the
	// addressable identifier for the cancel/get/list endpoints.
	AgentID string `json:"agent_id,omitempty"`
	// ParentAgentID is set on sub-agent runs to the spawning
	// agent's AgentID. Drives cascade-cancel.
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	// ParentRunID is the direct parent run for sub-agent runs.
	// Useful for transcript stitching.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// UserID is denormalised from the session for fast cancel/list
	// lookups without a session join. Set at run creation; never
	// mutated.
	UserID string `json:"user_id,omitempty"`
	// LastHeartbeatAt is updated by the loop at each iteration so
	// a future sweeper can detect crashed runs (no heartbeat for
	// > N minutes → presumed dead). Zero-time means no heartbeat
	// yet (run never reached its first iteration).
	LastHeartbeatAt time.Time `json:"last_heartbeat_at,omitempty"`
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

// RunIdentity carries the v0.4 tracking fields a CreateRun caller can
// supply. Zero-value fields mean "no value" — implementations must
// store them as NULL (or empty string for TEXT columns) so historical
// rows remain queryable.
type RunIdentity struct {
	// AgentID is the caller-supplied tracking handle, or
	// loomcycle-generated for top-level runs without a caller value
	// and for sub-agent runs (which always get a fresh ID).
	AgentID string
	// ParentAgentID is set only on sub-agent runs (the spawning
	// agent's AgentID).
	ParentAgentID string
	// ParentRunID is the direct parent run row's ID. Set with
	// ParentAgentID on sub-agent runs.
	ParentRunID string
	// UserID is denormalised from the session at run creation for
	// fast lookups without a session join. Callers SHOULD pass the
	// session's user_id here; the implementation does not enforce
	// consistency (cheaper to trust the caller than to JOIN on
	// every CreateRun).
	UserID string
}

// UserSummary is one row of ListUsers' output: distinct user_id with
// summary stats. Drives the Web UI's user picker so operators can see
// who has active runs and pick from a list rather than typing a UUID.
//
// `LastStartedAt` is the most recent run start (any status) — useful
// for sorting by activity. `RunningCount` is the in-flight count
// (status=="running"); `TotalCount` includes everything ever.
type UserSummary struct {
	UserID        string    `json:"user_id"`
	RunningCount  int       `json:"running_count"`
	TotalCount    int       `json:"total_count"`
	LastStartedAt time.Time `json:"last_started_at"`
}

// Store is the persistence backend. SQLite is the default; Postgres / Redis
// adapters slot in behind this interface in v0.4.
//
// All methods take ctx. Implementations must honour ctx cancellation for
// long-running queries (transcript replay especially).
type Store interface {
	// CreateSession creates a new session with a generated ID. userID
	// may be empty for v0.3 back-compat callers.
	CreateSession(ctx context.Context, tenantID, agent, userID string) (Session, error)

	// GetSession returns the session metadata. Returns ErrNotFound if
	// the ID doesn't exist.
	GetSession(ctx context.Context, sessionID string) (Session, error)

	// CreateRun starts a new run within an existing session, in
	// status "running". Returns ErrNotFound if sessionID doesn't exist.
	// identity carries the v0.4 tracking fields; pass a zero value to
	// behave as v0.3.
	CreateRun(ctx context.Context, sessionID string, identity RunIdentity) (Run, error)

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

	// GetRunByAgentID returns the most recently started run carrying
	// the given agent_id. Returns *ErrNotFound when no such row.
	// Used by the GET /v1/agents/{agent_id} and cancel endpoints to
	// resolve the API-facing handle to a Run.
	GetRunByAgentID(ctx context.Context, agentID string) (Run, error)

	// ListActiveRunsByUser returns runs for userID whose status matches
	// the supplied filter. An empty status returns ALL statuses
	// (caller can filter further). Results are bounded — 100 rows max,
	// ordered by started_at DESC.
	ListActiveRunsByUser(ctx context.Context, userID string, status RunStatus) ([]Run, error)

	// ListRunsByParentAgentID returns the runs whose parent_agent_id
	// matches the given value. Drives cascade-cancel discovery.
	ListRunsByParentAgentID(ctx context.Context, parentAgentID string) ([]Run, error)

	// ListUsers returns the distinct user_ids that have runs in the
	// store, with summary stats per user (run counts by status, last
	// activity). Drives the v0.7.3 Web UI's user picker so operators
	// don't have to type a UUID. Excludes runs with empty user_id
	// (the default for callers that don't supply identity).
	//
	// Capped at 200 users ordered by last_started_at DESC. A bigger
	// list would be a UX problem anyway — the UI then needs filtering
	// rather than dropdown.
	ListUsers(ctx context.Context) ([]UserSummary, error)

	// UpdateHeartbeat sets last_heartbeat_at on a run to the current
	// time. Called by the loop at each iteration. No-op if the run
	// is not running (terminal runs don't accept heartbeats).
	UpdateHeartbeat(ctx context.Context, runID string) error

	// SweepStaleRuns marks every running row that hasn't heartbeated
	// since `cutoff` as failed with error="heartbeat timeout". Returns
	// the number of rows updated.
	//
	// "Hasn't heartbeated" includes runs that never set
	// last_heartbeat_at at all (crashed before the first iteration);
	// for those, started_at is the cutoff comparison. Implementations
	// MUST treat both cases consistently — without this, a process that
	// crashes between CreateRun and the loop's first heartbeat tick
	// would never get cleaned up.
	//
	// The query is a single atomic UPDATE so concurrent sweepers race
	// correctly: whichever sweeper commits first wins; later sweepers
	// see WHERE status='running' fail-match and update zero rows.
	// Used by internal/heartbeat for the periodic sweep goroutine.
	SweepStaleRuns(ctx context.Context, cutoff time.Time) (int, error)

	// MemorySet writes a Memory entry. ttl > 0 sets an expiry; ttl <= 0
	// stores with no expiry (the row's expires_at column is NULL). The
	// row is upserted on the (scope, scopeID, key) primary key —
	// re-writes overwrite the value and bump updated_at. Implementations
	// are responsible for surfacing wire-level constraints (max value
	// bytes, scope quota) as ErrMemoryQuotaExceeded — the tool layer
	// trusts the store's verdict.
	MemorySet(ctx context.Context, scope MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error

	// MemoryGet reads one entry. Returns *ErrNotFound for both "key
	// missing" and "key expired" — callers don't need to distinguish.
	// Implementations MUST treat an entry whose expires_at is in the
	// past as missing (returns ErrNotFound) regardless of whether the
	// sweeper has reaped it yet — the sweeper is best-effort.
	MemoryGet(ctx context.Context, scope MemoryScope, scopeID, key string) (MemoryEntry, error)

	// MemoryDelete removes an entry. Returns true when a row was
	// actually deleted, false when the key didn't exist (or had
	// already expired). Both are non-error paths.
	MemoryDelete(ctx context.Context, scope MemoryScope, scopeID, key string) (bool, error)

	// MemoryList returns entries for the (scope, scopeID) tuple whose
	// key starts with prefix. An empty prefix returns every key in the
	// scope. Capped at limit rows; if more rows would match, callers
	// see truncated == true. Expired rows are filtered out — callers
	// never see them.
	MemoryList(ctx context.Context, scope MemoryScope, scopeID, prefix string, limit int) (entries []MemoryEntry, truncated bool, err error)

	// MemoryIncrement is an atomic add over the JSON-number value at
	// (scope, scopeID, key). If the key doesn't exist, it's created
	// with the delta as the value. If the existing value isn't a
	// JSON number, returns ErrMemoryWrongType. Optional ttl sets (or
	// resets, on a re-incr) the expiry; ttl <= 0 keeps the existing
	// expiry untouched (or no expiry on a fresh row).
	MemoryIncrement(ctx context.Context, scope MemoryScope, scopeID, key string, delta int64, ttl time.Duration) (int64, error)

	// MemorySweep deletes every Memory row whose expires_at has passed.
	// Returns the row count deleted. Safe to run from a periodic
	// goroutine; idempotent under concurrent sweepers (single atomic
	// DELETE).
	MemorySweep(ctx context.Context) (int, error)

	// MemoryListScopeIDs returns one row per distinct scope_id under
	// the given scope, with summary stats (key count, total bytes,
	// most recent updated_at). Drives the v0.8.0 Web UI's Memory
	// page picker. Expired rows are excluded — operators see live
	// state only. Capped at 200 rows ordered by updated_at DESC.
	MemoryListScopeIDs(ctx context.Context, scope MemoryScope) ([]MemoryScopeIDSummary, error)

	// Close releases backend resources. Idempotent.
	Close() error
}

// MemoryScope is the addressing axis for a Memory row. v0.8.0 ships
// `agent` and `user`; the type is forward-compatible for adding
// `session` / `tenant` later — a new scope value is a yaml + adapter
// allowlist update, not a wire-protocol change.
type MemoryScope string

const (
	// MemoryScopeAgent — keyed by yaml agent name. Cross-run state for
	// one agent type (counters, summaries, learned facts).
	MemoryScopeAgent MemoryScope = "agent"
	// MemoryScopeUser — keyed by user_id. Per-end-user state shared
	// across every agent that's allowed to read the `user` scope.
	MemoryScopeUser MemoryScope = "user"
)

// MemoryEntry is one row in the memory table. ExpiresAt is zero when
// the row has no expiry. CreatedAt and UpdatedAt are the row's
// lifecycle timestamps; UpdatedAt advances on overwrite or
// MemoryIncrement.
type MemoryEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	ExpiresAt time.Time       `json:"expires_at,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MemoryScopeIDSummary is one row of MemoryListScopeIDs' output.
// KeyCount is the live key count (expired rows excluded). Bytes is
// the sum of key+value bytes — gives operators a quick "how full is
// this scope" view in the UI.
type MemoryScopeIDSummary struct {
	ScopeID   string    `json:"scope_id"`
	KeyCount  int       `json:"key_count"`
	Bytes     int       `json:"bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrMemoryWrongType is returned by MemoryIncrement when the existing
// value at the target key is not a JSON number. Callers (the Memory
// tool) surface this as a typed tool-result error.
var ErrMemoryWrongType = &MemoryError{Code: "wrong_type", Msg: "memory: existing value is not a JSON number"}

// ErrMemoryQuotaExceeded is returned by MemorySet / MemoryIncrement
// when the write would push the (scope, scopeID) tuple past its
// configured byte cap. The caller should drop or replace existing
// keys; loomcycle does not auto-evict.
var ErrMemoryQuotaExceeded = &MemoryError{Code: "quota_exceeded", Msg: "memory: scope quota exceeded"}

// ErrMemoryValueTooLarge is returned when a single value exceeds the
// per-write byte cap (LOOMCYCLE_MEMORY_MAX_VALUE_BYTES).
var ErrMemoryValueTooLarge = &MemoryError{Code: "value_too_large", Msg: "memory: value exceeds max bytes"}

// MemoryError is a typed error so the Memory tool can surface a
// stable error code to the agent. The Code is wire-stable; the Msg
// is human-readable and may evolve.
type MemoryError struct {
	Code string
	Msg  string
}

func (e *MemoryError) Error() string { return e.Msg }

// ErrNotFound is returned when a session or run ID isn't in the store.
type ErrNotFound struct {
	Kind string // "session" | "run"
	ID   string
}

func (e *ErrNotFound) Error() string { return e.Kind + " not found: " + e.ID }
