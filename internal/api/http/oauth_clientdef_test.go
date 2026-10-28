package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// RFC FL §5.10 and §5.11: runtime clients (ClientDef) and the tenant fence.

func clientOperatorCtx(tenant, subject string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeTenant}})
}

func clientAdminCtx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "default", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
}

func boardClient(redirects ...string) map[string]any {
	if len(redirects) == 0 {
		redirects = []string{"https://acme-board.example/cb"}
	}
	return map[string]any{"kind": "public", "redirect_uris": redirects, "allowed_origins": []string{"https://acme-board.example"}}
}

// clientDefCall runs one op and returns the result and its decoded body.
func clientDefCall(t *testing.T, s *Server, ctx context.Context, in map[string]any) (bool, map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := s.ClientDef(ctx, raw)
	if err != nil {
		t.Fatalf("ClientDef(%v): %v", in, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Text), &body)
	return !res.IsError, body, res.Text
}

func mustClientDef(t *testing.T, s *Server, ctx context.Context, in map[string]any) map[string]any {
	t.Helper()
	ok, body, text := clientDefCall(t, s, ctx, in)
	if !ok {
		t.Fatalf("ClientDef(%v) refused: %s", in, text)
	}
	return body
}

// §5.10: a tenant operator registers for its own tenant only, and cannot see,
// change or retire another tenant's client, nor any config client.
func TestClientDef_TenantOperatorIsConfinedToItsOwnTenant(t *testing.T) {
	f := newAppFixture(t, "tenant")
	acme, globex := clientOperatorCtx("acme", "op-a"), clientOperatorCtx("globex", "op-g")
	got := mustClientDef(t, f.s, acme, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	if got["tenant_id"] != "acme" || got["version"] != float64(1) || got["created_by"] != "op-a" {
		t.Fatalf("created = %v", got)
	}
	// Another tenant: not found, not listed, not alterable.
	for _, op := range []map[string]any{
		{"op": "get", "name": "acme-board"},
		{"op": "update", "name": "acme-board", "client": boardClient("https://evil.example/cb")},
		{"op": "retire", "name": "acme-board"},
	} {
		ok, _, text := clientDefCall(t, f.s, globex, op)
		if ok || !strings.Contains(text, "not found") {
			t.Errorf("globex %v = %v %s, want an opaque not-found", op["op"], ok, text)
		}
	}
	_, body, _ := clientDefCall(t, f.s, globex, map[string]any{"op": "list"})
	for _, c := range body["clients"].([]any) {
		if c.(map[string]any)["name"] == "acme-board" {
			t.Fatal("globex lists acme's client")
		}
	}
	// It cannot take the name either, and the refusal names no tenant.
	ok, _, text := clientDefCall(t, f.s, globex, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	if ok || strings.Contains(text, "acme") && !strings.Contains(text, "acme-board") {
		t.Fatalf("globex create of a taken name = %v %s", ok, text)
	}
	// A tenant operator cannot register for another tenant.
	if ok, _, _ := clientDefCall(t, f.s, globex, map[string]any{"op": "create", "name": "g2", "tenant": "acme", "client": boardClient()}); ok {
		t.Fatal("globex registered a client for acme")
	}
	// Nobody alters a config client through the API, an admin included.
	for _, ctx := range []context.Context{acme, clientAdminCtx()} {
		for _, op := range []string{"create", "update", "retire"} {
			in := map[string]any{"op": op, "name": "board", "client": boardClient()}
			if ok, _, text := clientDefCall(t, f.s, ctx, in); ok || !strings.Contains(text, "config") {
				t.Errorf("%s of the config client = %v %s", op, ok, text)
			}
		}
	}
	// A member (no substrate:tenant) registers nothing.
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	if ok, _, _ := clientDefCall(t, f.s, member, map[string]any{"op": "create", "name": "m-app", "client": boardClient()}); ok {
		t.Fatal("a member registered a client")
	}
}

// §5.10: a redirect URI outside the allowed forms is refused at write.
func TestClientDef_RefusesARedirectOutsideTheAllowedForms(t *testing.T) {
	f := newAppFixture(t, "tenant")
	for _, uri := range []string{"http://evil.example/cb", "https://x.example/cb#f", "https://*.example/cb", "javascript:alert(1)"} {
		ok, _, text := clientDefCall(t, f.s, clientOperatorCtx("acme", "op"), map[string]any{"op": "create", "name": "bad", "client": boardClient(uri)})
		if ok || !strings.Contains(text, "redirect_uris") {
			t.Errorf("%q = %v %s", uri, ok, text)
		}
	}
	if list, _ := f.st.ClientDefList(context.Background()); len(list) != 0 {
		t.Fatalf("a refused client was stored: %+v", list)
	}
}

// §5.10: every write is audited with its author and its redirect URIs.
func TestClientDef_EveryWriteIsAuditedWithItsAuthor(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := clientOperatorCtx("acme", "op-a")
	mustClientDef(t, f.s, ctx, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	mustClientDef(t, f.s, ctx, map[string]any{"op": "update", "name": "acme-board", "client": boardClient("https://acme-board.example/cb2")})
	mustClientDef(t, f.s, ctx, map[string]any{"op": "retire", "name": "acme-board"})
	for action, uri := range map[string]string{"client_def_created": "https://acme-board.example/cb",
		"client_def_changed": "https://acme-board.example/cb2", "client_def_retired": "https://acme-board.example/cb2"} {
		ev := lastEvent(t, f.sink, action)
		if ev.ActorSubject != "op-a" || ev.ActorTenant != "acme" || ev.TargetTenant != "acme" || ev.AuthClient != "acme-board" ||
			len(ev.AuthRedirectURIs) != 1 || ev.AuthRedirectURIs[0] != uri {
			t.Errorf("%s = %+v", action, ev)
		}
	}
}

// signInThrough runs the flow through a client other than the fixture's.
func (f *appFixture) signInThrough(t *testing.T, client, redirect string) (string, string, string) {
	t.Helper()
	q := appAuthorizeParams(func(v url.Values) { v.Set("client_id", client); v.Set("redirect_uri", redirect) })
	rec := f.authorize(t, q, true)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code through %s: %d %s %s", client, rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	tok, body := f.exchange(t, code, func(v url.Values) { v.Set("client_id", client); v.Set("redirect_uri", redirect) })
	if tok.Code != http.StatusOK {
		t.Fatalf("exchange through %s = %d %v", client, tok.Code, body)
	}
	// A second, unused code for the retire to void.
	rec = f.authorize(t, q, true)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	return body["access_token"].(string), body["refresh_token"].(string), loc.Query().Get("code")
}

// §5.10: retiring a client voids its codes, revokes its refresh families and
// retires their access tokens — in the shared store, so on every replica.
func TestClientDef_RetireEndsEverySignInThroughIt(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := clientOperatorCtx("acme", "op-a")
	const cb = "https://acme-board.example/cb"
	mustClientDef(t, f.s, ctx, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	at, rt, unused := f.signInThrough(t, "acme-board", cb)
	if c, _ := f.bearerMe(t, at); c != http.StatusOK {
		t.Fatalf("token before retire = %d", c)
	}
	mustClientDef(t, f.s, ctx, map[string]any{"op": "retire", "name": "acme-board"})
	if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
		t.Fatalf("access token after retire = %d", c)
	}
	// Bring the name back: what the retired client issued stays dead.
	mustClientDef(t, f.s, ctx, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	refresh := func(tok string) int {
		rec, _ := f.post(t, "/v1/_oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {"acme-board"}})
		return rec.Code
	}
	if c := refresh(rt); c != http.StatusBadRequest {
		t.Fatalf("refresh after retire = %d", c)
	}
	rec, body := f.exchange(t, unused, func(v url.Values) { v.Set("client_id", "acme-board"); v.Set("redirect_uri", cb) })
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code issued before the retire = %d %v", rec.Code, body)
	}
	if fam, err := f.st.OAuthFamilyGet(context.Background(), lastEvent(t, f.sink, "oauth_token_issued").AuthRecordID); err != nil ||
		fam.RevokeReason != oauthRevokeClientRetired {
		t.Fatalf("family after retire = %+v %v", fam, err)
	}
}

// A change voids the client's unused codes at once.
func TestClientDef_UpdateVoidsUnusedCodes(t *testing.T) {
	f := newAppFixture(t, "tenant")
	ctx := clientOperatorCtx("acme", "op-a")
	const cb = "https://acme-board.example/cb"
	mustClientDef(t, f.s, ctx, map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	_, _, unused := f.signInThrough(t, "acme-board", cb)
	mustClientDef(t, f.s, ctx, map[string]any{"op": "update", "name": "acme-board", "client": boardClient(cb, "http://127.0.0.1/cb")})
	rec, body := f.exchange(t, unused, func(v url.Values) { v.Set("client_id", "acme-board"); v.Set("redirect_uri", cb) })
	if rec.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code from before the change = %d %v", rec.Code, body)
	}
	got := mustClientDef(t, f.s, ctx, map[string]any{"op": "get", "name": "acme-board"})
	if got["version"] != float64(2) {
		t.Fatalf("version after update = %v", got["version"])
	}
	v := mustClientDef(t, f.s, ctx, map[string]any{"op": "verify", "name": "acme-board", "content_sha256": got["content_sha256"]})
	if v["matches"] != true {
		t.Fatalf("verify = %v", v)
	}
}

// §5.11 (D10): a tenant's client signs in only that tenant's people — a user
// of another tenant who already holds a session ends on loomcycle's page with
// no code.
func TestOAuthAuthorize_TenantClientRefusesAnotherTenantsUser(t *testing.T) {
	f := newAppFixture(t, "tenant") // google:100 of tenant acme, signed in
	mustClientDef(t, f.s, clientOperatorCtx("globex", "op-g"), map[string]any{"op": "create", "name": "globex-board", "client": boardClient()})
	q := appAuthorizeParams(func(v url.Values) {
		v.Set("client_id", "globex-board")
		v.Set("redirect_uri", "https://acme-board.example/cb")
	})
	rec := f.authorize(t, q, true)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), "code=") {
		t.Fatalf("cross-tenant sign-in = %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if ev := lastEvent(t, f.sink, "oauth_refused"); ev.AuthReason != "tenant" || ev.AuthClient != "globex-board" {
		t.Fatalf("refusal audit = %+v", ev)
	}
	// An admin's every-tenant client serves acme's user.
	mustClientDef(t, f.s, clientAdminCtx(), map[string]any{"op": "create", "name": "shared-board", "client": boardClient()})
	at, _, _ := f.signInThrough(t, "shared-board", "https://acme-board.example/cb")
	if c, me := f.bearerMe(t, at); c != http.StatusOK || me["tenant_id"] != "acme" {
		t.Fatalf("every-tenant client = %d %v", c, me)
	}
}

// The token of a tenant client stops on use if the client stops serving its
// tenant (enforced in resolvePrincipal, not only at refresh).
func TestOAuthToken_TenantClientTokenIsEnforcedOnUse(t *testing.T) {
	f := newAppFixture(t, "tenant")
	mustClientDef(t, f.s, clientOperatorCtx("acme", "op-a"), map[string]any{"op": "create", "name": "acme-board", "client": boardClient()})
	at, _, _ := f.signInThrough(t, "acme-board", "https://acme-board.example/cb")
	// Retire behind the server's back (as another replica's retire would
	// look to this one before its cache flush arrives).
	if _, err := f.st.ClientDefRetire(context.Background(), "acme-board", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.s.tokenCache.flush()
	if c, _ := f.bearerMe(t, at); c != http.StatusUnauthorized {
		t.Fatalf("token of a retired client = %d", c)
	}
}

// The HTTP route: a tenant operator reaches it, a member does not.
func TestClientDefRoute_OperatorReachesItMemberDoesNot(t *testing.T) {
	f := newAppFixture(t, "tenant")
	seedToken(t, f.st, "lc_op_token_for_clientdef_route_test", "acme", "op", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, f.st, "lc_member_token_for_clientdef_route_t", "acme", "member", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	call := func(tok string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/_clientdef", strings.NewReader(`{"op":"list"}`))
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		f.s.authMiddleware(http.HandlerFunc(f.s.handleSubstrateClientDef)).ServeHTTP(rec, r)
		return rec.Code
	}
	if c := call("lc_op_token_for_clientdef_route_test"); c != http.StatusOK {
		t.Fatalf("operator = %d", c)
	}
	if c := call("lc_member_token_for_clientdef_route_t"); c != http.StatusForbidden {
		t.Fatalf("member = %d", c)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/_clientdef/names", nil)
	r.Header.Set("Authorization", "Bearer lc_op_token_for_clientdef_route_test")
	rec := httptest.NewRecorder()
	f.s.authMiddleware(http.HandlerFunc(f.s.handleListClientDefNames)).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"board"`) {
		t.Fatalf("names = %d %s", rec.Code, rec.Body.String())
	}
}
