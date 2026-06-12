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
	"errors"
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

// EncodeChannelCursor renders a (visible_at, msg_id) tuple as the
// opaque cursor token agents receive. Format:
//
//	cur_<16hex-visible_at-unixNanos>_<msg_id>
//
// Lex-sortable by visible_at (16 hex digits cover unixNanos through
// year 2262) and then by msg_id within the same nanosecond. The
// `cur_0` sentinel (for replay-from-oldest) is the one input that
// never round-trips through this function — callers check it
// upstream.
//
// v0.8.6: this format REPLACES the v0.8.4 `msg_<hex>` cursor shape.
// Legacy cursors are wiped by the 0005_channel_visible_at migration;
// agents that stored cursors externally will see a one-shot
// replay-from-oldest on first subscribe after the upgrade.
func EncodeChannelCursor(visibleAt time.Time, msgID string) string {
	if msgID == "" {
		return ""
	}
	return fmt.Sprintf("cur_%016x_%s", uint64(visibleAt.UnixNano()), msgID)
}

// DecodeChannelCursor parses a cursor token into its (visible_at,
// msg_id) tuple. Returns (zero-time, "", true) for "cur_0" and the
// empty string (both interpreted as "from oldest non-expired"); the
// last return reports the "from-oldest" sentinel form so callers can
// skip the WHERE-tuple clause entirely.
//
// Malformed cursors return an error so the tool layer surfaces a
// clear "invalid cursor" rejection rather than treating garbage as
// "from oldest".
func DecodeChannelCursor(token string) (visibleAt time.Time, msgID string, fromOldest bool, err error) {
	if token == "" || token == "cur_0" {
		return time.Time{}, "", true, nil
	}
	const prefix = "cur_"
	if len(token) < len(prefix)+16+1 || token[:len(prefix)] != prefix {
		return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: expected `cur_<16hex>_<msg_id>` or `cur_0`", token)
	}
	hex16 := token[len(prefix) : len(prefix)+16]
	if token[len(prefix)+16] != '_' {
		return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: missing `_` after timestamp", token)
	}
	msgID = token[len(prefix)+16+1:]
	if msgID == "" {
		return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: empty msg_id", token)
	}
	// msg_id format check: MintChannelMessageID produces exactly
	// `msg_<16hex-unixNanos><8hex-rand>` = 4 + 24 = 28 chars. Without
	// this guard, a cursor like `cur_<vh>_msg_<hex>_junk` would pass
	// the prefix check and then be stored verbatim via ChannelAck —
	// later read queries comparing tuple (visible_at, msg_id) against
	// the bogus suffix would find no rows and the subscriber would
	// silently stall forever. Accept only the well-formed shape.
	const msgIDLen = 4 + 16 + 8 // "msg_" + nanos-hex + rand-hex
	if len(msgID) != msgIDLen || msgID[:4] != "msg_" {
		return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: malformed msg_id %q (want `msg_<24hex>`)", token, msgID)
	}
	for i := 4; i < len(msgID); i++ {
		c := msgID[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: msg_id contains non-hex char", token)
		}
	}
	var nanos uint64
	if _, err := fmt.Sscanf(hex16, "%016x", &nanos); err != nil {
		return time.Time{}, "", false, fmt.Errorf("invalid channel cursor %q: timestamp parse: %w", token, err)
	}
	return time.Unix(0, int64(nanos)), msgID, false, nil
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
	// Provider is the provider ID that ACTUALLY served the final
	// successful iteration of this run. Distinct from the
	// yaml-configured provider when v0.8.2 runtime fallback engaged
	// mid-run (e.g., anthropic-oauth-dev → ollama after a 429).
	// Empty for pre-v0.12.7-telemetry runs and for runs that never
	// completed an iteration.
	Provider string `json:"provider,omitempty"`
	ErrorMsg string `json:"error,omitempty"`

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
	// TenantID is the authoritative tenant (RFC L), denormalised at run
	// creation so tenant-scoped reads filter without a sessions JOIN.
	// Empty/"default" on legacy single-tenant rows.
	TenantID string `json:"tenant_id,omitempty"`
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

	// AgentDefID is the v0.8.5 substrate audit column — populated
	// when the parent's Agent tool call pinned a specific def_id, or
	// when an admin path resolves through agent_def_active. Empty =
	// the run resolved through static cfg.Agents only. The
	// Evaluation tool's submit op reads this to denormalise def_id
	// onto each evaluation row at write time.
	AgentDefID string `json:"agent_def_id,omitempty"`

	// PauseState is the v0.8.17 substrate marker for a run's
	// participation in the runtime-wide quiesce protocol. One of
	// PauseStateRunning (default, never paused or already resumed),
	// PauseStatePausing (operator issued POST /v1/runtime/pause; the
	// loop is between tool calls or waiting on a non-idempotent tool's
	// timeout), or PauseStatePaused (the loop has reached an
	// iteration boundary and persisted the pause).
	//
	// The column default is "running" so existing rows back-fill
	// without surprise. The loop reads this column on resume to
	// distinguish runs that need to re-enter from runs that finished
	// during pause (status terminal already).
	PauseState string `json:"pause_state,omitempty"`

	// ReplicaID is the replica that created this run and owns its
	// live cancel handle (v0.12.2 Phase 3). NULL/empty on rows
	// created before v0.12.2 or in single-replica mode. Cross-replica
	// cancel routes via this column.
	ReplicaID string `json:"replica_id,omitempty"`

	// ParentContext is the opaque caller-tracking lineage (v0.12.x),
	// set on the root run and copied onto every sub-agent. Persisted as
	// the runs.parent_context JSON column; nil for rows created before
	// the column landed or for runs with no context. Echoed on the
	// per-agent report surfaces so a consumer can attribute a child
	// sub-agent's usage to the user-initiated request.
	ParentContext *ParentContext `json:"parent_context,omitempty"`

	// IdempotencyKey is the RFC H Decision 10 "Layer 2" durable dedup
	// key the run was created with (runs.idempotency_key). Empty on rows
	// created without one (the common case) and on pre-migration rows.
	// Round-trips on read so a deduped caller can look up the winning run
	// and confirm the key. Not a secret.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Interactive marks a persistent interactive run (F42 / RFC X Phase 2):
	// it parks at end_turn (awaiting_input) instead of terminating. Stamped
	// at CreateRun from the run request's `interactive` flag and persisted
	// (runs.interactive) so a snapshotted + restored paused run can be
	// re-dispatched on another instance with the correct park-vs-complete
	// semantics. false on legacy rows + batch runs.
	Interactive bool `json:"interactive,omitempty"`
}

// PauseState constants — the wire string values stored in runs.pause_state.
// Validation lives at the Store boundary (SetRunPauseState refuses unknown
// values) so the loop and HTTP handlers can rely on these being the only
// possible reads.
const (
	PauseStateRunning = "running" // default; the run is executing or already finished
	PauseStatePausing = "pausing" // pause requested; loop is winding down to an iteration boundary
	PauseStatePaused  = "paused"  // loop reached the boundary and persisted; awaiting resume
)

// EventFilter narrows a ListEvents query. Zero values mean "no
// filter on this dimension":
//   - Type == ""  → all event types
//   - From / To zero-value time.Time → unbounded on that side
//
// Use From + To together for a window; either alone is supported.
type EventFilter struct {
	Type string
	From time.Time
	To   time.Time
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
	// Provider is the resolver-active provider ID at the final
	// successful iteration. Carried alongside Model so downstream
	// consumers see "actually served by" rather than guess from
	// model-name conventions. Differs from agent yaml when v0.8.2
	// fallback engaged.
	Provider string
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
	// TenantID is the authoritative tenant (RFC L), denormalised onto
	// the run row so tenant-scoped list/read queries (the Web UI's
	// per-tenant workspace) filter without a sessions JOIN. Set from the
	// run's effective tenant at creation; "" / "default" on legacy
	// single-tenant rows. The tenant-authz boundary keys on this.
	TenantID string
	// UserTier is the v0.8.2 user-tier marker captured at run
	// creation. Empty when the request didn't carry user_tier (back-
	// compat) or the operator's yaml has no user_tiers block.
	UserTier string
	// AgentDefID pins this run to a specific agent_defs row (v0.8.5).
	// Empty = the run resolved through static cfg.Agents only; non-
	// empty = parent called Agent tool with a def_id. Persisted as
	// runs.agent_def_id so the Evaluation tool can denormalise it
	// into evaluations.def_id at submit time.
	AgentDefID string
	// Model is the resolved (provider, model) decision applied at
	// run creation — written to runs.model immediately so the row is
	// queryable mid-flight, not just after FinishRun. Empty = unknown
	// at creation time (back-compat with callers that haven't been
	// updated yet). FinishRun overwrites this with the final model
	// recorded by the loop, which may differ if cross-provider
	// fallback fired mid-run.
	Model string
	// ReplicaID stamps this run with the replica that owns its live
	// cancel handle (v0.12.2 Phase 3). Empty in single-replica mode
	// (LOOMCYCLE_REPLICA_ID unset); the column stays NULL. In cluster
	// mode it routes cross-replica cancel requests to the owning
	// replica via backplane broadcast. Postgres-only; SQLite ignores.
	ReplicaID string
	// ParentContext is opaque caller-tracking lineage (v0.12.x). Set on
	// the root run and copied verbatim onto every sub-agent the Agent
	// tool spawns; persisted as runs.parent_context (JSON). nil = no
	// context (back-compat; old rows decode to nil). See ParentContext.
	ParentContext *ParentContext

	// IdempotencyKey is the optional RFC H Decision 10 "Layer 2"
	// durable dedup key. Empty (the default) = no dedup; the run is
	// created unconditionally. Non-empty = persisted to
	// runs.idempotency_key (a partial unique index); a second CreateRun
	// with the same key returns ErrDuplicateIdempotencyKey instead of
	// inserting a duplicate row. The webhook spawn path sets this to the
	// delivery id so a redelivery that survives past the in-memory
	// Layer-1 TTL — or lands on a different replica — still dedups.
	// Not a secret (safe to persist + echo).
	IdempotencyKey string

	// Interactive marks a persistent interactive run (F42 / RFC X Phase 2).
	// Persisted to runs.interactive so a snapshotted + restored paused run
	// re-dispatches with the correct park-at-end_turn (vs run-to-completion)
	// semantics. false = batch run (the default).
	Interactive bool
}

// ParentContext is the typed caller-tracking lineage attached to a run
// and propagated to all its sub-agents. The runtime stores and echoes
// these fields verbatim and never branches on their values — they are
// consumer-domain concepts (a deliberate, operator-requested exception
// to loomcycle's usual domain-agnostic posture).
//
// It lives in the store package — the lowest layer, importing no other
// internal package — so runner, tools, connector, and the wire surfaces
// can all reference one type without an import cycle (the same reason
// store.RunIdentity and tools.RunIdentityValue are kept separate: loop
// imports tools, so tools cannot import runner).
//
// Unlike UserBearer/UserCredentials this is NOT a secret: safe to
// persist, log, and emit in events. All fields optional; an all-empty
// struct is treated as absent (nil) at wire entry.
type ParentContext struct {
	// RootAgentRunID is the consumer's identifier for the user-
	// initiated run at the root of the spawn tree. Echoed on every
	// descendant so the consumer can attribute child costs to it.
	RootAgentRunID string `json:"root_agent_run_id,omitempty"`
	// FunctionKey is the consumer's logical-operation key for the root
	// request (e.g. its cost-aggregation bucket).
	FunctionKey string `json:"function_key,omitempty"`
	// TierAtRun is the consumer's tier marker captured at root-run time.
	// Distinct from UserTier (loomcycle's resolver policy) — this is the
	// consumer's own snapshot, carried verbatim.
	TierAtRun string `json:"tier_at_run,omitempty"`
}

// IsZero reports whether every field is empty (no meaningful tracking
// context). Wire entry points normalise a zero struct to nil so
// back-compat decode paths stay clean.
func (p *ParentContext) IsZero() bool {
	return p == nil || (p.RootAgentRunID == "" && p.FunctionKey == "" && p.TierAtRun == "")
}

