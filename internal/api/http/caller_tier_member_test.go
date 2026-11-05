package http

// A member is not the operator plane (RFC FY Part B). RFC CB opened the
// substrate:tenant HTTP surfaces to non-isolated members after those surfaces
// had been written for tenant operators, and every off-run call carried the
// operator's authority with it: a member could confirm the ontology, author
// its review policy, rule in another subject's scope and hold a scope's lease
// under the identity every other caller shared. These tests drive the real
// /v1/_document handler and the real route gate as a member and as a tenant
// operator, side by side.

import (
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
	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

const tierTestOntology = "# Tenant Ontology\n\n## event\n- `occurred_at`\n"

// tierFixture is a server whose /v1/_document dispatches the real Document tool
// on SQL Memory, for tenant acme.
func tierFixture(t *testing.T) (*Server, memInject) {
	t.Helper()
	s, mi := ontologyFixture(t)
	s.cfgHolder = config.NewHolder(&config.Config{})
	s.tools = []tools.Tool{&builtin.Document{Store: s.store, SqlMem: s.sqlMem}}
	return s, mi
}

// tierOperator is a tenant operator of tenant acting as subject.
func tierOperator(tenant, subject string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeTenant}})
}

// tierMember is a non-isolated member of tenant: the scopes a tenant-mode user
// holds, and a rule set that lets it read, write, link and propose on every
// document — so a refusal below is the tier's, not a missing grant's.
func tierMember(t *testing.T, tenant, subject string) context.Context {
	t.Helper()
	rules := []authz.Rule{
		{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"memory:*", "sqlmem:*"}},
		{Effect: authz.Allow, Actions: []string{"read", "write", "delete", "link", "propose"}, Resources: []string{"document:*"}},
	}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: tenant, ID: subject},
		Bindings: []authz.Binding{{ACL: "m", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"m": {ACL: authz.ACL{Name: "m", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes, Effective: eff})
}

// postDocument drives POST /v1/_document as the principal on ctx, with query as
// its query string. It returns the status and the decoded body.
func (s *Server) postDocument(t *testing.T, ctx context.Context, query, body string) (int, map[string]any) {
	t.Helper()
	target := "/v1/_document"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleSubstrateDocument(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// mustDocument is postDocument that fails the test on anything but 200.
func (s *Server) mustDocument(t *testing.T, ctx context.Context, query, body string) map[string]any {
	t.Helper()
	code, out := s.postDocument(t, ctx, query, body)
	if code != http.StatusOK {
		t.Fatalf("%s = %d %v", body, code, out)
	}
	return out
}

// installTierOntology writes md as the tenant ontology, as its operator.
func (s *Server) installTierOntology(t *testing.T, op context.Context, md string) (docID, rootID string) {
	t.Helper()
	mj, _ := json.Marshal(md)
	out := s.mustDocument(t, op, "", `{"op":"import_md","scope":"tenant","markdown":`+string(mj)+`}`)
	docID, rootID = out["document_id"].(string), out["root_chunk_id"].(string)
	pj, _ := json.Marshal(meminject.OntologyPath)
	s.mustDocument(t, op, "", fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":%s}`, docID, pj))
	return docID, rootID
}

func (s *Server) tierOntologyRead(t *testing.T, mi memInject) builtin.OntologyRead {
	t.Helper()
	read, err := s.ontologyDocument().OntologyTermsFromTree(withTenantDocGrants(s.docToolCtx(context.Background(), mi)), "tenant", meminject.OntologyPath)
	if err != nil {
		t.Fatalf("read the ontology: %v", err)
	}
	return read
}

func (s *Server) chunkRevision(t *testing.T, ctx context.Context, scope, id string) int {
	t.Helper()
	g := s.mustDocument(t, ctx, "", fmt.Sprintf(`{"op":"get_chunk","scope":%q,"id":%q}`, scope, id))
	return int(g["revision"].(float64))
}

// errText is the refusal text of a tool_refused body.
func errText(out map[string]any) string {
	s, _ := out["error"].(string)
	return s
}

// TestOntologyGuard_AMemberMayProposeButNotDecide is the member twin of the
// builtin TestOntologyGuard_AnAgentMayProposeButNotDecide, on the HTTP plane a
// member actually reaches: it may file a proposal through /v1/_document, and it
// may not decide one there or on the /v1/_ontology routes.
func TestOntologyGuard_AMemberMayProposeButNotDecide(t *testing.T) {
	s, mi := tierFixture(t)
	op := tierOperator(mi.Tenant, "ops")
	member := tierMember(t, mi.Tenant, "bob")
	docID, rootID := s.installTierOntology(t, op, tierTestOntology)

	// PROPOSE: allowed, and inert.
	code, out := s.postDocument(t, member, "", `{"op":"propose_entity","name":"outage","parent":"event","body":"Seen 9 times."}`)
	if code != http.StatusOK {
		t.Fatalf("a member's propose_entity = %d %v, want 200", code, out)
	}
	proposalID, _ := out["chunk_id"].(string)
	if proposalID == "" {
		t.Fatalf("propose_entity returned no chunk id: %v", out)
	}

	// DECIDE: refused, in every spelling a member can reach over /v1/_document.
	for _, c := range []struct{ name, body string }{
		{"confirm the ontology", fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"confirmed","revision":%d}`,
			rootID, s.chunkRevision(t, op, "tenant", rootID))},
		{"accept a proposal", fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"","revision":%d}`,
			proposalID, s.chunkRevision(t, op, "tenant", proposalID))},
		{"create a live entity", fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"decided","body":""}`,
			docID, rootID)},
		{"delete a proposal", fmt.Sprintf(`{"op":"delete_chunk","scope":"tenant","id":%q}`, proposalID)},
	} {
		if code, out := s.postDocument(t, member, "", c.body); code == http.StatusOK {
			t.Errorf("a member was allowed to %s: %v", c.name, out)
		}
	}
	read := s.tierOntologyRead(t, mi)
	if read.Confirmed {
		t.Error("a member's write confirmed the ontology")
	}
	for _, term := range read.Terms {
		if term.Name == "outage" || term.Name == "decided" {
			t.Errorf("a member's write put %q in force", term.Name)
		}
	}

	// The three /v1/_ontology routes through the real gate: a member reads the
	// ontology; confirming, adopting and ruling on a proposal are the operator's.
	gate, st := tokenAuthServer(t, "legacy")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	seedToken(t, st, "lct_member", mi.Tenant, "bob", memberScopes, time.Time{})
	seedToken(t, st, "lct_operator", mi.Tenant, "ops", []string{auth.ScopeTenant}, time.Time{})
	if reached, code := runMW(t, gate, http.MethodGet, "/v1/_ontology", "lct_member"); !reached || code == http.StatusForbidden {
		t.Errorf("a member reading the ontology = %d reached=%v, want admitted", code, reached)
	}
	for _, path := range []string{"/v1/_ontology", "/v1/_ontology/adopt", "/v1/_ontology/proposals"} {
		if reached, code := runMW(t, gate, http.MethodPost, path, "lct_member"); reached || code != http.StatusForbidden {
			t.Errorf("a member on POST %s = %d reached=%v, want 403", path, code, reached)
		}
		if reached, code := runMW(t, gate, http.MethodPost, path, "lct_operator"); !reached || code == http.StatusForbidden {
			t.Errorf("the tenant operator on POST %s = %d reached=%v, want admitted", path, code, reached)
		}
	}

	// THE OPERATOR is unaffected: the same accept, allowed.
	if code, out := s.postDocument(t, op, "", fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"","revision":%d}`,
		proposalID, s.chunkRevision(t, op, "tenant", proposalID))); code != http.StatusOK {
		t.Fatalf("the tenant operator's accept = %d %v", code, out)
	}
	if read := s.tierOntologyRead(t, mi); !hasTerm(read.Terms, "outage") {
		t.Error("the operator's accept did not put the proposal in force")
	}
}

// TestReviewPolicy_OnlyTheOperatorPlaneWritesIt: the review policy decides
// which proposals are accepted without a person, so writing it is deciding. A
// member may neither edit the Review section nor file a chunk the reader takes
// for one; the operator may.
func TestReviewPolicy_OnlyTheOperatorPlaneWritesIt(t *testing.T) {
	t.Run("editing the section", func(t *testing.T) {
		s, mi := tierFixture(t)
		op := tierOperator(mi.Tenant, "ops")
		member := tierMember(t, mi.Tenant, "bob")
		s.installTierOntology(t, op, tierTestOntology+"\n## Review\n@section review\n- human *\n")
		reviewID := s.tierOntologyRead(t, mi).SectionChunks[meminject.SectionReview]
		if reviewID == "" {
			t.Fatal("fixture: no Review section")
		}
		edit := func() string {
			return fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"@section review\n- accept kind:cites\n","revision":%d}`,
				reviewID, s.chunkRevision(t, op, "tenant", reviewID))
		}
		if code, out := s.postDocument(t, member, "", edit()); code == http.StatusOK {
			t.Errorf("a member rewrote the review policy: %v", out)
		}
		if p := s.tierOntologyRead(t, mi).Review; p == nil || len(p.Rules) != 1 || p.Rules[0].Action != meminject.ReviewHuman {
			t.Fatalf("the review policy changed under a member: %+v", p)
		}
		if code, out := s.postDocument(t, op, "", edit()); code != http.StatusOK {
			t.Fatalf("the tenant operator's edit = %d %v", code, out)
		}
		if p := s.tierOntologyRead(t, mi).Review; p == nil || len(p.Rules) != 1 || p.Rules[0].Action != meminject.ReviewAccept {
			t.Errorf("the operator's edit did not take effect: %+v", p)
		}
	})
	t.Run("a proposed chunk is not a section", func(t *testing.T) {
		s, mi := tierFixture(t)
		op := tierOperator(mi.Tenant, "ops")
		member := tierMember(t, mi.Tenant, "bob")
		docID, rootID := s.installTierOntology(t, op, tierTestOntology)
		// A proposal is the one write the guard lets a non-operator make on the
		// ontology document; one whose body declares the Review section must stay
		// a proposal, not become the policy.
		code, out := s.postDocument(t, member, "", fmt.Sprintf(
			`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"Review","status":"proposed","body":"@section review\n- accept *\n"}`,
			docID, rootID))
		if code != http.StatusOK {
			t.Logf("the proposal was refused outright (%d %v); the reader check below still applies", code, out)
		}
		if p := s.tierOntologyRead(t, mi).Review; p != nil {
			t.Errorf("a member's proposed chunk became the review policy: %+v", p)
		}
	})
}

// TestAcceptStatement_AMemberRulesOnlyInItsOwnUserScope: an operator's ruling
// on a proposed statement, which a member may make in its own user scope and in
// no one else's.
func TestAcceptStatement_AMemberRulesOnlyInItsOwnUserScope(t *testing.T) {
	s, mi := tierFixture(t)
	alice := tierMember(t, mi.Tenant, "alice")
	bob := tierMember(t, mi.Tenant, "bob")
	propose := func(ctx context.Context) string {
		d := s.mustDocument(t, ctx, "", `{"op":"create_document","scope":"user","title":"notes","path":"/notes"}`)["document_id"].(string)
		one := s.mustDocument(t, ctx, "", fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"a","body":"text"}`, d))["id"].(string)
		two := s.mustDocument(t, ctx, "", fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"b","body":"text"}`, d))["id"].(string)
		return s.mustDocument(t, ctx, "", fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, one, two))["id"].(string)
	}
	alices, bobs := propose(alice), propose(bob)

	if code, out := s.postDocument(t, bob, "scope_id=alice", fmt.Sprintf(`{"op":"accept_statement","scope":"user","id":%q}`, alices)); code == http.StatusOK {
		t.Errorf("a member accepted a statement in another subject's user scope: %v", out)
	}
	if code, out := s.postDocument(t, bob, "scope_id=alice", fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q,"reason":"no"}`, alices)); code == http.StatusOK {
		t.Errorf("a member rejected a statement in another subject's user scope: %v", out)
	}
	if code, out := s.postDocument(t, bob, "", fmt.Sprintf(`{"op":"accept_statement","scope":"user","id":%q}`, bobs)); code != http.StatusOK {
		t.Errorf("a member's ruling in its own user scope = %d %v, want 200", code, out)
	}
	// Alice's proposal is still a proposal.
	g := s.mustDocument(t, alice, "", fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, alices))
	if st, _ := g["statement"].(map[string]any); st["status"] != "proposed" {
		t.Errorf("alice's statement = %v, want still proposed", g)
	}
}

