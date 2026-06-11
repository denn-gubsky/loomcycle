package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DefaultMaxBytes caps the in-memory snapshot envelope size unless
// LOOMCYCLE_SNAPSHOT_MAX_BYTES overrides. 512 MB is the Phase-1
// safety stance: Capture() loads the entire envelope into a Go []byte
// before writing to snapshots.json_content. For larger experiments,
// streaming export is a v0.9.x candidate (deferred per the RFC's
// "Out of scope" section).
const DefaultMaxBytes = 512 * 1024 * 1024

// ErrSnapshotTooLarge is returned by Capture when the serialised
// envelope exceeds CaptureOptions.MaxBytes. Operators get the
// concrete size + cap so they can decide to raise the cap or scope
// the capture.
type ErrSnapshotTooLarge struct {
	SizeBytes int64
	MaxBytes  int64
}

func (e *ErrSnapshotTooLarge) Error() string {
	return fmt.Sprintf("snapshot: serialised size %d bytes exceeds cap %d bytes (raise LOOMCYCLE_SNAPSHOT_MAX_BYTES or narrow the capture scope)",
		e.SizeBytes, e.MaxBytes)
}

// CaptureOptions controls what Capture() serialises and how big the
// resulting envelope can be.
type CaptureOptions struct {
	// Label is the operator-supplied free-text marker that lands in
	// snapshots.label. Optional. The HTTP handler passes through
	// whatever the operator sent in the request body.
	Label string

	// MaxBytes caps the serialised JSON size. 0 = use DefaultMaxBytes.
	// Capture returns *ErrSnapshotTooLarge when the envelope exceeds
	// the cap.
	MaxBytes int64

	// IncludeHistory toggles the optional interaction_history
	// section. False (default) keeps snapshots focused on
	// running-state — paused runs + their transcripts only. True
	// captures every event since IncludeHistorySince across every
	// session (large; operators only opt in for experiment
	// reproduction).
	IncludeHistory bool

	// IncludeHistorySince scopes the interaction_history section
	// when IncludeHistory is true. Zero time means "all history."
	IncludeHistorySince time.Time

	// Channels is the operator-yaml channel config from
	// cfg.Channels. The snapshot envelope embeds the configured
	// channel shape so a restore on a different machine can
	// reconcile against its own yaml. Pass nil if the operator yaml
	// declares no channels (the section emits an empty config slice).
	Channels []ChannelConfigEntry
}

// Capture reads every section the pause-resume-snapshot RFC defines
// and returns a serialised *store.SnapshotRow ready for SnapshotCreate.
// The envelope's JSON bytes are also returned alongside the row so
// callers (the HTTP export handler) can serve them without a re-Get.
//
// Capture is read-only with respect to the store. It does NOT modify
// any rows; concurrent agent activity continues unaffected. The
// snapshot represents the store's state at the read instant — there
// is no transactional snapshot isolation across sections (a row
// inserted between section reads will appear in one but not the
// other). Operators wanting strict point-in-time consistency should
// pause the runtime first (POST /v1/runtime/pause) before capturing.
//
// Returns *ErrSnapshotTooLarge when the serialised envelope exceeds
// opts.MaxBytes.
func Capture(ctx context.Context, s store.Store, opts CaptureOptions) (*store.SnapshotRow, []byte, error) {
	if s == nil {
		return nil, nil, errors.New("snapshot capture: nil store")
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	envelope := &Envelope{
		SchemaVersion: SchemaVersion,
		CreatedAt:     time.Now().UTC(),
	}

	// Section reads. Each helper translates the store-level types
	// into the snapshot-level JSON shape. Order matches the RFC's
	// section dependency walk so a future restore-in-the-same-call
	// path could write in this order naturally.
	if err := captureAgentDefs(ctx, s, &envelope.Sections.AgentDefs); err != nil {
		return nil, nil, err
	}
	if err := captureAgentDefActive(ctx, s, &envelope.Sections.AgentDefActive); err != nil {
		return nil, nil, err
	}
	if err := captureSkillDefs(ctx, s, &envelope.Sections.SkillDefs); err != nil {
		return nil, nil, err
	}
	if err := captureSkillDefActive(ctx, s, &envelope.Sections.SkillDefActive); err != nil {
		return nil, nil, err
	}
	if err := captureMCPServerDefs(ctx, s, &envelope.Sections.MCPServerDefs); err != nil {
		return nil, nil, err
	}
	if err := captureMCPServerDefActive(ctx, s, &envelope.Sections.MCPServerDefActive); err != nil {
		return nil, nil, err
	}
	if err := captureMemory(ctx, s, &envelope.Sections.Memory); err != nil {
		return nil, nil, err
	}
	if err := captureChannels(ctx, s, opts.Channels, &envelope.Sections.Channels); err != nil {
		return nil, nil, err
	}
	if err := captureEvaluations(ctx, s, &envelope.Sections.Evaluations); err != nil {
		return nil, nil, err
	}
	if err := capturePausedRuns(ctx, s, &envelope.Sections.PausedRuns); err != nil {
		return nil, nil, err
	}
	if opts.IncludeHistory {
		hist := &InteractionHistorySection{
			Version: SectionVersion,
			SinceTs: opts.IncludeHistorySince,
		}
		if err := captureInteractionHistory(ctx, s, opts.IncludeHistorySince, hist); err != nil {
			return nil, nil, err
		}
		envelope.Sections.InteractionHistory = hist
	}

	jsonBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot capture: marshal envelope: %w", err)
	}
	if int64(len(jsonBytes)) > maxBytes {
		return nil, nil, &ErrSnapshotTooLarge{
			SizeBytes: int64(len(jsonBytes)),
			MaxBytes:  maxBytes,
		}
	}

	row := &store.SnapshotRow{
		ID:            mintID(envelope.CreatedAt),
		CreatedAt:     envelope.CreatedAt,
		Label:         opts.Label,
		SchemaVersion: SchemaVersion,
		ByteSize:      int64(len(jsonBytes)),
		JSONContent:   jsonBytes,
	}
	return row, jsonBytes, nil
}