// Clone returns a deep copy (nil-safe) so a parent's ParentContext can
// be handed to a child run without aliasing.
func (p *ParentContext) Clone() *ParentContext {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// EncodeParentContext returns the JSON to persist in the runs.parent_context
// column. ok=false means there's nothing to store (nil or all-empty) — the
// backend writes SQL NULL in that case. Backends share this so the SQLite
// and Postgres column formats can't drift.
func EncodeParentContext(p *ParentContext) (encoded string, ok bool, err error) {
	if p.IsZero() {
		return "", false, nil
	}
	b, mErr := json.Marshal(p)
	if mErr != nil {
		return "", false, mErr
	}
	return string(b), true, nil
}

// DecodeParentContext parses a stored runs.parent_context value. An empty
// string (NULL column / old row) decodes to nil.
func DecodeParentContext(s string) (*ParentContext, error) {
	if s == "" {
		return nil, nil
	}
	var p ParentContext
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, err
	}
	return &p, nil
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

	// GetRunEventsSince returns a run's events with Seq > afterSeq, ordered by
	// Seq ascending, capped at limit (<=0 means a sane default). Run-scoped
	// (not session-scoped) and incremental, so the interactive-run SSE tail
	// (GET /v1/runs/{run_id}/stream) can poll cheaply without re-reading the
	// whole session transcript each tick. Returns an empty slice (not error)
	// when nothing is newer than afterSeq.
	GetRunEventsSince(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error)

	// ListEvents returns events across all sessions matching the
	// filter, ordered by ts DESC (newest first). Used by the v0.8.21
	// /v1/_events audit endpoint. Returns the rows AND the total
	// match count (for pagination UIs that show "page N of M");
	// total is an unbounded COUNT(*) over the same filter — bounded
	// by indexes events_by_ts / events_by_type_ts. Pagination is
	// offset-based for simplicity; cursor-based pagination is a
	// follow-up if scale demands.
	ListEvents(ctx context.Context, filter EventFilter, limit, offset int) ([]Event, int64, error)

	// GetLastEventForRun returns the highest-seq event recorded for
	// the given run. v0.8.21 introduces this so the list-agents
	// endpoint can derive "awaited state" (channel/interrupted/
	// running) cheaply from one row per running agent — without
	// pulling the full transcript. Returns *ErrNotFound{Kind:"event"}
	// when the run has no events yet (just-created, hasn't streamed
	// anything).
	GetLastEventForRun(ctx context.Context, runID string) (Event, error)

	// GetRunByAgentID returns the most recently started run carrying
	// the given agent_id. Returns *ErrNotFound when no such row.
	// Used by the GET /v1/agents/{agent_id} and cancel endpoints to
	// resolve the API-facing handle to a Run.
	GetRunByAgentID(ctx context.Context, agentID string) (Run, error)

	// RunByIdempotencyKey returns the run created with the given RFC H
	// Decision 10 idempotency key. ok=false (with a nil error) when no
	// run carries the key. An empty key short-circuits to (Run{}, false,
	// nil) — callers don't have to pre-check. Used by the webhook
	// receiver to resolve a deduped delivery to its already-spawned run.
	RunByIdempotencyKey(ctx context.Context, key string) (Run, bool, error)

	// GetRun returns one row by run_id (the primary key on runs).
	// Distinct from GetRunByAgentID which queries by the caller-
	// supplied tracking handle. v0.8.5 Evaluation tool uses this to
	// look up the target run's AgentID + ParentAgentID at submit
	// time so it can derive emitter_role (self / sibling / parent /
	// external / unrelated) server-side. Returns *ErrNotFound on miss.
	GetRun(ctx context.Context, runID string) (Run, error)

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
	//
	// tenantID scopes the result to one tenant (the Web UI's per-tenant
	// workspace + the super-admin tenant-focus), filtering on the
	// denormalised runs.tenant_id; "" returns all tenants.
	ListUsers(ctx context.Context, tenantID string) ([]UserSummary, error)

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

	// SetRunPauseState writes runs.pause_state. The v0.8.17 PauseManager
	// uses this to transition runs through running → pausing → paused
	// (on pause), then back to running (on resume). Refuses unknown
	// state strings — callers must use the PauseState* constants.
	// Returns *ErrNotFound when no row matches runID.
	//
	// Idempotent: writing the current value is a no-op. Does NOT clear
	// pause_state for terminal runs (status in {completed, failed,
	// cancelled}) — the pause column is only meaningful while a run
	// could still be resumed; the column on terminal runs records what
	// state they were in when the loop exited.
	SetRunPauseState(ctx context.Context, runID, state string) error

	// ListPausedRuns returns runs whose pause_state is "paused" (the
	// at-rest paused state, not the in-flight "pausing" transition).
	// Used by the PauseManager on resume to find which runs need to
	// re-enter their loops, and by GET /v1/runtime/state for operator
	// visibility (the response payload's paused_run_count).
	//
	// Ordering: by started_at ASC so resume processes the oldest
	// pauses first (lower risk of stale state being overwritten by
	// fresher runs that paused later in the same window).
	ListPausedRuns(ctx context.Context) ([]Run, error)

	// ---- v0.8.17 Pause/Resume/Snapshot — Snapshot storage (PR 2) ----

	// SnapshotCreate inserts one row into the snapshots table.
	// Caller computes byte_size + JSON content; store records
	// created_at if zero. Returns *ErrConflict if the id already
	// exists (idempotent caller can detect the collision and skip).
	SnapshotCreate(ctx context.Context, row SnapshotRow) error

	// SnapshotGet returns one row by id, INCLUDING the JSON content
	// (export endpoints need the full payload). Returns *ErrNotFound
	// when no row matches.
	SnapshotGet(ctx context.Context, id string) (SnapshotRow, error)

	// SnapshotList returns snapshot metadata (no JSON content — the
	// list endpoint shows id/created_at/label/byte_size only, keeping
	// the response cheap when there are hundreds of snapshots). The
	// optional labelContains parameter narrows by case-insensitive
	// substring match; empty string returns all. limit caps the
	// result count (0 = no cap; recommend 200 default at the handler
	// layer to bound payload size).
	SnapshotList(ctx context.Context, labelContains string, limit int) ([]SnapshotListEntry, error)

	// SnapshotDelete removes one snapshot. Returns true when a row
	// was removed (existed pre-call); false when no match. Never
	// returns an error for the "doesn't exist" case — idempotent
	// delete is the operator-friendly default.
	SnapshotDelete(ctx context.Context, id string) (bool, error)

	// ---- v0.12.5 Phase 6: cluster-wide hook registry ----
	//
	// CreateHook / DeleteHook / ListHooks / GetHookByID persist hooks
	// for cross-replica visibility. The hooks.DBBackedRegistry wraps
	// these for the cluster-mode path; single-replica deployments use
	// the in-process hooks.Registry and never call these. SQLite
	// implementations are stubs (cluster mode refuses SQLite at boot).

	CreateHook(ctx context.Context, h HookRow) error
	DeleteHook(ctx context.Context, hookID string) error
	ListHooks(ctx context.Context) ([]HookRow, error)
	GetHookByID(ctx context.Context, hookID string) (HookRow, error)

	// ---- v0.8.17 Snapshot capture — bulk readers (PR 2.3a) ----
	//
	// The methods below return ALL rows in their tables. They power
	// the snapshot package's Capture(), which reads every section
	// into a single in-memory JSON envelope. Cost profile is
	// O(rows-in-table); operators should size LOOMCYCLE_SNAPSHOT_
	// MAX_BYTES accordingly. NOT for hot-path queries.

	// SnapshotReadAgentDefs returns every row in agent_defs across
	// all names + versions. Ordered by (name ASC, version ASC) so
	// the snapshot envelope's section is deterministic across
	// repeated captures of an unchanged store (round-trip tests
	// depend on this).
	SnapshotReadAgentDefs(ctx context.Context) ([]AgentDefRow, error)

	// SnapshotReadAgentDefActive returns every row in
	// agent_def_active. Ordered by name ASC for determinism.
	SnapshotReadAgentDefActive(ctx context.Context) ([]AgentDefActiveEntry, error)

	// SnapshotReadSkillDefs returns every row in skill_defs across
	// all names + versions. Ordered by (name ASC, version ASC) for
	// snapshot determinism. Mirrors SnapshotReadAgentDefs.
	SnapshotReadSkillDefs(ctx context.Context) ([]SkillDefRow, error)

	// SnapshotReadSkillDefActive returns every row in
	// skill_def_active. Ordered by name ASC for determinism.
	SnapshotReadSkillDefActive(ctx context.Context) ([]SkillDefActiveEntry, error)

	// SnapshotReadMCPServerDefs — v0.9.x mirror of SnapshotReadSkillDefs.
	SnapshotReadMCPServerDefs(ctx context.Context) ([]MCPServerDefRow, error)

	// SnapshotReadMCPServerDefActive — v0.9.x mirror.
	SnapshotReadMCPServerDefActive(ctx context.Context) ([]MCPServerDefActiveEntry, error)

	// SnapshotReadMemory returns every memory row across all scopes,
	// tagged with scope + scope_id. Ordered by (scope ASC, scope_id
	// ASC, key ASC). Filters out expired rows (consistent with
	// MemoryGet's behaviour) so the snapshot doesn't carry
	// already-stale entries.
	SnapshotReadMemory(ctx context.Context) ([]MemorySnapshotEntry, error)

	// SnapshotReadChannelMessages returns every channel_messages row.
	// Filters out expired rows. Ordered by (channel ASC, scope ASC,
	// scope_id ASC, visible_at ASC, id ASC) — matches the natural
	// delivery order so restore replays messages in their original
	// sequence.
	SnapshotReadChannelMessages(ctx context.Context) ([]ChannelMessage, error)

	// SnapshotReadChannelCursors returns every channel_cursors row.
	// Ordered by (channel ASC, scope ASC, scope_id ASC) for
	// determinism.
	SnapshotReadChannelCursors(ctx context.Context) ([]ChannelCursorEntry, error)

	// SnapshotReadEvaluations returns every evaluations row, ordered
	// by created_at ASC. The snapshot envelope's evaluations section
	// preserves submission order so post-restore Evaluation.aggregate
	// queries see the same time series.
	SnapshotReadEvaluations(ctx context.Context) ([]EvaluationRow, error)

	// ---- v0.8.17 Snapshot restore — idempotent raw inserts (PR 3.2a) ----
	//
	// The methods below INSERT rows with caller-supplied IDs +
	// timestamps + values, using ON CONFLICT DO NOTHING (Postgres) /
	// INSERT OR IGNORE (SQLite) semantics so a second restore of the
	// same envelope is a clean no-op. Distinct from the "live" insert
	// methods (CreateSession, AppendEvent, etc.) which mint their own
	// IDs and assume an empty starting state.
	//
	// All SnapshotRestore* methods are best-effort idempotent — if a
	// row with the same PK exists, the call silently succeeds. The
	// snapshot.Restore function relies on this to support partial
	// re-runs after a failed restore.
	//
	// Each method returns (inserted bool, err error). `inserted` is
	// true when a new row was actually written; false when the
	// underlying ON CONFLICT DO NOTHING / INSERT OR IGNORE swallowed
	// the row (or the equivalent UPSERT path observed an existing
	// row). Callers in internal/snapshot use this to increment
	// per-section counters only on real inserts so the
	// snapshotRestoreResponse a second (idempotent) restore returns
	// reads as "0 inserted" rather than misleadingly repeating the
	// first call's counts.

	// SnapshotRestoreSession inserts a session row preserving the
	// caller-supplied ID + CreatedAt. Idempotent.
	SnapshotRestoreSession(ctx context.Context, sess Session) (bool, error)

	// SnapshotRestoreRun inserts a run row preserving every field
	// including PauseState. Idempotent.
	SnapshotRestoreRun(ctx context.Context, r Run) (bool, error)

	// SnapshotRestoreEvent inserts one transcript event preserving
	// the supplied seq + RunID + Timestamp + Payload. Note: events
	// use BIGSERIAL/AUTOINCREMENT seq normally; this method writes
	// the seq explicitly. Idempotent on (run_id, seq).
	SnapshotRestoreEvent(ctx context.Context, e Event) (bool, error)

	// SnapshotRestoreAgentDef inserts one agent_defs row preserving
	// the supplied DefID + Version + parent linkage. Idempotent.
	SnapshotRestoreAgentDef(ctx context.Context, r AgentDefRow) (bool, error)

	// SnapshotRestoreAgentDefActive inserts one agent_def_active
	// pointer. ON CONFLICT (tenant_id, name) DO NOTHING — preserves the
	// snapshot's promoted_at + promoted_by_agent_id on first restore;
	// subsequent restores leave the row alone. `inserted` is true only
	// on the first write (no prior row for the (tenant_id, name)).
	SnapshotRestoreAgentDefActive(ctx context.Context, entry AgentDefActiveEntry) (bool, error)

	// SnapshotRestoreSkillDef mirrors SnapshotRestoreAgentDef for
	// skill_defs. Idempotent on def_id.
	SnapshotRestoreSkillDef(ctx context.Context, r SkillDefRow) (bool, error)

	// SnapshotRestoreSkillDefActive mirrors SnapshotRestoreAgentDefActive
	// for skill_def_active. ON CONFLICT (tenant_id, name) DO NOTHING.
	SnapshotRestoreSkillDefActive(ctx context.Context, entry SkillDefActiveEntry) (bool, error)

	// SnapshotRestoreMCPServerDef — v0.9.x mirror.
	SnapshotRestoreMCPServerDef(ctx context.Context, r MCPServerDefRow) (bool, error)

	// SnapshotRestoreMCPServerDefActive — v0.9.x mirror.
	SnapshotRestoreMCPServerDefActive(ctx context.Context, entry MCPServerDefActiveEntry) (bool, error)

	// SnapshotRestoreMemory inserts one memory row preserving
	// CreatedAt + UpdatedAt + ExpiresAt + Value. Idempotent on
	// (scope, scope_id, key).
	SnapshotRestoreMemory(ctx context.Context, entry MemorySnapshotEntry) (bool, error)

	// SnapshotRestoreChannelMessage inserts one channel_messages row
	// preserving the ID + timestamps. Idempotent on id (PK).
	SnapshotRestoreChannelMessage(ctx context.Context, msg ChannelMessage) (bool, error)

	// SnapshotRestoreChannelCursor UPSERTs one channel_cursors row.
	// ON CONFLICT (channel, scope, scope_id) DO UPDATE — preserves
	// the snapshot's cursor + updated_at. `inserted` is true only on
	// the first write.
	SnapshotRestoreChannelCursor(ctx context.Context, entry ChannelCursorEntry) (bool, error)

	// SnapshotRestoreEvaluation inserts one evaluation row
	// preserving EvalID + CreatedAt + emitter fields. Idempotent
	// on eval_id.
	SnapshotRestoreEvaluation(ctx context.Context, r EvaluationRow) (bool, error)

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

	// MemoryAtomicUpdate runs `reducer` as an atomic read-modify-write
	// against (scope, scopeID, key). The reducer receives the current
	// value (empty json.RawMessage when the row doesn't exist) and
	// returns the new value. The store wraps the call in a per-row
	// lock + transaction so concurrent updates serialize cleanly.
	//
	// Used by the Memory tool's reducer ops (merge / append_dedupe /
	// bounded_list) in v0.12.x — each op constructs a different
	// reducer closure but reuses this single atomic primitive.
	//
	// ttl > 0 sets (or resets) the expiry; ttl <= 0 keeps the
	// existing expiry on update / no expiry on a fresh row.
	//
	// Returns the value written. When the reducer returns an error,
	// the transaction rolls back and that error propagates verbatim
	// — the tool layer wraps it for the agent-visible message.
	MemoryAtomicUpdate(
		ctx context.Context,
		scope MemoryScope,
		scopeID, key string,
		ttl time.Duration,
		reducer func(existing json.RawMessage) (next json.RawMessage, err error),
	) (json.RawMessage, error)

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

	// SupportsVectors reports whether this backend instance can serve
	// the MemoryEmbed* family. Backends without a vector index loaded
	// return false; the Memory tool's `search` op + `embed: true`
	// field check this before calling and surface ErrVectorUnsupported
	// to the agent. v0.9.0: Postgres returns true iff
	// LOOMCYCLE_PGVECTOR_ENABLED=1 AND the pgvector extension is
	// installed; SQLite returns false unconditionally (sqlite-vec
	// support deferred to v0.9.1).
	SupportsVectors() bool

	// MemoryEmbedSet writes the embedding vector for the (scope,
	// scopeID, key) tuple. Idempotent — re-writes overwrite the
	// existing embedding row. Returns ErrVectorUnsupported when the
	// backend doesn't have a vector index. The base memory row must
	// exist; backends MUST enforce this (the FK CASCADE in the
	// memory_embeddings schema covers the inverse — base-row delete
	// drops the embedding).
	//
	// Embedding bytes do NOT count toward the per-(scope, scopeID)
	// quota — quota math is k/v-only per the v0.9.0 RFC §8 decision.
	MemoryEmbedSet(ctx context.Context, scope MemoryScope, scopeID, key string, e MemoryEmbedding) error

	// MemoryEmbedGet returns the stored embedding for one key, or
	// *ErrNotFound if no embedding exists. Used by the snapshot
	// Capture path (v0.9.0 PR 5) and the admin reembed endpoint.
	// ErrVectorUnsupported on backends without a vector index.
	MemoryEmbedGet(ctx context.Context, scope MemoryScope, scopeID, key string) (MemoryEmbedding, error)

	// MemoryEmbedSearch runs a Top-K cosine-similarity search over
	// rows in (scope, scopeID). keyPrefix is optional — empty string
	// matches every key. The returned MemorySearchEntry slice is
	// sorted by score DESC (higher = closer); Score is in [0, 1]
	// where 1.0 means identical direction.
	//
	// Dimension mismatch (query vector dimension ≠ stored dimension)
	// returns ErrDimensionMismatch with a message that includes both
	// dimensions. topK <= 0 is treated as 10; topK > 51 is clamped.
	// (The 51 cap, vs the RFC's agent-facing 50, lets the Memory
	// tool layer request topK+1 for its truncation-detection probe
	// at the boundary.)
	// Empty results return (nil, nil) — not an error.
	//
	// Backends MUST honour the base table's expires_at filter — a
	// matching vector for an expired row MUST NOT appear in results.
	MemoryEmbedSearch(ctx context.Context, scope MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]MemorySearchEntry, error)

	// MemoryEmbedListByModel returns entries whose stored embedding
	// was produced by a DIFFERENT (provider, model) than the supplied
	// pair. Drives the v0.9.0 PR 4 admin endpoint `/v1/_memory/
	// reembed` — operators query "which rows need re-embedding under
	// my current embedder config." limit <= 0 is treated as 1000.
	//
	// Returns ErrVectorUnsupported on backends without vector ops.
	MemoryEmbedListByModel(ctx context.Context, scope MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]MemoryEntry, error)

	// MemoryEmbedStats returns per-(provider, model) row counts and
	// total embedding bytes for the given scope. Drives the v0.9.0
	// PR 4 admin endpoint `/v1/_memory/embed_stats`. ErrVectorUnsupported
	// on backends without vector ops.
	MemoryEmbedStats(ctx context.Context, scope MemoryScope) (MemoryEmbedStats, error)

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

	// ChannelListCursorsForScope returns every channel_cursors row
	// matching (scope, scope_id). v0.9.x introspection — drives the
	// Web UI's "channels this agent has subscribed to" view via the
	// admin endpoint at GET /v1/agents/{name}/channels (scope=agent)
	// and the equivalent per-user path. Empty slice when the
	// (scope, scope_id) tuple has no cursors. Returns the full set so
	// the UI can render "all channels this agent has ack'd on"
	// without N+1 per-channel queries.
	ChannelListCursorsForScope(ctx context.Context, scope MemoryScope, scopeID string) ([]ChannelCursorEntry, error)

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

	// ChannelStats returns one row per channel that has at least one
	// non-expired message, with the aggregate count + oldest/newest
	// visible_at timestamps. Channels declared in operator yaml but
	// holding no messages do NOT appear in this result — the caller
	// joins against the declared list to surface "declared but empty"
	// channels with zero counts.
	//
	// Used by the GET /v1/_channels admin listing (Phase 0 of the
	// n8n integration RFC).
	ChannelStats(ctx context.Context) ([]ChannelStats, error)

	// BackfillAgentDefContentSHA256 walks every agent_defs row with a
	// NULL or empty content_sha256, calls signFn with the row's
	// (name, definition) JSON to compute the canonical hash, and
	// writes it back. Returns the count of rows updated.
	//
	// Idempotent: a second call after a complete backfill finds zero
	// NULL rows and returns 0. Boot-time hook runs this once after
	// migrations; subsequent boots are O(0) once the column is
	// fully populated.
	//
	// signFn is injected so the store package stays free of any
	// agents/skills import — the v0.9.x hash algorithm lives in
	// internal/agents.Sign, and main.go assembles the closure that
	// calls it.
	//
	// Errors from signFn ABORT the backfill (return on first error)
	// rather than skipping the row, because a hash-compute failure on
	// any well-formed Definition JSONB indicates a code bug, not a
	// data problem.
	BackfillAgentDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error)

	// BackfillAgentDefSystemPromptBase walks every agent_defs row,
	// decodes its definition, fills the `system_prompt_base` field
	// from `system_prompt` when the former is empty, and writes the
	// row back. Returns the count of rows updated.
	//
	// Idempotent: a second call after a complete backfill finds no
	// rows missing the field and returns 0. Boot-time hook runs this
	// once after migrations.
	//
	// Why: PR #186 fixed the runtime symptom (substrate-loaded agents
	// losing their instructions on skill-enabled runs) via a read-side
	// normalizer. This backfill closes the on-disk data gap so the
	// field is materialized for legacy rows too — useful when
	// snapshot/restore round-trips a substrate-only deployment to a
	// reader that strictly trusts the persisted shape.
	//
	// The transform happens at the JSON layer (the store doesn't
	// import mergedDef from internal/tools/builtin). Rows whose
	// definition JSON fails to decode are skipped with a log line
	// rather than aborting the backfill — a single hand-edited row
	// shouldn't block the rest.
	BackfillAgentDefSystemPromptBase(ctx context.Context) (int, error)

	// BackfillSkillDefContentSHA256 — mirror of the AgentDef backfill.
	BackfillSkillDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error)

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
	// `(tenantID, name)` to `defID`. promotedByAgentID is the agent_id
	// that performed the promotion (may be empty for admin API calls).
	// Idempotent: promote A → promote B → promote A leaves the
	// pointer at A with the latest promoted_at. RFC N: the active
	// pointer is per-tenant, and a def can only be promoted within its
	// own tenant — implementations refuse if the def's tenant_id ≠
	// tenantID. tenantID "" = the shared/operator/legacy tenant.
	AgentDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error

	// AgentDefGetActive returns the currently-active row for
	// `(tenantID, name)` — the (name, version) pointed at by
	// agent_def_active within the tenant. Returns *ErrNotFound when no
	// active pointer exists (the caller falls through to cfg.Agents —
	// the static fallback path). RFC N: tenantID "" = the shared/
	// operator/legacy tenant.
	AgentDefGetActive(ctx context.Context, tenantID, name string) (AgentDefRow, error)

	// AgentDefSetRetired flips the `retired` flag on one row, in a
	// transaction. The row stays visible in lineage queries with the flag
	// exposed. When retired=true AND this def is the CURRENT active pointer
	// for its (tenant, name), the active pointer is CLEARED in the same tx —
	// so the name becomes reclaimable (a fresh create allocates the next
	// version) and runs fall through to the static fallback / nothing
	// instead of resolving a retired def. retired=false never touches the
	// pointer (un-retire does NOT auto-promote — promotion is an explicit
	// op). Retiring a NON-active version leaves the pointer untouched.
	AgentDefSetRetired(ctx context.Context, defID string, retired bool) error

	// ---- v0.8.22 SkillDef substrate ----
	//
	// Mirror of AgentDef* with the same invariants. Concurrency
	// posture is identical: a per-name lock makes version monotonic
	// across concurrent forks. The Definition payload is a skill
	// body + metadata instead of an agent body — the store stays
	// content-agnostic.

	SkillDefCreate(ctx context.Context, row SkillDefRow) (SkillDefRow, error)
	SkillDefGet(ctx context.Context, defID string) (SkillDefRow, error)
	SkillDefGetByNameVersion(ctx context.Context, name string, version int) (SkillDefRow, error)
	SkillDefListByName(ctx context.Context, name string) ([]SkillDefRow, error)
	SkillDefListChildren(ctx context.Context, parentDefID string) ([]SkillDefRow, error)
	SkillDefListNames(ctx context.Context) ([]SkillDefNameSummary, error)
	// SkillDefSetActive UPSERTs the skill_def_active pointer for
	// `(tenantID, name)`. RFC N: the active pointer is per-tenant, and a
	// def can only be promoted within its own tenant — implementations
	// refuse if the def's tenant_id ≠ tenantID. tenantID "" = the shared/
	// operator/legacy tenant.
	SkillDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// SkillDefGetActive returns the currently-active row for
	// `(tenantID, name)`. Returns *ErrNotFound when no active pointer
	// exists (the caller falls through to the static skills.Set). RFC N:
	// tenantID "" = the shared/operator/legacy tenant.
	SkillDefGetActive(ctx context.Context, tenantID, name string) (SkillDefRow, error)
	SkillDefSetRetired(ctx context.Context, defID string, retired bool) error

	// ---- v0.9.x MCPServerDef substrate ----
	//
	// Third substrate primitive after AgentDef + SkillDef. Same shape
	// (per-name lock, monotonic versioning, append-only definition,
	// active-pointer overlay, retire flag). The Definition payload
	// carries the MCP server's connection metadata + discovered tools
	// — see internal/tools/builtin/mcpserverdef.go for the schema.
	//
	// Coexists with the static yaml `mcp_servers:` block: yaml entries
	// stay boot-loaded with no row representation; dynamic registrations
	// have rows. Name collisions with yaml entries are refused at the
	// tool layer.

	MCPServerDefCreate(ctx context.Context, row MCPServerDefRow) (MCPServerDefRow, error)
	MCPServerDefGet(ctx context.Context, defID string) (MCPServerDefRow, error)
	MCPServerDefGetByNameVersion(ctx context.Context, name string, version int) (MCPServerDefRow, error)
	MCPServerDefListByName(ctx context.Context, name string) ([]MCPServerDefRow, error)
	MCPServerDefListChildren(ctx context.Context, parentDefID string) ([]MCPServerDefRow, error)
	// MCPServerDefListNames returns one summary per (tenant, name) pair,
	// each carrying its TenantID. RFC N: the boot rehydrator + the
	// advertising filter rely on the TenantID so each run only resolves
	// its own + shared servers.
	MCPServerDefListNames(ctx context.Context) ([]MCPServerDefNameSummary, error)
	// MCPServerDefSetActive UPSERTs the mcp_server_def_active pointer for
	// (tenantID, name). RFC N: the active pointer is per-tenant, and a
	// def can only be promoted within its own tenant — implementations
	// refuse if the def's tenant_id ≠ tenantID. tenantID "" = the
	// shared/operator/legacy tenant.
	MCPServerDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// MCPServerDefGetActive returns the active row for (tenantID, name).
	// Returns *ErrNotFound when no active pointer exists. RFC N: tenantID
	// "" = the shared/operator/legacy tenant.
	MCPServerDefGetActive(ctx context.Context, tenantID, name string) (MCPServerDefRow, error)
	MCPServerDefSetRetired(ctx context.Context, defID string, retired bool) error

	// BackfillMCPServerDefContentSHA256 — mirror of the AgentDef /
	// SkillDef backfills. Walks NULL/empty content_sha256 rows + calls
	// the injected signFn. Idempotent; boot-time-only.
	BackfillMCPServerDefContentSHA256(ctx context.Context, signFn func(name string, def []byte) (string, error)) (int, error)

	// ---- v1.x RFC E ScheduleDef substrate ----
	//
	// Fourth substrate primitive after AgentDef + SkillDef + MCPServerDef.
	// Same shape (per-name lock, monotonic versioning, append-only
	// definition, active-pointer overlay, retire flag). The Definition
	// payload carries the schedule body (agent, cron, user_id,
	// user_credentials, on_complete, etc.) — see
	// internal/tools/builtin/scheduledef.go for the schema.
	//
	// Coexists with the static yaml `scheduled_runs:` block: yaml
	// entries stay boot-loaded as templates; per-user forks live in
	// the substrate with versioning + lineage. Name collisions with
	// yaml entries are refused at the tool layer (create), allowed
	// at the tool layer (fork against a yaml-defined template name).

	ScheduleDefCreate(ctx context.Context, row ScheduleDefRow) (ScheduleDefRow, error)
	ScheduleDefGet(ctx context.Context, defID string) (ScheduleDefRow, error)
	ScheduleDefGetByNameVersion(ctx context.Context, name string, version int) (ScheduleDefRow, error)
	ScheduleDefListByName(ctx context.Context, name string) ([]ScheduleDefRow, error)
	ScheduleDefListChildren(ctx context.Context, parentDefID string) ([]ScheduleDefRow, error)
	ScheduleDefListNames(ctx context.Context) ([]ScheduleDefNameSummary, error)
	// ScheduleDefSetActive UPSERTs the schedule_def_active pointer for
	// (tenantID, name). RFC N: per-tenant active pointer; a def can only be
	// promoted within its own tenant. tenantID "" = shared/operator/legacy.
	ScheduleDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// ScheduleDefGetActive returns the active row for (tenantID, name).
	// *ErrNotFound when no pointer exists. RFC N: tenantID "" = shared.
	ScheduleDefGetActive(ctx context.Context, tenantID, name string) (ScheduleDefRow, error)
	ScheduleDefSetRetired(ctx context.Context, defID string, retired bool) error

	// ---- v1.x RFC E ScheduleDef runtime (sweeper-side) ----
	//
	// schedule_run_state tracks per-def runtime state (last_run_id,
	// last_status, next_run_at, pause-until). One row per active def;
	// the scheduler seeds it when a def first becomes active and
	// updates it after each fire. ON DELETE CASCADE on the FK to
	// schedule_defs means retiring a def via DELETE auto-cleans state;
	// retired-via-flag rows keep their state but are filtered out by
	// ScheduleRunStateListDue's JOIN against the active pointer.

	// ScheduleRunStateSeed creates the state row for a def_id with the
	// provided next_run_at. Idempotent: if the row already exists,
	// updates next_run_at only (preserves last_*). Used when a new
	// def is promoted to active.
	ScheduleRunStateSeed(ctx context.Context, defID string, nextRunAt time.Time) error

	// ScheduleRunStateGet fetches one row. Returns ErrNotFound if no
	// state has been seeded for the def_id.
	ScheduleRunStateGet(ctx context.Context, defID string) (ScheduleRunStateRow, error)

	// ScheduleRunStateListDue returns the def_id + ScheduleDefRow of
	// every schedule whose next_run_at <= now AND def_id is the
	// active pointer for its name AND not retired AND not paused. The
	// JOIN happens store-side so the sweeper sees a single coherent
	// snapshot of "what should fire now." Empty slice = nothing due.
	ScheduleRunStateListDue(ctx context.Context, now time.Time) ([]ScheduleDueRow, error)

	// ScheduleRunStateRecordResult writes the outcome of a single
	// firing: last_run_id, last_status, last_error, last_run_at=now,
	// next_run_at advanced to the supplied value. Atomic.
	ScheduleRunStateRecordResult(ctx context.Context, in ScheduleRunResult) error

	// ---- v1.x RFC G A2A substrate (server + client sides) ----
	//
	// Two content-addressed Defs added for A2A protocol integration.
	// Both mirror ScheduleDef's store shape EXACTLY (per-name lock,
	// monotonic versioning, append-only definition, active-pointer
	// overlay, retire flag) — minus the sweeper-specific run_state
	// table, which is scheduler-only. The Definition payload schema is
	// owned by the tool layer (internal/tools/builtin); the store
	// treats it as opaque json.RawMessage.
	//
	// A2AServerCardDef declares which loomcycle agents are exposed via
	// A2A + the AgentCard metadata. A2AAgentDef declares a remote A2A
	// peer that loomcycle agents can call as a tool.

	A2AServerCardDefCreate(ctx context.Context, row A2AServerCardDefRow) (A2AServerCardDefRow, error)
	A2AServerCardDefGet(ctx context.Context, defID string) (A2AServerCardDefRow, error)
	A2AServerCardDefGetByNameVersion(ctx context.Context, name string, version int) (A2AServerCardDefRow, error)
	A2AServerCardDefListByName(ctx context.Context, name string) ([]A2AServerCardDefRow, error)
	A2AServerCardDefListChildren(ctx context.Context, parentDefID string) ([]A2AServerCardDefRow, error)
	A2AServerCardDefListNames(ctx context.Context) ([]A2AServerCardDefNameSummary, error)
	// A2AServerCardDefSetActive UPSERTs the a2a_server_card_def_active
	// pointer for (tenantID, name). RFC N: per-tenant active pointer; a def
	// can only be promoted within its own tenant. tenantID "" = shared/
	// operator/legacy (the tenant the operator-configured server surface
	// resolves under at boot).
	A2AServerCardDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// A2AServerCardDefGetActive returns the active row for (tenantID, name).
	// *ErrNotFound when no pointer exists. RFC N: tenantID "" = shared.
	A2AServerCardDefGetActive(ctx context.Context, tenantID, name string) (A2AServerCardDefRow, error)
	A2AServerCardDefSetRetired(ctx context.Context, defID string, retired bool) error

	A2AAgentDefCreate(ctx context.Context, row A2AAgentDefRow) (A2AAgentDefRow, error)
	A2AAgentDefGet(ctx context.Context, defID string) (A2AAgentDefRow, error)
	A2AAgentDefGetByNameVersion(ctx context.Context, name string, version int) (A2AAgentDefRow, error)
	A2AAgentDefListByName(ctx context.Context, name string) ([]A2AAgentDefRow, error)
	A2AAgentDefListChildren(ctx context.Context, parentDefID string) ([]A2AAgentDefRow, error)
	A2AAgentDefListNames(ctx context.Context) ([]A2AAgentDefNameSummary, error)
	// A2AAgentDefSetActive UPSERTs the a2a_agent_def_active pointer for
	// (tenantID, name). RFC N: per-tenant active pointer; a def can only be
	// promoted within its own tenant. tenantID "" = shared/operator/legacy.
	A2AAgentDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// A2AAgentDefGetActive returns the active row for (tenantID, name).
	// *ErrNotFound when no pointer exists. RFC N: tenantID "" = shared.
	A2AAgentDefGetActive(ctx context.Context, tenantID, name string) (A2AAgentDefRow, error)
	A2AAgentDefSetRetired(ctx context.Context, defID string, retired bool) error

	// WebhookDef is the v1.x RFC H Input Webhooks substrate — same
	// content-addressed identity + lineage + promotion shape as
	// A2AAgentDef, minus the sweeper run_state table. A WebhookDef
	// declares an inbound HTTP webhook endpoint (auth, rate limit,
	// delivery target, payload mapping, on_complete hooks); the
	// Definition payload schema is owned by the tool layer.
	WebhookDefCreate(ctx context.Context, row WebhookDefRow) (WebhookDefRow, error)
	WebhookDefGet(ctx context.Context, defID string) (WebhookDefRow, error)
	WebhookDefGetByNameVersion(ctx context.Context, name string, version int) (WebhookDefRow, error)
	WebhookDefListByName(ctx context.Context, name string) ([]WebhookDefRow, error)
	WebhookDefListChildren(ctx context.Context, parentDefID string) ([]WebhookDefRow, error)
	WebhookDefListNames(ctx context.Context) ([]WebhookDefNameSummary, error)
	// WebhookDefSetActive UPSERTs the webhook_def_active pointer for
	// (tenantID, name). RFC N: per-tenant active pointer; a def can only be
	// promoted within its own tenant. tenantID "" = shared/operator/legacy
	// (reachable via the bare-root inbound route POST /v1/_webhooks/{name};
	// per-tenant webhooks ride POST /v1/_webhooks/{tenant}/{name}).
	WebhookDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// WebhookDefGetActive returns the active row for (tenantID, name).
	// *ErrNotFound when no pointer exists. RFC N: tenantID "" = shared.
	WebhookDefGetActive(ctx context.Context, tenantID, name string) (WebhookDefRow, error)
	WebhookDefSetRetired(ctx context.Context, defID string, retired bool) error

	// MemoryBackendDef is the v1.x RFC I MR-3a substrate — a faithful
	// mirror of WebhookDef (same content-addressed identity + lineage +
	// promotion shape, no sweeper run_state table). A MemoryBackendDef
	// declares a named memory backend (kind inprocess|mem9, connection
	// config, tenancy strategy, fallback); the Definition payload schema
	// is owned by the tool layer. Nothing consumes the Def yet — the
	// per-agent routing + factory land in MR-3b.
	MemoryBackendDefCreate(ctx context.Context, row MemoryBackendDefRow) (MemoryBackendDefRow, error)
	MemoryBackendDefGet(ctx context.Context, defID string) (MemoryBackendDefRow, error)
	MemoryBackendDefGetByNameVersion(ctx context.Context, name string, version int) (MemoryBackendDefRow, error)
	MemoryBackendDefListByName(ctx context.Context, name string) ([]MemoryBackendDefRow, error)
	MemoryBackendDefListChildren(ctx context.Context, parentDefID string) ([]MemoryBackendDefRow, error)
	MemoryBackendDefListNames(ctx context.Context) ([]MemoryBackendDefNameSummary, error)
	// MemoryBackendDefSetActive UPSERTs the memory_backend_def_active
	// pointer for (tenantID, name). RFC N: the active pointer is per-tenant,
	// and a def can only be promoted within its own tenant — implementations
	// refuse if the def's tenant_id ≠ tenantID. tenantID "" = the shared/
	// operator/legacy tenant.
	MemoryBackendDefSetActive(ctx context.Context, tenantID, name, defID, promotedByAgentID string) error
	// MemoryBackendDefGetActive returns the active row for (tenantID, name).
	// Returns *ErrNotFound when no active pointer exists. RFC N: tenantID
	// "" = the shared/operator/legacy tenant.
	MemoryBackendDefGetActive(ctx context.Context, tenantID, name string) (MemoryBackendDefRow, error)
	MemoryBackendDefSetRetired(ctx context.Context, defID string, retired bool) error

	// ---- OperatorTokenDef (RFC L OSS multi-tenant authorization) ----
	//
	// Bearer tokens bound to an authoritative principal (tenant_id +
	// subject + allowed_scopes). NOT a versioned/forkable substrate Def:
	// no version, no active pointer, no parent — rotation is recorded via
	// rotated_from and validity via retired_at. The token plaintext is
	// never stored; only token_hash = SHA-256(pepper‖token).
	OperatorTokenDefCreate(ctx context.Context, row OperatorTokenDefRow) (OperatorTokenDefRow, error)
	OperatorTokenDefGet(ctx context.Context, defID string) (OperatorTokenDefRow, error)
	// OperatorTokenDefGetByTokenHash is the auth hot path: a single
	// indexed lookup. Returns ErrNotFound when no row matches. Validity
	// (retired_at vs now) is decided by the caller (the auth layer),
	// keeping the rotation-grace logic testable in one place.
	OperatorTokenDefGetByTokenHash(ctx context.Context, tokenHash string) (OperatorTokenDefRow, error)
	// OperatorTokenDefGetCurrentByName returns the name's current
	// (retired_at IS NULL) token, or ErrNotFound. There is at most one.
	OperatorTokenDefGetCurrentByName(ctx context.Context, name string) (OperatorTokenDefRow, error)
	OperatorTokenDefListByName(ctx context.Context, name string) ([]OperatorTokenDefRow, error)
	OperatorTokenDefListNames(ctx context.Context) ([]OperatorTokenDefNameSummary, error)
	// OperatorTokenDefSetRetiredAt sets retired_at. Used by both retire
	// (now → immediate) and rotate (now+grace on the prior row).
	OperatorTokenDefSetRetiredAt(ctx context.Context, defID string, retiredAt time.Time) error
	// OperatorTokenDefCountActiveAdmin counts non-retired tokens whose
	// allowed_scopes include "substrate:admin" (the no-lockout guard).
	OperatorTokenDefCountActiveAdmin(ctx context.Context) (int, error)

	// ScheduleRunStatePause sets paused_until = until (or NULL if
	// until.IsZero()). Resume = call with zero time.
	ScheduleRunStatePause(ctx context.Context, defID string, until time.Time) error

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

	// ---- Metrics sampler (v0.8.x) -------------------------------------

	// MetricsWriteSample persists one process_samples row. The caller
	// pre-generates SampleID via MintSampleID. SampledAt must be set;
	// the store does not stamp time on its own (the sampler decides
	// when "now" is — important for unit tests with deterministic
	// clocks).
	MetricsWriteSample(ctx context.Context, s ProcessSample) error

	// MetricsSampleWindow returns samples whose sampled_at falls in
	// [since, until] (inclusive both ends). Returns up to `limit`
	// rows (≤ 0 → 200 default; cap 1000). Cursor is an opaque token
	// from a previous call's nextCursor (empty = from start of
	// window). Returns nextCursor empty when no more rows.
	MetricsSampleWindow(ctx context.Context, since, until time.Time, limit int, cursor string) (samples []ProcessSample, nextCursor string, err error)

	// MetricsRunSummary returns peak/mean RSS + max CPU% from
	// process_samples rows whose sampled_at overlaps the run's
	// [started_at, COALESCE(completed_at, now())] window. Returns
	// MetricsRunWindow with zero SampleCount + zero values when no
	// samples overlap (in-flight run with metrics disabled, or a
	// freshly-started run that hasn't ticked yet). *ErrNotFound when
	// the run_id itself doesn't exist.
	MetricsRunSummary(ctx context.Context, runID string) (MetricsRunWindow, error)

	// MetricsSweep deletes samples whose sampled_at < cutoff. Returns
	// the count deleted. Idempotent under concurrent sweepers.
	MetricsSweep(ctx context.Context, cutoff time.Time) (int, error)

	// DynamicAgentUpsert writes a dynamic agent row. RFC N: the
	// (tenant_id, name) tuple is the primary key — re-upserting the
	// same (tenant, name) overwrites the definition and resets
	// expires_at. agent.TenantID is set from the authoritative
	// principal at the write site ("" = shared/legacy tenant).
	// expiresAt is zero-valued to mean "no expiry" (operator must
	// explicitly DynamicAgentDelete). v0.8.15+.
	DynamicAgentUpsert(ctx context.Context, agent DynamicAgent) error

	// DynamicAgentGet reads one dynamic agent by (tenantID, name).
	// Returns *ErrNotFound for both "row missing" and "row expired" —
	// callers don't need to distinguish. Expired rows are filtered
	// server-side regardless of whether the sweeper has reaped them
	// yet. RFC N: tenantID "" = the shared/operator/legacy tenant.
	DynamicAgentGet(ctx context.Context, tenantID, name string) (DynamicAgent, error)

	// DynamicAgentList enumerates non-expired dynamic agents.
	// Capped at 200 rows ordered by created_at DESC.
	DynamicAgentList(ctx context.Context) ([]DynamicAgent, error)

	// DynamicAgentDelete removes one dynamic agent scoped to (tenantID,
	// name) — RFC N: a principal may only delete its own tenant's agent,
	// never another tenant's same-named row. Returns true when a row was
	// actually deleted, false when no (tenant, name) match existed (or it
	// had already expired). Both are non-error paths. tenantID "" = the
	// shared/operator/legacy tenant.
	DynamicAgentDelete(ctx context.Context, tenantID, name string) (bool, error)

	// DynamicAgentSweep deletes every dynamic_agents row whose
	// expires_at has passed. Returns the row count deleted. Safe to
	// run from a periodic goroutine; idempotent under concurrent
	// sweepers (single atomic DELETE).
	DynamicAgentSweep(ctx context.Context) (int, error)

	// ---- Interruption (v0.8.16) -------------------------------------
	//
	// Durable row that survives process restart + drives the listing
	// APIs. The tool-layer waiter (channels.Bus key) carries the
	// in-process wake; the row carries the state.
	//
	// kind is a closed enum owned by loomcycle. v0.8.16 writes only
	// "question". Future kinds (pause / wait_until / approval) are
	// additive enum values on the same column; the schema does not
	// need to change. See doc-internal/rfcs/interruption-tool.md §8.

	// InterruptCreate inserts a fresh interrupt row in status=pending.
	// The caller pre-generates row.InterruptID via MintInterruptID.
	// CreatedAt is set by the store. Returns the persisted ID. On
	// row.ExpiresAt zero-value, no expiry is recorded (NULL column).
	InterruptCreate(ctx context.Context, row InterruptRow) (string, error)

	// InterruptGet returns one row by interrupt_id. *ErrNotFound on
	// miss (Kind: "interrupt").
	InterruptGet(ctx context.Context, interruptID string) (InterruptRow, error)

	// InterruptResolve transitions a pending interrupt to status=
	// resolved with the supplied answer + answer_meta. Sets
	// resolved_at = now() server-side. Returns ErrInterruptAlreadyTerminal
	// when the row was already in a terminal state (resolved /
	// timed_out / cancelled) — the UPDATE is gated by status='pending'.
	// answerMeta may be nil; the column then writes SQL NULL.
	InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error

	// InterruptFinish transitions a pending interrupt to a terminal
	// status WITHOUT an answer (used for timeout sweeper + agent-side
	// cancel). status must be one of: "timed_out" / "cancelled".
	// resolvedBy is recorded for audit. Returns
	// ErrInterruptAlreadyTerminal on a non-pending row.
	InterruptFinish(ctx context.Context, interruptID, status, resolvedBy string) error

	// InterruptListByRun returns interrupts for the given run_id,
	// newest first. statusFilter is one of: ""="all",
	// "pending" / "resolved" / "timed_out" / "cancelled".
	// Capped at 200 rows.
	InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]InterruptRow, error)

	// InterruptListByUser returns interrupts owned by user_id,
	// newest first. statusFilter same shape as ListByRun. The
	// denormalised user_id column drives this query without a JOIN
	// against runs. Capped at 200 rows.
	InterruptListByUser(ctx context.Context, userID, statusFilter string) ([]InterruptRow, error)

	// InterruptCountPendingByRun returns the count of status=pending
	// interrupts for the given run_id. Drives max_pending enforcement
	// at the tool layer (the count check is a single round trip; the
	// subsequent InterruptCreate is a separate transaction — operators
	// SHOULD treat max_pending as advisory, not a hard concurrency
	// guard. See rfcs/interruption-tool.md §6).
	InterruptCountPendingByRun(ctx context.Context, runID string) (int, error)

	// InterruptSweepExpired marks every status=pending interrupt
	// whose expires_at < now as timed_out. Returns the count
	// transitioned. Safe to run from a periodic goroutine;
	// idempotent under concurrent sweepers (single atomic UPDATE).
	InterruptSweepExpired(ctx context.Context) (int, error)

	// ---- v0.11.5 runtime channel CRUD ----
	//
	// Runtime-declared channels live in the `channels` table; yaml-
	// declared channels stay in cfg.Channels (in-memory only). The
	// HTTP admin layer merges both at read time with a `source`
	// discriminator (mirrors v0.10.4's static-vs-dynamic posture
	// for agents/skills/mcp-servers). Mutations on yaml-declared
	// channel names are refused at the handler boundary BEFORE
	// reaching the store — so these methods only ever see runtime
	// names.

	// ChannelsList returns every runtime-declared channel. yaml-
	// declared channels are NOT returned; the merge with cfg.Channels
	// happens in the HTTP handler.
	ChannelsList(ctx context.Context) ([]ChannelRow, error)

	// ChannelGet returns one runtime-declared channel by name. Returns
	// *ErrNotFound{Kind:"channel"} when the name isn't in the runtime
	// table (yaml-declared channels are NOT here — the caller checks
	// cfg.Channels first). A point lookup so the hot publish/subscribe/
	// peek/ack declared-check doesn't scan the whole table per op, and
	// so a real store fault surfaces as an error instead of an empty
	// list that masquerades as "not declared" (exp7 I5).
	ChannelGet(ctx context.Context, name string) (ChannelRow, error)

	// ChannelsCreate inserts a new runtime channel. Returns
	// *ErrConflict{Kind:"channel"} when a runtime row with the
	// same name already exists. yaml-name collisions are caught
	// upstream at the handler boundary.
	ChannelsCreate(ctx context.Context, row ChannelRow) error

	// ChannelsUpdate patches mutable fields on a runtime channel
	// (description, default_ttl, max_messages, semantic). Returns
	// ErrNotFound when the name isn't in the runtime table.
	ChannelsUpdate(ctx context.Context, name string, patch ChannelPatch) error

	// ChannelsDelete removes a runtime channel + cascades deletion
	// of its persisted messages + cursors. Returns ErrNotFound when
	// the name isn't in the runtime table.
	ChannelsDelete(ctx context.Context, name string) error

	// ChannelPurge deletes all buffered messages for a channel WITHOUT
	// removing the channel definition or subscriber cursors — the
	// "drain the queue" operation that ChannelsDelete's full teardown
	// is too blunt for. Works on ANY name: yaml-declared channels have
	// no runtime `channels` row, but their messages live in the same
	// channel_messages table. Idempotent — purging a channel with no
	// messages returns (0, nil) rather than ErrNotFound; existence is
	// the caller's concern. Returns the number of messages deleted.
	ChannelPurge(ctx context.Context, name string) (int, error)

	// Close releases backend resources. Idempotent.
	Close() error
}

