package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// identityDocsFixture is the ontology fixture plus a config holder, since provisioning
// reads the operator's setting and a Server with no config deliberately does nothing.
func identityDocsFixture(t *testing.T, enabled bool) (*Server, memInject) {
	t.Helper()
	s, mi := ontologyFixture(t)
	cfg := &config.Config{}
	cfg.Env.ProvisionIdentityDocs = enabled
	s.cfgHolder = config.NewHolder(cfg)
	return s, mi
}

// docExistsAt reports whether a document is present at path in scope, WITHOUT
// provisioning it — a probe that provisioned would make every assertion below vacuous.
func (s *Server) docExistsAt(t *testing.T, mi memInject, scope, path string) bool {
	t.Helper()
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.docToolCtx(context.Background(), mi)
	dctx = tools.WithMemoryPolicy(dctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}})
	req, _ := json.Marshal(map[string]any{"op": "get_document", "scope": scope, "path": path})
	res, _ := doc.Execute(dctx, req)
	return !res.IsError
}

func TestProvisionIdentityDocs_CreatesBothForAPrincipal(t *testing.T) {
	s, mi := identityDocsFixture(t, true)
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Fatal("fixture: the user-root document should not exist yet")
	}

	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)

	if !s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("the user-root document was not provisioned")
	}
	if !s.docExistsAt(t, mi, "tenant", meminject.TenantRootPath) {
		t.Error("the tenant-root document was not provisioned")
	}
}

// The point of provisioning eagerly is that the person has a TEMPLATE to fill in, so the
// document has to arrive with the template in it — including the Identity section that
// memory placement depends on.
func TestProvisionIdentityDocs_TheUserDocArrivesWithTheIdentityTemplate(t *testing.T) {
	s, mi := identityDocsFixture(t, true)
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)

	md := s.readUserRootMarkdown(context.Background(), mi)
	if md == "" {
		t.Fatal("the provisioned document is empty")
	}
	if !strings.Contains(md, "Identity") || !strings.Contains(md, meminject.SelfNameDirective) {
		t.Errorf("the provisioned document lacks the Identity section a person is meant to fill in:\n%s", md)
	}
	// And it must declare NOBODY until edited — the template's example is indented for
	// exactly this reason.
	if names := meminject.ParseSelfNames(md); len(names) != 0 {
		t.Errorf("a freshly provisioned profile already declares %v", names)
	}
}

// An empty subject establishes a tenant but no person, so only the tenant document is
// created — a user-root document keyed on nobody would be unreachable litter.
func TestProvisionIdentityDocs_EmptySubjectProvisionsOnlyTheTenantDoc(t *testing.T) {
	s, mi := identityDocsFixture(t, true)
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, "")

	if !s.docExistsAt(t, mi, "tenant", meminject.TenantRootPath) {
		t.Error("the tenant-root document should still be provisioned")
	}
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("a user-root document was created for an empty subject")
	}
}

func TestProvisionIdentityDocs_DisabledCreatesNothing(t *testing.T) {
	s, mi := identityDocsFixture(t, false)
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("provisioning is disabled but the user-root document was created")
	}
	if s.docExistsAt(t, mi, "tenant", meminject.TenantRootPath) {
		t.Error("provisioning is disabled but the tenant-root document was created")
	}
}

// A Server with no config must not provision, and must not panic reaching for one.
func TestProvisionIdentityDocs_NoConfigDoesNothingAndDoesNotPanic(t *testing.T) {
	s, mi := ontologyFixture(t) // no cfgHolder
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("a Server with no config provisioned a document")
	}
}

