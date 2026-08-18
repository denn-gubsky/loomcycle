package builtin

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestChunkLabels_ResolvesTheDocumentAndTheHeading is the whole feature: a search
// hit addressed only by `doc.chunk:<hex>` gets back the document it came from and
// the heading it sits under.
//
// The two fixture titles are DELIBERATELY different. A test that reused one title
// for both would pass against a projection that swapped the columns, which is the
// likeliest way to get this wrong (the query selects two `title` columns).
func TestChunkLabels_ResolvesTheDocumentAndTheHeading(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Verified writes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])

	child, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":%q}`,
		docID, root, "Reading your coverage", "the strip counts claims, not identities"))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	childID := asStr(child["id"])

	// The MEMORY plane's coordinates, as a search would supply them — not SQL
	// Memory's. Mapping between the two is what docScopeKeyFor owns.
	labels := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1", []string{childID})
	lb, ok := labels[childID]
	if !ok {
		t.Fatalf("no label for %s; got %+v", childID, labels)
	}
	if lb.Document != "Verified writes" {
		t.Errorf("Document = %q, want the DOCUMENT title %q", lb.Document, "Verified writes")
	}
	if lb.Title != "Reading your coverage" {
		t.Errorf("Title = %q, want the CHUNK heading %q", lb.Title, "Reading your coverage")
	}
}

// TestChunkLabels_ResolvesManyChunksInOneCall covers the batching contract: every
// requested id comes back from ONE call, including several chunks of the SAME
// document — the common case for a search, where a few hits usually share a source.
func TestChunkLabels_ResolvesManyChunksInOneCall(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"One doc"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])

	want := map[string]string{}
	ids := []string{}
	for _, title := range []string{"First", "Second", "Third"} {
		c, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":"x"}`,
			docID, root, title))
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", title, r.Text)
		}
		id := asStr(c["id"])
		want[id] = title
		ids = append(ids, id)
	}

	labels := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1", ids)
	if len(labels) != len(want) {
		t.Fatalf("got %d labels for %d distinct chunks: %+v", len(labels), len(want), labels)
	}
	for id, title := range want {
		if labels[id].Title != title {
			t.Errorf("chunk %s title = %q, want %q", id, labels[id].Title, title)
		}
		if labels[id].Document != "One doc" {
			t.Errorf("chunk %s document = %q, want %q", id, labels[id].Document, "One doc")
		}
	}
}

// TestChunkLabels_ScopeKeyMatchesResolveScope is the DRIFT test. The Memory plane
// and SQL Memory key the same logical scope differently (tenant most of all), so
// this label lookup carries a second copy of that mapping. If Document.resolveScope
// ever changes, a silent divergence here would mean labels resolve against an empty
// schema and quietly stop appearing — a miss that looks exactly like "no labels
// available". Compare the two directly instead.
func TestChunkLabels_ScopeKeyMatchesResolveScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// The fixture's identity: tenant "tnt", user "u1", agent "doc-agent".
	for _, tc := range []struct {
		requested string
		scope     store.MemoryScope
		scopeID   string // the MEMORY plane's id — "" for tenant, by design
	}{
		{"agent", store.MemoryScopeAgent, "doc-agent"},
		{"user", store.MemoryScopeUser, "u1"},
		{"tenant", store.MemoryScopeTenant, ""},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			// resolveScope gates scope=tenant on an ACL, so grant it for the comparison;
			// this test is about the KEY, not the authorization.
			// The tenant scope is gated on BOTH planes; grant them for the
			// comparison — this test is about the KEY, not the authorization.
			gctx := ctx
			if tc.requested == "tenant" {
				gctx = tools.WithMemoryPolicy(gctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
				gctx = tools.WithSqlMemPolicy(gctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
			}
			want, _, err := d.resolveScope(gctx, tc.requested)
			if err != nil {
				t.Skipf("resolveScope(%s) unavailable in this fixture: %v", tc.requested, err)
			}
			got, ok := docScopeKeyFor("tnt", tc.scope, tc.scopeID)
			if !ok {
				t.Fatalf("docScopeKeyFor(%s) refused; resolveScope produced %+v", tc.scope, want)
			}
			if got != want {
				t.Errorf("scope %s: label lookup keys %+v, Document keys %+v — the two planes have drifted",
					tc.scope, got, want)
			}
		})
	}
}

