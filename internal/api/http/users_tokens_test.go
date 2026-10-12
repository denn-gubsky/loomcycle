package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
// tenant to the principal's — a query "tenant" never reaches the stored row,
// and a body naming a tenant or scopes is refused outright (see
// TestHandleMintUserToken_UnknownBodyFieldRefused). The minted token also
// resolves back to exactly the derived principal (proves the pepper matches
// the auth hot path).
//
// SECURITY ASSERTION (fail-before target): the persisted row is under the
// principal tenant "acme", NEVER the "?tenant=evil" the request carries.
func TestHandleMintUserToken_TenantForcedAndScopesDerived(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}

	// An ISOLATED member — the grant must be exactly [substrate:user].
	seedUser(t, st, "acme", "alice", "isolated", "active")

	// A ?tenant=evil query, which the handler must ignore.
	body := ""
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

// mintFor mints a token for subject with the given body and returns the
// decoded response, failing the test on a non-201.
func mintFor(t *testing.T, s *Server, p auth.Principal, subject, body string) mintUserTokenResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/"+subject+"/tokens", body, p, "subject", subject))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint %s %s: status %d: %s", subject, body, rec.Code, rec.Body.String())
	}
	var resp mintUserTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// A body that names a tenant or scopes used to be ignored. It is now refused,
// and nothing is written: a mint that silently dropped a key would hand back a
// token that is not what the caller described.
func TestHandleMintUserToken_UnknownBodyFieldRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	for _, body := range []string{
		`{"tenant":"evil","scopes":["substrate:admin"]}`,
		`{"ttl":60}`,
	} {
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", body, acme, "subject", "alice"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	for _, tenant := range []string{"acme", "evil"} {
		rows, err := st.OperatorTokenDefListBySubject(context.Background(), tenant, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("%s/alice has %d token row(s) after refused mints, want 0", tenant, len(rows))
		}
	}
}

