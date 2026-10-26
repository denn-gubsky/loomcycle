package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The off-run POST /v1/_memory/search runs with an empty prefix, so its hits
// include document chunk bodies. A member or tenant operator whose access-control
// set denies reading a document must not read that document through this search
// any more than through Document op=search or the Memory tool in a run.

// originSearchStore fills each vector hit's Origin from its provenance row, as
// the real stores' search legs do. Without it every doc.chunk: row classifies as
// prose and a test could not tell a check by key from a check by class.
type originSearchStore struct{ *idFilterStore }

func (o *originSearchStore) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, q []float32, topK int) ([]store.MemorySearchEntry, error) {
	rows, err := o.idFilterStore.MemoryEmbedSearch(ctx, tenantID, scope, scopeID, f, q, topK)
	for i := range rows {
		if prov, perr := o.MemoryProvenanceGet(ctx, tenantID, scope, scopeID, rows[i].Key); perr == nil {
			rows[i].Origin = prov.Origin
		}
	}
	return rows, err
}

// aclSearchCorpus is tenant A, user alice, holding:
//   - norm: a readable document's chunk (/qg/norms/ed1)
//   - report: a denied document's prose chunk (/qg/reports/r1)
//   - fact: a consolidated fact homed in a denied document's chunk (/qg/reports/r2)
//   - unit: a denied document's chunk found only through a derived unit (/qg/reports/r3)
//   - a plain note and a plain fact outside any document.
type aclSearchCorpus struct {
	s                        *Server
	norm, report, fact, unit string
}

