package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/fallback"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Document filters on Memory op=search / op=recall: the board assistant recalls
// through the Memory tool and needs the gates' own reports kept out of what it
// recalls, without losing the facts and notes a mixed search returns beside them.

// memDocCorpus is a scope holding, all matching the query alike: norm clauses
// under /qg/norms, the gates' reports under /qg/reports, a note an agent wrote,
// and a consolidated fact homed in a chunk (provenance on a doc.chunk: row).
type memDocCorpus struct {
	*corpus
	m       *Memory
	ctx     context.Context
	norms   []string
	reports []string
}

func newMemDocCorpus(t *testing.T) *memDocCorpus {
	t.Helper()
	c := newCorpus(t, "norm", "clause")
	ctx := tools.UpdateRunPolicy(c.ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
	c.ctx = ctx
	mc := &memDocCorpus{corpus: c, ctx: ctx,
		m: &Memory{Store: c.spy, Embedder: c.d.Embedder, DocumentFilters: c.d}}
	norms := c.doc("/qg/norms/ed1")
	for i := 0; i < 3; i++ {
		mc.norms = append(mc.norms, c.chunk(norms, "norm clause", nil))
	}
	reports := c.doc("/qg/reports/r1")
	for i := 0; i < 3; i++ {
		mc.reports = append(mc.reports, c.chunk(reports, "norm clause", nil))
	}
	if r := mc.mem(`{"op":"set","scope":"user","key":"notes/n1","value":"norm clause","embed":true}`); r.IsError {
		t.Fatalf("seed the note: %s", r.Text)
	}
	// A fact homed in a chunk, as the consolidator writes one: provenance on a
	// doc.chunk: row whose chunk is in no document under either path.
	const factKey = "doc.chunk:fact00000000000000000000000000001"
	v, _ := json.Marshal("norm clause")
	if err := c.vs.Store.MemorySetProvenance(context.Background(), "tnt", store.MemoryScopeUser, "u1", factKey, v, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
		t.Fatal(err)
	}
	vec, _ := c.d.Embedder.Embed(context.Background(), []string{"norm clause"})
	if err := c.vs.MemoryEmbedSet(context.Background(), "tnt", store.MemoryScopeUser, "u1", factKey, store.MemoryEmbedding{
		Provider: "fake", Model: "m1", Dimension: len(vec[0]), Vector: vec[0], EmbedText: "norm clause", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return mc
}

func (mc *memDocCorpus) mem(body string) tools.Result {
	mc.t.Helper()
	r, err := mc.m.Execute(mc.ctx, json.RawMessage(body))
	if err != nil {
		mc.t.Fatalf("Execute: %v", err)
	}
	return r
}

// keys runs a search / recall and returns the hit keys and the filter echo.
func (mc *memDocCorpus) keys(body string) (map[string]bool, map[string]any) {
	mc.t.Helper()
	r := mc.mem(body)
	if r.IsError {
		mc.t.Fatalf("%s: %s", body, r.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		mc.t.Fatalf("decode %s: %v", r.Text, err)
	}
	got := map[string]bool{}
	for _, list := range []string{"entries", "memories"} {
		rows, _ := out[list].([]any)
		for _, row := range rows {
			e := row.(map[string]any)
			k, _ := e["key"].(string)
			if k == "" {
				k, _ = e["id"].(string)
			}
			got[k] = true
		}
	}
	echo, _ := out["filter"].(map[string]any)
	return got, echo
}

func chunkKeys(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = chunkBodyKey(id)
	}
	return out
}

func TestMemorySearch_UnderPathsRestrictDocumentsOnly(t *testing.T) {
	mc := newMemDocCorpus(t)
	const fact = "doc.chunk:fact00000000000000000000000000001"

	// Precondition: unfiltered, every kind of row comes back, and no echo.
	got, echo := mc.keys(`{"op":"search","scope":"user","query":"norm clause","top_k":50}`)
	for _, k := range append(append(chunkKeys(mc.reports), chunkKeys(mc.norms)...), "notes/n1", fact) {
		if !got[k] {
			t.Fatalf("precondition: unfiltered search lacks %s: %v", k, got)
		}
	}
	if echo != nil {
		t.Errorf("a filter-free search carries a filter echo: %v", echo)
	}

	for _, body := range []string{
		`{"op":"search","scope":"user","query":"norm clause","top_k":50,"under_paths":["/qg/norms"]}`,
		`{"op":"search","scope":"user","query":"norm clause","top_k":50,"exclude_under_paths":["/qg/reports"]}`,
	} {
		got, echo = mc.keys(body)
		for _, k := range chunkKeys(mc.reports) {
			if got[k] {
				t.Errorf("%s: returned a report chunk %s", body, k)
			}
		}
		for _, k := range append(chunkKeys(mc.norms), "notes/n1", fact) {
			if !got[k] {
				t.Errorf("%s: dropped %s — only document rows may be filtered", body, k)
			}
		}
		if echo == nil || echoInt(echo, "matched_documents") < 1 {
			t.Errorf("%s: echo = %v, want matched_documents reported", body, echo)
		}
	}
}

func TestMemorySearch_UnresolvedPathKeepsFactsAndNotes(t *testing.T) {
	mc := newMemDocCorpus(t)
	mc.spy.reset()
	got, echo := mc.keys(`{"op":"search","scope":"user","query":"norm clause","top_k":50,"under_paths":["/nowhere"]}`)
	// An empty id list would mean "every document", so the documents are dropped
	// at the store instead: the document class and its units, on every leg. (The
	// fake store does not apply provenance, so this is asserted on what each leg
	// was asked, not on what the fake returned.)
	if len(mc.spy.filters) == 0 {
		t.Fatal("the search ran no leg")
	}
	for _, f := range mc.spy.filters {
		if f.ExcludeDocumentPrefix != memrank.DocumentChunkKeyPrefix || f.ExcludeUnitPrefix != memrank.DocumentUnitKeyPrefix ||
			len(f.IDs) != 0 || len(f.ExcludeIDs) != 0 {
			t.Errorf("leg filter = %+v, want documents and units excluded and no id list", f)
		}
	}
	if !got["notes/n1"] {
		t.Errorf("a path matching no document dropped the note: %v", got)
	}
	if echoInt(echo, "matched_documents") != 0 || echoInt(echo, "matched_chunks") != 0 {
		t.Errorf("echo = %v, want zero matches", echo)
	}

	// Documents only: nothing can match, so the store is not searched at all.
	mc.emb.n.Store(0)
	got, echo = mc.keys(`{"op":"search","scope":"user","query":"norm clause","sources":["documents"],"under_paths":["/nowhere"]}`)
	if len(got) != 0 || echo == nil {
		t.Errorf("documents-only search over an unresolved path = %v / %v, want no entries and an echo", got, echo)
	}
	if n := mc.emb.n.Load(); n != 0 {
		t.Errorf("an unresolvable documents-only filter embedded the query %d times", n)
	}
}

func TestMemorySearch_DocumentFilterRefusedOnAFactsOnlySelector(t *testing.T) {
	mc := newMemDocCorpus(t)
	for _, body := range []string{
		`{"op":"search","scope":"user","query":"norm","sources":["facts"],"under_paths":["/qg/norms"]}`,
		`{"op":"search","scope":"user","query":"norm","sources":["traces"],"exclude_document_ids":["x"]}`,
		`{"op":"search","scope":"user","query":"norm","prefix":"notes/","document_ids":["x"]}`,
		// recall's default is facts and notes: a document filter needs documents asked for.
		`{"op":"recall","scope":"user","query":"norm","exclude_under_paths":["/qg/reports"]}`,
	} {
		r := mc.mem(body)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
			t.Errorf("%s: want a validation refusal, got %s", body, r.Text)
			continue
		}
		var in map[string]any
		_ = json.Unmarshal([]byte(body), &in)
		for _, sel := range []string{"under_paths", "exclude_under_paths", "document_ids", "exclude_document_ids"} {
			if _, ok := in[sel]; ok && !strings.Contains(r.Text, sel) {
				t.Errorf("%s: the refusal does not name %s: %s", body, sel, r.Text)
			}
		}
	}
}

func TestMemorySearch_DocumentFilterRefusedWithoutDocuments(t *testing.T) {
	mc := newMemDocCorpus(t)
	for name, resolver := range map[string]func(){
		"no resolver":   func() { mc.m.DocumentFilters = nil },
		"no SQL Memory": func() { mc.m.DocumentFilters = &Document{Store: mc.spy} },
		"remote backends": func() {
			mc.m.Backend = fallback.New(inprocess.New(mc.spy, mc.d.Embedder), inprocess.New(mc.spy, mc.d.Embedder), slog.Default())
		},
	} {
		mc.m.DocumentFilters, mc.m.Backend = mc.d, nil
		resolver()
		r := mc.mem(`{"op":"search","scope":"user","query":"norm clause","under_paths":["/qg/norms"]}`)
		if !r.IsError {
			t.Errorf("%s: a document filter was accepted: %s", name, r.Text)
		}
	}
	mc.m.DocumentFilters, mc.m.Backend = nil, nil
	if r := mc.mem(`{"op":"search","scope":"user","query":"norm clause","under_paths":["/qg/norms"]}`); !strings.Contains(r.Text, "documents not configured on this server") {
		t.Errorf("no resolver: refusal = %s", r.Text)
	}
	// A search with no document filter is untouched by the missing resolver.
	if r := mc.mem(`{"op":"search","scope":"user","query":"norm clause"}`); r.IsError {
		t.Errorf("a filter-free search failed without a resolver: %s", r.Text)
	}
}

// TestMemoryRecall_ExcludeUnderPathsReachesEveryScope — a path is a per-scope
// name: the same filter, recalled in each scope, resolves in THAT scope's tree,
// and each answer reports its own scope's counts. (A Memory call reads one scope;
// a caller consulting several makes one call per scope.) The user and agent
// scopes: those are the two whose document bodies share the Memory tool's scope
// id (a tenant document's bodies are keyed by the SQL Memory tenant id, which a
// tenant-scope Memory call does not read — a separate, pre-existing split).
func TestMemoryRecall_ExcludeUnderPathsReachesEveryScope(t *testing.T) {
	mc := newMemDocCorpus(t)
	// The agent scope's own documents under the same two paths.
	agentDoc := func(path string) string {
		out := mc.exec(map[string]any{"op": "create_document", "scope": "agent", "title": "Doc", "path": path})
		return out["document_id"].(string)
	}
	var agentReports, agentNorms []string
	ar, an := agentDoc("/qg/reports/a1"), agentDoc("/qg/norms/a1")
	for i := 0; i < 2; i++ {
		agentReports = append(agentReports, mc.exec(map[string]any{"op": "create_chunk", "scope": "agent", "document_id": ar, "title": "P", "body": "norm clause"})["id"].(string))
	}
	agentNorms = append(agentNorms, mc.exec(map[string]any{"op": "create_chunk", "scope": "agent", "document_id": an, "title": "P", "body": "norm clause"})["id"].(string))

	for _, c := range []struct {
		scope         string
		reports, kept []string
	}{
		{"user", mc.reports, mc.norms},
		{"agent", agentReports, agentNorms},
	} {
		got, echo := mc.keys(`{"op":"recall","scope":"` + c.scope + `","query":"norm clause","top_k":50,"sources":["documents"],"exclude_under_paths":["/qg/reports"]}`)
		for _, k := range chunkKeys(c.reports) {
			if got[k] {
				t.Errorf("%s recall returned its report chunk %s", c.scope, k)
			}
		}
		for _, k := range chunkKeys(c.kept) {
			if !got[k] {
				t.Errorf("%s recall dropped %s, outside the excluded path", c.scope, k)
			}
		}
		// Each scope holds two documents, one excluded: its own count, resolved in
		// its own tree — not the other scope's.
		if echo == nil || echoInt(echo, "matched_documents") != 1 {
			t.Errorf("%s recall echo = %v, want matched_documents 1", c.scope, echo)
		}
	}
}

// TestMemorySearch_FilterIsResolvedAtUseNotAtBuild — boot wires the Memory tool
// to the Document tool before the Document tool's store and SQL Memory exist;
// the resolver must read them when a search runs, not when it was assigned.
func TestMemorySearch_FilterIsResolvedAtUseNotAtBuild(t *testing.T) {
	mc := newMemDocCorpus(t)
	late := &Document{}
	mc.m.DocumentFilters = late
	late.Store, late.SqlMem, late.Embedder = mc.d.Store, mc.d.SqlMem, mc.d.Embedder
	got, echo := mc.keys(`{"op":"search","scope":"user","query":"norm clause","top_k":50,"under_paths":["/qg/norms"]}`)
	if echo == nil {
		t.Fatal("a resolver bound after assignment was not used")
	}
	for _, k := range chunkKeys(mc.reports) {
		if got[k] {
			t.Errorf("returned the report chunk %s", k)
		}
	}
}

// ---- the Document tool's scope gate holds for the resolver too ---------------

func scopeExists(t *testing.T, d *Document, ctx context.Context, scope string) bool {
	t.Helper()
	tenant := sqlScopeTenant(ctx)
	id := tenant
	switch scope {
	case "user":
		id = tools.RunIdentity(ctx).UserID
	case "agent":
		id = tools.AgentName(ctx)
	}
	ok, err := d.SqlMem.ScopeExists(ctx, sqlmem.ScopeKey{Tenant: tenant, Scope: scope, ScopeID: id})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestMemorySearch_TenantDocumentFilterIsRefusedAndProvisionsNothing — a
// tenant-scope Memory call holding memory_scopes [tenant] but no SQL Memory
// tenant grant must not reach the tenant's SQL Memory through the filter: the
// Document tool would refuse that scope, and resolving would provision it.
func TestMemorySearch_TenantDocumentFilterIsRefusedAndProvisionsNothing(t *testing.T) {
	mc := newMemDocCorpus(t)
	mc.ctx = tools.UpdateRunPolicy(mc.ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{}
	})
	if scopeExists(t, mc.d, mc.ctx, "tenant") {
		t.Fatal("precondition: the tenant SQL Memory scope already exists")
	}
	r := mc.mem(`{"op":"search","scope":"tenant","query":"norm clause","under_paths":["/qg/norms"]}`)
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
		t.Errorf("tenant-scope document filter = %s, want a validation refusal", r.Text)
	}
	if scopeExists(t, mc.d, mc.ctx, "tenant") {
		t.Error("the refused filter provisioned the tenant's SQL Memory scope")
	}
}

// TestDocumentFilterResolver_ConfinesAnIsolatedMemberAsTheDocumentToolDoes —
// asked directly (not through the Memory tool's own gate), the resolver refuses
// the shared scope to an isolated member exactly as Document op=search does.
func TestDocumentFilterResolver_ConfinesAnIsolatedMemberAsTheDocumentToolDoes(t *testing.T) {
	mc := newMemDocCorpus(t)
	id := tools.RunIdentity(mc.ctx)
	id.Isolated = true
	ctx := tools.WithRunIdentity(mc.ctx, id)
	f := memrank.DocumentFilter{UnderPaths: []string{"/qg/norms"}}

	b, _ := json.Marshal(map[string]any{"op": "search", "scope": "tenant", "query": "norm clause", "under_path": "/qg/norms"})
	if r, _ := mc.d.Execute(ctx, b); !r.IsError {
		t.Fatalf("precondition: Document op=search let an isolated member search the tenant scope: %s", r.Text)
	}
	if _, err := mc.d.ResolveDocumentFilter(ctx, "search", store.MemoryScopeTenant, "", f); err == nil {
		t.Error("the resolver resolved a tenant-scope filter for an isolated member")
	}
	if scopeExists(t, mc.d, ctx, "tenant") {
		t.Error("the refused filter provisioned the tenant's SQL Memory scope")
	}
	// Its own user scope stays open to it, as on Document op=search.
	res, err := mc.d.ResolveDocumentFilter(ctx, "search", store.MemoryScopeUser, "u1", f)
	if err != nil || res.MatchedDocuments != 1 {
		t.Errorf("isolated member's own scope: %+v, %v — want the one norms document", res, err)
	}
}

// TestDocumentFilterResolver_ScopeWithoutDocumentsMatchesNothingAndCreatesNothing
// — a scope whose SQL Memory was never provisioned holds no documents: the
// filter matches nothing and leaves no database (or login role) behind.
func TestDocumentFilterResolver_ScopeWithoutDocumentsMatchesNothingAndCreatesNothing(t *testing.T) {
	mc := newMemDocCorpus(t)
	if scopeExists(t, mc.d, mc.ctx, "agent") {
		t.Fatal("precondition: the agent SQL Memory scope already exists")
	}
	res, err := mc.d.ResolveDocumentFilter(mc.ctx, "search", store.MemoryScopeAgent, "doc-agent",
		memrank.DocumentFilter{ExcludeUnderPaths: []string{"/qg/reports/"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedChunks != 0 || res.MatchedDocuments != 0 || len(res.IDs) != 0 || len(res.ExcludeIDs) != 0 {
		t.Errorf("unprovisioned scope resolved to %+v, want no match", res)
	}
	if got := res.Applied.ExcludeUnderPaths; len(got) != 1 || got[0] != "/qg/reports" {
		t.Errorf("applied = %+v, want the canonical path echoed", res.Applied)
	}
	if scopeExists(t, mc.d, mc.ctx, "agent") {
		t.Error("resolving a filter provisioned the agent's SQL Memory scope")
	}
	// Through the Memory tool: a documents-only search answers empty, without a store call.
	got, echo := mc.keys(`{"op":"search","scope":"agent","query":"norm clause","sources":["documents"],"under_paths":["/qg/norms"]}`)
	if len(got) != 0 || echoInt(echo, "matched_documents") != 0 {
		t.Errorf("agent-scope search = %v / %v, want empty with matched_documents 0", got, echo)
	}
}
