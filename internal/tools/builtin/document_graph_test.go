package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// graphFixture builds a small entity graph:
//
//	Ada --works_at--> Analytical Engine Co
//	Ada --knows-----> Charles
//	Analytical Engine Co --located_in--> London     (2 hops from Ada)
//
// Ada is the seed in most tests, so Charles and the company are one hop and London
// is two — which is what makes the hop bound observable.
func graphFixture(t *testing.T) (*Document, contextT, string, map[string]string) {
	t.Helper()
	d, ctx, docID, root := entityFixture(t)
	ids := map[string]string{}
	mk := func(name, typ string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+root+`","title":"`+name+`","type":"`+typ+`","natural_key":"`+typ+`:`+name+`"}`)
		if r.IsError {
			t.Fatalf("upsert %s: %s", name, r.Text)
		}
		id := asStr(out["id"])
		ids[name] = id
		return id
	}
	link := func(from, to, kind string) {
		if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
			`","from_id":"`+ids[from]+`","to_id":"`+ids[to]+`","kind":"`+kind+`"}`); r.IsError {
			t.Fatalf("link %s->%s: %s", from, to, r.Text)
		}
	}
	mk("Ada", "person")
	mk("Charles", "person")
	mk("Analytical Engine Co", "organization")
	mk("London", "location")
	link("Ada", "Analytical Engine Co", "works_at")
	link("Ada", "Charles", "knows")
	link("Analytical Engine Co", "London", "located_in")
	return d, ctx, docID, ids
}

func graphIDs(out map[string]any) map[string]int {
	got := map[string]int{}
	chunks, _ := out["chunks"].([]any)
	for _, c := range chunks {
		m, _ := c.(map[string]any)
		title, _ := m["title"].(string)
		hop := 0
		if h, ok := m["hop"].(float64); ok {
			hop = int(h)
		}
		got[title] = hop
	}
	return got
}

// TestGraphRecall_HopBoundIsObservable: 0 hops is the seed alone, 1 reaches its
// neighbours, 2 reaches theirs. Without a bound each hop multiplies the frontier by
// the average degree, and the result stops describing what was asked.
func TestGraphRecall_HopBoundIsObservable(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	for _, tc := range []struct {
		hops int
		want []string
		deny []string
	}{
		{0, []string{"Ada"}, []string{"Charles", "London"}},
		{1, []string{"Ada", "Charles", "Analytical Engine Co"}, []string{"London"}},
		{2, []string{"Ada", "Charles", "Analytical Engine Co", "London"}, nil},
	} {
		t.Run(fmt.Sprintf("hops=%d", tc.hops), func(t *testing.T) {
			out, r := docExec(t, d, ctx, fmt.Sprintf(
				`{"op":"graph_recall","scope":"user","document_id":"%s","seed_ids":["%s"],"hops":%d}`,
				docID, ids["Ada"], tc.hops))
			if r.IsError {
				t.Fatalf("graph_recall: %s", r.Text)
			}
			got := graphIDs(out)
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("hops=%d should reach %q; got %v", tc.hops, w, got)
				}
			}
			for _, dn := range tc.deny {
				if _, ok := got[dn]; ok {
					t.Errorf("hops=%d must NOT reach %q (that is %d+ hops away); got %v", tc.hops, dn, tc.hops+1, got)
				}
			}
		})
	}
}

// TestGraphRecall_WalksBothDirections: an entity is as related to the facts that
// point AT it as to the ones it points at, so a forward-only walk answers half the
// question. This is what PR 1's chunk_edges(to_id, kind) index exists for.
func TestGraphRecall_WalksBothDirections(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	// London is only ever an edge TARGET. Seeding from it must still find the company.
	out, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["London"]+`"],"hops":1}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	got := graphIDs(out)
	if _, ok := got["Analytical Engine Co"]; !ok {
		t.Errorf("a reverse hop was not followed — seeding from an edge TARGET found nothing: %v", got)
	}
}

// TestGraphRecall_ShallowestPathWins: a diamond must report a chunk at its nearest
// distance. Reporting two hops when a one-hop path exists overstates how far the
// chunk is from the question.
func TestGraphRecall_ShallowestPathWins(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	// Add Ada --lives_in--> London, so London is reachable at 1 AND 2 hops.
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
		`","from_id":"`+ids["Ada"]+`","to_id":"`+ids["London"]+`","kind":"lives_in"}`); r.IsError {
		t.Fatalf("link: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["Ada"]+`"],"hops":2}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if hop := graphIDs(out)["London"]; hop != 1 {
		t.Errorf("London is one hop away via lives_in but was reported at hop %d", hop)
	}
}

// TestGraphRecall_ExcludesSupersededByDefault is the temporal default: a recall
// that returned both a fact and its correction, with no way to tell them apart, is
// worse than one that returned neither.
func TestGraphRecall_ExcludesSupersededByDefault(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	// Charles is corrected by a replacement.
	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+ids["Ada"]+`","title":"Charles Babbage","natural_key":"person:charles-babbage"}`)
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	newID := asStr(out["id"])
	if _, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+newID+
		`","supersedes_id":"`+ids["Charles"]+`"}`); r.IsError {
		t.Fatalf("supersede: %s", r.Text)
	}

	cur, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["Ada"]+`"],"hops":1}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if _, ok := graphIDs(cur)["Charles"]; ok {
		t.Error("a superseded chunk was returned as current")
	}

	// include_retired asks the other question.
	all, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["Ada"]+`"],"hops":1,"include_retired":true}`)
	if r.IsError {
		t.Fatalf("graph_recall(include_retired): %s", r.Text)
	}
	if _, ok := graphIDs(all)["Charles"]; !ok {
		t.Error("include_retired should return the superseded chunk")
	}
}

