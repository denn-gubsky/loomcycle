package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/fallback"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// documents_scope: one Memory call searches the call's own scope AND the documents
// of another, ranked as one list. The board assistant keeps its notes in user and
// reads the board's files in tenant.

type dsFixture struct {
	t   *testing.T
	d   *Document
	m   *Memory
	emb *countingEmbedder
	ctx context.Context // every grant
	// chunk ids
	tenantNorm, tenantReport, userNorm, userReport, agentDoc string
}

func newDSFixture(t *testing.T) *dsFixture {
	t.Helper()
	d, vs, ctx := tenantDocFixture(t, "tnt", "quorum", "board", "norm", "report", "agent")
	emb := &countingEmbedder{Embedder: d.Embedder}
	d.Embedder = emb
	f := &dsFixture{t: t, d: d, emb: emb, ctx: ctx,
		m: &Memory{Store: &classFilterStore{vs}, Embedder: emb, SqlMem: d.SqlMem, DocumentFilters: d}}
	chunk := func(scope, path, body string) string {
		doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": scope, "title": "Doc", "path": path})
		return docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": scope,
			"document_id": doc["document_id"], "title": "Part", "body": body})["id"].(string)
	}
	// Every score within one scope differs for the query "quorum board": the
	// shared fake store breaks ties in map order, and the sugar test compares
	// responses byte for byte.
	f.tenantNorm = chunk("tenant", "/norms/board", "quorum board norm")
	f.tenantReport = chunk("tenant", "/reports/r1", "quorum report")
	f.userNorm = chunk("user", "/norms/mine", "quorum norm")
	f.userReport = chunk("user", "/reports/mine", "quorum board report")
	f.agentDoc = chunk("agent", "/agent/notes", "quorum agent")
	for _, scope := range []string{"user", "tenant"} {
		if r := memRun(t, f.m, ctx, `{"op":"set","scope":"`+scope+`","key":"notes/q","value":"quorum board","embed":true}`); r.IsError {
			t.Fatalf("seed %s note: %s", scope, r.Text)
		}
	}
	return f
}

// call runs a Memory call that must succeed: hit key → its `scope` ("" when the
// hit has none), and the decoded response.
func (f *dsFixture) call(ctx context.Context, body string) (map[string]string, map[string]any) {
	f.t.Helper()
	r := memRun(f.t, f.m, ctx, body)
	if r.IsError {
		f.t.Fatalf("%s: %s", body, r.Text)
	}
	return dsHits(f.t, r)
}

func dsHits(t *testing.T, r tools.Result) (map[string]string, map[string]any) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		t.Fatalf("decode %s: %v", r.Text, err)
	}
	hits := map[string]string{}
	for _, list := range []string{"entries", "memories"} {
		rows, _ := out[list].([]any)
		for _, row := range rows {
			e := row.(map[string]any)
			k, _ := e["key"].(string)
			if k == "" {
				k, _ = e["id"].(string)
			}
			s, _ := e["scope"].(string)
			hits[k] = s
		}
	}
	return hits, out
}

func TestMemoryRecall_DocumentsScopeFusesTenantDocumentsWithUserMemory(t *testing.T) {
	f := newDSFixture(t)
	hits, _ := f.call(f.ctx, `{"op":"recall","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`)
	want := map[string]string{
		"notes/q":                    "user",
		chunkBodyKey(f.tenantNorm):   "tenant",
		chunkBodyKey(f.tenantReport): "tenant",
	}
	for k, s := range want {
		if hits[k] != s {
			t.Errorf("%s: scope %q, want %q (hits %v)", k, hits[k], s, hits)
		}
	}
	// Recall's own default still holds for the user scope (facts and notes, no
	// prose), and the tenant leg takes documents only — not the tenant's note,
	// which shares its key with the user's.
	for _, k := range []string{chunkBodyKey(f.userNorm), chunkBodyKey(f.userReport)} {
		if _, ok := hits[k]; ok {
			t.Errorf("recall returned the user's own document %s: %v", k, hits)
		}
	}
	if len(hits) != len(want) {
		t.Errorf("hits %v, want exactly %v", hits, want)
	}
	for k, s := range hits {
		if s == "" {
			t.Errorf("%s carries no scope", k)
		}
	}
}

// grantCtx is f.ctx with exactly these grants.
func grantCtx(ctx context.Context, mem, sql []string) context.Context {
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: mem}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: sql}
	})
}

