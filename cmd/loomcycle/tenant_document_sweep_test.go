package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestBootSweep_MovesTenantDocumentRowsAnOldReplicaWrote — a row an old-binary
// replica writes after the migration ran (here: after this store opened) is moved
// by the next boot's sweep.
func TestBootSweep_MovesTenantDocumentRowsAnOldReplicaWrote(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.MemorySet(ctx, "tnt", store.MemoryScopeTenant, "tnt", "doc.chunk:late", json.RawMessage(`"body"`), 0); err != nil {
		t.Fatal(err)
	}
	sweepTenantDocumentRows(st)
	if _, err := st.MemoryGet(ctx, "tnt", store.MemoryScopeTenant, "", "doc.chunk:late"); err != nil {
		t.Errorf("the sweep did not move the late row: %v", err)
	}
}
