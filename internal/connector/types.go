package connector

import (
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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
}

// CancelRunResult reports the outcome of CancelRun.
type CancelRunResult struct {
	Cancelled    bool `json:"cancelled"`
	CascadeCount int  `json:"cascade_count"` // sub-runs also cancelled
	AlreadyEnded bool `json:"already_ended"` // run had already completed/failed
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

// --- Pause/Resume/Snapshot types (mocked in v0.8.15) ---
//
// All include FeatureStatus="preview" in v0.8.15 responses to signal
// the stub state. Real implementations in v0.8.16+ leave the field
// empty (or set "stable") and populate the meaningful fields.

// PauseResult reports the outcome of PauseRuntime.
type PauseResult struct {
	Status              string `json:"status"`                   // "paused"
	DurationMS          int64  `json:"duration_ms"`              // time taken to reach paused state
	ForceCancelledCount int    `json:"force_cancelled_count"`    // non-idempotent tool calls force-cancelled
	FeatureStatus       string `json:"feature_status,omitempty"` // "preview" in v0.8.15
	Note                string `json:"note,omitempty"`           // human-readable feature-status explanation
}

// ResumeResult reports the outcome of ResumeRuntime.
type ResumeResult struct {
	Status          string `json:"status"`            // "running"
	ResumedRunCount int    `json:"resumed_run_count"` // paused runs resumed
	FeatureStatus   string `json:"feature_status,omitempty"`
	Note            string `json:"note,omitempty"`
}

// RuntimeState is the response shape for GetRuntimeState.
type RuntimeState struct {
	Status         string     `json:"status"` // "running" | "pausing" | "paused" | "resuming" | "restoring"
	PausedAt       *time.Time `json:"paused_at,omitempty"`
	PausedRunCount int        `json:"paused_run_count"`
	SnapshotsCount int        `json:"snapshots_count"`
	FeatureStatus  string     `json:"feature_status,omitempty"`
}

// CreateSnapshotRequest is the input to CreateSnapshot.
type CreateSnapshotRequest struct {
	IncludeHistory bool       `json:"include_history,omitempty"`
	SinceTS        *time.Time `json:"since_ts,omitempty"`
	Description    string     `json:"description,omitempty"`
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

// ExportSnapshotResult is the response shape for ExportSnapshot.
type ExportSnapshotResult struct {
	SnapshotID    string `json:"snapshot_id"`
	FilePath      string `json:"file_path"` // absolute path on the loomcycle host
	Checksum      string `json:"checksum"`  // "sha256:..."
	SizeBytes     int64  `json:"size_bytes"`
	FeatureStatus string `json:"feature_status,omitempty"`
}

// RestoreSnapshotRequest is the input to RestoreSnapshot — exactly
// one of SnapshotID (restore from same-instance snapshot) or
// FilePath (load from disk) must be non-empty.
type RestoreSnapshotRequest struct {
	SnapshotID string `json:"snapshot_id,omitempty"`
	FilePath   string `json:"file_path,omitempty"`
}

// RestoreSnapshotResult is the response shape for RestoreSnapshot.
type RestoreSnapshotResult struct {
	Restored         map[string]int `json:"restored"` // section name → row count
	FormatMigrations []string       `json:"format_migrations,omitempty"`
	FeatureStatus    string         `json:"feature_status,omitempty"`
}