func newACLSearchCorpus(t *testing.T) aclSearchCorpus {
	t.Helper()
	s, fs, mgr := docFilterServer(t)
	s.store = &originSearchStore{fs}
	c := aclSearchCorpus{s: s}
	c.norm = seedDocAt(t, fs, mgr, "A", "alice", "/qg/norms/ed1")
	c.report = seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/r1")
	c.fact = seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/r2")
	c.unit = seedDocAt(t, fs, mgr, "A", "alice", "/qg/reports/r3")
	ctx := context.Background()

	// The fact a consolidator homed in r2's chunk: the chunk row carries provenance.
	v, _ := json.Marshal("supplier audit fact from the report")
	if err := fs.MemorySetProvenance(ctx, "A", store.MemoryScopeUser, "alice", "doc.chunk:"+c.fact, v, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
		t.Fatal(err)
	}
	// r3's chunk body is not embedded; only its derived unit is, so the chunk
	// comes back through the unit.
	fs.mu.Lock()
	delete(fs.embeds, "A|user|alice|doc.chunk:"+c.unit)
	fs.mu.Unlock()
	uk := memrank.UnitKey(c.unit, memrank.UnitClaim, 0)
	uv, _ := json.Marshal(memrank.UnitValue{ChunkID: c.unit, Kind: memrank.UnitClaim, Text: "the report claims an audit"})
	if err := fs.MemorySet(ctx, "A", store.MemoryScopeUser, "alice", uk, uv, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.MemoryEmbedSet(ctx, "A", store.MemoryScopeUser, "alice", uk,
		store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	seedSearchRow(t, fs.searchVectorStore, "A", "alice", "notes/n1", `"supplier audit note"`)
	fv, _ := json.Marshal("supplier audit plain fact")
	if err := fs.MemorySetProvenance(ctx, "A", store.MemoryScopeUser, "alice", "facts/f1", fv, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.MemoryEmbedSet(ctx, "A", store.MemoryScopeUser, "alice", "facts/f1",
		store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	return c
}

// denyReportsSet denies reading every document filed under /qg/reports.
func denyReportsSet(t *testing.T, subject string) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: subject},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
			{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:user:/qg/reports/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// allowDocumentsSet names document rules but denies nothing.
func allowDocumentsSet(t *testing.T, subject string) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: subject},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func searchAs(t *testing.T, s *Server, p auth.Principal, body string) (int, map[string]any, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/search", strings.NewReader(body)).
		WithContext(auth.WithPrincipal(context.Background(), p))
	rec := httptest.NewRecorder()
	s.handleMemorySearch(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec.Code, out, rec.Body.Bytes()
}

// limitedCallers are the two kinds of principal the route admits with a set
// that denies the reports: a tenant member and a tenant operator.
func limitedCallers(t *testing.T) map[string]auth.Principal {
	return map[string]auth.Principal{
		"member":          {TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: denyReportsSet(t, "bob")},
		"tenant operator": {TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeTenant}, Effective: denyReportsSet(t, "op")},
	}
}

const aclSearchBody = `{"query":"audit","scope":"user","scope_id":"alice","top_k":50}`

func TestMemorySearchEndpoint_ACLDeniedDocumentHitsAreAbsent(t *testing.T) {
	c := newACLSearchCorpus(t)
	for name, p := range limitedCallers(t) {
		code, out, raw := searchAs(t, c.s, p, aclSearchBody)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, code, raw)
		}
		got := entryKeys(out)
		for what, id := range map[string]string{"prose chunk": c.report, "homed fact": c.fact, "unit-found chunk": c.unit} {
			if got["doc.chunk:"+id] {
				t.Errorf("%s: a read-denied document's %s came back: %v", name, what, got)
			}
			if bytes.Contains(raw, []byte(id)) {
				t.Errorf("%s: the response names the read-denied %s %s", name, what, id)
			}
		}
		if bytes.Contains(raw, []byte("the report claims an audit")) {
			t.Errorf("%s: the denied unit's text came back: %s", name, raw)
		}
		for _, k := range []string{"doc.chunk:" + c.norm, "notes/n1", "facts/f1"} {
			if !got[k] {
				t.Errorf("%s: %s is missing: %v", name, k, got)
			}
		}
	}
}

// TestMemorySearchEndpoint_UnrestrictedCallerAnswerIsUnchanged — a caller with
// no set, and one whose set denies no document, see every hit, and the same bytes.
// The fixture's hits are what the denied-caller test needs them to be: the homed
// fact classifies as a fact and the r3 chunk is found by its unit.
func TestMemorySearchEndpoint_UnrestrictedCallerAnswerIsUnchanged(t *testing.T) {
	c := newACLSearchCorpus(t)
	code, out, plain := searchAs(t, c.s, auth.Principal{TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeAdmin}}, aclSearchBody)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, plain)
	}
	rows, _ := out["entries"].([]any)
	byKey := map[string]map[string]any{}
	for _, r := range rows {
		e := r.(map[string]any)
		byKey[e["key"].(string)] = e
	}
	for _, id := range []string{c.norm, c.report, c.fact, c.unit} {
		if byKey["doc.chunk:"+id] == nil {
			t.Errorf("an unrestricted caller lost doc.chunk:%s: %s", id, plain)
		}
	}
	if e := byKey["doc.chunk:"+c.fact]; e == nil || e["kind"] != "fact" {
		t.Errorf("the homed fact = %v, want kind fact (the fixture must hold a provenance-bearing chunk row)", e)
	}
	if e := byKey["doc.chunk:"+c.unit]; e == nil || e["matched_unit"] == nil {
		t.Errorf("the r3 chunk = %v, want it found through its unit", e)
	}

	unlimited := auth.Principal{TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: allowDocumentsSet(t, "bob")}
	if _, _, raw := searchAs(t, c.s, unlimited, aclSearchBody); !bytes.Equal(raw, plain) {
		t.Errorf("a set that denies nothing changed the answer:\n got %s\nwant %s", raw, plain)
	}
}