// Twice is once. The provisioners are idempotent, and this asserts it through the eager
// path — a second mint for the same principal must not leave a duplicate document.
func TestProvisionIdentityDocs_IsIdempotent(t *testing.T) {
	s, mi := identityDocsFixture(t, true)
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)
	first := s.readUserRootMarkdown(context.Background(), mi)
	s.ProvisionIdentityDocs(context.Background(), mi.Tenant, mi.UserID)
	second := s.readUserRootMarkdown(context.Background(), mi)
	if first != second || first == "" {
		t.Errorf("a second provision changed the document.\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestProvisionDeclaredPrincipalDocs_ProvisionsEachOnceAtBoot(t *testing.T) {
	s, _ := identityDocsFixture(t, true)
	cfg := s.cfg()
	cfg.ResolvedPrincipals = []auth.DeclaredPrincipal{
		{Principal: auth.Principal{TenantID: "acme", Subject: "alice"}},
		{Principal: auth.Principal{TenantID: "acme", Subject: "bob"}},
		// Two tokens for one person: deduped, so the exists-check is not paid twice.
		{Principal: auth.Principal{TenantID: "acme", Subject: "alice"}},
		{Principal: auth.Principal{TenantID: "other", Subject: "carol"}},
	}

	s.ProvisionDeclaredPrincipalDocs(context.Background())

	for _, want := range []memInject{
		{Tenant: "acme", UserID: "alice"},
		{Tenant: "acme", UserID: "bob"},
		{Tenant: "other", UserID: "carol"},
	} {
		if !s.docExistsAt(t, want, "user", meminject.UserRootPath) {
			t.Errorf("no user-root document for declared principal %s/%s", want.Tenant, want.UserID)
		}
		if !s.docExistsAt(t, want, "tenant", meminject.TenantRootPath) {
			t.Errorf("no tenant-root document for tenant %s", want.Tenant)
		}
	}
	// A principal in another tenant must not have reached this one's user scope.
	if s.docExistsAt(t, memInject{Tenant: "acme", UserID: "carol"}, "user", meminject.UserRootPath) {
		t.Error("a declared principal's document leaked into the wrong tenant")
	}
}

// Only `create` establishes a principal. rotate and retire act on one that already
// exists, and retire in particular must not resurrect a document for a principal being
// taken away.
func TestIsTokenCreateOp_OnlyCreateEstablishesAPrincipal(t *testing.T) {
	for body, want := range map[string]bool{
		`{"op":"create","name":"n"}`: true,
		`{"op":"rotate","name":"n"}`: false,
		`{"op":"retire","name":"n"}`: false,
		`{"op":"get","name":"n"}`:    false,
		`{"op":"list"}`:              false,
		`not json`:                   false,
	} {
		if got := isTokenCreateOp(json.RawMessage(body)); got != want {
			t.Errorf("isTokenCreateOp(%s) = %v, want %v", body, got, want)
		}
	}
}

// The subject is read off the RESPONSE because it is optional on the wire and defaults to
// a derived per-token id — taking it from the request would provision nothing for every
// token minted without an explicit subject.
func TestCreatedTokenPrincipal_ReadsTheResolvedSubjectFromTheResponse(t *testing.T) {
	tenant, subject, ok := createdTokenPrincipal(`{"tenant_id":"acme","subject":"tok-ci-runner","name":"ci-runner"}`)
	if !ok || tenant != "acme" || subject != "tok-ci-runner" {
		t.Errorf("got (%q, %q, %v)", tenant, subject, ok)
	}
	// A tenant-scoped token with no distinct subject still establishes the tenant.
	if tenant, subject, ok := createdTokenPrincipal(`{"tenant_id":"acme","subject":""}`); !ok || tenant != "acme" || subject != "" {
		t.Errorf("empty subject: got (%q, %q, %v)", tenant, subject, ok)
	}
	// No tenant means nothing to provision against.
	if _, _, ok := createdTokenPrincipal(`{"subject":"alice"}`); ok {
		t.Error("a response with no tenant_id must not be provisioned against")
	}
	if _, _, ok := createdTokenPrincipal(`nonsense`); ok {
		t.Error("an unparseable response must not be provisioned against")
	}
}

// TestOperatorTokenDef_MintProvisionsTheIdentityDocs closes the wiring gap: the pieces
// above are each tested, but nothing asserted that a real mint actually reaches them.
// The composition is three lines and exactly the kind that is silently wrong.
func TestOperatorTokenDef_MintProvisionsTheIdentityDocs(t *testing.T) {
	s, _ := identityDocsFixture(t, true)
	s.SetOperatorTokenDefTool(&builtin.OperatorTokenDef{Store: s.store})

	mi := memInject{Tenant: "acme", UserID: "alice"}
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Fatal("fixture: nothing should exist yet")
	}

	req, _ := json.Marshal(map[string]any{
		"op": "create", "name": "alice-cli", "tenant_id": "acme", "subject": "alice",
		"scopes": []string{"runs:create"},
	})
	res, err := s.OperatorTokenDef(tokenAdminCtx(), req)
	if err != nil {
		t.Fatalf("OperatorTokenDef: %v", err)
	}
	if res.IsError {
		t.Fatalf("create failed: %s", res.Text)
	}

	if !s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("minting a token did not provision the user-root document")
	}
	if !s.docExistsAt(t, mi, "tenant", meminject.TenantRootPath) {
		t.Error("minting a token did not provision the tenant-root document")
	}
}

// A mint with no explicit subject still establishes a principal — the derived `tok-<name>`
// id — so it must still get a profile. Taking the subject from the REQUEST would provision
// nothing here, which is why it is read off the response.
func TestOperatorTokenDef_MintWithoutAnExplicitSubjectStillProvisions(t *testing.T) {
	s, _ := identityDocsFixture(t, true)
	s.SetOperatorTokenDefTool(&builtin.OperatorTokenDef{Store: s.store})

	req, _ := json.Marshal(map[string]any{
		"op": "create", "name": "ci-runner", "tenant_id": "acme",
		"scopes": []string{"runs:create"},
	})
	res, err := s.OperatorTokenDef(tokenAdminCtx(), req)
	if err != nil || res.IsError {
		t.Fatalf("create failed: %v %s", err, res.Text)
	}
	var got struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Subject == "" {
		t.Fatal("precondition: create should derive a subject when none is given")
	}
	if !s.docExistsAt(t, memInject{Tenant: "acme", UserID: got.Subject}, "user", meminject.UserRootPath) {
		t.Errorf("no profile provisioned for the derived subject %q", got.Subject)
	}
}