func subsets(all []string) [][]string {
	var out [][]string
	for mask := 0; mask < 1<<len(all); mask++ {
		var s []string
		for i, v := range all {
			if mask&(1<<i) != 0 {
				s = append(s, v)
			}
		}
		out = append(out, s)
	}
	return out
}

// Every pairing of a call scope with a different documents scope, under every
// combination of memory_scopes and sql_scopes: the document leg is allowed exactly
// when Document op=search in that scope is allowed AND memory_scopes holds it.
// Never wider than Document op=search; a refusal is a permission error naming the
// missing grant, and refuses the whole call.
func TestMemorySearch_DocumentsScopeNeedsWhatDocumentSearchNeeds(t *testing.T) {
	f := newDSFixture(t)
	scopes := []string{"agent", "user", "tenant"}
	for _, own := range scopes {
		for _, leg := range scopes {
			if leg == own {
				continue
			}
			for _, mem := range subsets(scopes) {
				if !contains(mem, own) {
					continue // the call's own scope must be granted for any call
				}
				for _, sql := range subsets(scopes) {
					ctx := grantCtx(f.ctx, mem, sql)
					docRes, err := f.d.Execute(ctx, json.RawMessage(`{"op":"search","scope":"`+leg+`","query":"quorum"}`))
					if err != nil {
						t.Fatal(err)
					}
					docAllowed := !docRes.IsError
					want := docAllowed && contains(mem, leg)
					for _, op := range []string{"search", "recall"} {
						body := `{"op":"` + op + `","scope":"` + own + `","documents_scope":"` + leg + `","query":"quorum","top_k":50}`
						r := memRun(t, f.m, ctx, body)
						name := op + " " + own + "+" + leg + " mem=" + strings.Join(mem, ",") + " sql=" + strings.Join(sql, ",")
						if r.IsError != !want {
							t.Errorf("%s: refused=%v, want refused=%v (Document op=search allowed=%v): %s", name, r.IsError, !want, docAllowed, r.Text)
							continue
						}
						if !r.IsError {
							hits, _ := dsHits(t, r)
							found := false
							for _, s := range hits {
								found = found || s == leg
							}
							if !found {
								t.Errorf("%s: allowed but no hit from %s: %v", name, leg, hits)
							}
							continue
						}
						if r.Error == nil || r.Error.Category != tools.CategoryPermission {
							t.Errorf("%s: refusal is not a permission error: %+v %s", name, r.Error, r.Text)
						}
						missing := "memory_scopes"
						if contains(mem, leg) {
							missing = "sql_scopes"
						}
						if !strings.Contains(r.Text, missing) || !strings.Contains(r.Text, "documents_scope") {
							t.Errorf("%s: refusal does not name documents_scope and the missing %s: %s", name, missing, r.Text)
						}
						if strings.Contains(r.Text, `"entries"`) || strings.Contains(r.Text, `"memories"`) {
							t.Errorf("%s: a refused call answered from one scope: %s", name, r.Text)
						}
					}
				}
			}
		}
	}
}

// A user documents scope needs the run's user id, as Document op=search does.
func TestMemorySearch_DocumentsScopeNeedsTheUserID(t *testing.T) {
	f := newDSFixture(t)
	ctx := tools.WithRunIdentity(f.ctx, tools.RunIdentityValue{AgentID: "a", TenantID: "tnt"})
	r := memRun(t, f.m, ctx, `{"op":"search","scope":"agent","documents_scope":"user","query":"quorum"}`)
	if !r.IsError || !strings.Contains(r.Text, "user_id") || strings.Contains(r.Text, `"entries"`) {
		t.Errorf("a user documents scope with no user id: %s", r.Text)
	}
}

