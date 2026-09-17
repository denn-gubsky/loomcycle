package builtin

import "testing"

// TestSchemaMemo_SelfHealsWhenAScopeGoesStale.
//
// ensureSchema is memoised per process, which is what removed 71% of a get_chunk
// call. The memo is a claim about THIS PROCESS, not about the database, so the
// question it raises is what happens when the two disagree — a column a later
// release adds via ALTER, on a scope this process already marked provisioned.
//
// Without the staleness retry that is a PERMANENT failure: the memo says done, the
// ALTER never runs again, and every write of the new column fails for the life of
// the process. So a statement failing on a missing column re-provisions and retries
// once, which turns a dead scope into a slow first call.
func TestSchemaMemo_SelfHealsWhenAScopeGoesStale(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"seed"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)

	// Take the scope back to a pre-migration shape WITHOUT touching the memo, so the
	// process still believes this scope is fully provisioned.
	if _, err := d.SqlMem.Exec(ctx, key, `ALTER TABLE chunk_memory_meta DROP COLUMN judged_by`, nil, 0); err != nil {
		t.Skipf("this tier cannot drop a column: %v", err)
	}
	if d.tableHasColumn(ctx, key, "chunk_memory_meta", "judged_by") {
		t.Fatal("the rewind did not take — the test cannot reach the state it is for")
	}

	// An ordinary statement touching the missing column must recover on its own.
	if _, err := d.query(ctx, key, `SELECT judged_by FROM chunk_memory_meta WHERE 1=0`); err != nil {
		t.Fatalf("a stale scope did not self-heal: %v — without this the memo converts a "+
			"pending migration into a permanent failure", err)
	}
	if !d.tableHasColumn(ctx, key, "chunk_memory_meta", "judged_by") {
		t.Error("the column is still missing after the retry — provisioning did not re-run")
	}
}

// TestSchemaMemo_ProbesDoNotRecurse.
//
// The migrations ask "does this column exist" by running a SELECT and reading the
// ERROR as the answer. The staleness retry reads that same error as a stale memo. If
// the probe goes through the retry, re-provisioning runs the migrations, which probe
// again — a loop with no exit, and it does not announce itself as a loop: it
// presents as one test hanging.
func TestSchemaMemo_ProbesDoNotRecurse(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"seed"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)
	// A column that does not exist and never will. The probe must simply answer
	// false; anything that re-enters provisioning would not return at all.
	if d.tableHasColumn(ctx, key, "chunk_memory_meta", "no_such_column_ever") {
		t.Fatal("probe reported a column that does not exist")
	}
}
