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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MintChannelMessageID returns a fresh channel-message id that's
// monotonic-by-publish-time AND globally unique. Format:
// "msg_<16-hex unixNanos><8-hex rand>" — 24 hex chars after the
// prefix. Sortable lexicographically by publish time within the
// resolution of a single nanosecond; the 4-byte random suffix
// collision-protects same-nanosecond publishes.
//
// The lex-order-matches-publish-time invariant holds while
// uint64(UnixNano) fits in 16 hex digits — true through year 2262
// (then the value overflows 17 hex digits and the %016x padding
// breaks the lex ordering). The cursor regression check in
// ChannelAck relies on this; any future format change must preserve
// the property or update the comparison.
//
// Why not ULID: adding an external dep for one purpose is bigger
// than the ~10 lines we save. The format is intentionally
// inspect-friendly — operators can eyeball "this message was
// published before that one" from the hex prefix.
func MintChannelMessageID(t time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("msg_%016x%s", uint64(t.UnixNano()), hex.EncodeToString(buf[:]))
}

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

	// UserTier is the v0.8.2 user-facing-tier marker — the name of
	// the user_tier policy applied to this run for resolver overlay
	// + (PR 2) runtime fallback. Empty when the run was created
	// without a user_tier field on the request body (back-compat
	// with v0.7.x clients) OR when the operator's yaml doesn't
	// define a user_tiers block at all. Lets compliance / cost
	// retrospective queries facet by tier without grepping logs.
	UserTier string `json:"user_tier,omitempty"`
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
	// UserTier is the v0.8.2 user-tier marker captured at run
	// creation. Empty when the request didn't carry user_tier (back-
	// compat) or the operator's yaml has no user_tiers block.
	UserTier string
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

	// ChannelPublish appends one message to a channel. The message's
	// ID is assigned by the store (ULID — sortable by publish time);
	// the returned id is the cursor agents pass back on subsequent
	// reads. msg.PublishedAt + msg.ExpiresAt are server-assigned and
	// may overwrite caller-supplied values for correctness; msg.ID
	// is ignored on input.
	//
	// Enforces the per-(channel, scope, scope_id) max_messages cap by
	// trimming the oldest entries inside the same txn — lossy-on-
	// overflow, per the v0.8.4 design (publisher never blocks).
	// Returns the count of messages trimmed (zero in steady state)
	// so the tool layer can emit EventChannelOverflow.
	ChannelPublish(ctx context.Context, msg ChannelMessage, maxMessages int) (id string, dropped int, err error)

	// ChannelSubscribe reads up to `limit` messages newer than
	// `fromCursor` (the empty string and the sentinel "cur_0" both
	// mean "from oldest non-expired"). Returns the batch + the
	// `nextCursor` ready for the next call. Expired rows are filtered
	// at read time so callers never see stale messages even if the
	// sweeper has lagged.
	//
	// nextCursor is the id of the LAST message in the returned batch
	// (empty when batch is empty); committing it via ChannelAck
	// advances the per-subscriber position.
	ChannelSubscribe(ctx context.Context, channel string, scope MemoryScope, scopeID, fromCursor string, limit int) (msgs []ChannelMessage, nextCursor string, err error)

	// ChannelAck advances the committed cursor for one subscriber to
	// the supplied cursor value. Idempotent — re-acking the same
	// cursor is a no-op. Acking a cursor older than the current
	// committed value is rejected with ErrChannelCursorRegression so
	// out-of-order acks from buggy agents can't rewind delivery.
	ChannelAck(ctx context.Context, channel string, scope MemoryScope, scopeID, cursor string) error

	// ChannelCommittedCursor returns the most recent cursor a
	// subscriber acked, or empty string when no ack has happened
	// yet (= read from oldest non-expired). Used by ChannelSubscribe
	// when callers omit fromCursor — "pick up where I left off".
	ChannelCommittedCursor(ctx context.Context, channel string, scope MemoryScope, scopeID string) (string, error)

	// ChannelSweepExpired deletes every channel_messages row whose
	// expires_at has passed. Returns the deleted row count for the
	// sweeper's log line. Safe under concurrent sweepers; mirrors
	// MemorySweep's shape.
	ChannelSweepExpired(ctx context.Context) (int, error)

	// ChannelPeek is the non-consuming read. Same args as Subscribe
	// but never updates a cursor and never auto-advances. Powers
	// the tool's "peek" op for debugging — operators can replay
	// from cur_0 without disturbing the consumer's position.
	ChannelPeek(ctx context.Context, channel string, scope MemoryScope, scopeID, fromCursor string, limit int) ([]ChannelMessage, error)

	// ---- v0.8.5 Self-Evolution Substrate ----
	//
	// `AgentDef` is the agent-authored agent-definition versioning
	// layer. Static `<name>.md` files remain the operator-blessed
	// root; the database holds the derived layer of agent-created
	// versions. Append-only. version is server-allocated under a
	// per-name lock so concurrent forks against the same parent each
	// get a distinct, monotonic version with no gaps.

	// AgentDefCreate inserts a fresh row. The caller passes the row
	// shape; the store allocates Version under the per-name lock,
	// sets CreatedAt server-side, validates the parent (if any), and
	// returns the persisted row. The DefID is caller-generated to
	// support deterministic-ID workflows (test fixtures, externally-
	// authored bootstrap rows).
	//
	// Errors:
	//   - parent_def_id supplied but not found → ErrAgentDefParentNotFound
	//   - name + version already exists (deterministic ID collision) → wraps the underlying constraint error
	AgentDefCreate(ctx context.Context, row AgentDefRow) (AgentDefRow, error)

	// AgentDefGet returns a single row by def_id. Returns *ErrNotFound
	// when the row doesn't exist.
	AgentDefGet(ctx context.Context, defID string) (AgentDefRow, error)

	// AgentDefGetByNameVersion returns one row by (name, version).
	// Useful for friendly lookups in the admin API. Returns
	// *ErrNotFound on miss.
	AgentDefGetByNameVersion(ctx context.Context, name string, version int) (AgentDefRow, error)

	// AgentDefListByName returns every row for one name, ordered by
	// version DESC. Empty slice (not nil) when the name has no rows.
	// Retired rows are included; the caller filters as needed.
	AgentDefListByName(ctx context.Context, name string) ([]AgentDefRow, error)

	// AgentDefListChildren returns the immediate-children rows
	// (parent_def_id == argument). One hop only — callers that need
	// the full descendant tree walk iteratively.
	AgentDefListChildren(ctx context.Context, parentDefID string) ([]AgentDefRow, error)

	// AgentDefListNames returns one summary row per distinct name.
	// Drives the admin API's name-list endpoint. count is the per-
	// name version count; active_def_id is the agent_def_active
	// pointer (empty when no row is promoted).
	AgentDefListNames(ctx context.Context) ([]AgentDefNameSummary, error)

	// AgentDefSetActive UPSERTs the agent_def_active pointer for
	// `name` to `defID`. promotedByAgentID is the agent_id that
	// performed the promotion (may be empty for admin API calls).
	// Idempotent: promote A → promote B → promote A leaves the
	// pointer at A with the latest promoted_at.
	AgentDefSetActive(ctx context.Context, name, defID, promotedByAgentID string) error

	// AgentDefGetActive returns the currently-active row for `name`
	// — the (name, version) pointed at by agent_def_active. Returns
	// *ErrNotFound when no active pointer exists (the caller falls
	// through to cfg.Agents — the static fallback path).
	AgentDefGetActive(ctx context.Context, name string) (AgentDefRow, error)

	// AgentDefSetRetired flips the `retired` flag on one row. The
	// row stays visible in lineage queries with the flag exposed;
	// the resolver skips retired rows when picking the next default
	// for runs that don't pin def_id.
	AgentDefSetRetired(ctx context.Context, defID string, retired bool) error

	// ---- Evaluation ----
	//
	// `Evaluation` is the score-attached-to-(run, def) primitive.
	// Pure-insert (no per-row mutation), so no concurrency lock is
	// needed. EvalID is caller-generated.

	// EvaluationSubmit inserts a row. The caller stamps EmitterRole
	// (derived server-side from ctx + run identity in the tool
	// layer; the store does not interpret). CreatedAt is set by the
	// store. Returns the persisted row.
	EvaluationSubmit(ctx context.Context, row EvaluationRow) (EvaluationRow, error)

	// EvaluationGet returns one row by eval_id. *ErrNotFound on miss.
	EvaluationGet(ctx context.Context, evalID string) (EvaluationRow, error)

	// EvaluationListForRun returns evaluations targeting a run,
	// newest first. limit ≤ 0 falls through to a sane default.
	EvaluationListForRun(ctx context.Context, runID string, limit int) ([]EvaluationRow, error)

	// EvaluationListForDef returns evaluations targeting one def
	// (denormalised def_id column). Same ordering + limit semantics
	// as ListForRun.
	EvaluationListForDef(ctx context.Context, defID string, limit int) ([]EvaluationRow, error)

	// EvaluationAggregate computes summary statistics for a def_id.
	// When opts.IncludeLineage is true, recursively walks parent_def_id
	// and includes evaluations of every ancestor (depth-first;
	// retired ancestors included). The returned LineageIncluded flag
	// echoes the option for caller-side assertion.
	EvaluationAggregate(ctx context.Context, defID string, opts AggregateOpts) (AggregateResult, error)

	// Close releases backend resources. Idempotent.
	Close() error
}