func TestMemorySearch_DocumentsScopeConfinesIsolatedMembers(t *testing.T) {
	f := newDSFixture(t)
	ctx := tools.WithRunIdentity(f.ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt", Isolated: true})
	for _, op := range []string{"search", "recall"} {
		r := memRun(t, f.m, ctx, `{"op":"`+op+`","scope":"user","documents_scope":"tenant","query":"quorum","top_k":50}`)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission || !strings.Contains(r.Text, "isolated") {
			t.Errorf("%s: an isolated member reached the tenant's documents: %+v %s", op, r.Error, r.Text)
		}
		// Its own scopes stay open to it.
		hits, _ := f.call(ctx, `{"op":"`+op+`","scope":"user","documents_scope":"agent","query":"quorum","top_k":50}`)
		if hits[chunkBodyKey(f.agentDoc)] != "agent" {
			t.Errorf("%s: an isolated member lost its own agent documents: %v", op, hits)
		}
	}
}

// recordingRanker reverses whatever it is shown and records it.
type recordingRanker struct {
	mu    sync.Mutex
	calls int
	texts []string
}

func (r *recordingRanker) Rank(_ context.Context, _ string, texts []string, _ int) ([]int, memrank.RerankReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.texts = append([]string(nil), texts...)
	order := make([]int, len(texts))
	for i := range order {
		order[i] = len(texts) - 1 - i
	}
	return order, memrank.RerankReport{Applied: true, Candidates: len(texts)}
}

// The rerank is shown ONE pool, drawn from both scopes, once — and a search of
// the agent's own notes, which on its own the rerank would not touch under the
// default (documents-only) sources, is reranked because the pool now holds
// documents.
func TestMemorySearch_DocumentsScopeRerankSeesOnePool(t *testing.T) {
	f := newDSFixture(t)
	rr := &recordingRanker{}
	f.m.Reranker = rr
	on := true
	ctx := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.Memory.Rerank = &config.MemoryRerank{Enabled: &on}
	})
	hits, out := f.call(ctx, `{"op":"search","scope":"user","sources":["facts","notes"],"documents_scope":"tenant","query":"quorum board","top_k":50}`)
	if rr.calls != 1 {
		t.Fatalf("the rerank ran %d times, want once over one pool", rr.calls)
	}
	if out["reranked"] != true {
		t.Errorf("reranked = %v, want true", out["reranked"])
	}
	var sawNote, sawTenantDoc bool
	for _, txt := range rr.texts {
		sawNote = sawNote || strings.Contains(txt, "quorum board") && !strings.Contains(txt, "norm") && !strings.Contains(txt, "report")
		sawTenantDoc = sawTenantDoc || strings.Contains(txt, "quorum board norm")
	}
	if !sawNote || !sawTenantDoc {
		t.Errorf("the rerank pool lacks one scope (note %v, tenant document %v): %q", sawNote, sawTenantDoc, rr.texts)
	}
	if hits["notes/q"] != "user" || hits[chunkBodyKey(f.tenantNorm)] != "tenant" {
		t.Errorf("hits after the rerank: %v", hits)
	}
}

// captureBackend records the queries a search hands its backend.
type captureBackend struct {
	memrank.Backend
	mu      sync.Mutex
	queries []memrank.SearchQuery
}

func (c *captureBackend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memrank.SearchQuery,
	rank memrank.RankConfig, dedup memrank.DedupConfig) (memrank.SearchResult, error) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
	return c.Backend.Search(ctx, scope, scopeID, q, rank, dedup)
}

// Without documents_scope nothing changes: no second leg reaches the backend, no
// hit carries a scope, and a filter echo names none — the response is the one
// this call has always produced.
func TestMemorySearch_NoDocumentsScopeIsByteIdentical(t *testing.T) {
	f := newDSFixture(t)
	for _, body := range []string{
		`{"op":"search","scope":"user","query":"quorum board","top_k":50}`,
		`{"op":"search","scope":"user","query":"quorum board","top_k":50,"under_paths":["/norms"]}`,
		`{"op":"search","scope":"tenant","query":"quorum board","top_k":50,"sources":["documents"]}`,
	} {
		r := memRun(t, f.m, f.ctx, body)
		if r.IsError {
			t.Fatalf("%s: %s", body, r.Text)
		}
		// The same call with an empty documents_scope is the same call.
		empty := memRun(t, f.m, f.ctx, strings.Replace(body, `"top_k":50`, `"top_k":50,"documents_scope":""`, 1))
		if empty.Text != r.Text {
			t.Errorf("%s: an empty documents_scope changed the response:\n%s\n%s", body, r.Text, empty.Text)
		}
		hits, out := dsHits(t, r)
		for k, s := range hits {
			if s != "" {
				t.Errorf("%s: %s carries scope %q without documents_scope", body, k, s)
			}
		}
		if echo, ok := out["filter"].(map[string]any); ok {
			if _, has := echo["scope"]; has {
				t.Errorf("%s: the filter echo names a scope without documents_scope: %v", body, echo)
			}
		}
	}
	// The query the backend is handed carries no second leg. (Through a wrapper,
	// which the document filters refuse as a non-local backend, so filter-free.)
	cb := &captureBackend{Backend: inprocess.New(f.m.Store, f.emb)}
	f.m.Backend = cb
	f.call(f.ctx, `{"op":"search","scope":"user","query":"quorum board","top_k":50}`)
	if len(cb.queries) != 1 {
		t.Fatalf("captured %d searches, want 1", len(cb.queries))
	}
	for _, q := range cb.queries {
		if q.Documents != nil {
			t.Errorf("a documents leg reached the backend without documents_scope: %+v", q.Documents)
		}
	}
}

