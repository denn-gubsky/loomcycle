package builtin

import (
	"context"
	"testing"
)

// The chunk plane's third timeline (RFC CV P2a).
//
// A fact carries three independent instants: when it was SAID (observed_at), when
// it became true (valid_at) and when the system learned it (created_at). The
// sidecar had columns for the last two pairs and none for the first, so a fact
// mirrored from the k/v plane into the graph arrived with its only populated date
// missing — measured at 103/103 facts on a real corpus — while the sidecar's own
// writer stamped valid_at = now, making 117 of 120 rows claim a world-time they
// had never been told.

// sidecarInstants reads the three temporal columns for a chunk, as nil-able
// values, so "unset" stays distinguishable from "set to the epoch".
func sidecarInstants(t *testing.T, d *Document, ctx context.Context, chunkID string) (observed, valid *int64) {
	t.Helper()
	res, err := d.SqlMem.Query(ctx, sidecarScope(t, d, ctx),
		d.SqlMem.Rebind(`SELECT observed_at, valid_at FROM chunk_memory_meta WHERE chunk_id = ?`),
		[]any{chunkID})
	if err != nil {
		t.Fatalf("read sidecar instants: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no sidecar row for %s", chunkID)
	}
	return asInt64Ptr(res.Rows[0][0]), asInt64Ptr(res.Rows[0][1])
}

// TestUpsertChunk_ObservedAtReachesTheSidecarAndReadsBack. The write path and the
// read path have to agree, and the round trip through get_chunk is the half a
// column addition usually forgets: a value that lands in SQL but is not emitted is
// invisible to every consumer, which is exactly how the k/v plane's temporal
// columns went unread for a whole session (#1144).
func TestUpsertChunk_ObservedAtReachesTheSidecarAndReadsBack(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	const said = int64(1688759760000000000) // 2023-07-07T19:56:00Z, the LoCoMo shape

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`","natural_key":"memory/fact/denn-prefers-go",
		"title":"Denn prefers Go","body":"Denn prefers Go for backend services.",
		"type":"fact","subject":"Denn","observed_at":1688759760000000000}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])

	if observed, _ := sidecarInstants(t, d, ctx, id); observed == nil || *observed != said {
		t.Errorf("sidecar observed_at = %v, want %d — the fact's utterance time did not survive the write", observed, said)
	}

	// And it must come back out: `when` narrows on this field, so a value the read
	// surface drops is a value no retrieval can use.
	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	ent, _ := got["entity"].(map[string]any)
	if ent == nil {
		t.Fatalf("get_chunk returned no entity block: %v", got)
	}
	if v, ok := ent["observed_at"]; !ok {
		t.Errorf("get_chunk omitted observed_at; entity block = %v", ent)
	} else if int64(v.(float64)) != said {
		t.Errorf("get_chunk observed_at = %v, want %d", v, said)
	}
}

// TestUpsertChunk_AnUndatedFactGetsNoFabricatedValidAt is the correction half.
//
// valid_at defaulted to now, which asserts a world-time nobody supplied. It is not
// a harmless default: a reader cannot tell a fabricated instant from a real one, so
// every undated fact silently claimed to have become true at the moment it was
// written. Leaving it NULL is safe by construction rather than by luck — the as-of
// predicate is already `(valid_at IS NULL OR valid_at <= ?)`, which the companion
// test below exercises.
func TestUpsertChunk_AnUndatedFactGetsNoFabricatedValidAt(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`","natural_key":"memory/fact/undated",
		"title":"Releases are never cut on a Friday","body":"Releases are never cut on a Friday.",
		"type":"fact","subject":"the team"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])

	if _, valid := sidecarInstants(t, d, ctx, id); valid != nil {
		t.Errorf("valid_at = %d for a fact whose caller supplied none — a write timestamp is not a world time", *valid)
	}
}

// TestUpsertChunk_AnUndatedFactStillAnswersAsOf proves the NULL is not a
// regression. This is the property that makes dropping the default safe, so it is
// asserted rather than assumed: an undated fact is not excluded from a temporal
// question, it is only absent from a window it never claimed to be in.
func TestUpsertChunk_AnUndatedFactStillAnswersAsOf(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`","natural_key":"memory/fact/undated-recall",
		"title":"Ada owns the ledger service","body":"Ada owns the ledger service.",
		"type":"person","subject":"Ada"}`); r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	// as_of well BEFORE the write: an undated fact has no start to fall outside of.
	out, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","as_of":1000000000000000000}`)
	if r.IsError {
		t.Fatalf("list_facts as_of: %s", r.Text)
	}
	rows, _ := out["facts"].([]any)
	if len(rows) == 0 {
		t.Errorf("an undated fact vanished from an as_of query — dropping the valid_at default would be a retrieval regression; got %v", out)
	}
}

// TestMigrateFactObservation_ReachesAnExistingScope. A `CREATE TABLE IF NOT
// EXISTS` leaves a provisioned table alone, so a fresh-database test cannot see
// whether an UPGRADE works — the columns would appear on new scopes and never on
// the ones that already hold data. The old shape is reconstructed here by dropping
// the columns from a live scope and re-running ensureSchema.
func TestMigrateFactObservation_ReachesAnExistingScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// Provision the scope the ordinary way.
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"seed"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	key := sidecarScope(t, d, ctx)

	// Rewind to the pre-migration shape.
	for _, col := range []string{"observed_at", "access_count", "last_accessed_at"} {
		if _, err := d.SqlMem.Exec(ctx, key, `ALTER TABLE chunk_memory_meta DROP COLUMN `+col, nil, 0); err != nil {
			t.Fatalf("rewind %s: %v", col, err)
		}
	}
	if d.tableHasColumn(ctx, key, "chunk_memory_meta", "observed_at") {
		t.Fatal("rewind did not take — the test would pass against the unmigrated code")
	}

	if err := d.ensureSchema(ctx, key); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	for _, col := range []string{"observed_at", "access_count", "last_accessed_at"} {
		if !d.tableHasColumn(ctx, key, "chunk_memory_meta", col) {
			t.Errorf("%s absent after ensureSchema — an existing scope never gains the column", col)
		}
	}
}
