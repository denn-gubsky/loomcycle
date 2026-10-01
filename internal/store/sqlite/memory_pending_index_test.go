package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// legacyMemoryPendingDDL is memory_pending as the consolidation substrate first
// created it, before the origin column arrived by ALTER. A long-lived deployment
// has this shape on disk, so the undrained index must be creatable against it.
const legacyMemoryPendingDDL = `CREATE TABLE memory_pending (
	id                TEXT    PRIMARY KEY,
	tenant_id         TEXT    NOT NULL DEFAULT '',
	scope             TEXT    NOT NULL,
	scope_id          TEXT    NOT NULL,
	payload           TEXT    NOT NULL,
	source_session_id TEXT,
	source_run_id     TEXT,
	created_at        INTEGER NOT NULL,
	drained_at        INTEGER
)`

// TestMemoryPendingUndrainedIndex_ServesTheTenantlessLookupOnAnUpgradedDB pins
// the index the operator-layer consolidation fan-out and the snapshot read need:
// both filter `scope = ? AND drained_at IS NULL` with no tenant, which the
// by-target index (leading tenant_id) cannot serve, so every tick scanned the
// queue's whole history. Starts from the legacy table so the index is proven
// creatable on an upgraded DB, not only on a fresh one.
func TestMemoryPendingUndrainedIndex_ServesTheTenantlessLookupOnAnUpgradedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := seed.Exec(legacyMemoryPendingDDL); err != nil {
		t.Fatalf("create legacy memory_pending: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	rows, err := st.db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN
		 SELECT tenant_id, scope_id FROM memory_pending
		 WHERE scope = ? AND drained_at IS NULL
		 GROUP BY tenant_id, scope_id`, "user")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "memory_pending_undrained_by_scope") {
		t.Errorf("the tenant-less undrained lookup does not use memory_pending_undrained_by_scope; plan:\n%s",
			strings.Join(plan, "\n"))
	}
}
