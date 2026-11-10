package snapshot

import (
	"context"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// evaluations (no FK enforced — runs.agent_def_id is
// denormalised at submit time; runs may not even exist on the
// restoring host)
func restoreEvaluationsSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionEvaluations]; ok {
		var sec EvaluationsSection
		if err := decodeWithMigration(migrations.SectionEvaluations, rawSection, &sec); err != nil {
			return err
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
	return nil
}

// paused_runs (after sessions are synthesized — FK on session_id)
func restorePausedRunsSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionPausedRuns]; ok {
		var sec PausedRunsSection
		if err := decodeWithMigration(migrations.SectionPausedRuns, rawSection, &sec); err != nil {
			return err
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
				TenantID:  e.TenantID,
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

			// An entry from a snapshot older than the run's tenant field
			// restores under its session's tenant — the rule the runs
			// tenant column was backfilled with. That session is the one
			// already in the target store when there is one (the insert
			// above is a no-op on a conflict), else the synthesized "".
			tenantID := e.TenantID
			if tenantID == "" {
				sess, err := s.GetSession(ctx, sessionID)
				if err != nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: read session %s for its tenant: %v", e.RunID, sessionID, err))
					continue
				}
				tenantID = sess.TenantID
			}

			runRow := store.Run{
				ID:            e.RunID,
				SessionID:     sessionID,
				TenantID:      tenantID,
				Status:        store.RunRunning, // resume sets terminal status on completion
				StartedAt:     e.StartedAt,
				Model:         e.Model,
				AgentID:       e.AgentID,
				ParentAgentID: e.ParentAgentID,
				ParentRunID:   e.ParentRunID,
				UserID:        e.UserID,
				UserTier:      e.UserTier,
				AgentDefID:    e.AgentDefID,
				PauseState:    e.PauseState,
				Interactive:   e.Interactive,   // F42: park-vs-complete semantics on re-dispatch
				ParentContext: e.ParentContext, // v0.12.x: restore the run's tracking lineage
				RunConfig:     e.RunConfig,     // resume on the run's own settings, not the def's current ones

				// Resume reads the confinement bits from this row.
				OperatorKeyRestricted:  e.OperatorKeyRestricted,
				Isolated:               e.Isolated,
				CredentialsNoUserScope: e.CredentialsNoUserScope,

				IdempotencyKey: e.IdempotencyKey,
				DeliveryAltKey: e.DeliveryAltKey,
			}
			// The keys are unique across the target. If another run here
			// already holds one — the same request was retried on this
			// instance before the restore — the run still comes back, without
			// its keys: losing the run would be worse than losing its claim.
			if keys := nonEmpty(e.IdempotencyKey, e.DeliveryAltKey); len(keys) > 0 {
				held, found, err := s.RunByDeliveryKeys(ctx, keys)
				switch {
				case err != nil:
					result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: check its dedup keys: %v — restored without them", e.RunID, err))
					runRow.IdempotencyKey, runRow.DeliveryAltKey = "", ""
				case found && held.ID != e.RunID:
					result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: run %s on this target already holds its dedup key — restored without it", e.RunID, held.ID))
					runRow.IdempotencyKey, runRow.DeliveryAltKey = "", ""
				}
			}
			runInserted, err := s.SnapshotRestoreRun(ctx, runRow)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: %v", e.RunID, err))
				continue
			}
			if runInserted {
				result.PausedRunsRestored++
			}

			// Transcript events for the run, written as one batch in captured
			// order under seqs this store mints. The captured seq is the
			// source's key in a table-global sequence and is not carried.
			if len(e.TranscriptEvents) > 0 {
				evs := make([]store.Event, 0, len(e.TranscriptEvents))
				for _, te := range e.TranscriptEvents {
					evs = append(evs, store.Event{
						Timestamp: time.Unix(0, te.TsNs),
						Type:      te.Type,
						Payload:   te.Payload,
					})
				}
				written, err := s.SnapshotRestoreRunEvents(ctx, e.RunID, sessionID, evs)
				switch {
				case err != nil:
					result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: its %d captured transcript events were not written: %v", e.RunID, len(evs), err))
				case written < len(evs):
					result.Warnings = append(result.Warnings, fmt.Sprintf("paused_run %s: its %d captured transcript events were not written: the run already has a transcript on this target", e.RunID, len(evs)-written))
				}
				result.TranscriptEventsRestored += written
			}
		}
	}
	return nil
}

// queued_runs (after sessions; absent from snapshots older than it)
func restoreQueuedRunsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionQueuedRuns]; ok {
		var sec QueuedRunsSection
		if err := decodeWithMigration(migrations.SectionQueuedRuns, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			restoreQueuedRun(ctx, s, e, opts.ReplicaID, result)
		}
	}
	return nil
}

// interaction_history (optional, opt-in via RestoreOptions)
func restoreInteractionHistorySection(ctx context.Context, rs *restoreRun) error {
	opts, sections, result := rs.opts, rs.sections, rs.result
	if opts.IncludeHistory {
		if rawSection, ok := sections[migrations.SectionInteractionHistory]; ok {
			var sec InteractionHistorySection
			if err := decodeWithMigration(migrations.SectionInteractionHistory, rawSection, &sec); err != nil {
				return err
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
	return nil
}
