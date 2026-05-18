// Package snapshot implements the v0.8.17 runtime-state capture
// envelope. Capture() reads every section the
// pause-resume-snapshot RFC defines (agent_defs, agent_def_active,
// memory, channels, evaluations, paused_runs, interaction_history)
// into a single JSON envelope; HTTP handlers + the CLI consume the
// resulting *store.SnapshotRow to persist + export.
//
// The Memory section's `embedding` field is locked at v1.0 with the
// optional shape defined in doc-internal/rfcs/semantic-memory.md
// § "Snapshot integration". In Phase 1 (this package) every memory
// entry's embedding is null because the Phase 2 vector ops haven't
// landed yet; the shape ships now so the v1.0 → v1.1 migration
// is avoidable when vector lands. See memory_embedding.go.
package snapshot

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the outer envelope version. Bumped only when a
// structurally breaking change lands; per-section schemas evolve
// independently via the additive-fields rule (see
// doc-internal/rfcs/pause-resume-snapshot.md § "Forward-compat
// additive-fields rule").
const SchemaVersion = 1

// SectionVersion is the wire-stable per-section version string.
// Today every section is at "1.0"; PR 3 (export/restore) builds a
// per-section migration registry keyed on these strings.
const SectionVersion = "1.0"

// Envelope is the full snapshot JSON shape. Marshalled by Capture()
// and stored in snapshots.json_content. The outer shape pins:
//
//	{
//	  "schema_version": 1,
//	  "created_at": "...",
//	  "sections": {
//	    "agent_defs":         { "version": "1.0", "entries": [...] },
//	    "agent_def_active":   { "version": "1.0", "entries": [...] },
//	    "memory":             { "version": "1.0", "entries": [...] },
//	    "channels":           { "version": "1.0", "config":  [...], "messages": [...], "cursors": [...] },
//	    "evaluations":        { "version": "1.0", "entries": [...] },
//	    "paused_runs":        { "version": "1.0", "entries": [...] },
//	    "interaction_history":{ "version": "1.0", "since_ts": "...", "events": [...] }  // optional
//	  }
//	}
type Envelope struct {
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	Sections      Sections  `json:"sections"`
}

// Sections is the named section map. Each field is the section's
// declared shape; the per-section version field is the migration
// anchor (PR 3 reads it to dispatch into the registry).
type Sections struct {
	AgentDefs          AgentDefsSection           `json:"agent_defs"`
	AgentDefActive     AgentDefActiveSection      `json:"agent_def_active"`
	Memory             MemorySection              `json:"memory"`
	Channels           ChannelsSection            `json:"channels"`
	Evaluations        EvaluationsSection         `json:"evaluations"`
	PausedRuns         PausedRunsSection          `json:"paused_runs"`
	InteractionHistory *InteractionHistorySection `json:"interaction_history,omitempty"`
}

// AgentDefsSection wraps the list of every agent_defs row.
type AgentDefsSection struct {
	Version string          `json:"version"`
	Entries []AgentDefEntry `json:"entries"`
}