// Retiring a principal must not provision anything. A document appearing as a token is
// taken away would be the opposite of the intent.
func TestOperatorTokenDef_RetireProvisionsNothing(t *testing.T) {
	s, _ := identityDocsFixture(t, true)
	s.SetOperatorTokenDefTool(&builtin.OperatorTokenDef{Store: s.store})

	mk, _ := json.Marshal(map[string]any{
		"op": "create", "name": "temp", "tenant_id": "acme", "subject": "bob",
		"scopes": []string{"runs:create"},
	})
	if res, err := s.OperatorTokenDef(tokenAdminCtx(), mk); err != nil || res.IsError {
		t.Fatalf("seed create: %v %s", err, res.Text)
	}
	// Provisioning for a DIFFERENT principal, so the retire below has a clean target.
	mi := memInject{Tenant: "acme", UserID: "carol"}
	rt, _ := json.Marshal(map[string]any{"op": "retire", "name": "temp", "tenant_id": "acme"})
	if res, err := s.OperatorTokenDef(tokenAdminCtx(), rt); err != nil {
		t.Fatalf("retire: %v (%s)", err, res.Text)
	}
	if s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
		t.Error("a retire provisioned a document")
	}
}

// adminCtx grants the operator-admin policy the mint tool requires, which the real HTTP
// route stamps after its bearer check.
func tokenAdminCtx() context.Context {
	return tools.WithOperatorTokenDefPolicy(context.Background(),
		tools.OperatorTokenDefPolicyValue{Admin: true})
}

// TestProvisionIdentityDocs_EveryUserCreationPathProvisions is the guard for the defect
// that shipped in v1.68.0.
//
// Provisioning was hooked to the OperatorTokenDef substrate tool and nothing else, on the
// strength of one route dispatching through it. But there are THREE ways a principal comes
// into existence, and two of them write the row directly:
//
//	POST /v1/_users                  -> handleCreateUser     (store.UserCreate)
//	POST /v1/_users/{s}/tokens       -> handleMintUserToken  (store.OperatorTokenDefCreate)
//	OperatorTokenDef op=create       -> the substrate tool
//
// The Web UI drives the first two, so users created through it got no profile — no Identity
// section, so nobody could declare their own names, so placement could not tell a fact about
// them from a fact about a colleague. The feature was inert for exactly the people it was
// built for.
//
// This test enumerates the paths rather than testing one, because the failure was a MISSING
// call site, and a test per path written at the time would have covered the path that
// already worked.
func TestProvisionIdentityDocs_EveryUserCreationPathProvisions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		run     func(t *testing.T, s *Server, subject string)
	}{
		{
			name:    "POST /v1/_users",
			subject: "created-user",
			run: func(t *testing.T, s *Server, subject string) {
				body, _ := json.Marshal(map[string]string{"subject": subject, "display_name": "Created"})
				req := identityDocsReq(http.MethodPost, "/v1/_users", string(body))
				rec := httptest.NewRecorder()
				s.handleCreateUser(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("create user = %d: %s", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name:    "POST /v1/_users/{subject}/tokens",
			subject: "minted-user",
			run: func(t *testing.T, s *Server, subject string) {
				// The user must exist for the mint route to accept it.
				body, _ := json.Marshal(map[string]string{"subject": subject})
				cr := identityDocsReq(http.MethodPost, "/v1/_users", string(body))
				crRec := httptest.NewRecorder()
				s.handleCreateUser(crRec, cr)
				if crRec.Code != http.StatusCreated {
					t.Fatalf("seed user = %d: %s", crRec.Code, crRec.Body.String())
				}
				req := identityDocsReq(http.MethodPost, "/v1/_users/"+subject+"/tokens", `{}`)
				req.SetPathValue("subject", subject)
				rec := httptest.NewRecorder()
				s.handleMintUserToken(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := identityDocsFixture(t, true)
			tc.run(t, s, tc.subject)

			mi := memInject{Tenant: identityDocsTenant, UserID: tc.subject}
			if !s.docExistsAt(t, mi, "user", meminject.UserRootPath) {
				t.Errorf("%s created a principal but no user-root profile — the Identity "+
					"section it carries is what lets placement tell this person's facts "+
					"from a colleague's", tc.name)
			}
		})
	}
}

// identityDocsTenant is the tenant the guard test's principal belongs to. A token row
// requires a non-empty tenant, so the request has to carry a real principal.
const identityDocsTenant = "acme"

// identityDocsReq builds a request stamped with an admin principal in identityDocsTenant —
// what authMiddleware would have put there in production.
func identityDocsReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{
		TenantID: identityDocsTenant,
		Subject:  "operator",
		Scopes:   []string{auth.ScopeAdmin},
	}))
}
