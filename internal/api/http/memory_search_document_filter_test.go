package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Document filters on the off-run POST /v1/_memory/search: the same four lists
// the Memory tool takes, resolved by the Document tool under the CALLER's
// authoritative identity — tenant from the principal, never the body.

// idFilterStore is searchVectorStore holding document rows to the id lists the
// way the Postgres store does (store.MemorySearchFilter.IDsPrefixes): a row under
// one of the prefixes is kept only when its id segment passes IDs / ExcludeIDs;
// every other row passes. It records each leg's filter so a test can assert what
// was sent, not only what came back.
type idFilterStore struct {
	*searchVectorStore
	filters []store.MemorySearchFilter
}

func (v *idFilterStore) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, filter store.MemorySearchFilter, q []float32, topK int) ([]store.MemorySearchEntry, error) {
	v.mu.Lock()
	v.filters = append(v.filters, filter)
	v.mu.Unlock()
	all, err := v.searchVectorStore.MemoryEmbedSearch(ctx, tenantID, scope, scopeID, store.MemorySearchFilter{KeyPrefix: filter.KeyPrefix}, q, 0)
	if err != nil {
		return nil, err
	}
	prefixes := filter.IDsPrefixes
	if len(prefixes) == 0 && (len(filter.IDs) > 0 || len(filter.ExcludeIDs) > 0) {
		prefixes = []string{filter.KeyPrefix}
	}
	out := all[:0]
	for _, e := range all {
		keep := true
		for _, p := range prefixes {
			if !strings.HasPrefix(e.Key, p) {
				continue
			}
			seg := strings.SplitN(strings.TrimPrefix(e.Key, p), ":", 2)[0]
			if len(filter.IDs) > 0 && !contains(filter.IDs, seg) {
				keep = false
			}
			if contains(filter.ExcludeIDs, seg) {
				keep = false
			}
			break
		}
		if keep {
			out = append(out, e)
		}
	}
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// docFilterServer is a memory-search server with SQL Memory (so documents and
// their Path-tree names exist) and an id-honouring vector double.
func docFilterServer(t *testing.T) (*Server, *idFilterStore, *sqlmem.Manager) {
	t.Helper()
	s, vs := memorySearchServer(t)
	fs := &idFilterStore{searchVectorStore: vs}
	s.store = fs
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	s.sqlMem = mgr
	return s, fs, mgr
}

// seedDocAt files a document at path in (tenant, user) with one chunk, through
// the Document tool, and embeds the chunk body the tool wrote. Returns the chunk id.
func seedDocAt(t *testing.T, fs *idFilterStore, mgr *sqlmem.Manager, tenant, user, path string) string {
	t.Helper()
	d := &builtin.Document{Store: fs, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: user, TenantID: tenant})
	exec := func(in map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		res, err := d.Execute(ctx, b)
		if err != nil || res.IsError {
			t.Fatalf("Document %v: %v %s", in["op"], err, res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("decode Document result: %v", err)
		}
		return out
	}
	doc := exec(map[string]any{"op": "create_document", "scope": "user", "title": "Doc " + path, "path": path})
	chunk := exec(map[string]any{"op": "create_chunk", "scope": "user", "document_id": doc["document_id"],
		"parent_id": doc["root_chunk_id"], "title": "Clause", "body": "supplier audit clause"})
	id, _ := chunk["id"].(string)
	if id == "" {
		t.Fatalf("create_chunk returned no id: %+v", chunk)
	}
	if err := fs.MemoryEmbedSet(context.Background(), tenant, store.MemoryScopeUser, user, "doc.chunk:"+id,
		store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Fatalf("MemoryEmbedSet: %v", err)
	}
	return id
}

// searchRaw posts a search as an admin of tenant and decodes the body generically,
// so the assertions read the wire, not this package's response struct.
func searchRaw(t *testing.T, s *Server, tenant, body string) (int, map[string]any, []byte) {
	t.Helper()
	rec := postMemorySearch(s, tenant, body)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec.Code, out, rec.Body.Bytes()
}

func entryKeys(out map[string]any) map[string]bool {
	got := map[string]bool{}
	rows, _ := out["entries"].([]any)
	for _, r := range rows {
		k, _ := r.(map[string]any)["key"].(string)
		got[k] = true
	}
	return got
}

// TestMemorySearchEndpoint_UnderPathsRestrictDocumentsOnly — the admin search,
// aimed at /qg/norms, returns that document's chunk and not the report's, keeps
// the note beside them, and echoes what the path resolved to.
func TestMemorySearchEndpoint_UnderPathsRestrictDocumentsOnly(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	norm := seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/ed1")
	report := seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/r1")
	seedSearchRow(t, fs.searchVectorStore, "A", "alice", "notes/n1", `"supplier audit note"`)

	for _, body := range []string{
		`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"under_paths":["/qg/norms"]}`,
		`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"exclude_under_paths":["/qg/reports"]}`,
		`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"exclude_document_ids":["nope"],"under_paths":["/qg/norms/ed1"]}`,
	} {
		code, out, raw := searchRaw(t, s, "A", body)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", body, code, raw)
		}
		got := entryKeys(out)
		if got["doc.chunk:"+report] {
			t.Errorf("%s: returned the report chunk", body)
		}
		if !got["doc.chunk:"+norm] || !got["notes/n1"] {
			t.Errorf("%s: dropped the norm chunk or the note: %v", body, got)
		}
		echo, _ := out["filter"].(map[string]any)
		if echo == nil || echo["matched_documents"] != float64(1) {
			t.Errorf("%s: filter echo = %v, want matched_documents 1", body, out["filter"])
		}
	}
}

