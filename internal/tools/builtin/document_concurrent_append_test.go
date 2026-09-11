package builtin

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// OQ6 — write contention on a shared subject document, ANSWERED BY MEASUREMENT.
//
// The question: after subject-homing, many users' consolidators append facts under
// ONE parent — the tenant entity node — and create_chunk's append path reads
// max(position) and then inserts, outside a transaction. Two writers read the same
// max and both take it.
//
// The answer: a position TIE, and nothing else. Measured at 8 writers × 6 facts —
// 48/48 landed, zero errors, 7 positions shared by two chunks. No write is lost, no
// caller sees an error, and every reader is deterministic anyway because the orders
// that matter tiebreak on id (export_md is `ORDER BY parent_id, position, id`).
// reorder_chunk renumbers a sibling list to a contiguous 0..n-1 and so heals ties as
// a side effect of ordinary use.
//
// SO IT IS LEFT ALONE, deliberately. Serialising the read-and-insert would cost a
// transaction per fact — two extra round trips on the hottest write in a
// consolidation pass — to prevent an outcome with no observable consequence. The
// order of a subject's facts carries no meaning; the order of a document's sections
// does, and that path (create_chunk with after_id) is ALREADY transactional.
//
// This test exists so the reasoning is executable: if a tie ever starts costing a
// write or breaking a read, it fails here rather than in a store.
func TestConcurrentAppends_ContendOnPositionButNeverOnContent(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Denn","path":"/facts/denn",
		"type":"person","subject":"Denn","natural_key":"person:denn"}`)
	if r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)

	const writers, each = 8, 6
	var wg sync.WaitGroup
	errs := make(chan string, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				body := fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"parent_id":%q,
					"natural_key":"memory/fact/w%d-f%d","title":"f","body":"a fact","type":"fact"}`,
					docID, root, w, i)
				res, err := d.Execute(ctx, json.RawMessage(body))
				switch {
				case err != nil:
					errs <- "exec: " + err.Error()
				case res.IsError:
					errs <- res.Text
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("a concurrent append failed: %s", e)
	}

	key := sidecarScope(t, d, ctx)
	// EVERY WRITE LANDED. This is the assertion that matters: contention may reorder,
	// it may not lose.
	cnt, err := d.query(ctx, key, `SELECT count(*) FROM chunks WHERE parent_id = ?`, root)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n := asInt(cnt.Rows[0][0]); n != writers*each {
		t.Errorf("%d facts under the subject, want %d — concurrent appends lost writes", n, writers*each)
	}

	// AND EVERY READ IS DETERMINISTIC. Ties are expected; an order that depends on
	// which row the database happens to return is not, so the tiebreak is asserted by
	// reading twice and requiring the same answer.
	const stmt = `SELECT id FROM chunks WHERE parent_id = ? ORDER BY position, id`
	first, err := d.query(ctx, key, stmt, root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	second, err := d.query(ctx, key, stmt, root)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	for i := range first.Rows {
		if asStr(first.Rows[i][0]) != asStr(second.Rows[i][0]) {
			t.Fatalf("two reads of the same sibling list disagree at %d — a position tie is only "+
				"harmless while the order is settled by the id tiebreak", i)
		}
	}
}