// TestChunkLabels_BestEffortWhenLabelsCannotBeRead: every missing piece costs the
// cosmetic label and nothing else. A search must not fail, and must not report a
// wrong label, because SQL Memory is absent or a chunk has since gone.
func TestChunkLabels_BestEffortWhenLabelsCannotBeRead(t *testing.T) {
	d, ctx, _ := documentFixture(t)

	if got := ChunkLabelsFor(ctx, nil, "tnt", store.MemoryScopeUser, "u1", []string{"c1"}); got != nil {
		t.Errorf("no SQL Memory must yield no labels, got %+v", got)
	}
	if got := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1", nil); got != nil {
		t.Errorf("no ids must yield no labels, got %+v", got)
	}
	if got := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "", []string{"c1"}); got != nil {
		t.Errorf("a scope with no id must yield no labels, got %+v", got)
	}
	if got := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScope("run"), "r1", []string{"c1"}); got != nil {
		t.Errorf("a scope SQL Memory cannot key must yield no labels, got %+v", got)
	}
	// A scope that exists but holds no document tables yet: a store fault, not a
	// panic, and no labels.
	if got := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "never-used", []string{"c1"}); len(got) != 0 {
		t.Errorf("an empty scope must yield no labels, got %+v", got)
	}
	// A real scope, an id that is not in it.
	if _, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"real"}`); r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	got := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1", []string{"no-such-chunk"})
	if _, present := got["no-such-chunk"]; present {
		t.Errorf("an unknown chunk must have no entry, got %+v", got)
	}
}

// TestChunkLabels_RepeatedIDsDoNotCrowdOutTheCap pins the ORDER of two steps that
// look independent: ids are deduplicated BEFORE the lookup is capped. A page of
// hits from one document repeats that document's chunk ids, so capping first would
// let a repeat consume the budget and silently drop a DIFFERENT chunk's label —
// the label would just be missing, which is indistinguishable from "no labels
// available". Sized off the constant so it stays honest if the cap moves.
func TestChunkLabels_RepeatedIDsDoNotCrowdOutTheCap(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Crowded"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])

	mk := func(title string) string {
		c, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":"x"}`,
			docID, root, title))
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", title, r.Text)
		}
		return asStr(c["id"])
	}
	repeated, last := mk("Repeated"), mk("Last")

	// The repeated id fills the whole budget on its own; the distinct one is asked
	// for once, at the end.
	ids := make([]string, 0, maxChunkLabelLookup+1)
	for i := 0; i < maxChunkLabelLookup; i++ {
		ids = append(ids, repeated)
	}
	ids = append(ids, last)

	labels := ChunkLabelsFor(ctx, d.SqlMem, "tnt", store.MemoryScopeUser, "u1", ids)
	if labels[repeated].Title != "Repeated" {
		t.Errorf("repeated chunk lost its label: %+v", labels[repeated])
	}
	if labels[last].Title != "Last" {
		t.Errorf("the distinct chunk was crowded out by %d repeats of another id: %+v",
			maxChunkLabelLookup, labels)
	}
}

// TestMemorySearch_DocumentHitCarriesItsDocumentAndHeading is the IN-BAND twin of
// the HTTP test: an agent searching its own memory gets the same labels an operator
// gets from /v1/_memory/search. The two projections are hand-maintained copies —
// this pair is what catches a field added to one and not the other.
//
// It also closes a real gap the labels exposed: the in-band search never carried
// chunk_id at all, so an agent could not even fetch the chunk it had just been
// shown prose from.
func TestMemorySearch_DocumentHitCarriesItsDocumentAndHeading(t *testing.T) {
	d, ctx, base := documentFixture(t)
	vs := newVectorStore(base)
	d.Store = vs
	// The SAME embedder for both tools: the Document tool embeds the chunk body on
	// write, and the Memory tool embeds the query — a one-hot vocabulary makes the
	// cosine match exact.
	fake := newFakeEmbedder("fake", "stub", "claims", "strip", "counts")
	d.Embedder = fake

	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Verified writes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	child, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":%q}`,
		asStr(out["document_id"]), asStr(out["root_chunk_id"]),
		"Reading your coverage", "the strip counts claims"))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	childID := asStr(child["id"])

	m := &Memory{Store: vs, Embedder: fake, SqlMem: d.SqlMem}
	// The Memory tool gates scopes on the agent's own ACL; the Document tool's
	// grants above do not carry over.
	mctx := tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}})
	res, err := m.Execute(mctx, []byte(`{"op":"search","scope":"user","query":"claims","top_k":5}`))
	if err != nil {
		t.Fatalf("Memory.Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}

	var got struct {
		Entries []struct {
			Key      string `json:"key"`
			Kind     string `json:"kind"`
			ChunkID  string `json:"chunk_id"`
			Document string `json:"document"`
			Title    string `json:"title"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("decode search result: %v\n%s", err, res.Text)
	}
	// The ROOT chunk is embedded too (its body is empty, so the embed path falls back
	// to the title), so a search legitimately returns both. Assert on the CHILD — the
	// one whose heading differs from its document's title, and therefore the only one
	// that can catch a swapped projection.
	var childHit *struct {
		Key      string `json:"key"`
		Kind     string `json:"kind"`
		ChunkID  string `json:"chunk_id"`
		Document string `json:"document"`
		Title    string `json:"title"`
	}
	for i := range got.Entries {
		if got.Entries[i].Kind == "document" && got.Entries[i].ChunkID == childID {
			childHit = &got.Entries[i]
		}
	}
	if childHit == nil {
		t.Fatalf("no document hit for chunk %s; search returned: %s", childID, res.Text)
	}
	if childHit.Key != "doc.chunk:"+childID {
		t.Errorf("key = %q, want the addressable doc.chunk form", childHit.Key)
	}
	if childHit.Document != "Verified writes" {
		t.Errorf("document = %q, want the DOCUMENT title %q", childHit.Document, "Verified writes")
	}
	if childHit.Title != "Reading your coverage" {
		t.Errorf("title = %q, want the CHUNK heading %q", childHit.Title, "Reading your coverage")
	}
}