// ChannelRow is one runtime-declared channel persisted in the store.
// Mirrors the in-memory config.Channel struct field-for-field plus a
// CreatedAt timestamp for operator-visible audit. The `Source` field
// is set by the handler layer to "runtime" before returning to the
// caller; yaml-declared channels carry source="yaml" populated from
// cfg.Channels at merge time.
type ChannelRow struct {
	Name        string
	Description string
	Scope       string
	Semantic    string
	DefaultTTL  int
	MaxMessages int
	Publisher   string
	Period      string
	CreatedAt   time.Time
}

// ChannelPatch carries the subset of fields ChannelsUpdate can
// modify. Pointer fields so nil = "leave unchanged"; non-nil = set
// to the dereferenced value (including the zero value, e.g. setting
// MaxMessages back to 0).
type ChannelPatch struct {
	Description *string
	DefaultTTL  *int
	MaxMessages *int
	Semantic    *string
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

// MemoryEmbedding is the vector + metadata stored alongside a memory
// row. The wire format for Vector is float32 little-endian on the
// SQLite side; pgvector accepts its native `[1.0,2.0,...]` text
// representation. Provider + Model + Dimension are stored explicitly
// so dimension-mismatch checks at search time are O(1) — we don't
// need to inspect the vector itself to know its shape.
//
// CreatedAt is the embedding's own write time, independent of the
// base memory row's created_at / updated_at. A row that's been
// re-embedded twice has memory.updated_at < embedding.created_at.
type MemoryEmbedding struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Dimension int       `json:"dimension"`
	Vector    []float32 `json:"-"` // not JSON-serialised here; snapshot uses its own base64 path
	EmbedText string    `json:"embed_text"`
	CreatedAt time.Time `json:"created_at"`
}