// MemoryScope is the addressing axis for a Memory or Channel row.
// v0.8.0 shipped `agent` + `user`; v0.8.4 added `global` for the
// Channel tool's cross-tenant fan-out shape. The type is
// forward-compatible for adding `session` / `tenant` later — a new
// scope value is a yaml + adapter allowlist update, not a
// wire-protocol change.
type MemoryScope string

const (
	// MemoryScopeAgent — keyed by yaml agent name. Cross-run state for
	// one agent type (counters, summaries, learned facts).
	MemoryScopeAgent MemoryScope = "agent"
	// MemoryScopeUser — keyed by user_id. Per-end-user state shared
	// across every agent that's allowed to read the `user` scope.
	MemoryScopeUser MemoryScope = "user"
	// MemoryScopeGlobal — single shared keyspace (scope_id = "").
	// v0.8.4 Channel tool only — Memory does not expose this scope
	// (no per-agent memory_scopes value validates it). Channel
	// declares `scope: global` in the operator yaml; agents granted
	// publish/subscribe on a global channel read/write the same
	// cursor regardless of agent or user. Reserved for cross-tenant
	// fan-out streams the operator has reviewed.
	MemoryScopeGlobal MemoryScope = "global"
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

// ChannelMessage is one row in the channel_messages table. ID is a
// ULID assigned by the store at publish time (sortable by publish
// instant — gives "oldest first" reads for free). ExpiresAt is zero
// when the publisher passed no TTL AND the channel had no default;
// the read path filters expired rows regardless of whether the
// sweeper has run.
type ChannelMessage struct {
	ID          string          `json:"id"`
	Channel     string          `json:"channel"`
	Scope       MemoryScope     `json:"scope"` // re-uses MemoryScope so operators don't track two enums
	ScopeID     string          `json:"scope_id"`
	Payload     json.RawMessage `json:"payload"`
	PublishedAt time.Time       `json:"published_at"`
	ExpiresAt   time.Time       `json:"expires_at,omitempty"`
}

// ErrChannelCursorRegression is returned by ChannelAck when a caller
// tries to commit a cursor older than the currently committed one.
// Protects against buggy agents accidentally rewinding delivery —
// the cursor is monotonic by design.
var ErrChannelCursorRegression = &ChannelError{Code: "cursor_regression", Msg: "channel: ack cursor older than committed"}

// ErrChannelValueTooLarge is returned by ChannelPublish when a
// payload exceeds the per-write byte cap
// (LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES, default 64 KB). Mirrors
// ErrMemoryValueTooLarge — same shape, separate type so tool-layer
// error mapping is unambiguous.
var ErrChannelValueTooLarge = &ChannelError{Code: "value_too_large", Msg: "channel: payload exceeds max bytes"}

// ChannelError is the typed-error envelope for channel-specific
// failures the tool layer surfaces to agents. The Code is wire-
// stable; Msg is human-readable and may evolve.
type ChannelError struct {
	Code string
	Msg  string
}

func (e *ChannelError) Error() string { return e.Msg }

// ---- v0.8.5 Self-Evolution Substrate types ----

// AgentDefRow is one row in the agent_defs table. The Definition
// field carries the JSON-encoded AgentDef body verbatim — the store
// does NOT depend on internal/config (dep direction would invert),
// so callers at the tool / HTTP layer unmarshal into the concrete
// shape they need.
//
// Identity:
//   - DefID is the canonical handle (caller-generated UUID/ULID;
//     stable across renames). Use it for run pins and lineage edges.
//   - (Name, Version) is the human-friendly identifier. Version is
//     server-allocated, monotonic per Name with no gaps.
//
// Lineage:
//   - ParentDefID empty = no parent (top of a lineage, typically
//     bootstrapped from a static MD with BootstrappedFromStatic=true).
//   - Children query: AgentDefListChildren(parentDefID).
//
// Provenance:
//   - CreatedByAgentID + CreatedByRunID stamp the agent that called
//     AgentDef.create/fork at runtime. Empty for the static-bootstrap
//     row (its "creator" is the operator's MD file, not an agent).
type AgentDefRow struct {
	DefID                  string          `json:"def_id"`
	Name                   string          `json:"name"`
	Version                int             `json:"version"`
	ParentDefID            string          `json:"parent_def_id,omitempty"`
	Definition             json.RawMessage `json:"definition"`
	Description            string          `json:"description,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	CreatedByAgentID       string          `json:"created_by_agent_id,omitempty"`
	CreatedByRunID         string          `json:"created_by_run_id,omitempty"`
	Retired                bool            `json:"retired"`
	BootstrappedFromStatic bool            `json:"bootstrapped_from_static"`
}

// AgentDefNameSummary is one entry of AgentDefListNames' output.
// count is the version count; ActiveDefID is the agent_def_active
// pointer (empty when no row is promoted under this name).
type AgentDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
}

// EvaluationRow is one row in the evaluations table.
//
// DefID is denormalised from runs.agent_def_id at submit time —
// captures which version of the agent the run actually ran against.
// Empty for static-resolved runs (where the agent body came from
// cfg.Agents, not the database).
//
// Score is the required scalar (RL lingua franca). Dimensions are
// optional named axes for multi-fitness; nil = no dimensions.
// Judgement is a free-form structured payload; nil = absent.
// Rationale is natural-language reasoning for explainability + audit.
//
// EmitterRole is derived server-side from the emitter's ctx vs the
// target run's identity (parent / sibling / self / external /
// unrelated). The model NEVER supplies it.
type EvaluationRow struct {
	EvalID         string             `json:"eval_id"`
	RunID          string             `json:"run_id"`
	DefID          string             `json:"def_id,omitempty"`
	Score          float64            `json:"score"`
	Dimensions     map[string]float64 `json:"dimensions,omitempty"`
	Judgement      json.RawMessage    `json:"judgement,omitempty"`
	Rationale      string             `json:"rationale,omitempty"`
	EmitterRole    string             `json:"emitter_role"`
	EmitterAgentID string             `json:"emitter_agent_id,omitempty"`
	EmitterRunID   string             `json:"emitter_run_id,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
}

// AggregateOpts is the parameter struct for EvaluationAggregate.
type AggregateOpts struct {
	// IncludeLineage walks parent_def_id chain depth-first and
	// includes ancestors' evaluations in the aggregate. Retired
	// ancestors are included; the caller can filter post-hoc.
	IncludeLineage bool
}

// AggregateResult is the output of EvaluationAggregate.
//
// Count is the total evaluation row count contributing to the
// statistics (post-lineage-walk when IncludeLineage is true).
// Score aggregates the scalar field. Dimensions is keyed by the
// dimension name the evaluations supplied (only dimensions present
// in at least one row appear). ByEmitterRole breaks aggregates by
// role string. LineageIncluded echoes the option for caller-side
// assertion.
type AggregateResult struct {
	DefID           string                `json:"def_id"`
	Count           int                   `json:"count"`
	Score           ScoreStats            `json:"score"`
	Dimensions      map[string]ScoreStats `json:"dimensions,omitempty"`
	ByEmitterRole   map[string]ScoreStats `json:"by_emitter_role,omitempty"`
	LineageIncluded bool                  `json:"lineage_included"`
}

// ScoreStats is the summary-stats bundle used inside AggregateResult.
// All fields zero when Count is zero (an empty aggregate is a
// well-defined "no evaluations submitted yet" response, NOT an error).
type ScoreStats struct {
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Latest float64 `json:"latest"`
	Count  int     `json:"count"`
}

// ErrAgentDefParentNotFound is returned by AgentDefCreate when the
// caller supplied a parent_def_id that doesn't exist. Distinct from
// ErrNotFound so the tool layer can surface "your fork parent
// vanished" with a clean code.
var ErrAgentDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "agent_def: parent_def_id does not exist"}

// ErrAgentDefImmutable is returned by store-layer assertions if
// someone tries to UPDATE an agent_defs row's definition column.
// Append-only invariant. The adapter's contract test pins this.
var ErrAgentDefImmutable = &SubstrateError{Code: "immutable", Msg: "agent_def: rows are append-only; create a new version"}

// SubstrateError envelopes substrate-specific errors so the tool
// layer can pattern-match on Code. Mirror of MemoryError /
// ChannelError shape.
type SubstrateError struct {
	Code string
	Msg  string
}

func (e *SubstrateError) Error() string { return e.Msg }

// ErrNotFound is returned when a session or run ID isn't in the store.
type ErrNotFound struct {
	Kind string // "session" | "run"
	ID   string
}

func (e *ErrNotFound) Error() string { return e.Kind + " not found: " + e.ID }