// AgentDefEntry mirrors store.AgentDefRow with stable JSON field
// names. Kept distinct from the store type so the snapshot envelope
// can evolve independently — adding a field here doesn't require
// adding it to the store struct, and vice versa.
type AgentDefEntry struct {
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

// AgentDefActiveSection wraps the active-pointer entries.
type AgentDefActiveSection struct {
	Version string                `json:"version"`
	Entries []AgentDefActiveEntry `json:"entries"`
}

type AgentDefActiveEntry struct {
	Name              string    `json:"name"`
	DefID             string    `json:"def_id"`
	PromotedAt        time.Time `json:"promoted_at"`
	PromotedByAgentID string    `json:"promoted_by_agent_id,omitempty"`
}

// MemorySection wraps memory rows. Each entry carries an optional
// `embedding` field — null in Phase 1 (vector ops not yet shipped),
// populated in Phase 2. The field's presence-and-nil shape is the
// load-bearing forward-compat contract: snapshots written by Phase 1
// readers carry the field as null; Phase 2 readers populate it; old
// readers that don't know about embedding ignore the field
// gracefully (Go's encoding/json drops unknown fields).
type MemorySection struct {
	Version string        `json:"version"`
	Entries []MemoryEntry `json:"entries"`
}

// MemoryEntry mirrors store.MemorySnapshotEntry with the additional
// `embedding` field. CreatedAt / UpdatedAt come from MemorySnapshotEntry's
// embedded MemoryEntry. ExpiresAt is omitted when zero (no TTL).
type MemoryEntry struct {
	Scope     string                   `json:"scope"`
	ScopeID   string                   `json:"scope_id"`
	Key       string                   `json:"key"`
	Value     json.RawMessage          `json:"value"`
	ExpiresAt *time.Time               `json:"expires_at,omitempty"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
	Embedding *MemoryEmbeddingSnapshot `json:"embedding"` // explicit null when nil; see memory_embedding.go
}

// ChannelsSection wraps channels config + messages + cursors. Channel
// config (declared channels list from operator yaml) lives in the
// snapshot so a restore on a different machine knows what channels
// existed; the operator on the new machine reconciles against their
// own yaml.
type ChannelsSection struct {
	Version  string                `json:"version"`
	Config   []ChannelConfigEntry  `json:"config"`
	Messages []ChannelMessageEntry `json:"messages"`
	Cursors  []ChannelCursorEntry  `json:"cursors"`
}

// ChannelConfigEntry mirrors the operator-yaml shape that loomcycle
// validates per-channel. Captured at snapshot time so restore on a
// different host has the operator-declared shape to reconcile
// against.
type ChannelConfigEntry struct {
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Scope             string   `json:"scope"`
	TTLSeconds        int      `json:"ttl_seconds,omitempty"`
	MaxMessages       int      `json:"max_messages,omitempty"`
	AllowedPublishers []string `json:"allowed_publishers,omitempty"`
}

type ChannelMessageEntry struct {
	ID                string          `json:"id"`
	Channel           string          `json:"channel"`
	Scope             string          `json:"scope"`
	ScopeID           string          `json:"scope_id"`
	Payload           json.RawMessage `json:"payload"`
	PublishedAt       time.Time       `json:"published_at"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	VisibleAt         *time.Time      `json:"visible_at,omitempty"`
	PublishedByUserID string          `json:"published_by_user_id,omitempty"`
}

type ChannelCursorEntry struct {
	Channel   string    `json:"channel"`
	Scope     string    `json:"scope"`
	ScopeID   string    `json:"scope_id"`
	Cursor    string    `json:"cursor"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EvaluationsSection wraps evaluation rows.
type EvaluationsSection struct {
	Version string            `json:"version"`
	Entries []EvaluationEntry `json:"entries"`
}

type EvaluationEntry struct {
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

// PausedRunsSection wraps paused runs + their transcript events. The
// transcript events are required for resume — the model picks up
// from the last completed iteration boundary, and needs the prior
// turns in its context.
type PausedRunsSection struct {
	Version string           `json:"version"`
	Entries []PausedRunEntry `json:"entries"`
}

type PausedRunEntry struct {
	RunID            string            `json:"run_id"`
	AgentID          string            `json:"agent_id,omitempty"`
	ParentAgentID    string            `json:"parent_agent_id,omitempty"`
	UserID           string            `json:"user_id,omitempty"`
	UserTier         string            `json:"user_tier,omitempty"`
	Agent            string            `json:"agent"`
	AgentDefID       string            `json:"agent_def_id,omitempty"`
	SessionID        string            `json:"session_id"`
	StartedAt        time.Time         `json:"started_at"`
	Model            string            `json:"model,omitempty"`
	PauseState       string            `json:"pause_state"`
	TranscriptEvents []TranscriptEvent `json:"transcript_events"`
	// TranscriptError records a per-run transcript-read failure. Set
	// when GetTranscript returned an error during capture; the entry
	// is otherwise included in the snapshot (RunID, AgentID etc.
	// survive). Operators inspect this field to know which runs lost
	// their replay context and may need manual recovery.
	TranscriptError string `json:"transcript_error,omitempty"`
}

// TranscriptEvent is one event from the run's transcript — replayed
// to the model on resume to reconstruct context. Payload is the raw
// providers.Event JSON; the snapshot package doesn't decode it.
type TranscriptEvent struct {
	Seq     int64           `json:"seq"`
	TsNs    int64           `json:"ts_ns"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// InteractionHistorySection is optional — captured only when
// CaptureOptions.IncludeHistory is true. Carries every event whose
// ts >= since_ts across the captured run set (or all runs if no
// scope restriction was applied at snapshot time).
type InteractionHistorySection struct {
	Version string            `json:"version"`
	SinceTs time.Time         `json:"since_ts"`
	Events  []TranscriptEvent `json:"events"`
}