// ttl_seconds gives the token a fixed end, reported on the mint and in the
// listing. A mint without it has none, as before.
func TestHandleMintUserToken_TTLSetsExpiry(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	before := time.Now()
	resp := mintFor(t, s, acme, "alice", `{"ttl_seconds":3600,"name":"laptop-1"}`)
	if resp.ExpiresAt == nil {
		t.Fatal("mint with ttl_seconds returned no expires_at")
	}
	if d := resp.ExpiresAt.Sub(before); d < 3599*time.Second || d > 3601*time.Second {
		t.Errorf("expires_at is %s after the mint, want one hour", d)
	}
	if resp.Name != "u-laptop-1" {
		t.Errorf("name = %q, want the label asked for behind the user-token prefix", resp.Name)
	}
	plain := mintFor(t, s, acme, "alice", "")
	if plain.ExpiresAt != nil {
		t.Errorf("a mint with no body got expires_at %v, want none", plain.ExpiresAt)
	}

	rec := httptest.NewRecorder()
	s.handleListUserTokens(rec, bodyReq("GET", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	var list listUserTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]userTokenMeta{}
	for _, m := range list.Tokens {
		byID[m.DefID] = m
	}
	if m := byID[resp.DefID]; m.ExpiresAt == nil || !m.Active {
		t.Errorf("listing of the expiring token = %+v, want expires_at set and active", m)
	}
	if m := byID[plain.DefID]; m.ExpiresAt != nil {
		t.Errorf("listing of the plain token carries expires_at %v", m.ExpiresAt)
	}
}

// Bad lifetimes and names are refused before anything is minted.
func TestHandleMintUserToken_RefusesBadTTLNameAndMaxActive(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	for _, body := range []string{
		`{"ttl_seconds":0}`, `{"ttl_seconds":-5}`, `{"ttl_seconds":99999999999999}`,
		`{"name":"has space"}`, `{"name":"` + strings.Repeat("n", 63) + `"}`, `{"max_active":0}`,
	} {
		rec := httptest.NewRecorder()
		s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", body, acme, "subject", "alice"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	rows, _ := st.OperatorTokenDefListBySubject(context.Background(), "acme", "alice")
	if len(rows) != 0 {
		t.Errorf("%d token row(s) written by refused mints", len(rows))
	}
}

// With an operator maximum set, a longer lifetime is refused, and a mint that
// asks for none gets the maximum: leaving the field out must not be the way
// around the cap.
func TestHandleMintUserToken_MaxTTLCapsAndFillsIn(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.SetUserTokenMaxTTL(time.Hour)
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", `{"ttl_seconds":3601}`, acme, "subject", "alice"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "3600") {
		t.Errorf("ttl over the maximum: status %d, want 400 naming the maximum: %s", rec.Code, rec.Body.String())
	}

	before := time.Now()
	for _, body := range []string{"", `{}`, `{"name":"x"}`} {
		resp := mintFor(t, s, acme, "alice", body)
		if resp.ExpiresAt == nil {
			t.Errorf("body %q under a maximum: no expires_at, so the token never expires", body)
			continue
		}
		if d := resp.ExpiresAt.Sub(before); d < 3599*time.Second || d > 3602*time.Second {
			t.Errorf("body %q: expires %s after the mint, want the one-hour maximum", body, d)
		}
	}
	if resp := mintFor(t, s, acme, "alice", `{"ttl_seconds":60}`); resp.ExpiresAt == nil || resp.ExpiresAt.Sub(before) > 2*time.Minute {
		t.Errorf("a shorter lifetime under the maximum was not honoured: %v", resp.ExpiresAt)
	}
}

// max_active keeps the newest N of the user's tokens alive and retires the
// rest at once. Another user's tokens, and a privileged token that shares the
// subject, are not touched.
func TestHandleMintUserToken_MaxActiveRetiresTheOldest(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	seedUser(t, st, "acme", "bob", "isolated", "active")

	bob := mintFor(t, s, acme, "bob", "")
	// A tenant-operator token that happens to name alice as its subject.
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_priv", Name: "priv", TenantID: "acme", Subject: "alice",
		TokenHash: "h-priv", AllowedScopes: []string{auth.ScopeTenant},
	}); err != nil {
		t.Fatal(err)
	}

	var minted []mintUserTokenResponse
	for i := 0; i < 3; i++ {
		minted = append(minted, mintFor(t, s, acme, "alice", ""))
		time.Sleep(2 * time.Millisecond) // distinct created_at
	}
	last := mintFor(t, s, acme, "alice", `{"max_active":2}`)
	if len(last.Retired) != 2 {
		t.Fatalf("retired = %v, want the two oldest", last.Retired)
	}
	wantRetired := map[string]bool{minted[0].DefID: true, minted[1].DefID: true}
	for _, id := range last.Retired {
		if !wantRetired[id] {
			t.Errorf("retired %s, which is not one of the two oldest", id)
		}
	}
	for i, m := range append(minted, last) {
		_, ok := s.resolvePrincipal(ctx, m.Token)
		if want := i >= 2; ok != want {
			t.Errorf("token %d authenticates = %v, want %v", i, ok, want)
		}
	}
	if _, ok := s.resolvePrincipal(ctx, bob.Token); !ok {
		t.Error("another user's token was retired")
	}
	if row, err := st.OperatorTokenDefGet(ctx, "def_priv"); err != nil || !row.RetiredAt.IsZero() {
		t.Errorf("the privileged token sharing the subject was retired (err=%v retired_at=%v)", err, row.RetiredAt)
	}
}

