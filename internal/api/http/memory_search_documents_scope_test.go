package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// documents_scope on the off-run POST /v1/_memory/search: one call searches a
// scope's memory AND a second scope's documents, ranked as one list, with every
// hit naming its scope — the in-run Memory tool's argument, with the second
// subject named by documents_scope_id.

// seedTenantDocAt files a tenant document at path with one chunk, through the
// Document tool, and embeds the chunk body. Returns the chunk id.
func seedTenantDocAt(t *testing.T, fs *idFilterStore, mgr *sqlmem.Manager, tenant, path string) string {
	t.Helper()
	d := &builtin.Document{Store: fs, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: tenant})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
	exec := func(in map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		res, err := d.Execute(ctx, b)
		if err != nil || res.IsError {
			t.Fatalf("Document %v: %v %s", in["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	doc := exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc " + path, "path": path})
	chunk := exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"],
		"title": "Clause", "body": "supplier audit clause"})
	id, _ := chunk["id"].(string)
	if id == "" {
		t.Fatalf("create_chunk returned no id: %+v", chunk)
	}
	if err := fs.MemoryEmbedSet(context.Background(), tenant, store.MemoryScopeTenant, "", "doc.chunk:"+id,
		store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	return id
}

// entryScopes maps each hit's key to the scope it reports ("" when absent).
func entryScopes(out map[string]any) map[string]string {
	got := map[string]string{}
	rows, _ := out["entries"].([]any)
	for _, r := range rows {
		e, _ := r.(map[string]any)
		k, _ := e["key"].(string)
		sc, _ := e["scope"].(string)
		got[k] = sc
	}
	return got
}

