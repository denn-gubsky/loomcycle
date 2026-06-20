package snapshot

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// sqlmem.go — RFC AA Phase 3e: the SQL Memory facet of the snapshot envelope.
// SQL Memory lives in its own per-scope databases (separate from the main
// store), so it is captured/restored through a small interface the *sqlmem.Manager
// satisfies, kept optional (nil ⇒ the section is absent) so a runtime without
// SQL Memory is byte-identical to the pre-3e envelope.

// SqlMemSnapshotter is the SQL Memory capability the snapshot needs.
// *sqlmem.Manager implements it; Capture/Restore take it via the options so a
// runtime without SQL Memory passes nil and the section never appears.
type SqlMemSnapshotter interface {
	// Tier reports the storage tier ("sqlite" | "postgres"); stamped on the
	// section and checked on restore (logical dumps are tier-specific).
	Tier() string
	// ListScopes enumerates the durable scopes to capture.
	ListScopes(ctx context.Context) ([]sqlmem.ScopeKey, error)
	// ExportScope produces one scope's logical dump (DDL + data).
	ExportScope(ctx context.Context, key sqlmem.ScopeKey) (*sqlmem.ScopeDump, error)
	// RestoreScope materializes a scope from a dump (idempotent).
	RestoreScope(ctx context.Context, key sqlmem.ScopeKey, dump *sqlmem.ScopeDump) error
}

// captureSqlMem dumps every durable scope into the section shape. A failure on
// any scope aborts the capture (loud, like captureMemory's embedding rule) so an
// operator never ships a silently-partial SQL Memory archive.
func captureSqlMem(ctx context.Context, snap SqlMemSnapshotter) (*SqlMemSection, error) {
	keys, err := snap.ListScopes(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot sqlmem: list scopes: %w", err)
	}
	sec := &SqlMemSection{
		Version: SectionVersion,
		Tier:    snap.Tier(),
		Scopes:  make([]SqlMemScope, 0, len(keys)),
	}
	for _, k := range keys {
		dump, err := snap.ExportScope(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("snapshot sqlmem: export %s/%s/%s: %w", k.Tenant, k.Scope, k.ScopeID, err)
		}
		sc := SqlMemScope{
			Tenant:  k.Tenant,
			Scope:   k.Scope,
			ScopeID: k.ScopeID,
			DDL:     dump.DDL,
			PostDDL: dump.PostDDL,
			Tables:  make([]SqlMemTable, 0, len(dump.Tables)),
		}
		for _, t := range dump.Tables {
			sc.Tables = append(sc.Tables, SqlMemTable{
				Name:        t.Name,
				Columns:     t.Columns,
				ColumnTypes: t.ColumnTypes,
				Rows:        t.Rows,
			})
		}
		sec.Scopes = append(sec.Scopes, sc)
	}
	return sec, nil
}

// restoreSqlMem replays each scope's dump through the manager. A tier mismatch
// skips the whole section (logical dumps don't move across tiers); a per-scope
// failure is a warning, not fatal, so one bad scope can't abort a restore.
func restoreSqlMem(ctx context.Context, snap SqlMemSnapshotter, sec *SqlMemSection, result *RestoreResult) {
	target := snap.Tier()
	if sec.Tier != target {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("sqlmem section is tier %q but this host is tier %q; skipped (SQL Memory dumps are tier-specific)", sec.Tier, target))
		return
	}
	for _, sc := range sec.Scopes {
		dump := &sqlmem.ScopeDump{DDL: sc.DDL, PostDDL: sc.PostDDL}
		for _, t := range sc.Tables {
			dump.Tables = append(dump.Tables, sqlmem.TableDump{
				Name:        t.Name,
				Columns:     t.Columns,
				ColumnTypes: t.ColumnTypes,
				Rows:        t.Rows,
			})
		}
		key := sqlmem.ScopeKey{Tenant: sc.Tenant, Scope: sc.Scope, ScopeID: sc.ScopeID}
		if err := snap.RestoreScope(ctx, key, dump); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("sqlmem scope %s/%s/%s: %v", sc.Tenant, sc.Scope, sc.ScopeID, err))
			continue
		}
		result.SqlMemScopesRestored++
	}
}
