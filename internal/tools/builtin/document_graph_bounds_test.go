package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// TestBatchIDs_SplitsAtTheCeiling.
//
// ⚠️ This is the ONLY layer at which the placeholder-ceiling fix is testable, and
// saying so is part of the test. The end-to-end failure needs more ids in one
// IN(...) than the driver accepts — 32766 on SQLite, 65535 on Postgres — and a
// fixture that large is not a unit test. So the split itself is pinned here, and
// the call sites are reviewed to go through it.
func TestBatchIDs_SplitsAtTheCeiling(t *testing.T) {
	ids := make([]string, 1201)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	got := batchIDs(ids, 500)
	if len(got) != 3 {
		t.Fatalf("batchIDs(1201, 500) = %d batches, want 3", len(got))
	}
	seen := 0
	for _, b := range got {
		if len(b) > 500 {
			t.Errorf("batch of %d exceeds the ceiling — one over is still one query too big", len(b))
		}
		seen += len(b)
	}
	if seen != len(ids) {
		t.Errorf("batches cover %d ids, want all %d — a dropped batch is a silently smaller answer", seen, len(ids))
	}
	if n := len(batchIDs(ids[:10], 500)); n != 1 {
		t.Errorf("a short list became %d batches, want 1", n)
	}
	if n := len(batchIDs(nil, 500)); n != 1 {
		t.Errorf("batchIDs(nil) = %d batches, want 1 (the empty one)", n)
	}
}

// TestGraphRecall_LimitBoundsEvenWithABudget.
//
// `limit` used to sit in the `else` of the budget test, so passing both silently
// dropped the row cap: accepted, ignored, no signal that it had not applied. Same
// shape as the `sources` selector that decoded and was discarded, and as `limit`
// vs `top_k` on the search path. The two bounds compose — budget caps by content,
// limit caps by rows, whichever binds first wins.
func TestGraphRecall_LimitBoundsEvenWithABudget(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Hubtown","type":"location","natural_key":"location:Hubtown"}`)
	if r.IsError {
		t.Fatalf("upsert hub: %s", r.Text)
	}
	hub := asStr(out["id"])
	// The facts must arrive by EXPANSION, not as seeds: a seed list is itself
	// bounded by limit, so seeding twelve rows would test the seed query rather
	// than the budget/limit pass this regression is about.
	for i := 0; i < 12; i++ {
		n := fmt.Sprint(i)
		o, rr := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"Fact number `+n+` about Hubtown.","type":"fact",`+
			`"natural_key":"fact:n`+n+`"}`)
		if rr.IsError {
			t.Fatalf("upsert %d: %s", i, rr.Text)
		}
		if _, rr := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
			`","from_id":"`+asStr(o["id"])+`","to_id":"`+hub+`","kind":"about"}`); rr.IsError {
			t.Fatalf("link %d: %s", i, rr.Text)
		}
	}

	// A budget far larger than the content, so ONLY limit can bind.
	got, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+hub+
		`"],"hops":1,"limit":3,"budget_chars":100000}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	chunks, _ := got["chunks"].([]any)
	if len(chunks) > 3 {
		t.Errorf("returned %d chunks under limit=3 with a budget set — the row cap was "+
			"accepted and then ignored, which is the failure a caller cannot see", len(chunks))
	}
	if len(chunks) == 0 {
		t.Fatal("returned nothing — the fixture no longer exercises the bound")
	}
	if got["truncated"] != true {
		t.Error("a result cut short by limit must report truncated")
	}
}

// TestGraphNeighbours_CapsRowsPerHop.
//
// graphFrontierCap bounds how many nodes a hop expands FROM; it does not bound how
// many rows the hop RETURNS, which is nodes × degree. Degree is not small in
// practice: on a 304-chunk benchmark store the maximum in-degree was 162 — one hub
// entity with 162 facts pointing at it — so a single frontier node returns 162 rows
// and a full frontier returns five figures, all materialised and all accumulated
// into `order`, which then becomes one placeholder per id.
//
// Exercised through the rowCap PARAMETER rather than the production constant. The
// first version of this test built a fixture one past graphHopRowCap — 2001 chunks
// and 2001 edges — and cost 95s under -race on a package that already runs close to
// its CI timeout. A bound passed in is the same bound, and testing it costs six rows.
func TestGraphNeighbours_CapsRowsPerHop(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Hub","type":"person","natural_key":"person:Hub"}`)
	if r.IsError {
		t.Fatalf("upsert hub: %s", r.Text)
	}
	hub := asStr(out["id"])

	const cap = 5
	// One past the cap, so the cap is what binds rather than the fixture size.
	for i := 0; i < cap+1; i++ {
		n := fmt.Sprint(i)
		o, rr := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"f`+n+`","type":"fact","natural_key":"fact:f`+n+`"}`)
		if rr.IsError {
			t.Fatalf("upsert %d: %s", i, rr.Text)
		}
		if _, rr := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
			`","from_id":"`+asStr(o["id"])+`","to_id":"`+hub+`","kind":"about"}`); rr.IsError {
			t.Fatalf("link %d: %s", i, rr.Text)
		}
	}

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	rows, over, err := d.graphNeighbours(ctx, key, []string{hub}, 1, docInput{}, cap)
	if err != nil {
		t.Fatalf("graphNeighbours: %v", err)
	}
	if len(rows) > cap {
		t.Errorf("one hop returned %d rows with a cap of %d — the frontier cap bounds NODES, "+
			"not rows, so a hub is unbounded without this", len(rows), cap)
	}
	if !over {
		t.Error("a hop that hit the cap must say so, or the caller reads a truncated graph as the whole one")
	}

	// And the cap is not merely a ceiling nobody reaches: under it, everything comes back.
	rows, over, err = d.graphNeighbours(ctx, key, []string{hub}, 1, docInput{}, 100)
	if err != nil {
		t.Fatalf("graphNeighbours (roomy): %v", err)
	}
	if over || len(rows) != cap+1 {
		t.Errorf("with room for all, got %d rows over=%v, want %d and false", len(rows), over, cap+1)
	}
}