// TestMemorySearchEndpoint_ACLDeniedDocumentAbsentUnderDocumentFilter — a
// document filter that spans readable and denied documents, or names a denied
// one, returns none of the denied document's hits.
func TestMemorySearchEndpoint_ACLDeniedDocumentAbsentUnderDocumentFilter(t *testing.T) {
	c := newACLSearchCorpus(t)
	for name, p := range limitedCallers(t) {
		for _, body := range []string{
			`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"under_paths":["/qg"]}`,
			`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"under_paths":["/qg/reports"]}`,
			`{"query":"audit","scope":"user","scope_id":"alice","top_k":50,"exclude_under_paths":["/qg/norms"]}`,
		} {
			code, out, raw := searchAs(t, c.s, p, body)
			if code != http.StatusOK {
				t.Errorf("%s %s: status %d: %s", name, body, code, raw)
				continue
			}
			got := entryKeys(out)
			for _, id := range []string{c.report, c.fact, c.unit} {
				if got["doc.chunk:"+id] {
					t.Errorf("%s %s: a read-denied document's chunk %s came back: %v", name, body, id, got)
				}
			}
			if strings.Contains(body, `["/qg"]`) && !got["doc.chunk:"+c.norm] {
				t.Errorf("%s %s: the readable norm is missing: %v", name, body, got)
			}
		}
	}
}

// TestMemorySearchEndpoint_ACLDeniedTenantDocumentIsAbsent — the tenant scope's
// documents are checked in the tenant's own tree, as its filter resolves there.
func TestMemorySearchEndpoint_ACLDeniedTenantDocumentIsAbsent(t *testing.T) {
	s, fs, mgr := docFilterServer(t)
	d := &builtin.Document{Store: fs, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: "A"})
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
	seed := func(path string) string {
		t.Helper()
		doc := exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Doc " + path, "path": path})
		chunk := exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"],
			"title": "Clause", "body": "supplier audit clause"})
		id, _ := chunk["id"].(string)
		if err := fs.MemoryEmbedSet(context.Background(), "A", store.MemoryScopeTenant, "", "doc.chunk:"+id,
			store.MemoryEmbedding{Provider: "test", Model: "stub", Dimension: 4, Vector: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	norm, report := seed("/qg/norms/t1"), seed("/qg/reports/t1")
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: "bob"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
			{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/qg/reports/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	member := auth.Principal{TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: eff}
	code, out, raw := searchAs(t, s, member, `{"query":"audit","scope":"tenant","top_k":50}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := entryKeys(out)
	if got["doc.chunk:"+report] || !got["doc.chunk:"+norm] {
		t.Errorf("entries = %v, want the tenant norm chunk %s and not the denied report %s", got, norm, report)
	}
}

// cancelAfterSearch cancels the request once the vector legs have answered, so
// the access check that follows cannot read the document structure.
type cancelAfterSearch struct {
	*originSearchStore
	cancel context.CancelFunc
}

func (c *cancelAfterSearch) MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string,
	f store.MemorySearchFilter, q []float32, topK int) ([]store.MemorySearchEntry, error) {
	rows, err := c.originSearchStore.MemoryEmbedSearch(ctx, tenantID, scope, scopeID, f, q, topK)
	c.cancel()
	return rows, err
}

// TestMemorySearchEndpoint_ACLCheckFailureFailsTheSearch — when which document a
// hit belongs to cannot be read, a limited caller's search fails; it is never
// answered with the hits unchecked.
func TestMemorySearchEndpoint_ACLCheckFailureFailsTheSearch(t *testing.T) {
	c := newACLSearchCorpus(t)
	base := c.s.store.(*originSearchStore)
	for name, p := range limitedCallers(t) {
		ctx, cancel := context.WithCancel(auth.WithPrincipal(context.Background(), p))
		c.s.store = &cancelAfterSearch{originSearchStore: base, cancel: cancel}
		req := httptest.NewRequest(http.MethodPost, "/v1/_memory/search", strings.NewReader(aclSearchBody)).WithContext(ctx)
		rec := httptest.NewRecorder()
		c.s.handleMemorySearch(rec, req)
		cancel()
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"search failed"`) {
			t.Errorf("%s: %d %s, want 500 search failed", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), c.report) {
			t.Errorf("%s: a hit came back unchecked: %s", name, rec.Body.String())
		}
	}
}
