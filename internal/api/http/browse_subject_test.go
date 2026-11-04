package http

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC FY Part A: an off-run browse names a subject (?scope_id= or the memory
// routes' {scope_id}) other than the caller's own only on the operator plane,
// or as a member the access-control rules let browse that subject's scope.
// Everything else is refused with one opaque not-found, whoever the named
// subject is, so a member cannot list or probe its colleagues.

// memberBaselineSet is the set the resolver compiles for a non-isolated
// member: the built-in member list bound tenant-wide, plus extra rules in a
// tenant list of their own.
func memberBaselineSet(t *testing.T, tenant, subject string, extra ...authz.Rule) *authz.Effective {
	t.Helper()
	acls := map[string]authz.ACLVersion{authz.BaselineMember: {ACL: authz.Baselines()[authz.BaselineMember]}}
	bindings := []authz.Binding{{ACL: authz.BaselineMember, SubjectKind: authz.SubjectTenant}}
	if len(extra) > 0 {
		acls["extra"] = authz.ACLVersion{ACL: authz.ACL{Name: "extra", Rules: extra}, Version: 1}
		bindings = append(bindings, authz.Binding{ACL: "extra", SubjectKind: authz.SubjectTenant})
	}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: tenant, ID: subject},
		Bindings: bindings, ACLs: acls, Now: time.Now(), Baseline: authz.BaselineMember})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// browseFixture is the feed fixture with the Document and Path tools wired, as
// the substrate routes dispatch to them, and alice's private user-scope
// document holding a secret, an image and a Path name, plus agent-scope
// documents of two agents.
type browseFixture struct {
	*feedFixture
	secretDoc, secretChunk string
	agentDocs              map[string]string
}

const browseSecret = "alice private salary review"

// onePixelPNG is a valid 1x1 PNG, the smallest image set_asset accepts.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func newBrowseFixture(t *testing.T) *browseFixture {
	t.Helper()
	f := &browseFixture{feedFixture: newFeedFixture(t), agentDocs: map[string]string{}}
	f.srv.tools = []tools.Tool{f.doc, &builtin.Path{Store: f.store}}
	cfg := &config.Config{}
	cfg.Memory.Projection.Enabled = true
	f.srv.cfgHolder = config.NewHolder(cfg)

	d := f.op(t, f.writer, `{"op":"create_document","scope":"user","title":"Plan","path":"/secret/plan"}`)
	f.secretDoc, _ = d["document_id"].(string)
	c := f.op(t, f.writer, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"part","body":%q}`, f.secretDoc, browseSecret))
	f.secretChunk, _ = c["id"].(string)
	f.op(t, f.writer, fmt.Sprintf(`{"op":"set_asset","scope":"user","id":%q,"media_type":"image/png","data":%q}`, f.secretChunk, onePixelPNG))

	for _, agent := range []string{"helper", "secret-agent"} {
		ctx := tools.WithAgentName(tools.WithRunIdentity(context.Background(),
			tools.RunIdentityValue{AgentID: "w", UserID: "alice", TenantID: "tnt"}), agent)
		ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{
			Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}},
			SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"agent"}},
		})
		ad := f.op(t, ctx, fmt.Sprintf(`{"op":"create_document","scope":"agent","title":"Notes of %s","path":"/notes"}`, agent))
		f.agentDocs[agent], _ = ad["document_id"].(string)
		f.op(t, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"agent","document_id":%q,"title":"part","body":"agent memo of %s"}`, f.agentDocs[agent], agent))
	}
	return f
}

// call serves one request as p through the routes under test, with a deadline
// so a stream that was wrongly opened ends.
func (f *browseFixture) call(t *testing.T, p auth.Principal, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/_document", f.srv.handleSubstrateDocument)
	mux.HandleFunc("POST /v1/_path", f.srv.handleSubstratePath)
	mux.HandleFunc("GET /v1/_document/asset/{chunk_id}", f.srv.handleSubstrateDocumentAsset)
	mux.HandleFunc("POST /v1/_document/project", f.srv.handleDocumentProject)
	mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys", f.srv.handleListMemoryEntries)
	mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", f.srv.handleGetMemoryEntry)
	ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.Background(), p), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func (f *browseFixture) feed(t *testing.T, p auth.Principal, query string) *feedStream {
	t.Helper()
	return openFeed(t, f.feedFixture, p, query, testFeedPolling(&feedClock{now: time.Now()}, nil))
}

