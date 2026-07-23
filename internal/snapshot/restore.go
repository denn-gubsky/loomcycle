package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RestoreOptions controls the Restore() behaviour.
type RestoreOptions struct {
	// IncludeHistory toggles restoring the optional
	// interaction_history section. False (default) skips it even
	// when present in the envelope — operators often want
	// running-state restore only (paused runs + their transcripts +
	// Memory + Channels + AgentDefs + Evaluations) without
	// pollution from archived sessions.
	IncludeHistory bool

	// ForceProbe, when non-nil, is called by Restore after section
	// writes complete. The pause-resume-snapshot RFC says the
	// resolver matrix is excluded from snapshots — restore triggers
	// an immediate probe so the matrix is populated before the
	// operator calls Resume. Pass nil to skip.
	ForceProbe func(ctx context.Context)

	// SqlMem restores the RFC AA Phase-3e SQL Memory facet when non-nil. nil
	// (SQL Memory disabled on the restoring host) skips the section with a
	// warning. The call sites pass the runtime's *sqlmem.Manager.
	SqlMem SqlMemSnapshotter
}

// RestoreResult is the operator-facing summary of a Restore() call.
// Warnings carry every non-fatal anomaly the restore encountered:
// synthesized sessions, dropped expired rows, skipped sections, etc.
// Operators read warnings to decide whether the restore is "clean
// enough" to call Resume.
type RestoreResult struct {
	AgentDefsRestored          int      `json:"agent_defs_restored"`
	AgentDefActiveRestored     int      `json:"agent_def_active_restored"`
	SkillDefsRestored          int      `json:"skill_defs_restored"`
	SkillDefActiveRestored     int      `json:"skill_def_active_restored"`
	TeamDefsRestored           int      `json:"team_defs_restored"`
	TeamDefActiveRestored      int      `json:"team_def_active_restored"`
	MCPServerDefsRestored      int      `json:"mcp_server_defs_restored"`
	MCPServerDefActiveRestored int      `json:"mcp_server_def_active_restored"`
	MemoryRestored             int      `json:"memory_restored"`
	ChannelMessagesRestored    int      `json:"channel_messages_restored"`
	ChannelCursorsRestored     int      `json:"channel_cursors_restored"`
	EvaluationsRestored        int      `json:"evaluations_restored"`
	PausedRunsRestored         int      `json:"paused_runs_restored"`
	SynthesizedSessions        int      `json:"synthesized_sessions"`
	TranscriptEventsRestored   int      `json:"transcript_events_restored"`
	InteractionHistoryRestored int      `json:"interaction_history_restored"`
	SqlMemScopesRestored       int      `json:"sqlmem_scopes_restored"`
	Warnings                   []string `json:"warnings,omitempty"`
}

