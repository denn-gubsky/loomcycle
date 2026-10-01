package postgres

import (
	"context"
	"strings"
	"testing"
)

// TestMemoryPendingUndrainedIndex_ServesTheTenantlessLookup pins migration
// 0089: the operator-layer consolidation fan-out and the snapshot read filter
// `scope = $1 AND drained_at IS NULL` with no tenant, which the by-target index
// (leading tenant_id) cannot serve, so every tick scanned the queue's whole
// history. Seq scans are disabled for the EXPLAIN because an empty table is
// always cheapest to scan; the question is which index the planner CAN use.
func TestMemoryPendingUndrainedIndex_ServesTheTenantlessLookup(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	ctx := context.Background()

	conn, err := fix.store.(*Store).Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	var def string
	if err := conn.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = current_schema() AND indexname = 'memory_pending_undrained_by_scope'`,
	).Scan(&def); err != nil {
		t.Fatalf("memory_pending_undrained_by_scope missing after migrate: %v", err)
	}
	if !strings.Contains(def, "WHERE (drained_at IS NULL)") {
		t.Errorf("index is not partial on undrained rows: %s", def)
	}

	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET enable_seqscan`) }()
	rows, err := conn.Query(ctx,
		`EXPLAIN SELECT tenant_id, scope_id FROM memory_pending
		  WHERE scope = 'user' AND drained_at IS NULL
		  GROUP BY tenant_id, scope_id`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "memory_pending_undrained_by_scope") {
		t.Errorf("the tenant-less undrained lookup does not use memory_pending_undrained_by_scope; plan:\n%s",
			strings.Join(plan, "\n"))
	}
}