// documents_scope equal to scope is exactly the call with documents added to
// sources, byte for byte.
func TestMemorySearch_DocumentsScopeEqualToScopeIsSugar(t *testing.T) {
	f := newDSFixture(t)
	for _, c := range []struct{ sugar, plain string }{
		// Recall's default is facts and notes: documents are added to it.
		{`{"op":"recall","scope":"user","documents_scope":"user","query":"quorum board","top_k":50}`,
			`{"op":"recall","scope":"user","sources":["facts","notes","documents"],"query":"quorum board","top_k":50}`},
		{`{"op":"search","scope":"user","documents_scope":"user","sources":["facts","notes"],"query":"quorum board","top_k":50}`,
			`{"op":"search","scope":"user","sources":["facts","notes","documents"],"query":"quorum board","top_k":50}`},
		// A search with no sources already returns documents.
		{`{"op":"search","scope":"tenant","documents_scope":"tenant","query":"quorum board","top_k":50}`,
			`{"op":"search","scope":"tenant","query":"quorum board","top_k":50}`},
		// The document filters apply to the call's own scope, as they always have.
		{`{"op":"search","scope":"user","documents_scope":"user","sources":["documents"],"under_paths":["/norms"],"query":"quorum","top_k":50}`,
			`{"op":"search","scope":"user","sources":["documents"],"under_paths":["/norms"],"query":"quorum","top_k":50}`},
	} {
		s, p := memRun(t, f.m, f.ctx, c.sugar), memRun(t, f.m, f.ctx, c.plain)
		if s.IsError || s.Text != p.Text {
			t.Errorf("%s\n  = %s\nwant %s", c.sugar, s.Text, p.Text)
		}
	}
	// It refuses what the plain call refuses, in the same words.
	s := memRun(t, f.m, f.ctx, `{"op":"search","scope":"user","documents_scope":"user","sources":["notes"],"query":"quorum"}`)
	p := memRun(t, f.m, f.ctx, `{"op":"search","scope":"user","sources":["notes","documents"],"query":"quorum"}`)
	if !s.IsError || s.Text != p.Text {
		t.Errorf("sugar refusal %q, want the plain call's %q", s.Text, p.Text)
	}
	// It works on a backend that cannot serve a second scope: it is not one.
	f.m.Backend = fallback.New(inprocess.New(f.m.Store, f.emb), inprocess.New(f.m.Store, f.emb), slog.Default())
	if r := memRun(t, f.m, f.ctx, `{"op":"search","scope":"user","documents_scope":"user","query":"quorum"}`); r.IsError {
		t.Errorf("equal documents_scope refused on a non-local backend: %s", r.Text)
	}
}