// TestMemorySearchEndpoint_PathResolvesInTheCallersTenantOnly — the same paths in
// two tenants name different documents. A tenant-A caller's filter resolves in
// A's tree only: A's /shared/x chunk comes back and A's /other/y chunk does not,
// and no id of tenant B's travels to the store. (Each tenant holds a document
// outside the path, so a filter resolved in the wrong tenant cannot match a
// whole scope and send no list at all.)
func TestMemorySearchEndpoint_PathResolvesInTheCallersTenantOnly(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	a := seedDocAt(t, fs, mgr, "A", "alice", "/shared/x")
	aOther := seedDocAt(t, fs, mgr, "A", "alice", "/other/y")
	b := seedDocAt(t, fs, mgr, "B", "alice", "/shared/x")
	bOther := seedDocAt(t, fs, mgr, "B", "alice", "/other/y")

	code, out, raw := searchRaw(t, s, "A", `{"query":"audit","scope":"user","scope_id":"alice","under_paths":["/shared/x"]}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := entryKeys(out)
	if !got["doc.chunk:"+a] || got["doc.chunk:"+aOther] {
		t.Errorf("entries = %v, want tenant A's /shared/x chunk %s and not its /other/y chunk", got, a)
	}
	if len(fs.filters) == 0 {
		t.Fatal("the search reached no store leg")
	}
	for _, f := range fs.filters {
		for _, id := range []string{b, bOther} {
			if contains(f.IDs, id) || contains(f.ExcludeIDs, id) {
				t.Errorf("tenant B's chunk id %s reached a tenant-A search: %+v", id, f)
			}
		}
	}
	if echo, _ := out["filter"].(map[string]any); echo == nil || echo["matched_documents"] != float64(1) {
		t.Errorf("filter echo = %v, want tenant A's one document", out["filter"])
	}
}

// TestMemorySearchEndpoint_FilterRefusalsKeepTheirClass — a refused filter is
// answered by its class: the Document tool's own refusals reach the caller as
// they would in a run, never as a blind 500, and never by running unfiltered.
func TestMemorySearchEndpoint_FilterRefusalsKeepTheirClass(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/ed1")
	for _, c := range []struct {
		body, code string
		status     int
	}{
		// A facts-only search cannot return documents.
		{`{"query":"audit","scope":"user","scope_id":"alice","sources":["facts"],"under_paths":["/qg/norms"]}`, "invalid_filter", http.StatusBadRequest},
		// A malformed path.
		{`{"query":"audit","scope":"user","scope_id":"alice","under_paths":["no-slash"]}`, "invalid_filter", http.StatusBadRequest},
	} {
		code, out, raw := searchRaw(t, s, "A", c.body)
		if code != c.status || out["code"] != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.body, code, raw, c.status, c.code)
		}
	}

	// No SQL Memory: the same "documents not configured" refusal the tool gives.
	s.sqlMem = nil
	code, out, raw := searchRaw(t, s, "A", `{"query":"audit","scope":"user","scope_id":"alice","under_paths":["/qg/norms"]}`)
	if code != http.StatusBadRequest || out["code"] != "filter_unavailable" ||
		!strings.Contains(fmt.Sprint(out["error"]), "documents not configured on this server") {
		t.Errorf("no SQL Memory: %d %s, want 400 filter_unavailable naming documents not configured", code, raw)
	}
}

// TestMemorySearchFilterRefusalStatus_PermissionIsForbidden — the class-to-status
// map. Off-run every caller the route admits holds the SQL Memory grants
// (substrateAdminCtx), so the Document gate's permission refusal is reached here
// through the map, and in a run through the Memory tool.
func TestMemorySearchFilterRefusalStatus_PermissionIsForbidden(t *testing.T) {
	for cat, want := range map[tools.ErrorCategory]struct {
		status int
		code   string
	}{
		tools.CategoryPermission: {http.StatusForbidden, "permission_denied"},
		tools.CategoryValidation: {http.StatusBadRequest, "invalid_filter"},
		tools.CategoryBusiness:   {http.StatusBadRequest, "filter_unavailable"},
		tools.CategoryTransient:  {http.StatusServiceUnavailable, "filter_unavailable"},
	} {
		status, code := memorySearchFilterRefusalStatus(tools.Result{IsError: true, Error: &tools.ErrorInfo{Category: cat}})
		if status != want.status || code != want.code {
			t.Errorf("%s → %d %s, want %d %s", cat, status, code, want.status, want.code)
		}
	}
	if status, code := memorySearchFilterRefusalStatus(tools.Result{IsError: true}); status != http.StatusInternalServerError || code != "internal" {
		t.Errorf("unclassified → %d %s, want 500 internal", status, code)
	}
}

// TestMemorySearchEndpoint_NoFilterIsByteIdentical — without a filter, or with
// only blank entries, the response carries no filter key and is byte-for-byte
// the response of a request that never named one.
func TestMemorySearchEndpoint_NoFilterIsByteIdentical(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/ed1")
	seedSearchRow(t, fs.searchVectorStore, "A", "alice", "notes/n1", `"supplier audit note"`)

	_, out, plain := searchRaw(t, s, "A", `{"query":"audit","scope":"user","scope_id":"alice"}`)
	if _, ok := out["filter"]; ok {
		t.Errorf("a filter-free search carries a filter key: %s", plain)
	}
	for _, body := range []string{
		`{"query":"audit","scope":"user","scope_id":"alice","under_paths":[],"document_ids":null}`,
		`{"query":"audit","scope":"user","scope_id":"alice","exclude_under_paths":[""," "]}`,
	} {
		_, _, raw := searchRaw(t, s, "A", body)
		if !bytes.Equal(raw, plain) {
			t.Errorf("%s:\n got %s\nwant %s", body, raw, plain)
		}
	}
	// And the principal's own tenant: a body cannot name another one.
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/search",
		strings.NewReader(`{"query":"audit","scope":"user","scope_id":"alice","tenant":"B"}`)).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeAdmin}}))
	rec := httptest.NewRecorder()
	s.handleMemorySearch(rec, req)
	if !bytes.Equal(rec.Body.Bytes(), plain) {
		t.Errorf("a body tenant changed the answer: %s", rec.Body.String())
	}
}

// TestMemorySearchEndpoint_ACLLimitedCallerSendsNoDeniedID — the resolver reads
// the CALLER's access-control set off the principal, as the off-run Document
// routes do: for a caller that may not read the reports, the filter travels as
// an allowlist of what it may read, and no report chunk id reaches the store.
func TestMemorySearchEndpoint_ACLLimitedCallerSendsNoDeniedID(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	norm := seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/ed1")
	report := seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/r1")
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: "op"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:user:/qg/reports/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/search",
		strings.NewReader(`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"exclude_under_paths":["/nowhere"]}`)).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeTenant}, Effective: eff,
		}))
	rec := httptest.NewRecorder()
	s.handleMemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.filters) == 0 {
		t.Fatal("the search reached no store leg")
	}
	for _, f := range fs.filters {
		if len(f.ExcludeIDs) != 0 {
			t.Errorf("a limited caller's filter was sent as a denylist: %v", f.ExcludeIDs)
		}
		if contains(f.IDs, report) || !contains(f.IDs, norm) {
			t.Errorf("leg ids = %v, want the readable norm %s and not the report %s", f.IDs, norm, report)
		}
	}
	if strings.Contains(rec.Body.String(), report) {
		t.Errorf("the read-denied report chunk came back: %s", rec.Body.String())
	}
}

// TestMemorySearchEndpoint_TenantScopeFilterResolvesInTheTenantTree — the tenant
// scope's document rows live where the off-run search reads that scope, so its
// filter resolves in the tenant's own tree and returns the tenant's chunks.
func TestMemorySearchEndpoint_TenantScopeFilterResolvesInTheTenantTree(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	d := &builtin.Document{Store: fs, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: "A"})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	})
	seed := func(path string) string {
		t.Helper()
		var ids [2]string
		for i, in := range []map[string]any{
			{"op": "create_document", "scope": "tenant", "title": "Doc " + path, "path": path},
			{"op": "create_chunk", "scope": "tenant", "title": "Clause", "body": "supplier audit clause"},
		} {
			if i == 1 {
				in["document_id"] = ids[0]
			}
			b, _ := json.Marshal(in)
			res, err := d.Execute(ctx, b)
			if err != nil || res.IsError {
				t.Fatalf("Document %v: %v %s", in["op"], err, res.Text)
			}
			var out map[string]any
			_ = json.Unmarshal([]byte(res.Text), &out)
			if i == 0 {
				ids[0], _ = out["document_id"].(string)
			} else {
				ids[1], _ = out["id"].(string)
			}
		}
		if err := fs.MemoryEmbedSet(context.Background(), "A", store.MemoryScopeTenant, "", "doc.chunk:"+ids[1],
			store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("MemoryEmbedSet: %v", err)
		}
		return ids[1]
	}
	norm, report := seed("/qg/norms/t1"), seed("/qg/reports/t1")
	code, out, raw := searchRaw(t, s, "A", `{"query":"audit","scope":"tenant","top_k":50,"under_paths":["/qg/norms"]}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := entryKeys(out)
	if !got["doc.chunk:"+norm] || got["doc.chunk:"+report] {
		t.Errorf("entries = %v, want the tenant norm chunk %s and not the report %s", got, norm, report)
	}
	if echo, _ := out["filter"].(map[string]any); echo == nil || echo["matched_documents"] != float64(1) {
		t.Errorf("filter echo = %v, want matched_documents 1", out["filter"])
	}
}