// MemorySearchEntry is one result row of MemoryEmbedSearch. It
// embeds the base memory entry plus the similarity score and the
// (provider, model) that produced the stored embedding — the latter
// lets a caller spot rows embedded under an older model without a
// separate query.
//
// Score is cosine similarity in [0, 1] (higher = closer). Backends
// convert from their native distance function before returning.
//
// Vector is the entry's stored embedding, populated by MemoryEmbedSearch
// for client-side search-time dedup (RFC I MR-5 / Decision 2). It is
// json:"-" — never serialized to the agent, exactly like
// MemoryEmbedding.Vector; it exists only so the dedup pass can compute
// pairwise cosine distances without a second round-trip. It is EMPTY when
// the backend can't supply it (e.g. the Mem9 REST backend, which embeds +
// scores server-side and returns no vectors); dedup then degrades to a
// no-op for that entry (an empty-Vector entry is never treated as a
// duplicate, so it is kept).
type MemorySearchEntry struct {
	MemoryEntry
	Score        float64 `json:"score"`
	EmbeddedWith struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"embedded_with"`
	Vector []float32 `json:"-"`
}

// MemoryEmbedStats summarises the embedded rows under one scope.
// Drives the v0.9.0 admin endpoint `/v1/_memory/embed_stats`. The
// per-model row_count + dimension lets operators see at a glance
// when they have rows under multiple embedders (the dimension-
// mismatch + reembed migration cue).
type MemoryEmbedStats struct {
	Scope               MemoryScope             `json:"scope"`
	Models              []MemoryEmbedModelStats `json:"models"`
	TotalEmbeddingBytes int64                   `json:"total_embedding_bytes"`
}

// MemoryEmbedModelStats is one row inside MemoryEmbedStats.Models.
type MemoryEmbedModelStats struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
	RowCount  int    `json:"row_count"`
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

// ErrVectorUnsupported is returned by every MemoryEmbed* method when
// the backend was built or configured without vector index support.
// v0.9.0: SQLite returns this for every Memory.search call;
// Postgres returns this when LOOMCYCLE_PGVECTOR_ENABLED is unset.
// The Memory tool layer surfaces this as a tool-result error so
// agents see a clear "vectors not configured" message rather than
// a runtime exception.
var ErrVectorUnsupported = &MemoryError{Code: "vector_unsupported", Msg: "memory: vector index not configured (set LOOMCYCLE_PGVECTOR_ENABLED=1 on Postgres; SQLite vector backend ships in v0.9.1)"}

// ErrDimensionMismatch is returned by MemoryEmbedSearch when the
// query vector's dimension doesn't match the stored rows' dimension.
// The error's Msg includes both dimensions so the operator can spot
// the model swap that caused it; the admin reembed endpoint is the
// migration path.
var ErrDimensionMismatch = &MemoryError{Code: "dimension_mismatch", Msg: "memory: query embedding dimension does not match stored rows — run /v1/_memory/reembed to migrate"}

// ErrEmbedderNotConfigured is returned by the Memory tool's `search`
// op + `set` with `embed: true` when no `memory.embedder:` block was
// provided in the operator yaml. The Store itself doesn't raise this
// error — the tool layer does, since the Store doesn't know about
// the embedder. Defined here for code-locality with the rest of the
// MemoryError set so callers can switch on a single error family.
var ErrEmbedderNotConfigured = &MemoryError{Code: "embedder_not_configured", Msg: "memory: no embedder configured — set memory.embedder in operator yaml"}

// ErrCapabilityUnsupported is returned by the Memory tool's `add` / `recall`
// ops (RFC K) when the agent's resolved memory backend does not implement
// the MemoryLayer capability — e.g. the default in-process KV+vector backend,
// which is not an LLM-extract memory layer. Mirrors the vector_unsupported
// fail-closed posture: the agent sees a clear "this backend isn't a memory
// layer" message rather than a silent no-op. The Store doesn't raise this —
// the tool layer does, after probing the backend's Capabilities.
var ErrCapabilityUnsupported = &MemoryError{Code: "capability_unsupported", Msg: "memory: add/recall require a memory-layer backend (memory_backend with a MemoryLayer-capable kind, e.g. mem9); the default in-process backend is a key/value+vector store, not a memory layer"}

// ErrEmbedderNotImplemented is returned by an embedder driver that
// is registered but not functionally implemented. v0.9.0–v0.10.1
// shipped this for the Anthropic stub; v0.10.2 made the Anthropic
// slot a working Voyage AI proxy so this error is now reserved for
// any future placeholder drivers. Tool-layer error, like
// ErrEmbedderNotConfigured.
var ErrEmbedderNotImplemented = &MemoryError{Code: "embedder_not_implemented", Msg: "memory: this embedder is not implemented in this build"}

// MemoryError is a typed error so the Memory tool can surface a
// stable error code to the agent. The Code is wire-stable; the Msg
// is human-readable and may evolve.
type MemoryError struct {
	Code string
	Msg  string
}

func (e *MemoryError) Error() string { return e.Msg }

// Is implements errors.Is comparison by Code so backend
// implementations that construct a NEW *MemoryError with the same
// Code (e.g. the postgres adapter formatting a dimension-mismatch
// message with the concrete dims) still match the package-level
// sentinel via `errors.Is(err, ErrDimensionMismatch)`. Without this,
// errors.Is would only match by pointer identity — the sentinel
// would never be reached and callers would silently fall through.
func (e *MemoryError) Is(target error) bool {
	var t *MemoryError
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// ChannelMessage is one row in the channel_messages table. ID is a
// ULID assigned by the store at publish time (sortable by publish
// instant — gives "oldest first" reads for free). ExpiresAt is zero
// when the publisher passed no TTL AND the channel had no default;
// the read path filters expired rows regardless of whether the
// sweeper has run.
//
// v0.8.6 fields:
//   - VisibleAt — when this message becomes deliverable. Equals
//     PublishedAt for immediate publishes; set to deliver_at for
//     deferred publishes. Subscribe/peek read paths filter
//     `visible_at <= now()`. Delivery order = (VisibleAt, ID)
//     tuple, NOT pure ID order, so deferred messages don't get
//     silently skipped by subscribers that already progressed past
//     their publish-time ID.
//   - PublishedByUserID — audit column. Agent publishes set this
//     from the run's UserID; system publishes use the "_system"
//     sentinel; admin-endpoint publishes use the bearer's user.
type ChannelMessage struct {
	ID                string          `json:"id"`
	Channel           string          `json:"channel"`
	Scope             MemoryScope     `json:"scope"` // re-uses MemoryScope so operators don't track two enums
	ScopeID           string          `json:"scope_id"`
	Payload           json.RawMessage `json:"payload"`
	PublishedAt       time.Time       `json:"published_at"`
	ExpiresAt         time.Time       `json:"expires_at,omitempty"`
	VisibleAt         time.Time       `json:"visible_at,omitempty"`
	PublishedByUserID string          `json:"published_by_user_id,omitempty"`
}

// ChannelStats is one row in the result of ChannelStats — the
// aggregate over channel_messages for a single channel name. Expired
// rows are excluded from MessageCount + the visible_at bounds so the
// admin listing reflects what subscribers would actually receive.
type ChannelStats struct {
	Channel         string    `json:"channel"`
	MessageCount    int64     `json:"message_count"`
	OldestVisibleAt time.Time `json:"oldest_visible_at,omitempty"`
	NewestVisibleAt time.Time `json:"newest_visible_at,omitempty"`
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

// ---- v0.8.x Process-resource metrics sampler types ----

// MintSampleID returns a fresh process_samples row id. Format:
// "smp_<16-hex unixNanos><8-hex rand>" — sortable lexicographically
// by sample time within the resolution of a single nanosecond.
// Mirrors MintChannelMessageID; same trade-offs documented there.
func MintSampleID(t time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("smp_%016x%s", uint64(t.UnixNano()), hex.EncodeToString(buf[:]))
}

// ProcessSample is one row in the process_samples table. Captured
// by the metrics sampler when at least one agent run is active.
// Linux-only fields (RSS, CPU%) are 0 on non-Linux platforms; the
// sampler's build-tag-split readers handle the gating.
//
// System-wide fields are pointer-typed because they may be NULL —
// they're only populated when LOOMCYCLE_METRICS_COLLECT_SYSTEM=1
// AND the platform is Linux.
type ProcessSample struct {
	SampleID             string    `json:"sample_id"`  // "smp_<16hex><8hex>"
	ReplicaID            string    `json:"replica_id"` // "" in single-replica mode; set from LOOMCYCLE_REPLICA_ID so a shared process_samples table can be split per replica
	SampledAt            time.Time `json:"sampled_at"`
	ActiveRuns           int       `json:"active_runs"`
	QueuedRuns           int       `json:"queued_runs"`
	LoomcycleRSSBytes    int64     `json:"loomcycle_rss_bytes"` // 0 on non-Linux
	LoomcycleHeapAlloc   int64     `json:"loomcycle_heap_alloc_bytes"`
	LoomcycleHeapInuse   int64     `json:"loomcycle_heap_inuse_bytes"`
	LoomcycleGoroutines  int       `json:"loomcycle_num_goroutines"`
	LoomcycleCPUPctX100  int       `json:"loomcycle_cpu_pct_x100"` // 0 on non-Linux; %×100
	SystemCPUPctX100     *int      `json:"system_cpu_pct_x100,omitempty"`
	SystemMemUsedMB      *int      `json:"system_mem_used_mb,omitempty"`
	SystemMemAvailableMB *int      `json:"system_mem_available_mb,omitempty"`
}

// MetricsRunWindow is the result of MetricsRunSummary — peak/mean
// RSS + max CPU% from process_samples whose sampled_at overlaps the
// run's lifetime window. SampleCount=0 means no overlapping samples
// (in-flight run with no ticks yet, or metrics disabled when the
// run executed).
type MetricsRunWindow struct {
	RunID         string    `json:"run_id"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"` // zero when in-flight
	SampleCount   int       `json:"sample_count"`
	PeakRSSBytes  int64     `json:"peak_rss_bytes"`
	MeanRSSBytes  int64     `json:"mean_rss_bytes"`
	MaxCPUPctX100 int       `json:"max_cpu_pct_x100"`
}

