package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// TestDirentsDocumentIndex_ServesTheReverseLookup pins migration 0128 to the
// query that needs it: an expression index serves only a query that spells the
// expression exactly as the index does, so a drift between
// sqlshared.direntDocumentID and the migration would silently turn the reverse
// lookup back into a scan of the whole tree. The tree is seeded and analyzed
// so the planner chooses on real statistics.
func TestDirentsDocumentIndex_ServesTheReverseLookup(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	ctx := context.Background()

	// One large tree, analyzed: the case the index exists for. On an empty
	// table the planner reaches for the primary key's tree prefix and the plan
	// proves nothing.
	for i := 0; i < 3000; i++ {
		if _, err := fix.store.DirentCreate(ctx, store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "u1",
			ParentPath: fmt.Sprintf("/d%02d/", i%40), Name: fmt.Sprintf("doc-%04d", i), Kind: "document",
			ResourceRef: json.RawMessage(fmt.Sprintf(`{"document_id":"D%04d"}`, i))}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	conn, err := fix.store.(*Store).Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `ANALYZE dirents`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	q := sqlshared.Postgres.Rebind(sqlshared.DirentByDocumentsQuery(sqlshared.Postgres, 2))
	rows, err := conn.Query(ctx, "EXPLAIN "+q, "acme", "user", "u1", "D1", "D2")
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
	if !strings.Contains(strings.Join(plan, "\n"), "dirents_document_id") {
		t.Errorf("the reverse lookup does not use dirents_document_id; plan:\n%s", strings.Join(plan, "\n"))
	}
}
