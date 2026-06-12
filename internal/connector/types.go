package connector

import (
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ToolResult is the output of a builtin-tool invocation through the
// Connector. Mirrors tools.Result but lives in the connector package
// so transport adapters don't need to depend on internal/tools.
//
// Text is the model-facing payload (typically JSON for builtin tools).
// IsError signals a failed execution — the MCP wire layer maps this
// to `{"isError": true}` in the tool/call response so the orchestrator
// can render it differently.
type ToolResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// --- Run lifecycle types ---

// SpawnRunRequest is the input to SpawnRun. Mirrors the union of
// HTTP runRequest + runner.RunInput + gRPC RunRequest. Field
// semantics match POST /v1/runs (when SessionID is empty) and
// POST /v1/sessions/{id}/messages (when SessionID is non-empty).
type SpawnRunRequest struct {
	// Agent is the registered agent name. Required for fresh runs;
	// ignored for continuations (session's stored agent is the
	// source of truth).
	Agent string `json:"agent"`

	// SessionID — empty starts a fresh session+run; non-empty
	// continues an existing session and creates a new run inside it.
	SessionID string `json:"session_id,omitempty"`

	// TenantID is recorded on a fresh session. Ignored for continuations.
	TenantID string `json:"tenant_id,omitempty"`

	// Segments is the call's input prompt content. The caller does
	// NOT prepend the agent's system_prompt — the runner does that.
	Segments []loop.PromptSegment `json:"segments"`

	// AllowedTools narrows the agent's tool surface for this call.
	// Empty = use the agent's full configured allowlist.
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// AllowedHosts narrows HTTP / WebFetch / WebSearch host policy.
	// nil = no narrowing; &[]string{} = deny-all; &[]string{"foo"} =
	// intersection with operator's static list.
	AllowedHosts *[]string `json:"allowed_hosts,omitempty"`

	// WebSearchFilter is "drop" or "keep". Ignored when AllowedHosts is nil.
	WebSearchFilter string `json:"web_search_filter,omitempty"`

	// UserID binds the run to a user. Optional for fresh runs;
	// ignored for continuations (inherited from the session).
	UserID string `json:"user_id,omitempty"`

	// AgentID is the caller-supplied tracking handle. Optional;
	// the runner generates one when empty and returns it in result.
	AgentID string `json:"agent_id,omitempty"`

	// UserTier is the v0.8.2 user-facing-tier policy name. Empty
	// falls through to cfg.UserTiers["default"] when configured.
	UserTier string `json:"user_tier,omitempty"`

	// UserBearer is the v0.8.14 per-run MCP bearer token. Substituted
	// into MCP HTTP header values containing ${run.user_bearer}.
	UserBearer string `json:"user_bearer,omitempty"`

	// UserCredentials is the v1.x RFC F named-credentials map.
	// Per-tool/per-MCP-server bearers keyed by operator-chosen
	// name. Substituted into MCP HTTP header values containing
	// ${run.credentials.<name>}. Sub-agents inherit the whole map.
	// See rfcs/per-run-credentials.md for the locked design.
	UserCredentials map[string]string `json:"user_credentials,omitempty"`

	// ParentContext is opaque caller-tracking lineage (v0.12.x) carried
	// verbatim, inherited by every sub-agent the Agent tool spawns,
	// persisted on each run row, and echoed on the per-agent report
	// surfaces. Not a secret. Nil = no context (back-compat).
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`

	// Metadata is the optional NON-SECRET structured metadata passed to the
	// agent (repo name, review policy, …) — the same trusted channel the HTTP
	// /v1/runs `metadata` field feeds, so a gRPC / LoomCycle-MCP spawn_run /
	// connector caller reaches it too. A code-js agent reads it as
	// input.metadata; an LLM agent receives it as a trusted prompt block. Not
	// a secret — credentials use UserCredentials. Nil = none (back-compat).
	Metadata map[string]any `json:"metadata,omitempty"`

	// Compaction is an optional per-RUN context-compaction override, merged PER
	// FIELD over the agent's own compaction block (this wins; unset fields
	// inherit). nil = inherit the agent's entirely. Mirrors the HTTP /v1/runs
	// `compaction` field so a gRPC / LoomCycle-MCP spawn_run / fan-out child
	// reaches the same per-run knob. Carried verbatim to runner.RunInput.
	Compaction *config.Compaction `json:"compaction,omitempty"`
}

// SpawnRunResult is the final outcome of a SpawnRun call (returned
// only after the run completes — use streaming notifications at the
// transport layer for live progress).
type SpawnRunResult struct {
	AgentID    string           `json:"agent_id"`
	RunID      string           `json:"run_id"`
	SessionID  string           `json:"session_id"`
	Status     string           `json:"status"` // "completed" | "failed" | "cancelled"
	StopReason string           `json:"stop_reason,omitempty"`
	FinalText  string           `json:"final_text,omitempty"`
	Usage      *providers.Usage `json:"usage,omitempty"`
	Error      string           `json:"error,omitempty"`
	// ParentContext echoes the run's tracking lineage back to the
	// caller (v0.12.x) so a sub-agent's usage can be attributed to the
	// root request. Nil when the run carried no context.
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
}

// CancelRunResult reports the outcome of CancelRun.
type CancelRunResult struct {
	Cancelled    bool `json:"cancelled"`
	CascadeCount int  `json:"cascade_count"` // sub-runs also cancelled
	AlreadyEnded bool `json:"already_ended"` // run had already completed/failed
}

// CompactResult is the outcome of CompactRun — summarize a run's conversation
// to free context. Mirrors POST /v1/runs/{run_id}/compact's response body.
type CompactResult struct {
	RunID        string `json:"run_id"`
	Compacted    bool   `json:"compacted"`
	BeforeTokens int    `json:"before_tokens"`
	AfterTokens  int    `json:"after_tokens"`
	// Applied: "live" (pushed to the running loop), "marker" (persisted for a
	// terminal run's next continuation), or "noop" (too short to compact).
	Applied string `json:"applied"`
}

// Run is the status snapshot returned by GetRun / ListRuns. Distinct
// from store.Run — this is the wire shape (no internal-only fields).
type Run struct {
	AgentID       string           `json:"agent_id"`
	RunID         string           `json:"run_id"`
	SessionID     string           `json:"session_id"`
	UserID        string           `json:"user_id,omitempty"`
	Agent         string           `json:"agent"`
	ParentAgentID string           `json:"parent_agent_id,omitempty"`
	Status        string           `json:"status"` // "running" | "completed" | "failed" | "cancelled"
	StartedAt     time.Time        `json:"started_at"`
	CompletedAt   *time.Time       `json:"completed_at,omitempty"`
	StopReason    string           `json:"stop_reason,omitempty"`
	Usage         *providers.Usage `json:"usage,omitempty"`
	Error         string           `json:"error,omitempty"`
}

// ListRunsFilter selects which runs ListRuns returns. Empty fields
// mean "no filter on this dimension". Multiple non-empty fields are
// AND-combined.
type ListRunsFilter struct {
	UserID string `json:"user_id,omitempty"`
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"` // 0 = adapter default
}

// --- Agent management types ---

// RegisterAgentRequest is the input to RegisterAgent. Mirrors a
// subset of config.AgentDef sufficient for runtime-registered agents.
type RegisterAgentRequest struct {
	Name         string   `json:"name"`
	SystemPrompt string   `json:"system_prompt"`
	AllowedTools []string `json:"allowed_tools"`
	Tier         string   `json:"tier,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"` // "minimal" | "low" | "medium" | "high"
	MaxTokens    int      `json:"max_tokens,omitempty"`
	MemoryScopes []string `json:"memory_scopes,omitempty"`
	Description  string   `json:"description,omitempty"`

	// MaxIterations / Channels / EvaluationScopes / Interruption let an
	// MCP-registered agent be a COMPLETE interactive/multi-agent agent, not
	// just a tool-bearing one (F11/F14). They mirror config.AgentDef and flow
	// through DynamicAgent.Definition unchanged.
	MaxIterations    int                         `json:"max_iterations,omitempty"`
	Channels         config.AgentChannelACL      `json:"channels,omitempty"`
	EvaluationScopes []string                    `json:"evaluation_scopes,omitempty"`
	Interruption     config.AgentInterruptionACL `json:"interruption,omitempty"`

	// TTLSeconds is how long this dynamic agent should live. When
	// 0, falls back to LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS
	// (default 24h). A value of -1 means "no expiry" (operator
	// must explicitly UnregisterAgent).
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// AgentDescriptor is the public-facing view of an agent (static or
// dynamic). Distinct from config.AgentDef — this omits operator
// secrets like the resolved API key path.
type AgentDescriptor struct {
	Name         string     `json:"name"`
	Source       string     `json:"source"` // "static" | "dynamic"
	AllowedTools []string   `json:"allowed_tools"`
	Tier         string     `json:"tier,omitempty"`
	Provider     string     `json:"provider,omitempty"`
	Model        string     `json:"model,omitempty"`
	Description  string     `json:"description,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"` // dynamic only
	CreatedAt    *time.Time `json:"created_at,omitempty"` // dynamic only
}

// --- Pause/Resume/Snapshot types (real in v0.8.18) ---
//
// Wire shapes locked in v0.8.15; real implementations landed in
// v0.8.18 behind the same field shapes. The FeatureStatus field is
// retained on every response type as `omitempty` for back-compat
// with v0.8.15 PREVIEW clients (the field is now always left empty
// in real responses).

// PauseResult reports the outcome of PauseRuntime.
type PauseResult struct {
	Status              string   `json:"status"`                   // "paused"
	DurationMS          int64    `json:"duration_ms"`              // time taken to reach paused state
	ForceCancelledCount int      `json:"force_cancelled_count"`    // non-idempotent tool calls force-cancelled
	PausedRunsCount     int      `json:"paused_runs_count"`        // runs marked pause_state='paused' in the store
	Warnings            []string `json:"warnings,omitempty"`       // non-fatal issues encountered during pause
	FeatureStatus       string   `json:"feature_status,omitempty"` // empty in v0.8.18+; "preview" historically
	Note                string   `json:"note,omitempty"`           // human-readable feature-status explanation
}

// ResumeResult reports the outcome of ResumeRuntime.
type ResumeResult struct {
	Status          string   `json:"status"`             // "running"
	ResumedRunCount int      `json:"resumed_run_count"`  // paused runs resumed
	Warnings        []string `json:"warnings,omitempty"` // non-fatal issues encountered during resume
	FeatureStatus   string   `json:"feature_status,omitempty"`
	Note            string   `json:"note,omitempty"`
}

// RuntimeState is the response shape for GetRuntimeState.
type RuntimeState struct {
	Status         string     `json:"status"` // "running" | "pausing" | "paused"
	PausedAt       *time.Time `json:"paused_at,omitempty"`
	PausedRunCount int        `json:"paused_run_count"`
	SnapshotsCount int        `json:"snapshots_count"`
	FeatureStatus  string     `json:"feature_status,omitempty"`
}

// ResolverMatrix is the response shape for ResolveProbe — the resolver
// availability matrix captured immediately after a forced re-probe.
// JSON tags match the HTTP GET /v1/_resolver wire shape so dashboards
// reading either endpoint see identical fields.
type ResolverMatrix struct {
	GeneratedAt time.Time                               `json:"generated_at"`
	Providers   map[string]ResolverProviderAvailability `json:"providers"`
}

// ResolverProviderAvailability is one provider's row in the matrix.
type ResolverProviderAvailability struct {
	Excluded  bool                           `json:"excluded"`
	Reachable bool                           `json:"reachable"`
	Models    map[string]ResolverModelStatus `json:"models"`
	LastCheck time.Time                      `json:"last_check"`
	LastError string                         `json:"last_error,omitempty"`
}

// ResolverModelStatus is one model's status within a provider row.
type ResolverModelStatus struct {
	Listed  bool `json:"listed"`
	Stalled bool `json:"stalled"`
}

// CreateSnapshotRequest is the input to CreateSnapshot.
type CreateSnapshotRequest struct {
	IncludeHistory bool       `json:"include_history,omitempty"`
	SinceTS        *time.Time `json:"since_ts,omitempty"`
	Description    string     `json:"description,omitempty"`
	// MaxBytes overrides the operator's LOOMCYCLE_SNAPSHOT_MAX_BYTES
	// cap for this call. 0 = use the default.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// SnapshotDescriptor is the response shape for CreateSnapshot /
// elements of ListSnapshots.
type SnapshotDescriptor struct {
	SnapshotID      string     `json:"snapshot_id"`
	CreatedAt       time.Time  `json:"created_at"`
	SizeBytes       int64      `json:"size_bytes"`
	IncludesHistory bool       `json:"includes_history"`
	SinceTS         *time.Time `json:"since_ts,omitempty"`
	Description     string     `json:"description,omitempty"`
	FormatVersion   string     `json:"format_version"`
	FeatureStatus   string     `json:"feature_status,omitempty"`
}

// SnapshotEnvelope is the response shape for GetSnapshot — the full
// stored snapshot row including the JSON content. Distinct from
// ExportSnapshotResult, which is operator-facing "where did this
// land on the host" semantics.
type SnapshotEnvelope struct {
	SnapshotID    string          `json:"snapshot_id"`
	CreatedAt     time.Time       `json:"created_at"`
	Description   string          `json:"description,omitempty"`
	FormatVersion string          `json:"format_version"`
	SizeBytes     int64           `json:"size_bytes"`
	JSONContent   json.RawMessage `json:"json_content"`
}

// ExportSnapshotResult is the response shape for ExportSnapshot.
// In v0.8.18+, the canonical envelope bytes are returned via the
// RawJSON field — transports that want to stream the export use
// these bytes (HTTP /v1/_snapshots/{id}/export, gRPC streaming).
// FilePath / Checksum remain operator-facing fields for transports
// that materialise the export to disk first (CLI's --out flag); they
// are empty when the bytes-only path is used.
//
// RawJSON is typed as json.RawMessage so it marshals onto the JSON
// wire AS-IS (a nested JSON object), not as a base64-encoded string —
// Go's default []byte marshalling would base64-encode it, breaking
// any caller that tries to pipe `export_snapshot` output back into
// `restore_snapshot` input. gRPC transports convert their proto
// `bytes` field into this via json.RawMessage(...) cast.
type ExportSnapshotResult struct {
	SnapshotID    string          `json:"snapshot_id"`
	FilePath      string          `json:"file_path,omitempty"` // absolute path on the loomcycle host (when materialised)
	Checksum      string          `json:"checksum,omitempty"`  // "sha256:..." (when materialised)
	SizeBytes     int64           `json:"size_bytes"`
	RawJSON       json.RawMessage `json:"raw_json,omitempty"` // canonical envelope bytes (v0.8.18+); see type comment for marshalling rationale
	FeatureStatus string          `json:"feature_status,omitempty"`
}

// RestoreSnapshotRequest is the input to RestoreSnapshot. Exactly
// one of SnapshotID (restore from same-instance snapshot), RawJSON
// (cross-instance restore from inline bytes), or FilePath (load
// from disk) must be non-empty.
//
// RawJSON is typed as json.RawMessage so JSON-encoded transports
// (MCP, HTTP body) accept the envelope as a nested JSON object
// rather than a base64-encoded string — see ExportSnapshotResult's
// type comment for the same-shape rationale.
type RestoreSnapshotRequest struct {
	SnapshotID     string          `json:"snapshot_id,omitempty"`
	FilePath       string          `json:"file_path,omitempty"`
	RawJSON        json.RawMessage `json:"raw_json,omitempty"`
	IncludeHistory bool            `json:"include_history,omitempty"`
}

// RestoreSnapshotResult is the response shape for RestoreSnapshot.
// The Restored map is preserved for backwards-compat — populated
// from the per-section counters. Individual counter fields are also
// surfaced for transports that want strongly-typed access.
type RestoreSnapshotResult struct {
	Restored                   map[string]int `json:"restored"`
	AgentDefsRestored          int            `json:"agent_defs_restored"`
	AgentDefActiveRestored     int            `json:"agent_def_active_restored"`
	MemoryRestored             int            `json:"memory_restored"`
	ChannelMessagesRestored    int            `json:"channel_messages_restored"`
	ChannelCursorsRestored     int            `json:"channel_cursors_restored"`
	EvaluationsRestored        int            `json:"evaluations_restored"`
	PausedRunsRestored         int            `json:"paused_runs_restored"`
	SynthesizedSessions        int            `json:"synthesized_sessions"`
	TranscriptEventsRestored   int            `json:"transcript_events_restored"`
	InteractionHistoryRestored int            `json:"interaction_history_restored"`
	Warnings                   []string       `json:"warnings,omitempty"`
	FormatMigrations           []string       `json:"format_migrations,omitempty"`
	FeatureStatus              string         `json:"feature_status,omitempty"`
}

// ---- v0.9.x n8n RFC Phase 0 ----

// ChannelDescriptor is one row in ListChannels. Joins the operator-
// declared yaml channel with the runtime aggregate stats over
// channel_messages. Channels declared with no published messages
// still surface (MessageCount=0); rows for channels NOT declared but
// holding orphaned messages also surface, for forensics.
type ChannelDescriptor struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Semantic        string `json:"semantic,omitempty"`
	Publisher       string `json:"publisher,omitempty"`
	Period          string `json:"period,omitempty"`
	DefaultTTL      int    `json:"default_ttl,omitempty"`
	MaxMessages     int    `json:"max_messages,omitempty"`
	MessageCount    int64  `json:"message_count"`
	OldestVisibleAt string `json:"oldest_visible_at,omitempty"` // RFC3339; empty when count=0
	NewestVisibleAt string `json:"newest_visible_at,omitempty"`
	// v0.11.5: discriminator between yaml-declared (static, immutable)
	// and runtime-declared (substrate-persisted, CRUD-mutable) rows.
	// "yaml" | "runtime" | "orphan" (no declaration, only orphan
	// messages on the underlying tables).
	Source string `json:"source,omitempty"`
}

// ListChannelsResponse is the response shape for Connector.ListChannels.
type ListChannelsResponse struct {
	Channels []ChannelDescriptor `json:"channels"`
}

// --- Channel CRUD types (v0.9.x admin + per-user surface) ---

// ChannelPublishRequest is the input to Connector.PublishChannel. Scope
// + ScopeID determine the cursor namespace: scope="global" + ScopeID=""
// addresses the admin surface; scope="user" + ScopeID=<user_id>
// addresses a per-user channel cursor. DeliverAt is RFC3339Nano; empty
// means publish immediately.
type ChannelPublishRequest struct {
	Channel   string          `json:"channel"`
	Scope     string          `json:"scope"`
	ScopeID   string          `json:"scope_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	DeliverAt string          `json:"deliver_at,omitempty"`
}

// ChannelPublishResult mirrors the in-band Channel tool's publish
// response. MsgID is the server-assigned id; CreatedAt is the
// PublishedAt time; VisibleAt is set only when the publish was
// deferred (deliver_at > created_at).
type ChannelPublishResult struct {
	MsgID     string `json:"msg_id"`
	Channel   string `json:"channel"`
	CreatedAt string `json:"created_at"`           // RFC3339Nano
	VisibleAt string `json:"visible_at,omitempty"` // RFC3339Nano; omitted when not deferred
}

// ChannelSubscribeRequest is the input to Connector.SubscribeChannel.
// FromCursor empty means "from the committed cursor"; FromCursor "cur_0"
// means "from the beginning" (replay). MaxMessages clamped to [1, 100],
// default 10. WaitMS is the long-poll timeout; 0 = poll-and-return.
type ChannelSubscribeRequest struct {
	Channel     string `json:"channel"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scope_id,omitempty"`
	FromCursor  string `json:"from_cursor,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty"`
	WaitMS      int    `json:"wait_ms,omitempty"`
}

// ChannelSubscribeResult is the synchronous batch result. Auto-commits
// the cursor when Messages is non-empty (mirror of the in-band tool's
// at-most-once shape). NextCursor is the cursor to pass on the next
// call to continue forward.
type ChannelSubscribeResult struct {
	Channel    string           `json:"channel"`
	Messages   []ChannelMessage `json:"messages"`
	NextCursor string           `json:"next_cursor"`
}

// ChannelMessage is one delivered message. The wire shape is stable
// from v0.8.4+: id, value (the JSON payload), published_at RFC3339Nano.
type ChannelMessage struct {
	ID          string          `json:"id"`
	Value       json.RawMessage `json:"value"`
	PublishedAt string          `json:"published_at"`
}

// ChannelPeekRequest is the input to Connector.PeekChannel. Non-
// destructive read (does NOT advance the committed cursor).
type ChannelPeekRequest struct {
	Channel     string `json:"channel"`
	Scope       string `json:"scope"`
	ScopeID     string `json:"scope_id,omitempty"`
	FromCursor  string `json:"from_cursor,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty"`
}

// ChannelPeekResult mirrors ChannelSubscribeResult minus NextCursor —
// peek never advances the cursor.
type ChannelPeekResult struct {
	Channel  string           `json:"channel"`
	Messages []ChannelMessage `json:"messages"`
}

// ChannelAckRequest advances the committed cursor for a subscriber.
// Returns ErrChannelCursorRegression if Cursor is older than the
// committed value (monotonic-cursor contract).
type ChannelAckRequest struct {
	Channel string `json:"channel"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id,omitempty"`
	Cursor  string `json:"cursor"`
}

// ChannelAckResult is `{"ok": true}` on success. Failure modes propagate
// as Go errors (cursor regression, store error).
type ChannelAckResult struct {
	OK bool `json:"ok"`
}

// --- Channel fan-in / fan-out (RFC S client twins) ---
//
// AwaitChannels is the client-facing twin of the in-band Channel.await
// op; BroadcastChannels the twin of Channel.broadcast. They let an
// external orchestrator (n8n, an app server) fan-in / fan-out over the
// SAME bus + store agents use. Scope + ScopeID apply to EVERY channel in
// the set (a fan operates within one scope).

// ChannelAwaitRequest fans IN across Channels: wait until the Mode
// predicate is met (any / all / at_least N), or WaitMS elapses. Reads are
// NON-committing (detection only) — the caller subscribe/acks what it
// processes. Channels capped at 32.
type ChannelAwaitRequest struct {
	Channels    []string `json:"channels"`
	Scope       string   `json:"scope"`
	ScopeID     string   `json:"scope_id,omitempty"`
	Mode        string   `json:"mode,omitempty"` // any | all | at_least; default any
	N           int      `json:"n,omitempty"`    // threshold for at_least (>0)
	FromCursor  string   `json:"from_cursor,omitempty"`
	MaxMessages int      `json:"max_messages,omitempty"`
	WaitMS      int      `json:"wait_ms,omitempty"`
}

// ChannelAwaitEntry is one fired channel's accumulated messages + the
// cursor to continue from (NON-committing — the cursor is not advanced).
type ChannelAwaitEntry struct {
	Messages   []ChannelMessage `json:"messages"`
	NextCursor string           `json:"next_cursor"`
}

// ChannelAwaitResult mirrors the in-band Channel.await response. TimedOut
// is true only when the predicate was unmet within WaitMS (never an
// error). Results is keyed by channel name.
type ChannelAwaitResult struct {
	Satisfied     bool                         `json:"satisfied"`
	TimedOut      bool                         `json:"timed_out"`
	Mode          string                       `json:"mode"`
	Fired         []string                     `json:"fired"`
	TotalMessages int                          `json:"total_messages"`
	Results       map[string]ChannelAwaitEntry `json:"results"`
}

// ChannelBroadcastRequest fans OUT: publish Payload to every channel in
// Channels (same payload, same scope). DeliverAt is RFC3339Nano; empty =
// immediate. Channels capped at 32. ATOMIC at the ACL pre-flight — one
// undeclared/invalid channel refuses the whole op (no partial broadcast).
type ChannelBroadcastRequest struct {
	Channels  []string        `json:"channels"`
	Scope     string          `json:"scope"`
	ScopeID   string          `json:"scope_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	DeliverAt string          `json:"deliver_at,omitempty"`
}

// ChannelBroadcastEntry is one channel's publish outcome. Error is set
// (and MsgID empty) when that channel's write failed after the pre-flight
// passed — the successful publishes still stand.
type ChannelBroadcastEntry struct {
	Channel   string `json:"channel"`
	MsgID     string `json:"msg_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"` // RFC3339Nano
	VisibleAt string `json:"visible_at,omitempty"` // RFC3339Nano; set when deferred
	Error     string `json:"error,omitempty"`
}

// ChannelBroadcastResult reports how many channels received the payload.
// Published + Failed = the deduped channel count.
type ChannelBroadcastResult struct {
	Published int                     `json:"published"`
	Failed    int                     `json:"failed"`
	Results   []ChannelBroadcastEntry `json:"results"`
}

// --- Channel admin CRUD (v0.11.5) ---

// ChannelCreateRequest is the input to Connector.CreateChannel — the
// substrate-persisted equivalent of the operator-yaml `channels:` block.
// Name uniqueness is checked across both yaml and runtime sources (the
// yaml side is the floor; a runtime create on a yaml name is refused).
type ChannelCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`        // "global" | "agent" | "user"; default "global"
	Semantic    string `json:"semantic,omitempty"`     // "queue" | "topic"; default "queue"
	DefaultTTL  int    `json:"default_ttl,omitempty"`  // seconds; 0 = no TTL
	MaxMessages int    `json:"max_messages,omitempty"` // bounded queue; 0 = unbounded
	Publisher   string `json:"publisher,omitempty"`    // free-form attribution; not enforced
	Period      string `json:"period,omitempty"`       // free-form retention hint; not enforced
}

// ChannelUpdateRequest is the input to Connector.UpdateChannel. Nil
// pointers leave the corresponding field unchanged. Name is in the
// path, not the body (channel name is the primary key — not editable).
type ChannelUpdateRequest struct {
	Description *string `json:"description,omitempty"`
	DefaultTTL  *int    `json:"default_ttl,omitempty"`
	MaxMessages *int    `json:"max_messages,omitempty"`
	Semantic    *string `json:"semantic,omitempty"`
}

// ChannelPurgeResult is the output of Connector.PurgeChannel — the
// channel name and the count of buffered messages cleared.
type ChannelPurgeResult struct {
	Name   string `json:"name"`
	Purged int    `json:"purged"`
}

// StreamUserRunStatesRequest is the input to Connector.StreamUserRunStates.
// UserID is required; Statuses and Agent are optional filters that match
// the SSE handler's ?status=...&agent=... query params.
type StreamUserRunStatesRequest struct {
	UserID   string   `json:"user_id"`
	Statuses []string `json:"statuses,omitempty"`
	Agent    string   `json:"agent,omitempty"`
}

// RunStateEvent is the payload yielded by RunStateVisitor for each
// state transition. Mirrors runstate.RunStateEvent exactly; defined
// here so the Connector interface doesn't depend on internal/runstate
// (mcp / grpc adapters use only the connector package).
type RunStateEvent struct {
	RunID         string `json:"run_id"`
	AgentID       string `json:"agent_id"`
	Agent         string `json:"agent"`
	UserID        string `json:"user_id"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	Status        string `json:"status"`
	StopReason    string `json:"stop_reason,omitempty"`
	Error         string `json:"error,omitempty"`
	TS            string `json:"ts"` // RFC3339
	// ParentContext echoes the run's opaque tracking lineage (v0.12.x).
	// Nil when the run carried no context.
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
}