// Restore reads a canonical JSON envelope (Export's output shape),
// migrates each section per the migrations registry, and writes
// rows back into the store via the SnapshotRestore* methods. The
// restore order matches the section FK dependency graph:
//
//	agent_defs        → agent_def_active (FK: name → agent_defs.def_id)
//	(sessions synth)  → paused_runs       (FK: session_id → sessions.id)
//	                  → transcript events (FK: run_id    → runs.id)
//	channels.messages, .cursors
//	evaluations       (no FKs)
//	interaction_history (optional)
//
// Idempotent: a second Restore call on the same envelope is a clean
// no-op (every SnapshotRestore* method uses ON CONFLICT DO NOTHING /
// INSERT OR IGNORE).
//
// Critical correctness detail (the architect's catch in PR 3
// planning): the snapshot's paused_runs section references
// session_id values, but sessions are NOT a captured section.
// Restore synthesizes a minimal session row per paused run with
// ID = "snap_sess_" + run.ID — deterministic so a re-run produces
// the same synthetic ID. RestoreResult.SynthesizedSessions counts
// these for operator visibility.
//
// Returns *migrations.ErrSnapshotVersionTooNew when any section's
// version is newer than the reader's CurrentVersion. Returns
// *migrations.ErrUnknownSectionVersion for corrupted / unsupported
// snapshots. Both errors carry the section + version strings for
// operator-actionable messaging.
func Restore(ctx context.Context, s store.Store, raw []byte, opts RestoreOptions) (RestoreResult, error) {
	if s == nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: nil store")
	}
	if len(raw) == 0 {
		return RestoreResult{}, fmt.Errorf("snapshot restore: empty input")
	}

	// Stage 1: deserialise the outer envelope to discover sections
	// and their declared versions. We deserialise via a deferred
	// map[string]json.RawMessage rather than directly into Envelope
	// so each section's raw bytes can pass through Migrate() before
	// being typed-decoded — that's the migration mechanism's hook.
	var outer struct {
		SchemaVersion int             `json:"schema_version"`
		CreatedAt     time.Time       `json:"created_at"`
		Sections      json.RawMessage `json:"sections"`
		Checksum      string          `json:"checksum,omitempty"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: parse envelope: %w", err)
	}
	if outer.SchemaVersion > SchemaVersion {
		return RestoreResult{}, fmt.Errorf("snapshot restore: envelope schema_version %d is newer than reader %d", outer.SchemaVersion, SchemaVersion)
	}

	// exp7 I4: verify the integrity digest BEFORE any decode/insert, when
	// present. outer.Sections holds the raw "sections" bytes exactly as they
	// appear in the document (json.RawMessage), so hashing them reproduces
	// Export's digest. A snapshot without a checksum (captured before I4)
	// skips verification and restores unchanged — additive, not a gate.
	if outer.Checksum != "" {
		if got := sectionChecksum(outer.Sections); got != outer.Checksum {
			return RestoreResult{}, fmt.Errorf("snapshot restore: checksum mismatch (want %s, got %s) — snapshot is truncated or tampered", outer.Checksum, got)
		}
	}

	// Decode the section map from the raw sections bytes for the per-section
	// migration + insert logic below.
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(outer.Sections, &sections); err != nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: parse sections: %w", err)
	}

	result := RestoreResult{}

	// Stage 2: per-section migration + decode + insert. Order
	// matters for FK reasons (agent_defs before agent_def_active;
	// sessions before paused_runs).

	// agent_defs
	if rawSection, ok := sections[migrations.SectionAgentDefs]; ok {
		var sec AgentDefsSection
		if err := decodeWithMigration(migrations.SectionAgentDefs, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreAgentDef(ctx, store.AgentDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("agent_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.AgentDefsRestored++
			}
		}
	}

	// agent_def_active (after agent_defs for FK)
	if rawSection, ok := sections[migrations.SectionAgentDefActive]; ok {
		var sec AgentDefActiveSection
		if err := decodeWithMigration(migrations.SectionAgentDefActive, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreAgentDefActive(ctx, store.AgentDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("agent_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.AgentDefActiveRestored++
			}
		}
	}

	// skill_defs (v0.8.22) — mirror of agent_defs restore
	if rawSection, ok := sections[migrations.SectionSkillDefs]; ok {
		var sec SkillDefsSection
		if err := decodeWithMigration(migrations.SectionSkillDefs, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreSkillDef(ctx, store.SkillDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("skill_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.SkillDefsRestored++
			}
		}
	}

	// skill_def_active (after skill_defs for FK)
	if rawSection, ok := sections[migrations.SectionSkillDefActive]; ok {
		var sec SkillDefActiveSection
		if err := decodeWithMigration(migrations.SectionSkillDefActive, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreSkillDefActive(ctx, store.SkillDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("skill_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.SkillDefActiveRestored++
			}
		}
	}

	// team_defs — mirror of skill_defs restore
	if rawSection, ok := sections[migrations.SectionTeamDefs]; ok {
		var sec TeamDefsSection
		if err := decodeWithMigration(migrations.SectionTeamDefs, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreTeamDef(ctx, store.TeamDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("team_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.TeamDefsRestored++
			}
		}
	}

	// team_def_active (after team_defs for FK)
	if rawSection, ok := sections[migrations.SectionTeamDefActive]; ok {
		var sec TeamDefActiveSection
		if err := decodeWithMigration(migrations.SectionTeamDefActive, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("team_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.TeamDefActiveRestored++
			}
		}
	}

	// mcp_server_defs (v0.9.x) — mirror of agent_defs / skill_defs restore
	if rawSection, ok := sections[migrations.SectionMCPServerDefs]; ok {
		var sec MCPServerDefsSection
		if err := decodeWithMigration(migrations.SectionMCPServerDefs, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreMCPServerDef(ctx, store.MCPServerDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("mcp_server_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.MCPServerDefsRestored++
			}
		}
	}

	// mcp_server_def_active (after mcp_server_defs for FK)
	if rawSection, ok := sections[migrations.SectionMCPServerDefActive]; ok {
		var sec MCPServerDefActiveSection
		if err := decodeWithMigration(migrations.SectionMCPServerDefActive, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreMCPServerDefActive(ctx, store.MCPServerDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("mcp_server_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.MCPServerDefActiveRestored++
			}
		}
	}

	// memory (no FK; embedding field carried but Phase 1 always
	// null; Phase 2 will populate)
	if rawSection, ok := sections[migrations.SectionMemory]; ok {
		var sec MemorySection
		if err := decodeWithMigration(migrations.SectionMemory, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			var expires time.Time
			if e.ExpiresAt != nil {
				expires = *e.ExpiresAt
			}
			entry := store.MemorySnapshotEntry{
				Scope:   store.MemoryScope(e.Scope),
				ScopeID: e.ScopeID,
				MemoryEntry: store.MemoryEntry{
					Key:       e.Key,
					Value:     e.Value,
					ExpiresAt: expires,
					CreatedAt: e.CreatedAt,
					UpdatedAt: e.UpdatedAt,
				},
			}
			inserted, err := s.SnapshotRestoreMemory(ctx, entry)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("memory %s/%s/%s: %v", e.Scope, e.ScopeID, e.Key, err))
				continue
			}
			if inserted {
				result.MemoryRestored++
			}
			// v0.9.0 Vector Memory: when the snapshot carries an
			// embedding, write it after the base row lands. Skipped
			// silently (with a warning) when the restoring backend
			// has no vector support — the k/v row still restores,
			// just without the searchable embedding. Operators can
			// re-embed via /v1/_memory/reembed once they've enabled
			// pgvector on the new backend.
			if e.Embedding != nil {
				if !s.SupportsVectors() {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("memory %s/%s/%s: embedding dropped (target backend has no vector support; enable LOOMCYCLE_PGVECTOR_ENABLED then re-embed via /v1/_memory/reembed)",
							e.Scope, e.ScopeID, e.Key))
					continue
				}
				vec, err := decodeFloat32LEBase64(e.Embedding.Vector)
				if err != nil {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("memory %s/%s/%s embedding: %v", e.Scope, e.ScopeID, e.Key, err))
					continue
				}
				if len(vec) != e.Embedding.Dimension {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("memory %s/%s/%s embedding: decoded %d floats, expected dimension %d",
							e.Scope, e.ScopeID, e.Key, len(vec), e.Embedding.Dimension))
					continue
				}
				emb := store.MemoryEmbedding{
					Provider:  e.Embedding.Provider,
					Model:     e.Embedding.Model,
					Dimension: e.Embedding.Dimension,
					Vector:    vec,
					EmbedText: e.Embedding.EmbedText,
					CreatedAt: e.Embedding.CreatedAt,
				}
				if err := s.MemoryEmbedSet(ctx, "", entry.Scope, entry.ScopeID, e.Key, emb); err != nil {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("memory %s/%s/%s embedding write: %v", e.Scope, e.ScopeID, e.Key, err))
				}
			}
		}
	}

	// channels (config + messages + cursors). Config is operator-
	// yaml-side; restore doesn't write it back into a table — the
	// operator on the restoring host reconciles against their own
	// yaml. Messages + cursors get raw inserts.
	if rawSection, ok := sections[migrations.SectionChannels]; ok {
		var sec ChannelsSection
		if err := decodeWithMigration(migrations.SectionChannels, rawSection, &sec); err != nil {
			return result, err
		}
		if len(sec.Config) > 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("channels.config: %d declared channels in snapshot; reconcile against operator yaml on the restoring host", len(sec.Config)))
		}
		for _, m := range sec.Messages {
			var expires, visible time.Time
			if m.ExpiresAt != nil {
				expires = *m.ExpiresAt
			}
			if m.VisibleAt != nil {
				visible = *m.VisibleAt
			}
			inserted, err := s.SnapshotRestoreChannelMessage(ctx, store.ChannelMessage{
				ID:                m.ID,
				Channel:           m.Channel,
				Scope:             store.MemoryScope(m.Scope),
				ScopeID:           m.ScopeID,
				Payload:           m.Payload,
				PublishedAt:       m.PublishedAt,
				ExpiresAt:         expires,
				VisibleAt:         visible,
				PublishedByUserID: m.PublishedByUserID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("channel_message %s: %v", m.ID, err))
				continue
			}
			if inserted {
				result.ChannelMessagesRestored++
			}
		}
		for _, c := range sec.Cursors {
			inserted, err := s.SnapshotRestoreChannelCursor(ctx, store.ChannelCursorEntry{
				Channel:   c.Channel,
				Scope:     store.MemoryScope(c.Scope),
				ScopeID:   c.ScopeID,
				Cursor:    c.Cursor,
				UpdatedAt: c.UpdatedAt,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("channel_cursor %s/%s/%s: %v", c.Channel, c.Scope, c.ScopeID, err))
				continue
			}
			if inserted {
				result.ChannelCursorsRestored++
			}
		}
	}

	// evaluations (no FK enforced — runs.agent_def_id is
	// denormalised at submit time; runs may not even exist on the
	// restoring host)
	if rawSection, ok := sections[migrations.SectionEvaluations]; ok {
		var sec EvaluationsSection
		if err := decodeWithMigration(migrations.SectionEvaluations, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			inserted, err := s.SnapshotRestoreEvaluation(ctx, store.EvaluationRow{
				EvalID:         e.EvalID,
				RunID:          e.RunID,
				DefID:          e.DefID,
				Score:          e.Score,
				Dimensions:     e.Dimensions,
				Judgement:      e.Judgement,
				Rationale:      e.Rationale,
				EmitterRole:    e.EmitterRole,
				EmitterAgentID: e.EmitterAgentID,
				EmitterRunID:   e.EmitterRunID,
				CreatedAt:      e.CreatedAt,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("evaluation %s: %v", e.EvalID, err))
				continue
			}
			if inserted {
				result.EvaluationsRestored++
			}
		}
	}

	// paused_runs (after sessions are synthesized — FK on session_id)
	if rawSection, ok := sections[migrations.SectionPausedRuns]; ok {
		var sec PausedRunsSection
		if err := decodeWithMigration(migrations.SectionPausedRuns, rawSection, &sec); err != nil {
			return result, err
		}
		for _, e := range sec.Entries {
			// Critical: synthesize a session row for this run BEFORE
			// inserting the run. The pause-resume-snapshot RFC
			// doesn't capture sessions (architect's catch); without
			// this step, every restore errors with FK violation on
			// runs.session_id.
			synthesizedID := "snap_sess_" + e.RunID
			sessionID := e.SessionID
			if sessionID == "" {
				sessionID = synthesizedID
			}
			synthSession := store.Session{
				ID:        sessionID,
				TenantID:  "",
				Agent:     e.Agent,
				CreatedAt: e.StartedAt,
				UserID:    e.UserID,
			}
			sessInserted, err := s.SnapshotRestoreSession(ctx, synthSession)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("synthesized session %s: %v", sessionID, err))
				continue
			}
			// The architect's blueprint flagged this as the load-
			// bearing correctness detail — count synthesized
			// sessions explicitly for operator visibility. Only
			// count when an INSERT actually happened so a re-restore
			// reads as "0 synthesized" rather than re-reporting
			// the first restore's count.
			if sessInserted && (e.SessionID == "" || e.SessionID == synthesizedID) {
				result.SynthesizedSessions++
			}

			runRow := store.Run{
				ID:            e.RunID,
				SessionID:     sessionID,
				Status:        store.RunRunning, // resume sets terminal status on completion
				StartedAt:     e.StartedAt,
				Model:         e.Model,
				AgentID:       e.AgentID,
				ParentAgentID: e.ParentAgentID,
				UserID:        e.UserID,
				UserTier:      e.UserTier,
				AgentDefID:    e.AgentDefID,
				PauseState:    e.PauseState,
				Interactive:   e.Interactive,   // F42: park-vs-complete semantics on re-dispatch
				ParentContext: e.ParentContext, // v0.12.x: restore the run's tracking lineage
			}
			runInserted, err := s.SnapshotRestoreRun(ctx, runRow)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: %v", e.RunID, err))
				continue
			}
			if runInserted {
				result.PausedRunsRestored++
			}

			// Transcript events for the run.
			for _, te := range e.TranscriptEvents {
				evtInserted, err := s.SnapshotRestoreEvent(ctx, store.Event{
					Seq:       te.Seq,
					SessionID: sessionID,
					RunID:     e.RunID,
					Timestamp: time.Unix(0, te.TsNs),
					Type:      te.Type,
					Payload:   te.Payload,
				})
				if err != nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf("transcript event run=%s seq=%d: %v", e.RunID, te.Seq, err))
					continue
				}
				if evtInserted {
					result.TranscriptEventsRestored++
				}
			}
		}
	}

	// interaction_history (optional, opt-in via RestoreOptions)
	if opts.IncludeHistory {
		if rawSection, ok := sections[migrations.SectionInteractionHistory]; ok {
			var sec InteractionHistorySection
			if err := decodeWithMigration(migrations.SectionInteractionHistory, rawSection, &sec); err != nil {
				return result, err
			}
			// Phase 1 limitation: snapshot.Capture's interaction_history
			// section is a stub (always empty events). When PR 3.x
			// or 1.5 adds the store-side bulk event reader and the
			// snapshot package populates this section, the restore
			// path here writes events back. Today there's nothing
			// to write, but we leave the loop in place for forward
			// compatibility.
			result.InteractionHistoryRestored = len(sec.Events)
			if len(sec.Events) > 0 {
				result.Warnings = append(result.Warnings,
					"interaction_history events in envelope: Phase 1 restore does not write these back yet; see snapshot.go captureInteractionHistory")
			}
		}
	} else if _, ok := sections[migrations.SectionInteractionHistory]; ok {
		result.Warnings = append(result.Warnings,
			"interaction_history section present in snapshot but RestoreOptions.IncludeHistory=false; skipped")
	}

	// sqlmem (RFC AA Phase 3e) — replayed through the live SQL Memory manager
	// (its own per-scope DBs, not the main store). Restored only when a manager
	// is wired AND the archive's tier matches; absent/disabled is a warning.
	if rawSection, ok := sections[migrations.SectionSqlMem]; ok {
		if opts.SqlMem == nil {
			result.Warnings = append(result.Warnings,
				"sqlmem section present in snapshot but SQL Memory is not enabled on this host; skipped")
		} else {
			var sec SqlMemSection
			if err := decodeWithMigration(migrations.SectionSqlMem, rawSection, &sec); err != nil {
				return result, err
			}
			restoreSqlMem(ctx, opts.SqlMem, &sec, &result)
		}
	}

	// Stage 3: trigger an immediate resolver probe so the matrix is
	// populated before the operator calls Resume. Done last so any
	// errors above are returned before the resolver work.
	if opts.ForceProbe != nil {
		opts.ForceProbe(ctx)
	}

	return result, nil
}

// decodeWithMigration runs the raw section bytes through the
// per-section migration registry then JSON-decodes into the typed
// destination. Returns the migration error verbatim (operator gets
// the version + section + suggested action).
func decodeWithMigration(section string, raw json.RawMessage, dst any) error {
	// Peek at the section's version. The section wrapper has
	// {"version": "1.0", ...} as its outer shape.
	var versioned struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &versioned); err != nil {
		return fmt.Errorf("snapshot section %s: parse version: %w", section, err)
	}
	if versioned.Version == "" {
		// Tolerant default — pre-versioned sections (corrupted /
		// hand-written) get current-version semantics. Future-strict
		// readers may flip this to refuse.
		versioned.Version = migrations.CurrentVersion
	}
	migrated, err := migrations.Migrate(section, versioned.Version, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(migrated, dst); err != nil {
		return fmt.Errorf("snapshot section %s: decode migrated bytes: %w", section, err)
	}
	return nil
}
