package snapshot

import (
	"context"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memory (no FK; embedding field carried but Phase 1 always
// null; Phase 2 will populate)
func restoreMemorySection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionMemory]; ok {
		var sec MemorySection
		if err := decodeWithMigration(migrations.SectionMemory, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			var expires time.Time
			if e.ExpiresAt != nil {
				expires = *e.ExpiresAt
			}
			// A snapshot written before these fields existed decodes them as
			// nil, which leaves the zero instant — undated, the honest answer.
			var observed, validAt, invalidAt time.Time
			if e.ObservedAt != nil {
				observed = *e.ObservedAt
			}
			if e.ValidAt != nil {
				validAt = *e.ValidAt
			}
			if e.InvalidAt != nil {
				invalidAt = *e.InvalidAt
			}
			entry := store.MemorySnapshotEntry{
				// RFC BL: restore into the row's tenant partition. An older
				// snapshot with no tenant_id decodes e.TenantID as "" (the
				// legacy tenant), preserving cross-version restore.
				TenantID: e.TenantID,
				Scope:    store.MemoryScope(e.Scope),
				// An older snapshot's tenant document rows move to the tenant
				// keyspace (tenant_document_keyspace.go).
				ScopeID: restoredMemoryScopeID(e.TenantID, store.MemoryScope(e.Scope), e.ScopeID, e.Key),
				MemoryEntry: store.MemoryEntry{
					Key:        e.Key,
					Value:      e.Value,
					ExpiresAt:  expires,
					CreatedAt:  e.CreatedAt,
					UpdatedAt:  e.UpdatedAt,
					ObservedAt: observed,
					ValidAt:    validAt,
					InvalidAt:  invalidAt,
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
				if err := s.MemoryEmbedSet(ctx, entry.TenantID, entry.Scope, entry.ScopeID, e.Key, emb); err != nil {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("memory %s/%s/%s embedding write: %v", e.Scope, e.ScopeID, e.Key, err))
				}
			}
		}
	}
	return nil
}

// memory_pending: after memory and the memory-backend defs, before any
// resumed run. Each row lands un-drained and unleased (the per-target
// lease never travels), so the target's consolidator picks it up.
func restoreMemoryPendingSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionMemoryPending]; ok {
		var sec MemoryPendingSection
		if err := decodeWithMigration(migrations.SectionMemoryPending, rawSection, &sec); err != nil {
			return err
		}
		restoreMemoryPending(ctx, s, &sec, result)
	}
	return nil
}

// sqlmem (RFC AA Phase 3e) — replayed through the live SQL Memory manager
// (its own per-scope DBs, not the main store). Restored only when a manager
// is wired AND the archive's tier matches; absent/disabled is a warning.
func restoreSqlMemSection(ctx context.Context, rs *restoreRun) error {
	opts, sections, result := rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionSqlMem]; ok {
		if opts.SqlMem == nil {
			result.Warnings = append(result.Warnings,
				"sqlmem section present in snapshot but SQL Memory is not enabled on this host; skipped")
		} else {
			var sec SqlMemSection
			if err := decodeWithMigration(migrations.SectionSqlMem, rawSection, &sec); err != nil {
				return err
			}
			restoreSqlMem(ctx, opts.SqlMem, &sec, result)
		}
	}
	return nil
}

// dirents: LAST, once everything a name can point at has landed — the
// documents (memory + sqlmem), memory entries and volumes — so a name
// whose target did not arrive is found and skipped, never left dangling.
func restoreDirentsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionDirents]; ok {
		var sec DirentsSection
		if err := decodeWithMigration(migrations.SectionDirents, rawSection, &sec); err != nil {
			return err
		}
		restoreDirents(ctx, s, &sec, opts, result)
	}
	return nil
}