// TestDocumentRulings_AMemberIsRefusedTheOperatorsOps: the Document ops that
// decide for the tenant answer a member with a permission refusal, before any
// lookup — so the refusal says who may, not what was missing.
func TestDocumentRulings_AMemberIsRefusedTheOperatorsOps(t *testing.T) {
	s, mi := tierFixture(t)
	op := tierOperator(mi.Tenant, "ops")
	member := tierMember(t, mi.Tenant, "bob")
	d := s.mustDocument(t, op, "", `{"op":"create_document","scope":"tenant","title":"shared","path":"/shared"}`)["document_id"].(string)

	for _, c := range []struct{ name, body, want string }{
		{"accept a classification", fmt.Sprintf(`{"op":"accept_classification","scope":"tenant","document_id":%q}`, d), "only an operator"},
		{"define a tenant-wide type", `{"op":"define_type","scope":"tenant","name":"task","fields":{"owner":"who"}}`, "operator"},
		{"maintain a statement", `{"op":"maintain_statement","scope":"tenant","id":"nope","action":"verify"}`, "only a code agent granted statement_methods, or an operator"},
	} {
		code, out := s.postDocument(t, member, "", c.body)
		if code == http.StatusOK || !strings.Contains(errText(out), c.want) {
			t.Errorf("a member's %s = %d %v, want a refusal naming %q", c.name, code, out, c.want)
		}
	}
	// In its own user scope a member curates as the operator does: it accepts a
	// classification and maintains a statement there (each answer below is the
	// op's own, past the ruling gate), and does neither in another subject's.
	mine := s.mustDocument(t, member, "", `{"op":"create_document","scope":"user","title":"mine","path":"/mine"}`)["document_id"].(string)
	for _, c := range []struct{ name, query, body string }{
		{"accept a classification in its own user scope", "", fmt.Sprintf(`{"op":"accept_classification","scope":"user","document_id":%q}`, mine)},
		{"maintain a statement in its own user scope", "", `{"op":"maintain_statement","scope":"user","id":"nope","action":"verify"}`},
	} {
		if code, out := s.postDocument(t, member, c.query, c.body); code == http.StatusOK || strings.Contains(errText(out), "operator") {
			t.Errorf("a member's %s = %d %v, want past the ruling gate (the op's own refusal)", c.name, code, out)
		}
	}
	for _, c := range []struct{ name, body, want string }{
		{"accept a classification in another subject's scope", fmt.Sprintf(`{"op":"accept_classification","scope":"user","document_id":%q}`, mine), "only an operator"},
		{"maintain a statement in another subject's scope", `{"op":"maintain_statement","scope":"user","id":"nope","action":"verify"}`, "only a code agent granted statement_methods, or an operator"},
	} {
		code, out := s.postDocument(t, member, "scope_id=alice", c.body)
		if code == http.StatusOK || !strings.Contains(errText(out), c.want) {
			t.Errorf("a member's %s = %d %v, want a refusal naming %q", c.name, code, out, c.want)
		}
	}
	// The operator still defines a tenant-wide type, and a member still defines
	// one in its own user scope.
	if code, out := s.postDocument(t, op, "", `{"op":"define_type","scope":"tenant","name":"task","fields":{"owner":"who"}}`); code != http.StatusOK {
		t.Errorf("the tenant operator's define_type = %d %v", code, out)
	}
	if code, out := s.postDocument(t, member, "", `{"op":"define_type","scope":"user","name":"task","fields":{"owner":"who"}}`); code != http.StatusOK {
		t.Errorf("a member's define_type in its own user scope = %d %v", code, out)
	}
}

