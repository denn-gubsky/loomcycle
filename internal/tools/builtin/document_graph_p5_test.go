package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// RFC CV P5. The gate the phase was blocked on is cleared, and the benchmark
// that cleared it also said what P5 should be (CV decision 12): seed
// semantically, let the walk go deep enough to cross more than one relation, and
// cap on CONTENT with a backfill so a sparse graph cannot make traversal worse
// than not traversing.

// TestGraphRecall_WalksFarEnoughForAThreeHopChain.
//
// A hop is ONE edge, so a fact→entity→fact step costs two. The old cap of 2
// therefore answered a two-hop question and nothing deeper — Ada → the company →
// London is already at the ceiling, and anything past it was unreachable however
// the caller asked.
func TestGraphRecall_WalksFarEnoughForAThreeHopChain(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	// Extend the chain one relation past what the old cap could reach:
	// Ada → Analytical Engine Co → London → England.
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","title":"England","type":"location","natural_key":"location:England"}`)
	if r.IsError {
		t.Fatalf("upsert England: %s", r.Text)
	}
	england := asStr(out["id"])
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
		`","from_id":"`+ids["London"]+`","to_id":"`+england+`","kind":"located_in"}`); r.IsError {
		t.Fatalf("link London->England: %s", r.Text)
	}

	got, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":3}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	// graphIDs keys by TITLE, not by chunk id.
	hops := graphIDs(got)
	if hop, reached := hops["England"]; !reached {
		t.Errorf("England is three relations from Ada and was not reached; got %v", hops)
	} else if hop != 3 {
		t.Errorf("England reached at hop %d, want 3", hop)
	}
	_ = england

	// And the old ceiling could NOT reach it — the guard that makes this a real
	// regression test rather than a restatement of current behaviour.
	capped, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":2}`)
	if r.IsError {
		t.Fatalf("graph_recall hops=2: %s", r.Text)
	}
	if _, reached := graphIDs(capped)["England"]; reached {
		t.Error("England is reachable within 2 hops — the fixture no longer tests depth")
	}
}

// TestGraphRecall_BudgetCapsOnContentNotRows.
//
// A walk fetches more rows than a search by construction, so a row limit lets
// traversal win on volume rather than on the relations — the exact confound the
// shuffled-relation control exists to catch. The cap has to be on what reaches
// the reader.
func TestGraphRecall_BudgetCapsOnContentNotRows(t *testing.T) {
	d, ctx, _, ids := graphFixture(t)
	// Ada's own title is 3 chars, so a 12-char budget admits a couple of rows and
	// refuses the rest however many the walk finds.
	got, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":2,"budget_chars":12}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	used := asInt(got["chars_used"])
	if used > 12 {
		t.Errorf("chars_used = %d, over the 12-char budget", used)
	}
	if got["budget_chars"] == nil || got["chars_used"] == nil {
		t.Error("a budgeted recall must report the cap and the fill — an arm that " +
			"cannot fill its budget has to say so")
	}
	chunks, _ := got["chunks"].([]any)
	unbounded, r2 := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":2}`)
	if r2.IsError {
		t.Fatalf("graph_recall unbounded: %s", r2.Text)
	}
	all, _ := unbounded["chunks"].([]any)
	if len(chunks) >= len(all) {
		t.Errorf("budget did not bind: %d chunks under a 12-char budget vs %d without",
			len(chunks), len(all))
	}
}

// TestGraphRecall_ReportsHowItSeeded. A caller that cannot tell a semantic
// seeding from a title match cannot tell why a recall came back thin.
func TestGraphRecall_ReportsHowItSeeded(t *testing.T) {
	d, ctx, _, ids := graphFixture(t)
	byIDs, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+`"]}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if got := asStr(byIDs["seeded_by"]); got != "ids" {
		t.Errorf("seeded_by = %q, want \"ids\"", got)
	}
	// No embedder on this fixture, so a query must still answer the title way
	// rather than failing: a recall that worked before must not start refusing.
	byTitle, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","query":"Ada"}`)
	if r.IsError {
		t.Fatalf("graph_recall by query: %s", r.Text)
	}
	if got := asStr(byTitle["seeded_by"]); got != "title" {
		t.Errorf("seeded_by = %q, want \"title\" when there is no embedder", got)
	}
	if n := asInt(byTitle["seeds"]); n == 0 {
		t.Error("title seeding found nothing for a title that exists — the fallback is " +
			"what keeps a no-embedder deployment working")
	}
}

// TestGraphRecall_BackfillNeverClaimsAPath.
//
// A backfilled row was reached by RANK, never by an edge. Reporting it at a hop
// count would let a caller read an association as a relation, which is the
// distinction graphChunk.Hop exists to carry.
func TestGraphRecall_BackfillNeverClaimsAPath(t *testing.T) {
	d, ctx, _, ids := graphFixture(t)
	got, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":2,"budget_chars":400}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	// Seeding was by ids, so there is no ranked list and nothing to backfill from.
	if n := asInt(got["backfilled"]); n != 0 {
		t.Errorf("backfilled = %d, but an id-seeded walk has no ranked list to draw on", n)
	}
	raw, _ := json.Marshal(got["chunks"])
	var rows []graphChunk
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode chunks: %v", err)
	}
	for _, c := range rows {
		if c.Hop < 0 {
			t.Errorf("chunk %q reports hop %d: only a backfilled row may, and this walk "+
				"backfilled nothing", c.Title, c.Hop)
		}
	}
}

// TestGraphRecall_HopCeilingMessageNamesTheBudget. The refusal used to say "past
// two hops a result stops describing what you asked about", which stopped being
// true when the measurement showed a deeper walk still following relations —
// the shuffled control LOST to plain search at that depth.
func TestGraphRecall_HopCeilingMessageNamesTheBudget(t *testing.T) {
	d, ctx, _, ids := graphFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","seed_ids":["`+ids["Ada"]+
		`"],"hops":99}`)
	if !r.IsError {
		t.Fatal("hops past the ceiling must be refused")
	}
	if !strings.Contains(r.Text, "budget_chars") {
		t.Errorf("the refusal should point at what actually bounds a walk, got %q", r.Text)
	}
}