// browseCase is one request that names a subject, and what the answer must
// not carry.
type browseCase struct {
	name, method, target, body string
}

func (f *browseFixture) foreignCases(subject string) []browseCase {
	return []browseCase{
		{"document export_md", "POST", "/v1/_document?scope_id=" + subject,
			fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, f.secretDoc)},
		{"document delete_document", "POST", "/v1/_document?scope_id=" + subject,
			fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q}`, f.secretDoc)},
		{"document copy into the subject's scope", "POST", "/v1/_document?scope_id=" + subject,
			`{"op":"copy_documents","scope":"tenant","to_scope":"user","under_path":"/","dry_run":true}`},
		{"path ls", "POST", "/v1/_path?scope_id=" + subject, `{"op":"ls","scope":"user","path":"/","recursive":true}`},
		{"asset", "GET", "/v1/_document/asset/" + f.secretChunk + "?scope=user&scope_id=" + subject, ""},
		{"project", "POST", "/v1/_document/project?scope=user&scope_id=" + subject, ""},
	}
}

func TestSubstrateBrowseCtxFn_MemberAndIsolatedRoutes(t *testing.T) {
	f := newBrowseFixture(t)
	bob := member("tnt", "bob", memberBaselineSet(t, "tnt", "bob"))
	isolated := auth.Principal{TenantID: "tnt", Subject: "bob", Scopes: []string{auth.ScopeUser}, Effective: memberBaselineSet(t, "tnt", "bob")}
	for _, who := range []struct {
		name string
		p    auth.Principal
	}{{"member", bob}, {"isolated member", isolated}} {
		for _, c := range f.foreignCases("alice") {
			rec := f.call(t, who.p, c.method, c.target, c.body)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), browseSecret) ||
				strings.Contains(rec.Body.String(), "plan") {
				t.Errorf("%s %s naming alice: %d %s, want the opaque 404", who.name, c.name, rec.Code, rec.Body.String())
			}
			// The refusal is the same for a subject nobody holds, so it
			// says nothing about whether alice exists.
			ghost := f.call(t, who.p, c.method, strings.ReplaceAll(c.target, "scope_id=alice", "scope_id=nobody-here"), c.body)
			if ghost.Code != rec.Code || ghost.Body.String() != rec.Body.String() {
				t.Errorf("%s %s: naming alice answered %d %q, naming nobody %d %q — the answers must not differ",
					who.name, c.name, rec.Code, rec.Body.String(), ghost.Code, ghost.Body.String())
			}
		}
		if s := f.feed(t, who.p, "scope=user&scope_id=alice"); s.status != http.StatusNotFound {
			t.Errorf("%s feed naming alice: %d %s, want 404", who.name, s.status, s.body)
		}
	}
	// The document is still there: nothing above removed it.
	if out := f.op(t, f.writer, fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, f.secretDoc)); !strings.Contains(fmt.Sprint(out), browseSecret) {
		t.Fatalf("alice's document changed under the refused requests: %v", out)
	}

	// Naming one's own subject is the caller's own scope, as with no param.
	bobCtx := f.as("tnt", "bob")
	own, _ := f.newDoc(t, bobCtx, "user", "/mine")
	for _, q := range []string{"", "?scope_id=bob"} {
		rec := f.call(t, bob, "POST", "/v1/_document"+q, fmt.Sprintf(`{"op":"get_document","scope":"user","id":%q}`, own))
		if rec.Code != http.StatusOK {
			t.Errorf("member own document %q: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	if s := f.feed(t, bob, "scope=user&scope_id=bob"); s.status != http.StatusOK {
		t.Errorf("member own feed: %d %s", s.status, s.body)
	}
	// The operator plane still names any subject of its tenant.
	op := auth.Principal{TenantID: "tnt", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	rec := f.call(t, op, "POST", "/v1/_document?scope_id=alice",
		fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, f.secretDoc))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), browseSecret) {
		t.Errorf("tenant operator naming alice: %d %s, want her document", rec.Code, rec.Body.String())
	}
}

// D1: a tenant rule allowing `browse document:user` where the owner is not the
// caller lets a member read a colleague's user scope; the baseline alone does
// not, and the same rule for memory opens the memory key routes.
func TestBrowse_AnExplicitAllowLetsAMemberReadAColleaguesScope(t *testing.T) {
	f := newBrowseFixture(t)
	notSelf := false
	reviewer := member("tnt", "bob", memberBaselineSet(t, "tnt", "bob", authz.Rule{
		Effect: authz.Allow, Actions: []string{"browse"}, Resources: []string{"document:user", "memory:user"},
		When: &authz.Condition{OwnerIsSelf: &notSelf},
	}))
	plain := member("tnt", "bob", memberBaselineSet(t, "tnt", "bob"))
	if err := f.store.MemorySet(context.Background(), "tnt", "user", "alice", "diary", json.RawMessage(`"alice diary entry"`), 0); err != nil {
		t.Fatal(err)
	}
	export := fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, f.secretDoc)

	rec := f.call(t, reviewer, "POST", "/v1/_document?scope_id=alice", export)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), browseSecret) {
		t.Errorf("granted reviewer reading alice's document: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, reviewer, "GET", "/v1/_memory/scopes/user/alice/keys/diary", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "alice diary entry") {
		t.Errorf("granted reviewer reading alice's memory: %d %s", rec.Code, rec.Body.String())
	}
	if s := f.feed(t, reviewer, "scope=user&scope_id=alice"); s.status != http.StatusOK {
		t.Errorf("granted reviewer opening alice's feed: %d %s", s.status, s.body)
	}

	if rec := f.call(t, plain, "POST", "/v1/_document?scope_id=alice", export); rec.Code != http.StatusNotFound {
		t.Errorf("baseline member reading alice's document: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, plain, "GET", "/v1/_memory/scopes/user/alice/keys/diary", ""); rec.Code != http.StatusNotFound ||
		strings.Contains(rec.Body.String(), "alice diary entry") {
		t.Errorf("baseline member reading alice's memory: %d %s, want 404", rec.Code, rec.Body.String())
	}
	// A member with no set at all (no resolver, or it failed) is held to
	// itself: there is no rule to grant the wider reach.
	unresolved := member("tnt", "bob", nil)
	if rec := f.call(t, unresolved, "POST", "/v1/_document?scope_id=alice", export); rec.Code != http.StatusNotFound {
		t.Errorf("member with no set reading alice's document: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

// A member browses an agent's scope only when it may run that agent.
func TestBrowse_AMemberBrowsesOnlyAgentsItMayRun(t *testing.T) {
	f := newBrowseFixture(t)
	bob := member("tnt", "bob", memberBaselineSet(t, "tnt", "bob", authz.Rule{
		Effect: authz.Deny, Actions: []string{"run"}, Resources: []string{"agent:secret-agent"},
	}))
	get := func(agent string) *httptest.ResponseRecorder {
		return f.call(t, bob, "POST", "/v1/_document?scope_id="+agent,
			fmt.Sprintf(`{"op":"export_md","scope":"agent","document_id":%q,"include_metadata":false}`, f.agentDocs[agent]))
	}
	if rec := get("helper"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "agent memo of helper") {
		t.Errorf("an agent bob may run: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("secret-agent"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "agent memo") {
		t.Errorf("an agent bob may not run: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, bob, "GET", "/v1/_memory/scopes/agent/secret-agent/keys", ""); rec.Code != http.StatusNotFound {
		t.Errorf("the memory of an agent bob may not run: %d %s, want 404", rec.Code, rec.Body.String())
	}
	// A subject's name is no agent bob may run on its user scope's behalf: the
	// user plane keeps refusing even where the agent plane would allow.
	if rec := f.call(t, bob, "POST", "/v1/_document?scope_id=helper",
		fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, f.secretDoc)); rec.Code != http.StatusNotFound {
		t.Errorf("user scope named by an agent's name: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

// The audit probe, kept as the regression: a member (dev, not isolated)
// reaches another user's user-scope memory through the key routes' URL
// {scope_id}, the search body's scope_id and the scope listing — in compat
// AND enforce mode, since the member baseline allowed it in both. Each is
// refused, bob's row is untouched, and the member's own scope still works.
func TestMemoryKeyRoutes_AMemberCannotReachAnotherUsersScope(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			_, url, _ := aclFixture(t, mode)
			ts := httptestServerAt(url)
			const admin = "legacy-admin-secret"
			bobKey := "/v1/_memory/scopes/user/bob/keys/diary"
			if code, body := bearerCall(t, ts, admin, "PUT", bobKey+"?tenant=acme", `{"value":"bob private diary"}`); code != 200 {
				t.Fatalf("admin seeds bob's row: %d %s", code, body)
			}
			readBob := func() string {
				t.Helper()
				code, body := bearerCall(t, ts, admin, "GET", bobKey+"?tenant=acme", "")
				if code != 200 {
					t.Fatalf("admin reads bob's row: %d %s", code, body)
				}
				return body
			}

			for _, c := range []struct{ method, path, body string }{
				{"GET", bobKey, ""},
				{"PUT", bobKey, `{"value":"overwritten by dev"}`},
				{"DELETE", bobKey, ""},
				{"GET", "/v1/_memory/scopes/user/bob/keys", ""},
				{"POST", "/v1/_memory/search", `{"query":"diary","scope":"user","scope_id":"bob"}`},
				{"POST", "/v1/_memory/search", `{"query":"diary","scope":"tenant","documents_scope":"user","documents_scope_id":"bob"}`},
			} {
				code, body := bearerCall(t, ts, freeBearer, c.method, c.path, c.body)
				if code != http.StatusNotFound || strings.Contains(body, "bob private diary") {
					t.Errorf("member %s %s: %d %s, want the opaque 404", c.method, c.path, code, body)
				}
			}
			if got := readBob(); !strings.Contains(got, "bob private diary") {
				t.Errorf("bob's row changed under the refused writes: %s", got)
			}
			if code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_memory/scopes/user", ""); code != 200 || strings.Contains(body, `"bob"`) {
				t.Errorf("member lists user scopes: %d %s, want 200 without bob", code, body)
			}

			// The member's own scope is its own.
			if code, body := bearerCall(t, ts, freeBearer, "PUT", "/v1/_memory/scopes/user/dev/keys/note", `{"value":"mine"}`); code != 200 {
				t.Errorf("member writes its own row: %d %s", code, body)
			}
			if code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_memory/scopes/user/dev/keys/note", ""); code != 200 || !strings.Contains(body, "mine") {
				t.Errorf("member reads its own row: %d %s", code, body)
			}
			if code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_memory/scopes/user", ""); code != 200 || !strings.Contains(body, `"dev"`) {
				t.Errorf("member lists user scopes: %d %s, want its own", code, body)
			}
		})
	}
}

// The memory key routes ask the instance question the Memory tool asks in a
// run, `read`/`write memory:<scope>`, not only the route's type-level one: a
// tenant rule denying writes to the tenant keyspace refuses the PUT.
func TestMemoryKeyRoutes_AskTheInstanceVerb(t *testing.T) {
	_, url, st := aclFixture(t, authz.ModeCompat)
	ts := httptestServerAt(url)
	putACL(t, st, "ro-tenant-memory", `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"]}]`)
	bind(t, st, "ro-tenant-memory", "user", "dev")
	if code, body := bearerCall(t, ts, freeBearer, "PUT", "/v1/_memory/scopes/tenant/_/keys/k", `{"value":"x"}`); code != http.StatusForbidden ||
		!strings.Contains(body, "acl_denied") {
		t.Errorf("PUT tenant row under deny write memory:tenant: %d %s, want 403 acl_denied", code, body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "PUT", "/v1/_memory/scopes/user/dev/keys/k", `{"value":"x"}`); code != 200 {
		t.Errorf("PUT own user row: %d %s", code, body)
	}
}

// browseSubjectExempt are the handlers that read a scope_id from the query or
// the URL path without resolving it through browseSubjectFor, and why each is
// safe. "carved out" ones are asserted below to serve only routes the member
// path refuses, so only the operator plane — whose scope_id is its own to
// choose — ever reaches them.
var browseSubjectExempt = map[string]string{
	"handleMemoryReembed":              "carved out: memory maintenance sweep",
	"handleMemoryHomeFacts":            "carved out: memory maintenance sweep",
	"handleMemoryPurgeStaleEmbeddings": "carved out: memory maintenance sweep",
	"handleMemoryBackfillEmbeddings":   "carved out: memory maintenance sweep",
	"handleMemoryCollapseFacts":        "carved out: memory maintenance sweep",
	"handleLimitDelete":                "carved out: budget writes",
	// The review routes pass scope_id to the shared review operation, whose
	// reviewCtx resolves it (asserted below).
	"handleStatementReviewQueue": "review: reviewCtx",
	"handleStatementRule":        "review: reviewCtx",
	"handleStatementRevert":      "review: reviewCtx",
	"handleStatementRulings":     "review: reviewCtx",
	"handleStatementAgreement":   "review: reviewCtx",
}

// browseSubjectBodyReaders take a subject from the request BODY rather than
// the query or path, which the scan below cannot see; each must resolve it
// through the helper too.
var browseSubjectBodyReaders = []string{"handleMemorySearch"}

// Every function in this package that reads `scope_id` from a query string or
// a URL path value resolves it through browseSubjectFor (directly or through
// one wrapper), or is listed in browseSubjectExempt with the reason it need
// not.
func TestBrowseSubject_EveryScopeIDReaderGoesThroughTheHelper(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	readers := map[string]bool{}
	callsHelper := map[string]bool{}
	callees := map[string]map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callees[fn.Name.Name] == nil {
					callees[fn.Name.Name] = map[string]bool{}
				}
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					callees[fn.Name.Name][f.Sel.Name] = true
					if (f.Sel.Name == "Get" || f.Sel.Name == "PathValue") && len(call.Args) == 1 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"scope_id"` {
							readers[fn.Name.Name] = true
						}
					}
					if f.Sel.Name == "browseSubjectFor" {
						callsHelper[fn.Name.Name] = true
					}
				case *ast.Ident:
					callees[fn.Name.Name][f.Name] = true
					if f.Name == "browseSubjectFor" {
						callsHelper[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	// A function resolves the subject when it calls the helper, or calls a
	// function that does (one hop: the route wrappers memoryRouteCtx,
	// browseCtx, substrateBrowseCtxFn, memorySearchFilterCtx).
	resolves := func(fn string) bool {
		if callsHelper[fn] {
			return true
		}
		for c := range callees[fn] {
			if callsHelper[c] {
				return true
			}
		}
		return false
	}
	if len(readers) < 10 {
		t.Fatalf("found only %d scope_id readers; the scan no longer matches the code: %v", len(readers), readers)
	}
	for fn := range readers {
		if resolves(fn) {
			continue
		}
		if _, ok := browseSubjectExempt[fn]; !ok {
			t.Errorf("%s reads scope_id from the request but does not resolve it through browseSubjectFor, and is not exempt", fn)
		}
	}
	for _, fn := range browseSubjectBodyReaders {
		if !resolves(fn) {
			t.Errorf("%s takes a subject from its body but does not resolve it through browseSubjectFor", fn)
		}
	}
	if !callsHelper["reviewCtx"] {
		t.Error("reviewCtx does not resolve its scope_id through browseSubjectFor")
	}
	for fn := range browseSubjectExempt {
		if !readers[fn] {
			t.Errorf("%s is exempt but reads no scope_id any more: drop it from the list", fn)
		}
	}

	// Each carved-out exemption serves only routes the member path refuses.
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	routeRe := regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)",.*\bs\.(handle\w+)\)`)
	routes := map[string][][2]string{}
	for _, m := range routeRe.FindAllStringSubmatch(string(src), -1) {
		routes[m[3]] = append(routes[m[3]], [2]string{m[1], m[2]})
	}
	for fn, why := range browseSubjectExempt {
		if !strings.HasPrefix(why, "carved out") {
			continue
		}
		if len(routes[fn]) == 0 {
			t.Errorf("%s: no route registered in server.go", fn)
		}
		for _, r := range routes[fn] {
			if !memberpath.CarvedOut(r[0], r[1]) {
				t.Errorf("%s serves %s %s, which the member path admits: resolve its scope_id through browseSubjectFor", fn, r[0], r[1])
			}
		}
	}
}