// TestLease_TwoOffRunPrincipalsDoNotShareAnOwner: a lease held off-run belongs
// to the principal that took it. When every off-run caller was the same
// synthetic owner, a second caller's lease call extended the first one's lease
// and its release dropped it.
func TestLease_TwoOffRunPrincipalsDoNotShareAnOwner(t *testing.T) {
	s, mi := tierFixture(t)
	first, second := tierOperator(mi.Tenant, "ops-a"), tierOperator(mi.Tenant, "ops-b")
	if out := s.mustDocument(t, first, "", `{"op":"lease","scope":"tenant","name":"linker"}`); out["acquired"] != true {
		t.Fatalf("the first operator's lease = %v", out)
	}
	if out := s.mustDocument(t, second, "", `{"op":"lease","scope":"tenant","name":"linker"}`); out["acquired"] == true {
		t.Errorf("a second operator took a lease the first holds: %v", out)
	}
	if out := s.mustDocument(t, second, "", `{"op":"release","scope":"tenant","name":"linker"}`); out["released"] == true {
		t.Errorf("a second operator released the first one's lease: %v", out)
	}
	if out := s.mustDocument(t, first, "", `{"op":"release","scope":"tenant","name":"linker"}`); out["released"] != true {
		t.Errorf("the holder's own release = %v, want released", out)
	}
}