// DynamicAgent is one row in the dynamic_agents table. Holds the JSON-
// encoded AgentDef body verbatim (the store doesn't depend on
// internal/config — dep direction would invert; the v0.8.5 AgentDefRow
// uses the same pattern). v0.8.15 LoomCycle MCP adds runtime
// registration via `mcp__loomcycle__register_agent`.
//
// ExpiresAt is zero when the agent has no TTL (operator must
// explicitly unregister); non-zero rows are filtered by
// DynamicAgentGet / DynamicAgentList when expires_at < now().
type DynamicAgent struct {
	Name        string    `json:"name"`       // part of the PK; charset [A-Za-z0-9_-]{1,64}
	Definition  []byte    `json:"definition"` // JSON-encoded config.AgentDef body
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"` // zero = no expiry
	Description string    `json:"description,omitempty"`
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. With (tenant_id, name) as the PK, two
	// tenants register the same name independently. Set from the
	// authoritative principal at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

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
	// ContentSHA256 is the v0.9.x deterministic content-hash of the
	// agent's content-bearing fields, computed by internal/agents.Sign.
	// "sha256:" + 64 hex chars; empty when the row pre-dates the
	// content-signature migration and hasn't been backfilled yet.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. The UNIQUE constraint is (tenant_id, name,
	// version), so two tenants own the same name+version independently.
	// Deliberately NOT part of the content hash — tenant is operational
	// identity, not content (same rule RetryAttempts/RunTimeoutSeconds
	// follow), so two tenants forking the same body get the same
	// content_sha256. Set from the authoritative principal at the write
	// site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// AgentDefNameSummary is one entry of AgentDefListNames' output.
// count is the version count; ActiveDefID is the agent_def_active
// pointer (empty when no row is promoted under this name).
type AgentDefNameSummary struct {
	Name string `json:"name"`
	// TenantID is the RFC N owning tenant. A name owned by N tenants
	// yields N summary rows (one per tenant) — without grouping by tenant
	// the listing would merge distinct tenants' versions under one name.
	TenantID      string    `json:"tenant_id,omitempty"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// LiveVersionCount is VersionCount minus retired rows (additive; 0 for
	// pre-feature callers). A name whose LiveVersionCount is 0 (every
	// version retired) is "inactive" — the UI badges it and lets the
	// operator reclaim the name with a fresh create.
	LiveVersionCount int `json:"live_version_count"`
	// ActiveRetired is true when ActiveDefID points at a retired row — a
	// corrupt-legacy state (pre the retire-clears-active fix) the UI flags
	// until the next retire/promote heals it. Normally false: the
	// retire-of-active path now clears the pointer.
	ActiveRetired bool `json:"active_retired,omitempty"`
}

// ---- v0.8.22 SkillDef substrate types ----
//
// Mirror of AgentDef* — same identity / lineage / provenance
// semantics, but the Definition payload is a skill body + metadata
// instead of an agent body. See internal/tools/builtin/skilldef.go
// for the JSON shape (body / description / allowed_tools).
//
// Identity, lineage, and provenance fields carry identical
// invariants to AgentDefRow. See the AgentDefRow doc for full
// detail — the comments below only call out skill-specific quirks.
type SkillDefRow struct {
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
	// ContentSHA256 — see AgentDefRow.ContentSHA256. Same semantics.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. The UNIQUE constraint is (tenant_id, name,
	// version), so two tenants own the same name+version independently.
	// Deliberately NOT part of the content hash — tenant is operational
	// identity, not content (same rule AgentDefRow follows), so two
	// tenants forking the same body get the same content_sha256. Set from
	// the authoritative principal at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// SkillDefNameSummary mirrors AgentDefNameSummary.
type SkillDefNameSummary struct {
	Name string `json:"name"`
	// TenantID is the RFC N owning tenant. A name owned by N tenants
	// yields N summary rows (one per tenant) — without grouping by tenant
	// the listing would merge distinct tenants' versions under one name.
	TenantID      string    `json:"tenant_id,omitempty"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
}

// SkillDefActiveEntry mirrors AgentDefActiveEntry. Pairs a skill
// name with the def_id currently promoted to active.
type SkillDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// skill_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// ---- v0.9.x MCPServerDef substrate types ----
//
// Mirror of AgentDef* / SkillDef* with the same identity / lineage /
// provenance semantics. The Definition payload is an MCP server's
// connection metadata + the cached discovered tools (see
// internal/tools/builtin/mcpserverdef.go for the JSON shape:
// transport / url / headers / discovered_tools).
type MCPServerDefRow struct {
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
	// ContentSHA256 — see AgentDefRow.ContentSHA256.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. The UNIQUE constraint is (tenant_id, name,
	// version), so two tenants own the same name+version independently.
	// Deliberately NOT part of the content hash — tenant is operational
	// identity, not content — so two tenants registering the same body
	// get the same content_sha256. Set from the authoritative principal
	// at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// MCPServerDefNameSummary mirrors AgentDefNameSummary.
type MCPServerDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N tenant axis the name belongs to. "" = the
	// shared/operator/legacy tenant. The boot rehydrator + advertising
	// filter key the per-name GetActive call on this so a run only ever
	// sees its own + shared MCP servers.
	TenantID string `json:"tenant_id,omitempty"`
}

// MCPServerDefActiveEntry mirrors AgentDefActiveEntry / SkillDefActiveEntry.
type MCPServerDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// mcp_server_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// ScheduleDefRow mirrors AgentDefRow / SkillDefRow / MCPServerDefRow
// — same identity + lineage + retire flag shape. The Definition
// payload carries the JSON-encoded schedule body (cron expression,
// agent name, user_id, user_credentials map, on_complete hooks).
// v1.x RFC E.
type ScheduleDefRow struct {
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
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. UNIQUE(tenant_id, name, version). This is the
	// def's OWNING tenant — distinct from the run-execution tenant carried
	// inside the schedule's `definition` JSON (the tenant the fired run
	// executes as). Set from the authoritative principal at the write site.
	TenantID string `json:"tenant_id,omitempty"`
}

// ScheduleDefNameSummary mirrors the AgentDef equivalent.
type ScheduleDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N owning tenant. "" = the shared/operator/legacy
	// tenant. A name owned by N tenants yields N summary rows.
	TenantID string `json:"tenant_id,omitempty"`
}