// mintID returns a snapshot id in the form "snap_<unix_ms>_<8hex>".
// Unix-ms prefix gives operators a sortable, human-readable handle
// in the list view; the random suffix collision-protects same-ms
// captures (rare but possible during scripted bulk captures).
func mintID(t time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("snap_%d_%s", t.UnixMilli(), hex.EncodeToString(buf[:]))
}

// captureAgentDefs reads every agent_defs row and translates into
// the snapshot envelope shape.
func captureAgentDefs(ctx context.Context, s store.Store, out *AgentDefsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadAgentDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot agent_defs: %w", err)
	}
	out.Entries = make([]AgentDefEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, AgentDefEntry{
			DefID:                  r.DefID,
			Name:                   r.Name,
			Version:                r.Version,
			ParentDefID:            r.ParentDefID,
			Definition:             r.Definition,
			Description:            r.Description,
			CreatedAt:              r.CreatedAt,
			CreatedByAgentID:       r.CreatedByAgentID,
			CreatedByRunID:         r.CreatedByRunID,
			Retired:                r.Retired,
			BootstrappedFromStatic: r.BootstrappedFromStatic,
			ContentSHA256:          r.ContentSHA256,
		})
	}
	return nil
}

func captureAgentDefActive(ctx context.Context, s store.Store, out *AgentDefActiveSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadAgentDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot agent_def_active: %w", err)
	}
	out.Entries = make([]AgentDefActiveEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, AgentDefActiveEntry{
			Name:              r.Name,
			DefID:             r.DefID,
			PromotedAt:        r.PromotedAt,
			PromotedByAgentID: r.PromotedByAgentID,
		})
	}
	return nil
}

// captureSkillDefs mirrors captureAgentDefs against skill_defs.
func captureSkillDefs(ctx context.Context, s store.Store, out *SkillDefsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadSkillDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot skill_defs: %w", err)
	}
	out.Entries = make([]SkillDefEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, SkillDefEntry{
			DefID:                  r.DefID,
			Name:                   r.Name,
			Version:                r.Version,
			ParentDefID:            r.ParentDefID,
			Definition:             r.Definition,
			Description:            r.Description,
			CreatedAt:              r.CreatedAt,
			CreatedByAgentID:       r.CreatedByAgentID,
			CreatedByRunID:         r.CreatedByRunID,
			Retired:                r.Retired,
			BootstrappedFromStatic: r.BootstrappedFromStatic,
			ContentSHA256:          r.ContentSHA256,
		})
	}
	return nil
}

func captureSkillDefActive(ctx context.Context, s store.Store, out *SkillDefActiveSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadSkillDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot skill_def_active: %w", err)
	}
	out.Entries = make([]SkillDefActiveEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, SkillDefActiveEntry{
			Name:              r.Name,
			DefID:             r.DefID,
			PromotedAt:        r.PromotedAt,
			PromotedByAgentID: r.PromotedByAgentID,
		})
	}
	return nil
}

