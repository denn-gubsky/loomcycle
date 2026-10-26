package sqlshared

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The old keyspace of a tenant document row restates sqlmem.ScopeTenant in SQL —
// "" becomes "default", any other tenant is itself — because the store cannot
// import SQL Memory. The SQLite tier's test seeds the old rows with that
// function, so the two cannot drift silently.

// tenantDocRow matches a tenant-scope document row (a chunk body or a derived
// unit) at a non-empty scope_id, in table or alias t. substr, not LIKE: SQLite's
// LIKE folds ASCII case, and the namespaces are exact.
func tenantDocRow(t string) string {
	return fmt.Sprintf(`%[1]s.scope = 'tenant' AND %[1]s.scope_id <> ''
	   AND (substr(%[1]s.key, 1, 10) = 'doc.chunk:' OR substr(%[1]s.key, 1, 9) = 'doc.unit:')`, t)
}

// legacyTenantDocRow narrows tenantDocRow to the scope_id an older binary wrote:
// the SQL Memory scope id of the row's own tenant.
func legacyTenantDocRow(t string) string {
	return tenantDocRow(t) + fmt.Sprintf(`
	   AND %[1]s.scope_id = CASE WHEN %[1]s.tenant_id = '' THEN 'default' ELSE %[1]s.tenant_id END`, t)
}

// tenantDocDestination matches the "" row of the same tenant and key as alias o.
func tenantDocDestination(t, o string) string {
	return fmt.Sprintf(`%[1]s.tenant_id = %[2]s.tenant_id AND %[1]s.scope = 'tenant'
	   AND %[1]s.scope_id = '' AND %[1]s.key = %[2]s.key`, t, o)
}

// MigrateTenantDocumentRows implements store.Store.MigrateTenantDocumentRows for
// both stores. Postgres migration 0113 runs the same statements once, in SQL;
// this is what the SQLite open path and the boot sweep run, and what reports.
func MigrateTenantDocumentRows(ctx context.Context, db DB) (store.TenantDocumentKeyspaceReport, error) {
	var rep store.TenantDocumentKeyspaceReport

	// Strays first, read-only: they are reported on every call, including the
	// common one with nothing to move.
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM memory m WHERE `+tenantDocRow("m")+
		` AND NOT (`+legacyTenantDocRow("m")+`)`).Scan(&rep.Strays); err != nil {
		return rep, fmt.Errorf("tenant document rows: count strays: %w", err)
	}
	if rep.Strays > 0 {
		rows, err := db.Query(ctx, `SELECT m.tenant_id, m.scope_id, m.key FROM memory m WHERE `+tenantDocRow("m")+
			` AND NOT (`+legacyTenantDocRow("m")+`) ORDER BY m.tenant_id, m.scope_id, m.key LIMIT ?`,
			store.TenantDocumentReportSampleCap)
		if err != nil {
			return rep, fmt.Errorf("tenant document rows: list strays: %w", err)
		}
		for rows.Next() {
			var r store.TenantDocumentRowRef
			if err := rows.Scan(&r.TenantID, &r.ScopeID, &r.Key); err != nil {
				rows.Close()
				return rep, fmt.Errorf("tenant document rows: scan stray: %w", err)
			}
			rep.StraySample = append(rep.StraySample, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, fmt.Errorf("tenant document rows: list strays: %w", err)
		}
	}

	// Nothing to move is the state of every boot after the first, so it costs one
	// read and no transaction.
	var pending int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM memory m WHERE `+legacyTenantDocRow("m")).Scan(&pending); err != nil {
		return rep, fmt.Errorf("tenant document rows: count: %w", err)
	}
	if pending == 0 {
		return rep, nil
	}

	err := db.InTx(ctx, func(tx Querier) error {
		rep.Conflicts, rep.ConflictSample, rep.Moved = 0, nil, 0
		rows, err := tx.Query(ctx, `SELECT o.tenant_id, o.key, CASE WHEN o.updated_at > d.updated_at THEN 1 ELSE 0 END
			  FROM memory o JOIN memory d ON `+tenantDocDestination("d", "o")+`
			 WHERE `+legacyTenantDocRow("o")+`
			 ORDER BY o.tenant_id, o.key`)
		if err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}
		for rows.Next() {
			var c store.TenantDocumentRowConflict
			var keptOld int
			if err := rows.Scan(&c.TenantID, &c.Key, &keptOld); err != nil {
				rows.Close()
				return fmt.Errorf("scan conflict: %w", err)
			}
			c.KeptOld = keptOld == 1
			rep.Conflicts++
			if len(rep.ConflictSample) < store.TenantDocumentReportSampleCap {
				rep.ConflictSample = append(rep.ConflictSample, c)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}
		if rep.Conflicts > 0 {
			// The later write wins, on either side. A destination older than the
			// row about to move goes first (its embedding cascades on Postgres; the
			// SQLite tier keeps none), then every old row that still has a
			// destination — the destination was newer, or as new.
			if _, err := tx.Exec(ctx, `DELETE FROM memory WHERE scope = 'tenant' AND scope_id = ''
				   AND EXISTS (SELECT 1 FROM memory o WHERE `+tenantDocDestination("memory", "o")+`
				               AND `+legacyTenantDocRow("o")+` AND o.updated_at > memory.updated_at)`); err != nil {
				return fmt.Errorf("drop older destinations: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM memory WHERE `+legacyTenantDocRow("memory")+`
				   AND EXISTS (SELECT 1 FROM memory d WHERE `+tenantDocDestination("d", "memory")+`)`); err != nil {
				return fmt.Errorf("drop older old-keyspace rows: %w", err)
			}
		}
		moved, err := moveTenantDocRows(ctx, tx)
		if err != nil {
			return err
		}
		rep.Moved = moved
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("tenant document rows: %w", err)
	}
	return rep, nil
}

// moveTenantDocRows re-keys the old rows. On Postgres memory_embeddings
// references memory by the full key with no ON UPDATE action, so the two
// updates run as ONE statement: the foreign key is checked at its end, when
// both sides carry the new scope_id. The table exists only where pgvector
// loaded.
func moveTenantDocRows(ctx context.Context, tx Querier) (int, error) {
	if tx.Dialect() == Postgres {
		var hasEmbeddings bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('memory_embeddings') IS NOT NULL`).Scan(&hasEmbeddings); err != nil {
			return 0, fmt.Errorf("probe memory_embeddings: %w", err)
		}
		if hasEmbeddings {
			var moved int
			if err := tx.QueryRow(ctx, `WITH m AS (
				    UPDATE memory SET scope_id = '' WHERE `+legacyTenantDocRow("memory")+` RETURNING 1),
				  e AS (
				    UPDATE memory_embeddings SET scope_id = '' WHERE `+legacyTenantDocRow("memory_embeddings")+` RETURNING 1)
				SELECT (SELECT COUNT(*) FROM m)`).Scan(&moved); err != nil {
				return 0, fmt.Errorf("move rows and embeddings: %w", err)
			}
			return moved, nil
		}
	}
	n, err := tx.Exec(ctx, `UPDATE memory SET scope_id = '' WHERE `+legacyTenantDocRow("memory"))
	if err != nil {
		return 0, fmt.Errorf("move rows: %w", err)
	}
	return int(n), nil
}