// ScheduleDefActiveEntry mirrors the AgentDef equivalent.
type ScheduleDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// schedule_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AServerCardDefRow mirrors ScheduleDefRow — same identity +
// lineage + retire flag shape. The Definition payload carries the
// JSON-encoded server-card body (exposed agents, AgentCard metadata,
// security schemes); the schema is owned by the tool layer. v1.x RFC G.
type A2AServerCardDefRow struct {
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
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. UNIQUE(tenant_id, name, version). Set from the
	// authoritative principal at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AServerCardDefNameSummary mirrors ScheduleDefNameSummary.
type A2AServerCardDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N owning tenant. "" = the shared/operator/legacy
	// tenant. A name owned by N tenants yields N summary rows.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AServerCardDefActiveEntry mirrors ScheduleDefActiveEntry.
type A2AServerCardDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// a2a_server_card_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AAgentDefRow mirrors ScheduleDefRow — same identity + lineage +
// retire flag shape. The Definition payload carries the JSON-encoded
// remote-peer body (agent_card_url or endpoint+binding, auth scheme +
// credential_ref, expected_skills manifest); the schema is owned by
// the tool layer. v1.x RFC G.
type A2AAgentDefRow struct {
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
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. UNIQUE(tenant_id, name, version). Set from
	// the authoritative principal at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AAgentDefNameSummary mirrors ScheduleDefNameSummary.
type A2AAgentDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N owning tenant. "" = the shared/operator/legacy
	// tenant. A name owned by N tenants yields N summary rows.
	TenantID string `json:"tenant_id,omitempty"`
}

// A2AAgentDefActiveEntry mirrors ScheduleDefActiveEntry.
type A2AAgentDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// a2a_agent_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// WebhookDefRow mirrors A2AAgentDefRow — same identity + lineage +
// retire flag shape. The Definition payload carries the JSON-encoded
// inbound-webhook body (delivery target, auth scheme + signing-secret
// ref, rate limit, payload mapping, on_complete hooks); the schema is
// owned by the tool layer. v1.x RFC H.
type WebhookDefRow struct {
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
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. UNIQUE(tenant_id, name, version). Set from the
	// authoritative principal at the write site; never from the wire.
	TenantID string `json:"tenant_id,omitempty"`
}

// WebhookDefNameSummary mirrors A2AAgentDefNameSummary.
type WebhookDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N owning tenant. "" = the shared/operator/legacy
	// tenant. A name owned by N tenants yields N summary rows.
	TenantID string `json:"tenant_id,omitempty"`
}