// TestMemorySearchEndpoint_DocumentsScopeFusesTenantDocumentsWithUserMemory —
// a user-scope search with documents_scope tenant returns the user's note and
// the tenant's document in one list, each naming its scope, and echoes the
// second scope. Without documents_scope the tenant document is not there and
// no hit carries a scope.
func TestMemorySearchEndpoint_DocumentsScopeFusesTenantDocumentsWithUserMemory(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	seedSearchRow(t, fs.searchVectorStore, "A", "alice", "notes/n1", `"supplier audit note"`)
	tdoc := seedTenantDocAt(t, fs, mgr, "A", "/norms/t1")

	code, out, raw := searchRaw(t, s, "A", `{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"documents_scope":"tenant"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := entryScopes(out)
	if got["notes/n1"] != "user" || got["doc.chunk:"+tdoc] != "tenant" {
		t.Errorf("entry scopes = %v, want notes/n1=user and the tenant chunk %s=tenant", got, tdoc)
	}
	if out["documents_scope"] != "tenant" {
		t.Errorf("documents_scope echo = %v, want tenant", out["documents_scope"])
	}
	if id, ok := out["documents_scope_id"]; !ok || id != "" {
		t.Errorf("documents_scope_id echo = %v (present %v), want the tenant's empty store id", id, ok)
	}
	for _, r := range out["entries"].([]any) {
		e := r.(map[string]any)
		if e["key"] == "doc.chunk:"+tdoc && e["document"] != "Doc /norms/t1" {
			t.Errorf("tenant hit's document label = %v, want it read from the tenant tree", e["document"])
		}
	}

	code, out, raw = searchRaw(t, s, "A", `{"query":"audit","scope":"user","scope_id":"alice","top_k":50}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	if _, ok := entryScopes(out)["doc.chunk:"+tdoc]; ok {
		t.Errorf("a one-scope user search returned the tenant chunk: %s", raw)
	}
	if bytes.Contains(raw, []byte(`documents_scope`)) {
		t.Errorf("a one-scope search echoed documents_scope: %s", raw)
	}
	for _, r := range out["entries"].([]any) {
		if _, ok := r.(map[string]any)["scope"]; ok {
			t.Errorf("a one-scope search put scope on a hit: %s", raw)
		}
	}
}

// TestMemorySearchEndpoint_DocumentFiltersAimAtTheDocumentsScope — with a
// second scope, the document filter resolves in that scope's tree and narrows
// its documents only; the first scope's documents are not filtered, and the
// echo names the scope it resolved in.
func TestMemorySearchEndpoint_DocumentFiltersAimAtTheDocumentsScope(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	userReport := seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/u1")
	norm := seedTenantDocAt(t, fs, mgr, "A", "/qg/norms/t1")
	report := seedTenantDocAt(t, fs, mgr, "A", "/qg/reports/t1")

	code, out, raw := searchRaw(t, s, "A",
		`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"documents_scope":"tenant","under_paths":["/qg/norms"]}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := entryScopes(out)
	if got["doc.chunk:"+norm] != "tenant" {
		t.Errorf("entries = %v, want the tenant norm %s", got, norm)
	}
	if _, ok := got["doc.chunk:"+report]; ok {
		t.Errorf("entries = %v, the tenant report %s is outside under_paths", got, report)
	}
	if got["doc.chunk:"+userReport] != "user" {
		t.Errorf("entries = %v, want the user's own document %s unfiltered", got, userReport)
	}
	echo, _ := out["filter"].(map[string]any)
	if echo == nil || echo["scope"] != "tenant" || echo["matched_documents"] != float64(1) {
		t.Errorf("filter echo = %v, want scope tenant with matched_documents 1", out["filter"])
	}
}

// TestMemorySearchEndpoint_DocumentsScopeEqualToScopeIsSugar — documents_scope
// naming the call's own scope and subject is documents added to sources: the
// store is asked exactly what that call asks it, and the answer is the same
// bytes. With no sources (which already span documents) it is the call without
// it. The store double does not apply sources itself, so the legs it was sent
// are compared, not only the answers.
func TestMemorySearchEndpoint_DocumentsScopeEqualToScopeIsSugar(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/u1")
	seedSearchRow(t, fs.searchVectorStore, "A", "alice", "notes/n1", `"supplier audit note"`)
	search := func(body string) ([]byte, string) {
		t.Helper()
		fs.mu.Lock()
		fs.filters = nil
		fs.mu.Unlock()
		code, _, raw := searchRaw(t, s, "A", body)
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, raw)
		}
		legs, _ := json.Marshal(fs.filters)
		return raw, string(legs)
	}
	// The instrument can tell documents from no documents.
	_, without := search(`{"query":"audit","scope":"user","scope_id":"alice","sources":["facts","notes"]}`)
	_, with := search(`{"query":"audit","scope":"user","scope_id":"alice","sources":["facts","notes","documents"]}`)
	if without == with {
		t.Fatal("the store legs do not distinguish sources with and without documents; the comparison below would be vacuous")
	}
	for name, pair := range map[string][2]string{
		"sources gain documents": {
			`{"query":"audit","scope":"user","scope_id":"alice","sources":["facts","notes"],"documents_scope":"user"}`,
			`{"query":"audit","scope":"user","scope_id":"alice","sources":["facts","notes","documents"]}`,
		},
		"no sources, same subject named": {
			`{"query":"audit","scope":"user","scope_id":"alice","documents_scope":"user","documents_scope_id":"alice"}`,
			`{"query":"audit","scope":"user","scope_id":"alice"}`,
		},
	} {
		sugar, sugarLegs := search(pair[0])
		plain, plainLegs := search(pair[1])
		if sugarLegs != plainLegs {
			t.Errorf("%s: the store was asked something else\n got: %s\nwant: %s", name, sugarLegs, plainLegs)
		}
		if !bytes.Equal(sugar, plain) {
			t.Errorf("%s: sugar answer differs\n got: %s\nwant: %s", name, sugar, plain)
		}
	}
}

// TestMemorySearchEndpoint_DocumentsScopeRefusals — what documents_scope cannot
// mean is refused with a 400 naming the problem, never answered from one scope.
func TestMemorySearchEndpoint_DocumentsScopeRefusals(t *testing.T) {
	s, _, _ := docFilterServer(t)
	for name, tc := range map[string]struct{ body, code string }{
		"unknown scope": {`{"query":"q","scope":"user","scope_id":"alice","documents_scope":"org"}`, "invalid_documents_scope"},
		"no subject":    {`{"query":"q","scope":"agent","scope_id":"a1","documents_scope":"user"}`, "missing_documents_scope_id"},
		"same scope, other subject": {
			`{"query":"q","scope":"user","scope_id":"alice","documents_scope":"user","documents_scope_id":"bob"}`, "invalid_documents_scope"},
		"with traces": {`{"query":"q","scope":"user","scope_id":"alice","sources":["traces"],"documents_scope":"tenant"}`, "invalid_documents_scope"},
	} {
		code, out, raw := searchRaw(t, s, "A", tc.body)
		if code != http.StatusBadRequest || out["code"] != tc.code {
			t.Errorf("%s: %d %s, want 400 %s", name, code, raw, tc.code)
		}
	}
}

// TestMemorySearchEndpoint_DocumentsScopeWithoutDocumentsIsRefused — a server
// with no SQL Memory has no documents to search in a second scope: the call is
// refused, not answered from its first scope alone.
func TestMemorySearchEndpoint_DocumentsScopeWithoutDocumentsIsRefused(t *testing.T) {
	s, vs := memorySearchServer(t)
	seedSearchRow(t, vs, "A", "alice", "notes/n1", `"supplier audit note"`)
	rec := postMemorySearch(s, "A", `{"query":"audit","scope":"user","scope_id":"alice","documents_scope":"tenant"}`)
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte(`"filter_unavailable"`)) {
		t.Fatalf("status %d: %s, want 400 filter_unavailable", rec.Code, rec.Body.String())
	}
}

// TestMemorySearchEndpoint_DocumentsScopeChecksEachScopesDocuments — the
// caller's access-control set is applied to each scope's hits in that scope: a
// set denying the tenant's reports drops the tenant report and keeps the
// user's, and a set denying the user's reports does the reverse. Checking both
// scopes' hits against one scope would get one of the two wrong.
func TestMemorySearchEndpoint_DocumentsScopeChecksEachScopesDocuments(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	s.store = &originSearchStore{fs}
	userReport := seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/u1")
	tenantReport := seedTenantDocAt(t, fs, mgr, "A", "/qg/reports/t1")
	tenantNorm := seedTenantDocAt(t, fs, mgr, "A", "/qg/norms/t1")

	denying := func(resource string) *authz.Effective {
		t.Helper()
		eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: "bob"},
			Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
			ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
				{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
				{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{resource}},
			}}}}, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return eff
	}
	const body = `{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"documents_scope":"tenant"}`
	for name, tc := range map[string]struct {
		resource string
		gone     string
		kept     []string
	}{
		"tenant reports denied": {"document:tenant:/qg/reports/**", tenantReport, []string{userReport, tenantNorm}},
		"user reports denied":   {"document:user:/qg/reports/**", userReport, []string{tenantReport, tenantNorm}},
	} {
		member := auth.Principal{TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: denying(tc.resource)}
		code, out, raw := searchAs(t, s, member, body)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, code, raw)
		}
		got := entryScopes(out)
		if _, ok := got["doc.chunk:"+tc.gone]; ok {
			t.Errorf("%s: the denied chunk %s came back: %v", name, tc.gone, got)
		}
		for _, k := range tc.kept {
			if _, ok := got["doc.chunk:"+k]; !ok {
				t.Errorf("%s: the readable chunk %s is missing: %v", name, k, got)
			}
		}
	}
}