// captureMCPServerDefs mirrors captureSkillDefs against mcp_server_defs.
// v0.9.x dynamic MCP server registration substrate. Additive section —
// pre-v0.9.x readers don't know about it.
func captureMCPServerDefs(ctx context.Context, s store.Store, out *MCPServerDefsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadMCPServerDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot mcp_server_defs: %w", err)
	}
	out.Entries = make([]MCPServerDefEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, MCPServerDefEntry{
			DefID:                  r.DefID,
			Name:                   r.Name,
			Version:                r.Version,
			ParentDefID:            r.ParentDefID,
			Definition:             r.Definition,
			Description:            r.Description,
			CreatedAt:              r.CreatedAt,
			CreatedByAgentID:       r.CreatedByAgentID,
			CreatedByRunID:         r.CreatedByRunID,
			Retired:                r.Retired,
			BootstrappedFromStatic: r.BootstrappedFromStatic,
			ContentSHA256:          r.ContentSHA256,
		})
	}
	return nil
}

func captureMCPServerDefActive(ctx context.Context, s store.Store, out *MCPServerDefActiveSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadMCPServerDefActive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot mcp_server_def_active: %w", err)
	}
	out.Entries = make([]MCPServerDefActiveEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, MCPServerDefActiveEntry{
			Name:              r.Name,
			DefID:             r.DefID,
			PromotedAt:        r.PromotedAt,
			PromotedByAgentID: r.PromotedByAgentID,
		})
	}
	return nil
}

// captureMemory builds the memory section. For each k/v row,
// captureEmbedding looks up the matching embedding (if any) and
// populates the wire shape. Backends without vector support
// (SQLite in v0.9.0, or Postgres without LOOMCYCLE_PGVECTOR_ENABLED)
// silently return nil → JSON null, matching the Phase 1 contract.
//
// Embedding-capture failures are fatal: a backend that can't
// answer MemoryEmbedGet for a row that should have an embedding
// indicates a corruption/consistency issue; the snapshot must
// fail loudly so operators investigate rather than silently
// shipping a degraded archive.
func captureMemory(ctx context.Context, s store.Store, out *MemorySection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadMemory(ctx)
	if err != nil {
		return fmt.Errorf("snapshot memory: %w", err)
	}
	out.Entries = make([]MemoryEntry, 0, len(rows))
	for _, r := range rows {
		emb, err := captureEmbedding(ctx, s, r.Scope, r.ScopeID, r.Key)
		if err != nil {
			return fmt.Errorf("snapshot memory embedding: %w", err)
		}
		entry := MemoryEntry{
			Scope:     string(r.Scope),
			ScopeID:   r.ScopeID,
			Key:       r.Key,
			Value:     r.Value,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Embedding: emb,
		}
		if !r.ExpiresAt.IsZero() {
			t := r.ExpiresAt
			entry.ExpiresAt = &t
		}
		out.Entries = append(out.Entries, entry)
	}
	return nil
}

func captureChannels(ctx context.Context, s store.Store, cfg []ChannelConfigEntry, out *ChannelsSection) error {
	out.Version = SectionVersion
	if cfg == nil {
		out.Config = []ChannelConfigEntry{}
	} else {
		out.Config = cfg
	}
	msgs, err := s.SnapshotReadChannelMessages(ctx)
	if err != nil {
		return fmt.Errorf("snapshot channels.messages: %w", err)
	}
	out.Messages = make([]ChannelMessageEntry, 0, len(msgs))
	for _, m := range msgs {
		entry := ChannelMessageEntry{
			ID:                m.ID,
			Channel:           m.Channel,
			Scope:             string(m.Scope),
			ScopeID:           m.ScopeID,
			Payload:           m.Payload,
			PublishedAt:       m.PublishedAt,
			PublishedByUserID: m.PublishedByUserID,
		}
		if !m.ExpiresAt.IsZero() {
			t := m.ExpiresAt
			entry.ExpiresAt = &t
		}
		if !m.VisibleAt.IsZero() {
			t := m.VisibleAt
			entry.VisibleAt = &t
		}
		out.Messages = append(out.Messages, entry)
	}
	cursors, err := s.SnapshotReadChannelCursors(ctx)
	if err != nil {
		return fmt.Errorf("snapshot channels.cursors: %w", err)
	}
	out.Cursors = make([]ChannelCursorEntry, 0, len(cursors))
	for _, c := range cursors {
		out.Cursors = append(out.Cursors, ChannelCursorEntry{
			Channel:   c.Channel,
			Scope:     string(c.Scope),
			ScopeID:   c.ScopeID,
			Cursor:    c.Cursor,
			UpdatedAt: c.UpdatedAt,
		})
	}
	return nil
}