// WebhookDefActiveEntry mirrors A2AAgentDefActiveEntry.
type WebhookDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// webhook_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// MemoryBackendDefRow mirrors WebhookDefRow — same identity + lineage +
// retire flag shape. The Definition payload carries the JSON-encoded
// memory-backend body (kind, connection config, tenancy strategy,
// fallback); the schema is owned by the tool layer. RFC I MR-3a /
// mirrors WebhookDef.
type MemoryBackendDefRow struct {
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
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. The UNIQUE constraint is (tenant_id, name,
	// version), so two tenants own the same name+version independently. Set
	// from the authoritative principal at the write site; never from the
	// wire. (MemoryBackendDef has no content hash, so there is no
	// content-hash exclusion concern — unlike AgentDefRow.)
	TenantID string `json:"tenant_id,omitempty"`
}

// MemoryBackendDefNameSummary mirrors WebhookDefNameSummary.
type MemoryBackendDefNameSummary struct {
	Name          string    `json:"name"`
	VersionCount  int       `json:"version_count"`
	ActiveDefID   string    `json:"active_def_id,omitempty"`
	LatestVersion int       `json:"latest_version"`
	LastUpdated   time.Time `json:"last_updated"`
	// TenantID is the RFC N owning tenant. A name owned by N tenants yields
	// N summary rows (one per tenant). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// OperatorTokenDefRow is one auth-token row (RFC L). The token plaintext
// is NEVER stored — only TokenHash = SHA-256(pepper‖token). AllowedScopes
// is persisted as a JSON array. RotatedFrom links a rotated token to its
// predecessor; RetiredAt (zero = never) gates validity (valid iff zero or
// now < RetiredAt).
type OperatorTokenDefRow struct {
	DefID            string    `json:"def_id"`
	Name             string    `json:"name"`
	TenantID         string    `json:"tenant_id"`
	Subject          string    `json:"subject"`
	TokenHash        string    `json:"-"` // never serialised to wire/log
	AllowedScopes    []string  `json:"allowed_scopes"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedByAgentID string    `json:"created_by_agent_id,omitempty"`
	CreatedByRunID   string    `json:"created_by_run_id,omitempty"`
	RotatedFrom      string    `json:"rotated_from,omitempty"`
	RetiredAt        time.Time `json:"retired_at,omitempty"`
}

// OperatorTokenDefNameSummary is one row of the names listing — no
// secret material, suitable for GET /v1/_operatortokendef/names.
type OperatorTokenDefNameSummary struct {
	Name        string    `json:"name"`
	TenantID    string    `json:"tenant_id"`
	Subject     string    `json:"subject"`
	TokenCount  int       `json:"token_count"`  // including rotated/retired history
	HasCurrent  bool      `json:"has_current"`  // a non-retired token exists
	LastUpdated time.Time `json:"last_updated"` // newest created_at for the name
}

// MemoryBackendDefActiveEntry mirrors WebhookDefActiveEntry.
type MemoryBackendDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// memory_backend_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// ScheduleRunStateRow is one row in schedule_run_state — the
// sweeper's runtime view of a def. Seeded when a def becomes
// active; updated after each fire.
type ScheduleRunStateRow struct {
	DefID       string    `json:"def_id"`
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	LastRunID   string    `json:"last_run_id,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	NextRunAt   time.Time `json:"next_run_at"`
	PausedUntil time.Time `json:"paused_until,omitempty"`
	// FireCount is the lifetime count of fires recorded for this def
	// (RFC S / F36). RecordResult increments it when CountAsFire is set
	// (every real fire; NOT the disabled-skip advance). The scheduler
	// reads it after a fire to enforce ScheduledRun.MaxFires.
	FireCount int `json:"fire_count,omitempty"`
}

// ScheduleDueRow is the JOIN result returned by ScheduleRunStateListDue.
// The sweeper iterates these to fire each due schedule. Definition is
// the raw JSON body — the scheduler unmarshals it into its own
// merged-def shape (avoiding a cross-package dep on internal/tools/
// builtin's mergedScheduleDef).
type ScheduleDueRow struct {
	DefID      string          `json:"def_id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	NextRunAt  time.Time       `json:"next_run_at"`
}

// ScheduleRunResult is the input to ScheduleRunStateRecordResult.
// Bundled into a struct so the contract stays stable as we add
// fields (e.g. duration_ms in v1.1).
type ScheduleRunResult struct {
	DefID      string
	LastRunID  string
	LastStatus string // "completed" | "failed" | "cancelled" | "skipped"
	LastError  string
	LastRunAt  time.Time
	NextRunAt  time.Time
	// CountAsFire increments fire_count by one when true (RFC S / F36).
	// The scheduler sets it on every real fire (any status); the
	// disabled-skip advance (advanceOnly) leaves it false so a disabled
	// schedule doesn't consume its max_fires budget.
	CountAsFire bool
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

// ErrSkillDefParentNotFound mirrors ErrAgentDefParentNotFound for
// the SkillDef substrate.
var ErrSkillDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "skill_def: parent_def_id does not exist"}

// ErrMCPServerDefParentNotFound mirrors the AgentDef + SkillDef
// pattern for the v0.9.x MCPServerDef substrate.
var ErrMCPServerDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "mcp_server_def: parent_def_id does not exist"}

// ErrScheduleDefParentNotFound mirrors the AgentDef + SkillDef +
// MCPServerDef pattern for the v1.x RFC E ScheduleDef substrate.
var ErrScheduleDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "schedule_def: parent_def_id does not exist"}

// ErrA2AServerCardDefParentNotFound mirrors the ScheduleDef pattern
// for the v1.x RFC G A2AServerCardDef substrate.
var ErrA2AServerCardDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "a2a_server_card_def: parent_def_id does not exist"}

// ErrA2AAgentDefParentNotFound mirrors the ScheduleDef pattern for
// the v1.x RFC G A2AAgentDef substrate.
var ErrA2AAgentDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "a2a_agent_def: parent_def_id does not exist"}

// ErrWebhookDefParentNotFound mirrors the A2AAgentDef pattern for the
// v1.x RFC H WebhookDef substrate.
var ErrWebhookDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "webhook_def: parent_def_id does not exist"}

// ErrMemoryBackendDefParentNotFound mirrors the WebhookDef pattern for
// the v1.x RFC I MR-3a MemoryBackendDef substrate.
var ErrMemoryBackendDefParentNotFound = &SubstrateError{Code: "parent_not_found", Msg: "memory_backend_def: parent_def_id does not exist"}

// ErrAgentDefImmutable is returned by store-layer assertions if
// someone tries to UPDATE an agent_defs row's definition column.
// Append-only invariant. The adapter's contract test pins this.
var ErrAgentDefImmutable = &SubstrateError{Code: "immutable", Msg: "agent_def: rows are append-only; create a new version"}

// ---- Interruption (v0.8.16) -----------------------------------------

// Interrupt kind / status / resolved-by enum values. v0.8.16 only
// uses kind=question; future values land here as additive enum
// extensions.
const (
	InterruptKindQuestion = "question"

	InterruptStatusPending   = "pending"
	InterruptStatusResolved  = "resolved"
	InterruptStatusTimedOut  = "timed_out"
	InterruptStatusCancelled = "cancelled"

	InterruptPriorityLow    = "low"
	InterruptPriorityNormal = "normal"
	InterruptPriorityHigh   = "high"

	// ResolvedBy attribution values. The set is open at the type
	// level (TEXT column) but semantically closed — these are the
	// values loomcycle itself writes. External admin tooling may
	// invent its own (e.g. "claude_code") and that's allowed.
	InterruptResolvedByWebUI       = "webui"
	InterruptResolvedByMCP         = "mcp"
	InterruptResolvedByCLI         = "cli"
	InterruptResolvedByAPI         = "api"
	InterruptResolvedByTimeout     = "timeout"
	InterruptResolvedByAgentCancel = "agent_cancel"
)

// MintInterruptID returns a fresh interrupt_id that's monotonic-by-
// create-time AND globally unique. Format:
// "intr_<16-hex unixNanos><8-hex rand>" — 24 hex chars after the
// prefix. Mirrors MintChannelMessageID / MintSampleID; same lex-
// sortable invariant through year 2262.
func MintInterruptID(t time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("intr_%016x%s", uint64(t.UnixNano()), hex.EncodeToString(buf[:]))
}

// InterruptRow is one row in the `interrupts` table. Caller supplies
// InterruptID + RunID + Kind + (kind-specific fields); the store
// fills CreatedAt and (on resolve / finish) ResolvedAt + ResolvedBy.
//
// user_id / agent_id / agent_name are denormalised at create time
// from the run identity so listing queries don't need a JOIN. The
// caller MUST stamp them — the store never JOINs.
//
// Options and AnswerMeta are JSON-encoded blobs. For kind=question,
// Options is a JSON array of strings (NULL = free-text). AnswerMeta
// is kind-discriminated extra resolve data (NULL for v0.8.16
// question — the scalar Answer field carries everything).
type InterruptRow struct {
	InterruptID string          `json:"interrupt_id"`
	RunID       string          `json:"run_id"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	Question    string          `json:"question,omitempty"`
	Options     json.RawMessage `json:"options,omitempty"`
	ContextData string          `json:"context_data,omitempty"`
	Priority    string          `json:"priority"`
	Answer      string          `json:"answer,omitempty"`
	AnswerMeta  json.RawMessage `json:"answer_meta,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at,omitempty"` // zero = no expiry
	ResolvedAt  time.Time       `json:"resolved_at,omitempty"`
	ResolvedBy  string          `json:"resolved_by,omitempty"`
	UserID      string          `json:"user_id,omitempty"`
	AgentID     string          `json:"agent_id,omitempty"`
	AgentName   string          `json:"agent_name,omitempty"`
}

// ErrInterruptAlreadyTerminal is returned by InterruptResolve /
// InterruptFinish when the row is already in a terminal status
// (resolved / timed_out / cancelled). Distinct from ErrNotFound:
// the row exists, but the resolve / finish race lost. The tool
// layer maps this to HTTP 409 Conflict.
var ErrInterruptAlreadyTerminal = &SubstrateError{Code: "already_terminal", Msg: "interrupt: already resolved, timed out, or cancelled"}

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

// ErrConflict is returned by inserts that collide with an existing
// primary key. Used by SnapshotCreate so a caller doing
// captureOrSkip can distinguish "row already there" from a deeper
// DB error. Kind is "snapshot" for now; future tables that need
// the same shape can reuse this type with their own kind.
type ErrConflict struct {
	Kind string
	ID   string
}

func (e *ErrConflict) Error() string { return e.Kind + " already exists: " + e.ID }

// ErrDuplicateIdempotencyKey is returned by CreateRun when the supplied
// RunIdentity.IdempotencyKey collides with an existing run's key (RFC H
// Decision 10 "Layer 2" durable dedup). The caller is expected to look
// the existing run up via RunByIdempotencyKey and return it rather than
// treating this as a failure. It is a sentinel (errors.Is-comparable),
// distinct from *ErrConflict whose Kind/ID vary per call.
var ErrDuplicateIdempotencyKey = errors.New("duplicate idempotency_key")

// SnapshotRow is the persisted shape of one snapshots row, used by
// SnapshotCreate/Get. The JSONContent is the full envelope per the
// pause-resume-snapshot RFC § "Wire surface"; the store treats it
// as an opaque blob (validation happens at the snapshot package
// layer before insert).
type SnapshotRow struct {
	ID            string
	CreatedAt     time.Time
	Label         string
	SchemaVersion int
	ByteSize      int64
	JSONContent   []byte
}

// HookRow is the v0.12.5 Phase 6 cluster-wide hook registration
// shape. Mirrors internal/hooks.Hook but uses plain strings for
// Phase + FailMode so the store package stays free of an
// internal/hooks import (avoiding a circular dependency — hooks
// imports store via the DBBackedRegistry's hookStore interface).
//
// Conversion to *hooks.Hook happens in internal/hooks/db_registry.go
// where both package types are in scope.
type HookRow struct {
	ID               string
	Owner            string
	Name             string
	Phase            string // "pre" or "post"
	Agents           []string
	Tools            []string
	CallbackURL      string
	FailMode         string // "open" | "closed"
	TimeoutMs        int
	CreatedAt        time.Time
	CreatedByReplica string // nullable; observability only
}

// SnapshotListEntry is the metadata-only projection returned by
// SnapshotList. Excludes JSONContent so the list endpoint stays
// cheap when there are hundreds of snapshots in the table.
type SnapshotListEntry struct {
	ID            string
	CreatedAt     time.Time
	Label         string
	SchemaVersion int
	ByteSize      int64
}

// AgentDefActiveEntry is one row in the agent_def_active table —
// returned by SnapshotReadAgentDefActive for snapshot capture.
// Pairs an agent name with the def_id currently promoted to active.
type AgentDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
	// TenantID is the RFC N tenant-isolation axis (part of the
	// agent_def_active PK). "" = the shared/operator/legacy tenant.
	TenantID string `json:"tenant_id,omitempty"`
}

// MemorySnapshotEntry is one memory row enriched with its scope +
// scope_id columns. Returned by SnapshotReadMemory so the snapshot
// envelope can serialise rows without an additional lookup per row.
type MemorySnapshotEntry struct {
	Scope   MemoryScope `json:"scope"`
	ScopeID string      `json:"scope_id"`
	MemoryEntry
}

// ChannelCursorEntry is one row in the channel_cursors table —
// returned by SnapshotReadChannelCursors for snapshot capture. The
// cursor field is the opaque string form ack'd by the subscriber.
type ChannelCursorEntry struct {
	Channel   string      `json:"channel"`
	Scope     MemoryScope `json:"scope"`
	ScopeID   string      `json:"scope_id"`
	Cursor    string      `json:"cursor"`
	UpdatedAt time.Time   `json:"updated_at"`
}
