package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedUser registers a first-class user directly in the store (bypassing the
// handler) so token tests can set up their fixtures precisely.
func seedUser(t *testing.T, st store.Store, tenant, subject, accessMode, status string) {
	t.Helper()
	if err := st.UserCreate(context.Background(), store.UserRow{
		TenantID: tenant, Subject: subject, AccessMode: accessMode, Status: status,
	}); err != nil {
		t.Fatalf("seed user %s/%s: %v", tenant, subject, err)
	}
}

// Mint derives scopes from the target user's access_mode and FORCES the row's
// tenant to the principal's — a body/query "tenant" or "scopes" never reaches
// the stored row. The minted token also resolves back to exactly the derived
// principal (proves the pepper matches the auth hot path).
//
// SECURITY ASSERTION (fail-before target): the persisted row is under the
// principal tenant "acme", NEVER the "?tenant=evil" the request carries.
func TestHandleMintUserToken_TenantForcedAndScopesDerived(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}

	// An ISOLATED member — the grant must be exactly [substrate:user].
	seedUser(t, st, "acme", "alice", "isolated", "active")

	// A bogus tenant + admin scopes in the body AND a ?tenant=evil query, all of
	// which the handler must ignore.
	body := `{"tenant":"evil","scopes":["substrate:admin"]}`
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens?tenant=evil", body, acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint status %d: %s", rec.Code, rec.Body.String())
	}
	var resp mintUserTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" || resp.TokenSuffix == "" {
		t.Errorf("mint response missing show-once token/suffix: %+v", resp)
	}
	if resp.Name != "u-alice" {
		t.Errorf("derived name = %q, want u-alice", resp.Name)
	}
	if len(resp.Scopes) != 1 || resp.Scopes[0] != auth.ScopeUser {
		t.Errorf("scopes = %v, want [%s] (from access_mode=isolated, NOT the body's admin)", resp.Scopes, auth.ScopeUser)
	}

	// FORCED-TENANT assertion: the row is under "acme", never "evil".
	acmeRows, err := st.OperatorTokenDefListBySubject(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("list acme/alice: %v", err)
	}
	if len(acmeRows) != 1 {
		t.Fatalf("acme/alice rows = %d, want 1 (tenant must be forced to the principal)", len(acmeRows))
	}
	evilRows, err := st.OperatorTokenDefListBySubject(ctx, "evil", "alice")
	if err != nil {
		t.Fatalf("list evil/alice: %v", err)
	}
	if len(evilRows) != 0 {
		t.Errorf("evil/alice rows = %d, want 0 — a body/query tenant leaked into the row", len(evilRows))
	}

	// The minted token resolves to the derived principal (pepper matches).
	p, ok := s.resolvePrincipal(ctx, resp.Token)
	if !ok || p.TenantID != "acme" || p.Subject != "alice" {
		t.Errorf("resolvePrincipal(minted) = (%+v, %v), want acme/alice", p, ok)
	}
	if !auth.HasScope(p.Scopes, auth.ScopeUser) || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		t.Errorf("minted principal scopes = %v, want substrate:user only (no admin)", p.Scopes)
	}
}

// A tenant-mode member gets the whole-tenant collaboration grant (runs +
// channels), never an operator/admin scope.
func TestHandleMintUserToken_TenantModeGrant(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "carol", "tenant", "active")

	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/carol/tokens", "", acme, "subject", "carol"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint status %d: %s", rec.Code, rec.Body.String())
	}
	var resp mintUserTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	want := map[string]bool{
		auth.ScopeRunsCreate: true, auth.ScopeRunsRead: true,
		auth.ScopeChannelPublish: true, auth.ScopeChannelRead: true,
	}
	if len(resp.Scopes) != len(want) {
		t.Fatalf("scopes = %v, want the 4 tenant-member scopes", resp.Scopes)
	}
	for _, sc := range resp.Scopes {
		if !want[sc] {
			t.Errorf("unexpected/privileged scope %q in a tenant-member grant", sc)
		}
	}
}

