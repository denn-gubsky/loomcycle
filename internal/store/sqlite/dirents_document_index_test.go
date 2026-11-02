package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlshared"
)

// TestDirentsDocumentIndex_ServesTheReverseLookup pins the dirents_document_id
// expression index to the query that needs it: the planner uses an expression
// index only when the query spells the expression exactly as the index does,
// so a drift between sqlshared.direntDocumentID and the schema would silently
// turn the reverse lookup back into a scan of the whole tree.
func TestDirentsDocumentIndex_ServesTheReverseLookup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// One large tree, analyzed: the case the index exists for. On an empty
	// table every plan is a scan of nothing and proves nothing.
	for i := 0; i < 3000; i++ {
		if _, err := s.DirentCreate(ctx, store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "u1",
			ParentPath: fmt.Sprintf("/d%02d/", i%40), Name: fmt.Sprintf("doc-%04d", i), Kind: "document",
			ResourceRef: json.RawMessage(fmt.Sprintf(`{"document_id":"D%04d"}`, i))}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sqlshared.DirentByDocumentsQuery(sqlshared.SQLite, 2),
		"acme", "user", "u1", "D1", "D2")
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
	if !strings.Contains(strings.Join(plan, "\n"), "dirents_document_id") {
		t.Errorf("the reverse lookup does not use dirents_document_id; plan:\n%s", strings.Join(plan, "\n"))
	}
}