// TestLease_AMemberLeasesOnlyItsOwnUserScope: holding a pass off a scope for up
// to an hour is an operator's (or a statement-maintaining driver's) act on the
// tenant scope; a member may lease only its own user scope.
func TestLease_AMemberLeasesOnlyItsOwnUserScope(t *testing.T) {
	s, mi := tierFixture(t)
	member := tierMember(t, mi.Tenant, "bob")
	if code, out := s.postDocument(t, member, "", `{"op":"lease","scope":"tenant","name":"linker"}`); code == http.StatusOK {
		t.Errorf("a member leased the tenant scope: %v", out)
	}
	if code, out := s.postDocument(t, member, "scope_id=alice", `{"op":"lease","scope":"user","name":"linker"}`); code == http.StatusOK {
		t.Errorf("a member leased another subject's user scope: %v", out)
	}
	if out := s.mustDocument(t, member, "", `{"op":"lease","scope":"user","name":"linker"}`); out["acquired"] != true {
		t.Errorf("a member's lease on its own user scope = %v, want acquired", out)
	}
}

// TestCallerTier_EachPrincipalGetsItsTier pins who the HTTP substrate plane
// says is calling, for every kind of caller it admits, and that the spawn
// boundary takes the plane's tier off a child run.
func TestCallerTier_EachPrincipalGetsItsTier(t *testing.T) {
	isolated, _ := auth.GrantableUserScopes("isolated")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	as := func(p auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }
	for _, c := range []struct {
		name  string
		ctx   context.Context
		focus string // ?tenant=
		want  tools.Tier
	}{
		{"open mode", context.Background(), "", tools.TierOperatorPlane},
		{"admin", as(auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}), "", tools.TierOperatorPlane},
		{"admin focused on another tenant", as(auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}), "globex", tools.TierOperatorPlane},
		{"legacy", as(auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}), "", tools.TierOperatorPlane},
		{"tenant operator", as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}), "", tools.TierOperatorPlane},
		// The focus is ignored for a tenant operator: it stays in its own tenant,
		// where it is the operator plane.
		{"tenant operator naming another tenant", as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}), "globex", tools.TierOperatorPlane},
		{"member", as(auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes}), "", tools.TierMember},
		{"isolated member", as(auth.Principal{TenantID: "acme", Subject: "eve", Scopes: isolated}), "", tools.TierIsolated},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := tools.CallerTier(substrateAdminCtx(c.ctx)); got != c.want && c.focus == "" {
				t.Errorf("substrateAdminCtx tier = %s, want %s", got, c.want)
			}
			if got := tools.CallerTier(substrateAdminUserCtx(c.ctx)); got != c.want && c.focus == "" {
				t.Errorf("substrateAdminUserCtx tier = %s, want %s", got, c.want)
			}
			browse := substrateBrowseCtx(c.ctx, "someone", c.focus)
			if got := tools.CallerTier(browse); got != c.want {
				t.Errorf("substrateBrowseCtx tier = %s, want %s", got, c.want)
			}
			if p, ok := auth.PrincipalFromContext(c.ctx); ok && !p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
				if got := tools.RunIdentity(browse).TenantID; got != p.TenantID {
					t.Errorf("browse tenant = %q, want the principal's own %q", got, p.TenantID)
				}
			}
			if !tools.IsOperatorPlane(substrateAdminCtx(c.ctx)) && c.want == tools.TierOperatorPlane {
				t.Error("IsOperatorPlane is false for the operator plane off-run")
			}
			if tools.IsOperatorPlane(tools.WithRunID(substrateAdminCtx(c.ctx), "r_1")) {
				t.Error("IsOperatorPlane holds inside a run")
			}
		})
	}
	// A substrate:tenant principal acting in a tenant not its own is not trusted
	// as that tenant's operator.
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	if got := tools.TierForPrincipal(ops, true, "globex"); got != tools.TierMember {
		t.Errorf("a tenant operator acting in another tenant = %s, want member", got)
	}
	// The run: what the spawn boundary hands a child the plane started.
	parent := substrateAdminCtx(as(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
	if got := tools.CallerTier(stripOperatorPlane(parent)); got != tools.TierRun {
		t.Errorf("a child of the operator plane has tier %s, want run", got)
	}
	if got := tools.CallerTier(context.Background()); got != tools.TierRun {
		t.Errorf("an unstamped ctx has tier %s, want run", got)
	}
}