func captureEvaluations(ctx context.Context, s store.Store, out *EvaluationsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadEvaluations(ctx)
	if err != nil {
		return fmt.Errorf("snapshot evaluations: %w", err)
	}
	out.Entries = make([]EvaluationEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, EvaluationEntry{
			EvalID:         r.EvalID,
			RunID:          r.RunID,
			DefID:          r.DefID,
			Score:          r.Score,
			Dimensions:     r.Dimensions,
			Judgement:      r.Judgement,
			Rationale:      r.Rationale,
			EmitterRole:    r.EmitterRole,
			EmitterAgentID: r.EmitterAgentID,
			EmitterRunID:   r.EmitterRunID,
			CreatedAt:      r.CreatedAt,
		})
	}
	return nil
}

// capturePausedRuns reads runs with pause_state='paused' + their
// transcript events. Each run's transcript is filtered from its
// session transcript by run_id; the resulting events are what the
// model sees on resume.
func capturePausedRuns(ctx context.Context, s store.Store, out *PausedRunsSection) error {
	out.Version = SectionVersion
	runs, err := s.ListPausedRuns(ctx)
	if err != nil {
		return fmt.Errorf("snapshot paused_runs: %w", err)
	}
	out.Entries = make([]PausedRunEntry, 0, len(runs))
	for _, r := range runs {
		entry := PausedRunEntry{
			RunID:         r.ID,
			AgentID:       r.AgentID,
			ParentAgentID: r.ParentAgentID,
			UserID:        r.UserID,
			UserTier:      r.UserTier,
			Agent:         r.Agent,
			AgentDefID:    r.AgentDefID,
			SessionID:     r.SessionID,
			StartedAt:     r.StartedAt,
			Model:         r.Model,
			PauseState:    r.PauseState,
			Interactive:   r.Interactive,           // F42: re-dispatch with correct park-vs-complete semantics
			ParentContext: r.ParentContext.Clone(), // v0.12.x: survive pause→snapshot→restore
		}
		// Read the session transcript and filter by run_id. Cost:
		// O(events-in-session). For long-running sessions this is
		// the largest single read in the snapshot; the maxBytes cap
		// catches runaway payloads at marshal time.
		//
		// Per-run transcript reads are best-effort: a single corrupt
		// row or transient DB error MUST NOT abort the whole capture.
		// Record the per-run error on the entry and continue with the
		// remaining runs + sections. Operators inspecting
		// TranscriptError can re-capture or recover manually.
		events, terr := s.GetTranscript(ctx, r.SessionID)
		if terr != nil {
			entry.TranscriptError = terr.Error()
			entry.TranscriptEvents = []TranscriptEvent{}
			out.Entries = append(out.Entries, entry)
			continue
		}
		entry.TranscriptEvents = make([]TranscriptEvent, 0)
		for _, e := range events {
			if e.RunID != r.ID {
				continue
			}
			entry.TranscriptEvents = append(entry.TranscriptEvents, TranscriptEvent{
				Seq:     e.Seq,
				TsNs:    e.Timestamp.UnixNano(),
				Type:    e.Type,
				Payload: e.Payload,
			})
		}
		out.Entries = append(out.Entries, entry)
	}
	return nil
}

// captureInteractionHistory reads events ts >= sinceTs across all
// runs and sessions. Cost: O(events-in-store). Operators opt in
// per-capture; without opt-in the section is omitted entirely
// (omitempty on the InteractionHistory pointer in Sections).
//
// Phase 1 limitation: this reads every session's transcript and
// filters in-process. A future GetEventsSinceTs(ctx, since) store
// method would scope the read at the SQL level; that's a v0.9.x
// optimisation when interaction_history captures become routine.
func captureInteractionHistory(ctx context.Context, s store.Store, sinceTs time.Time, out *InteractionHistorySection) error {
	// Phase 1 simplification: this is implemented as a TODO with an
	// empty events slice. The architect blueprint flagged the
	// missing "GetEventsSinceTs" bulk-reader; rather than block
	// PR 2 on adding it, we ship the section shape with a documented
	// gap. PR 3 (restore) doesn't need this section to function;
	// it's strictly opt-in.
	//
	// When a consumer needs interaction_history, two implementation
	// paths exist:
	//   (a) Add SnapshotReadEventsSinceTs to the Store interface
	//       and a per-backend impl. Cheapest read; cleanest seam.
	//   (b) Iterate ListPausedRuns + per-session GetTranscript and
	//       filter in-process. Works today but O(sessions × events).
	// Defer the choice until a consumer surfaces the need.
	out.Events = []TranscriptEvent{}
	return nil
}