func TestMemorySearch_DocumentFiltersAimAtTheDocumentLeg(t *testing.T) {
	f := newDSFixture(t)
	hits, out := f.call(f.ctx, `{"op":"search","scope":"user","documents_scope":"tenant","under_paths":["/norms"],"query":"quorum board","top_k":50}`)
	if hits[chunkBodyKey(f.tenantNorm)] != "tenant" {
		t.Errorf("the tenant norm is missing: %v", hits)
	}
	if _, ok := hits[chunkBodyKey(f.tenantReport)]; ok {
		t.Errorf("the filter did not reach the tenant leg: %v", hits)
	}
	// The user's own documents are not filtered: under_paths aims at the leg.
	for _, k := range []string{chunkBodyKey(f.userNorm), chunkBodyKey(f.userReport), "notes/q"} {
		if hits[k] != "user" {
			t.Errorf("%s: scope %q, want user (unfiltered): %v", k, hits[k], hits)
		}
	}
	echo, _ := out["filter"].(map[string]any)
	if echo["scope"] != "tenant" || echo["matched_documents"] != float64(1) {
		t.Errorf("filter echo %v, want the tenant leg's: scope tenant, 1 document", echo)
	}

	// A filter that matches nothing there leaves the leg empty, not the call.
	hits, out = f.call(f.ctx, `{"op":"search","scope":"user","documents_scope":"tenant","under_paths":["/nowhere"],"query":"quorum board","top_k":50}`)
	for k, s := range hits {
		if s != "user" {
			t.Errorf("%s from %q, want only the user's own hits", k, s)
		}
	}
	if hits["notes/q"] != "user" {
		t.Errorf("the user's note is missing: %v", hits)
	}
	echo, _ = out["filter"].(map[string]any)
	if echo["scope"] != "tenant" || echo["matched_documents"] != float64(0) {
		t.Errorf("filter echo %v, want the tenant leg's with 0 documents", echo)
	}

	// Recall: the user's prose stays out by recall's default; the leg is filtered.
	hits, _ = f.call(f.ctx, `{"op":"recall","scope":"user","documents_scope":"tenant","exclude_under_paths":["/reports"],"query":"quorum board","top_k":50}`)
	if hits[chunkBodyKey(f.tenantNorm)] != "tenant" || hits["notes/q"] != "user" {
		t.Errorf("recall hits %v", hits)
	}
	if _, ok := hits[chunkBodyKey(f.tenantReport)]; ok {
		t.Errorf("recall: exclude_under_paths did not reach the leg: %v", hits)
	}
}

func TestMemorySearch_QueryEmbeddedOnce(t *testing.T) {
	f := newDSFixture(t)
	for _, body := range []string{
		`{"op":"search","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`,
		`{"op":"recall","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`,
	} {
		f.emb.n.Store(0)
		f.call(f.ctx, body)
		if n := f.emb.n.Load(); n != 1 {
			t.Errorf("%s: the query was embedded %d times, want once for both legs", body, n)
		}
	}
}

// The document leg's hits are held to the document access control like any
// other: a denied tenant document is left out of the fused list.
func TestMemorySearch_DocumentsScopeAppliesTheDocumentACL(t *testing.T) {
	f := newDSFixture(t)
	limited := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/reports/**"}})
	})
	for _, op := range []string{"search", "recall"} {
		hits, _ := f.call(limited, `{"op":"`+op+`","scope":"user","documents_scope":"tenant","query":"quorum board","top_k":50}`)
		if _, ok := hits[chunkBodyKey(f.tenantReport)]; ok {
			t.Errorf("%s: a read-denied tenant document came back through the leg: %v", op, hits)
		}
		if hits[chunkBodyKey(f.tenantNorm)] != "tenant" || hits["notes/q"] != "user" {
			t.Errorf("%s: readable hits lost: %v", op, hits)
		}
	}
}

func TestMemorySearch_DocumentsScopeRefusals(t *testing.T) {
	f := newDSFixture(t)
	for _, c := range []struct {
		body string
		cat  tools.ErrorCategory
		want string
	}{
		{`{"op":"search","scope":"user","documents_scope":"global","query":"quorum"}`, tools.CategoryValidation, "unknown documents_scope"},
		{`{"op":"search","scope":"user","documents_scope":"tenant","sources":["traces"],"query":"quorum"}`, tools.CategoryValidation, "traces"},
	} {
		r := memRun(t, f.m, f.ctx, c.body)
		if !r.IsError || r.Error == nil || r.Error.Category != c.cat || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: %+v %s", c.body, r.Error, r.Text)
		}
	}
	// A backend that serves memory from elsewhere cannot search a second scope.
	f.m.Backend = fallback.New(inprocess.New(f.m.Store, f.emb), inprocess.New(f.m.Store, f.emb), slog.Default())
	r := memRun(t, f.m, f.ctx, `{"op":"search","scope":"user","documents_scope":"tenant","query":"quorum"}`)
	if !r.IsError || !strings.Contains(r.Text, "another backend") {
		t.Errorf("a non-local backend ran a second scope: %s", r.Text)
	}
	// No Document tool: the leg cannot be authorized.
	f.m.Backend = nil
	f.m.DocumentFilters = nil
	r = memRun(t, f.m, f.ctx, `{"op":"search","scope":"user","documents_scope":"tenant","query":"quorum"}`)
	if !r.IsError || !strings.Contains(r.Text, "not configured") {
		t.Errorf("documents_scope with no Document tool: %s", r.Text)
	}
}