// Minting for a subject that is not a registered user of the caller's tenant
// (absent, or belonging to another tenant) is an opaque 404 — no oracle.
func TestHandleMintUserToken_UnknownUser404(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	// A user that exists ONLY in another tenant must not be mintable by acme.
	seedUser(t, st, "other", "carol", "isolated", "active")

	for _, subject := range []string{"ghost", "carol"} {
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/"+subject+"/tokens", "", acme, "subject", subject))
		if rec.Code != http.StatusNotFound {
			t.Errorf("mint for %q = %d, want 404", subject, rec.Code)
		}
	}
}

// A disabled user cannot be minted a token.
func TestHandleMintUserToken_DisabledUserRejected(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "bob", "tenant", "disabled")

	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/bob/tokens", "", acme, "subject", "bob"))
	if rec.Code != http.StatusConflict {
		t.Errorf("mint for a disabled user = %d, want 409", rec.Code)
	}
	// Nothing was persisted.
	rows, _ := st.OperatorTokenDefListBySubject(context.Background(), "acme", "bob")
	if len(rows) != 0 {
		t.Errorf("disabled-user mint persisted %d rows, want 0", len(rows))
	}
}

// The three token subroutes are ScopeTenant (via the /v1/_users/ prefix gate),
// and a substrate:user MEMBER does not satisfy ScopeTenant — so a member is
// auto-denied minting at the middleware, before the handler runs.
func TestRequiredScopeFor_UserTokenRoutes(t *testing.T) {
	cases := []struct{ method, path string }{
		{"POST", "/v1/_users/alice/tokens"},
		{"GET", "/v1/_users/alice/tokens"},
		{"DELETE", "/v1/_users/alice/tokens/def_x"},
	}
	for _, c := range cases {
		if got := requiredScopeFor(c.method, c.path); got != auth.ScopeTenant {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, auth.ScopeTenant)
		}
	}
	// A member (substrate:user) is denied the ScopeTenant gate.
	if auth.HasScope([]string{auth.ScopeUser}, auth.ScopeTenant) {
		t.Errorf("substrate:user must NOT satisfy substrate:tenant — a member could mint")
	}
	// A tenant operator (and admin) passes it.
	if !auth.HasScope([]string{auth.ScopeTenant}, auth.ScopeTenant) {
		t.Errorf("substrate:tenant must satisfy the mint gate")
	}
}

// List returns member-token METADATA ONLY (never a plaintext or token_hash) and
// excludes any privileged token that collides on (tenant, subject).
func TestHandleListUserTokens_NoSecretMaterial(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	// Two member tokens via the surface.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	// A privileged token colliding on (acme, alice), seeded directly — must NOT
	// surface through the member list.
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_priv", Name: "sneaky", TenantID: "acme", Subject: "alice",
		TokenHash: "h-priv", AllowedScopes: []string{auth.ScopeTenant},
	}); err != nil {
		t.Fatalf("seed privileged: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleListUserTokens(rec, bodyReq("GET", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", rec.Code, rec.Body.String())
	}
	// Decode loosely so a leaked secret key would be visible.
	var raw struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Tokens) != 2 {
		t.Fatalf("list returned %d tokens, want 2 member tokens (privileged excluded)", len(raw.Tokens))
	}
	for _, m := range raw.Tokens {
		if _, ok := m["token"]; ok {
			t.Errorf("list leaked a plaintext token: %v", m)
		}
		if _, ok := m["token_hash"]; ok {
			t.Errorf("list leaked token_hash: %v", m)
		}
		if _, ok := m["def_id"]; !ok {
			t.Errorf("list row missing def_id (the revoke handle): %v", m)
		}
	}
}