// TestGraphRecall_ExplicitSeedIDsAreNotTruncatedByLimit.
//
// `limit` used to bound the SEED query as well as the result, so handing in more ids
// than the limit walked from a subset of them — accepted, dropped, and
// indistinguishable from "the graph holds nothing else". Naming a chunk is an
// assertion about where to START; limit bounds what comes BACK.
func TestGraphRecall_ExplicitSeedIDsAreNotTruncatedByLimit(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	ids := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		n := fmt.Sprint(i)
		o, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"seed `+n+`","type":"fact","natural_key":"fact:s`+n+`"}`)
		if r.IsError {
			t.Fatalf("upsert %d: %s", i, r.Text)
		}
		ids = append(ids, `"`+asStr(o["id"])+`"`)
	}
	// limit BELOW the seed count: every id must still be walked from, and the cap
	// must show up on the result instead.
	got, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":[`+
		strings.Join(ids, ",")+`],"hops":0,"limit":3}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if n := asInt(got["seeds"]); n != 8 {
		t.Errorf("seeds = %d, want all 8 — a caller that NAMED its starting chunks had "+
			"some silently dropped, which reads as an empty graph", n)
	}
	chunks, _ := got["chunks"].([]any)
	if len(chunks) > 3 {
		t.Errorf("returned %d chunks under limit=3 — limit must still bound the RESULT", len(chunks))
	}
}

// TestGraphRecall_TooManySeedIDsIsRefusedNotTruncated. The seeds go into one
// IN(...), so an unbounded list also walks into the driver's placeholder ceiling.
func TestGraphRecall_TooManySeedIDsIsRefusedNotTruncated(t *testing.T) {
	d, ctx, _, _ := entityFixture(t)
	ids := make([]string, 0, graphFrontierCap+1)
	for i := 0; i < graphFrontierCap+1; i++ {
		ids = append(ids, `"c`+fmt.Sprint(i)+`"`)
	}
	_, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":[`+strings.Join(ids, ",")+`]}`)
	if !r.IsError {
		t.Fatal("an over-cap seed list must be refused, not quietly cut down")
	}
	if !strings.Contains(r.Text, "split them across calls") {
		t.Errorf("the refusal should tell the caller what to do instead, got %q", r.Text)
	}
}