// An expired token is refused, and refused exactly as an unknown one is.
func TestResolvePrincipal_ExpiredTokenRefused(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	mk := func(id, plaintext string, expires time.Time) {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: id, Name: id, TenantID: "acme", Subject: "alice",
			TokenHash:     auth.HashToken(testTokenPepper, plaintext),
			AllowedScopes: []string{auth.ScopeUser}, ExpiresAt: expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("def_live", "tok-live", time.Now().Add(time.Hour))
	mk("def_dead", "tok-dead", time.Now().Add(-time.Second))
	if _, ok := s.resolvePrincipal(ctx, "tok-live"); !ok {
		t.Error("a token inside its lifetime was refused")
	}
	if _, ok := s.resolvePrincipal(ctx, "tok-dead"); ok {
		t.Error("an expired token authenticated")
	}
}

// The case the cache makes hard: a token that expires while its resolution is
// cached. With a 30s cache it must still stop at its own end.
func TestResolvePrincipal_ExpiryHoldsThroughAWarmCache(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(30 * time.Second)
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_short", Name: "short", TenantID: "acme", Subject: "alice",
		TokenHash:     auth.HashToken(testTokenPepper, "tok-short"),
		AllowedScopes: []string{auth.ScopeUser}, ExpiresAt: time.Now().Add(300 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(ctx, "tok-short"); !ok {
		t.Fatal("the token was refused before its end")
	}
	time.Sleep(400 * time.Millisecond)
	if _, ok := s.resolvePrincipal(ctx, "tok-short"); ok {
		t.Error("the token authenticated after its end: the cached resolution outlived it")
	}
}

// The same hole existed for a rotated-out token: nothing flushes the cache
// when a grace window ends, so it kept authenticating for up to one cache TTL.
func TestResolvePrincipal_RotationGraceEndsThroughAWarmCache(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.EnableTokenCache(30 * time.Second)
	ctx := context.Background()
	seedToken(t, st, "tok-grace", "acme", "alice", []string{auth.ScopeUser}, time.Now().Add(300*time.Millisecond))
	if _, ok := s.resolvePrincipal(ctx, "tok-grace"); !ok {
		t.Fatal("the token was refused inside its grace window")
	}
	time.Sleep(400 * time.Millisecond)
	if _, ok := s.resolvePrincipal(ctx, "tok-grace"); ok {
		t.Error("the token authenticated after its grace window ended")
	}
}

// A token's name is one namespace for the whole deployment, and the
// operator-token tool acts on "the name's current token". A tenant operator
// who could name a user token freely could take an operator's token name, so
// that the operator's next rotate or retire by name landed on the tenant's
// token. Every name this route writes stays behind its own prefix.
func TestHandleMintUserToken_NameCannotTakeAnOperatorTokensName(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_ops", Name: "ops", TenantID: "root", Subject: "ops",
		TokenHash: "h-ops", AllowedScopes: []string{auth.ScopeAdmin},
	}); err != nil {
		t.Fatal(err)
	}
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	time.Sleep(2 * time.Millisecond) // the user token is the newer row

	resp := mintFor(t, s, acme, "alice", `{"name":"ops"}`)
	if resp.Name != "u-ops" {
		t.Errorf("stored name = %q, want u-ops", resp.Name)
	}
	cur, err := st.OperatorTokenDefGetCurrentByName(ctx, "ops")
	if err != nil || cur.DefID != "def_ops" {
		t.Errorf("the current token named \"ops\" is %q (err=%v), want the operator's def_ops", cur.DefID, err)
	}
}

// A token must not be able to mint one that outlives it: with an expiring
// tenant-operator token, a longer lifetime is refused and a mint that asks for
// none ends when the minter does.
func TestHandleMintUserToken_CannotOutliveTheMintingToken(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	minterEnds := time.Now().Add(10 * time.Minute)
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_minter", Name: "acme-ops", TenantID: "acme", Subject: "ops",
		TokenHash: "h-minter", AllowedScopes: []string{auth.ScopeTenant}, ExpiresAt: minterEnds,
	}); err != nil {
		t.Fatal(err)
	}
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}, TokenDefID: "def_minter"}
	seedUser(t, st, "acme", "alice", "isolated", "active")

	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", `{"ttl_seconds":3600}`, acme, "subject", "alice"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "outlive") {
		t.Errorf("a lifetime past the minter's end: status %d, want 400: %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{"", `{"name":"x"}`} {
		resp := mintFor(t, s, acme, "alice", body)
		if resp.ExpiresAt == nil {
			t.Errorf("body %q: the minted token never expires, though its minter does", body)
			continue
		}
		if d := resp.ExpiresAt.Sub(minterEnds); d < -time.Second || d > time.Second {
			t.Errorf("body %q: expires %v, want the minter's end %v", body, resp.ExpiresAt, minterEnds)
		}
	}
	if resp := mintFor(t, s, acme, "alice", `{"ttl_seconds":60}`); resp.ExpiresAt == nil || resp.ExpiresAt.After(minterEnds) {
		t.Errorf("a lifetime inside the minter's was not honoured: %v", resp.ExpiresAt)
	}

	// A minter with no end of its own bounds nothing.
	free := auth.Principal{TenantID: "acme", Subject: "ops2", Scopes: []string{auth.ScopeTenant}}
	if resp := mintFor(t, s, free, "alice", ""); resp.ExpiresAt != nil {
		t.Errorf("an unbounded minter minted a token ending %v", resp.ExpiresAt)
	}
}

// A principal that names a token row the store does not have is refused: not
// being able to learn how long the minter has must not read as "forever".
func TestHandleMintUserToken_UnreadableMintingTokenRefuses(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ghost := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}, TokenDefID: "def_gone"}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", ghost, "subject", "alice"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if rows, _ := st.OperatorTokenDefListBySubject(context.Background(), "acme", "alice"); len(rows) != 0 {
		t.Errorf("%d token(s) minted by a principal whose own token could not be read", len(rows))
	}
}