// Revoke is confined to the caller's own tenant + user's MEMBER tokens: a
// cross-tenant def_id, and a privileged token colliding on (tenant, subject),
// are both opaque 404s that leave the target token active; an own member token
// revokes and stops authenticating.
func TestHandleRevokeUserToken_ConfinedToOwnMemberTokens(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	// Mint a member token for acme/alice.
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var minted mintUserTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)

	// A privileged token colliding on (acme, alice), seeded directly.
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_priv", Name: "sneaky", TenantID: "acme", Subject: "alice",
		TokenHash: "h-priv", AllowedScopes: []string{auth.ScopeAdmin},
	}); err != nil {
		t.Fatalf("seed privileged: %v", err)
	}

	// (1) A cross-tenant operator cannot revoke acme/alice's token.
	other := auth.Principal{TenantID: "other", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec = httptest.NewRecorder()
	s.handleRevokeUserToken(rec, revokeReq(minted.DefID, other, "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant revoke = %d, want opaque 404", rec.Code)
	}
	if row, _ := st.OperatorTokenDefGet(ctx, minted.DefID); !row.RetiredAt.IsZero() {
		t.Errorf("cross-tenant revoke retired the token (retired_at=%v)", row.RetiredAt)
	}

	// (2) The tenant operator cannot revoke a PRIVILEGED token via the member
	// surface, even in its own tenant/subject.
	rec = httptest.NewRecorder()
	s.handleRevokeUserToken(rec, revokeReq("def_priv", acme, "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("privileged-token revoke = %d, want opaque 404", rec.Code)
	}
	if row, _ := st.OperatorTokenDefGet(ctx, "def_priv"); !row.RetiredAt.IsZero() {
		t.Errorf("member surface retired a privileged token — escalation")
	}

	// (3) The own member token revokes, and no longer authenticates.
	rec = httptest.NewRecorder()
	s.handleRevokeUserToken(rec, revokeReq(minted.DefID, acme, "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("own revoke = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if row, _ := st.OperatorTokenDefGet(ctx, minted.DefID); row.RetiredAt.IsZero() {
		t.Errorf("own member token not retired after revoke")
	}
	if _, ok := s.resolvePrincipal(ctx, minted.Token); ok {
		t.Errorf("revoked token still authenticates")
	}
}

// revokeReq builds a DELETE request with both {subject} and {def_id} path values
// stamped + the principal on ctx.
func revokeReq(defID string, p auth.Principal, subject string) *http.Request {
	req := httptest.NewRequest("DELETE", "/v1/_users/"+subject+"/tokens/"+defID, nil)
	req.SetPathValue("subject", subject)
	req.SetPathValue("def_id", defID)
	return req.WithContext(auth.WithPrincipal(req.Context(), p))
}

// Code-review HIGH regression: disabling or deleting a user must retire its
// delegated tokens immediately — the auth hot path is token-only and never
// consults users.status, so without the cascade a disabled/deleted user's
// bearer keeps authenticating until its own retired_at.
func mintForAlice(t *testing.T, s *Server) {
	t.Helper()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
}

func activeAliceTokens(t *testing.T, st store.Store) int {
	t.Helper()
	rows, err := st.OperatorTokenDefListBySubject(context.Background(), "acme", "alice")
	if err != nil {
		t.Fatalf("list acme/alice: %v", err)
	}
	n := 0
	for _, r := range rows {
		if tokenActive(r) {
			n++
		}
	}
	return n
}

func TestDisableUser_RetiresDelegatedTokens(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedUser(t, st, "acme", "alice", "isolated", "active")
	mintForAlice(t, s)
	if n := activeAliceTokens(t, st); n != 1 {
		t.Fatalf("precondition: active tokens = %d, want 1", n)
	}
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled"}`, acme, "subject", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if n := activeAliceTokens(t, st); n != 0 {
		t.Errorf("active tokens after disable = %d, want 0 (disable must retire delegated tokens)", n)
	}
}

func TestDeleteUser_RetiresDelegatedTokens(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedUser(t, st, "acme", "alice", "isolated", "active")
	mintForAlice(t, s)
	if n := activeAliceTokens(t, st); n != 1 {
		t.Fatalf("precondition: active tokens = %d, want 1", n)
	}
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/alice", "", acme, "subject", "alice"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// The token rows persist (retired) even though the identity row is gone.
	if n := activeAliceTokens(t, st); n != 0 {
		t.Errorf("active tokens after delete = %d, want 0 (delete must retire delegated tokens)", n)
	}
}