// TestGraphRecall_AsOfNeedsBothHalvesOfTheInterval. "What was true then" requires
// the fact to have become true at or before that moment AND not to have stopped
// before it. Checking only invalid_at would return facts the system had not yet
// learned — a memory that answers a question about June with something recorded in
// July.
func TestGraphRecall_AsOfNeedsBothHalvesOfTheInterval(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	seed := ids["Ada"]

	// The facts are LINKED to Ada, not merely parented under her. parent_id is
	// document-tree containment; the graph walk follows chunk_edges. Keeping them
	// separate is what makes a hop count mean something — if the tree counted, a
	// document's entire subtree would be one hop from its root.
	fact := func(title, key string, extra string) string {
		out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
			`","parent_id":"`+seed+`","title":"`+title+`","natural_key":"`+key+`"`+extra+`}`)
		if r.IsError {
			t.Fatalf("upsert %s: %s", title, r.Text)
		}
		id := asStr(out["id"])
		if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
			`","from_id":"`+seed+`","to_id":"`+id+`","kind":"about"}`); r.IsError {
			t.Fatalf("link %s: %s", title, r.Text)
		}
		return id
	}
	// Valid only in a past window, and one that only became true later.
	fact("Lived in Turin", "fact:turin", `,"valid_at":1000,"invalid_at":2000`)
	fact("Lived in Turin later", "fact:turin2", `,"valid_at":5000`)

	at1500, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+seed+`"],"hops":1,"as_of":1500}`)
	if r.IsError {
		t.Fatalf("graph_recall(as_of): %s", r.Text)
	}
	got := graphIDs(at1500)
	if _, ok := got["Lived in Turin"]; !ok {
		t.Errorf("as_of=1500 should see a fact valid 1000..2000: %v", got)
	}
	if _, ok := got["Lived in Turin later"]; ok {
		t.Errorf("as_of=1500 must NOT see a fact that only became true at 5000 — that is answering June with July: %v", got)
	}
}

// TestGraphRecall_ChunksWithNoSidecarAreCurrent: an ordinary chunk, or one written
// before the entity tier existed, has no temporal claim to violate. Excluding it
// would make graph_recall blind to the rest of the document.
func TestGraphRecall_ChunksWithNoSidecarAreCurrent(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	// A plain create_chunk — no natural key, so no sidecar row.
	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+ids["Ada"]+`","title":"Plain note"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","document_id":"`+docID+
		`","from_id":"`+ids["Ada"]+`","to_id":"`+asStr(out["id"])+`","kind":"notes"}`); r.IsError {
		t.Fatalf("link: %s", r.Text)
	}
	res, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["Ada"]+`"],"hops":1}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if _, ok := graphIDs(res)["Plain note"]; !ok {
		t.Error("a chunk with no sidecar row was treated as retired")
	}
}

// TestGraphRecall_QuerySeedsByTitle: the convenience path, honest about being a
// title match rather than a semantic search — for an entity graph the title IS the
// name, and it needs no embedder so it behaves identically on both tiers.
func TestGraphRecall_QuerySeedsByTitle(t *testing.T) {
	d, ctx, docID, _ := graphFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","query":"analytical","hops":1}`)
	if r.IsError {
		t.Fatalf("graph_recall(query): %s", r.Text)
	}
	got := graphIDs(out)
	if hop, ok := got["Analytical Engine Co"]; !ok || hop != 0 {
		t.Errorf("case-insensitive title match should seed the company at hop 0: %v", got)
	}
	if _, ok := got["Ada"]; !ok {
		t.Errorf("its neighbour Ada should be reached in one hop: %v", got)
	}
}

// TestGraphRecall_RefusesNonsense: no starting point, and hops outside the bound.
func TestGraphRecall_RefusesNonsense(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	for _, tc := range []struct{ name, body, want string }{
		{"no seed", `{"op":"graph_recall","scope":"user","document_id":"` + docID + `"}`, "seed_ids"},
		{"too many hops", `{"op":"graph_recall","scope":"user","seed_ids":["` + ids["Ada"] + `"],"hops":3}`, "0..2"},
		{"negative hops", `{"op":"graph_recall","scope":"user","seed_ids":["` + ids["Ada"] + `"],"hops":-1}`, "0..2"},
	} {
		_, r := docExec(t, d, ctx, tc.body)
		if !r.IsError {
			t.Errorf("%s: must be refused", tc.name)
			continue
		}
		if !strings.Contains(r.Text, tc.want) {
			t.Errorf("%s: message should mention %q, got %q", tc.name, tc.want, r.Text)
		}
	}
}

// TestGraphRecall_ReportsHowEachChunkWasReached: a caller that cannot tell a seed
// from a two-hop neighbour cannot tell a direct answer from an association.
func TestGraphRecall_ReportsHowEachChunkWasReached(t *testing.T) {
	d, ctx, docID, ids := graphFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","document_id":"`+docID+
		`","seed_ids":["`+ids["Ada"]+`"],"hops":1}`)
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	chunks, _ := out["chunks"].([]any)
	sawVia := false
	for _, c := range chunks {
		m, _ := c.(map[string]any)
		title, _ := m["title"].(string)
		hop, _ := m["hop"].(float64)
		kind, _ := m["via_kind"].(string)
		if title == "Ada" && hop != 0 {
			t.Errorf("the seed should be hop 0, got %v", hop)
		}
		if title == "Charles" {
			if hop != 1 {
				t.Errorf("Charles should be hop 1, got %v", hop)
			}
			if kind != "knows" {
				t.Errorf("Charles should report the edge kind it was reached by, got %q", kind)
			}
			sawVia = true
		}
	}
	if !sawVia {
		t.Error("no neighbour reported its edge kind")
	}
}
