package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY §8 / D4: which tokens are minted inherit. The default flip is a
// release decision, so it sits behind LOOMCYCLE_TOKEN_AUTHORITY_INHERIT; with
// it off a mint is what it was before this mode existed.

var tenantOp = auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}

// rawMint mints for alice with body and returns the status, the decoded JSON
// object and the stored row (zero when nothing was stored).
func rawMint(t *testing.T, s *Server, st store.Store, body string) (int, map[string]any, store.OperatorTokenDefRow) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", body, tenantOp, "subject", "alice"))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var row store.OperatorTokenDefRow
	if id, _ := out["def_id"].(string); id != "" {
		var err error
		if row, err = st.OperatorTokenDefGet(context.Background(), id); err != nil {
			t.Fatalf("read minted row: %v", err)
		}
	}
	return rec.Code, out, row
}

// The flag off and no authority asked: the row and the response are what a
// mint wrote before the mode existed — frozen, and no new response key.
func TestMintUserToken_FlagOffMintsFrozenWithThePreviousResponseShape(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedUser(t, st, "acme", "alice", "isolated", "active")
	code, out, row := rawMint(t, s, st, "")
	if code != http.StatusCreated {
		t.Fatalf("mint status %d: %v", code, out)
	}
	if row.Authority != "" {
		t.Errorf("stored authority = %q, want frozen (\"\")", row.Authority)
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"created_at", "def_id", "name", "scopes", "token", "token_suffix", "warning"}
	if !slices.Equal(keys, want) {
		t.Errorf("response keys = %v, want exactly %v", keys, want)
	}
	rec := httptest.NewRecorder()
	s.handleListUserTokens(rec, bodyReq("GET", "/v1/_users/alice/tokens", "", tenantOp, "subject", "alice"))
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Tokens) != 1 {
		t.Fatalf("listing %s: %v", rec.Body.String(), err)
	}
	if _, has := list.Tokens[0]["authority"]; has {
		t.Errorf("a frozen token's listing carries authority: %v", list.Tokens[0])
	}
}

// The flag on: a tenant's user-token mint is inherit, says so, and the token
// it hands back follows its user's promotion — the mint crossed into
// resolution.
func TestMintUserToken_FlagOnMintsInheritThatFollowsAPromotion(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.cfg().Env.TokenAuthorityInherit = true
	seedUser(t, st, "acme", "alice", "isolated", "active")
	code, out, row := rawMint(t, s, st, "")
	if code != http.StatusCreated || row.Authority != auth.AuthorityInherit || out["authority"] != auth.AuthorityInherit {
		t.Fatalf("mint: status %d stored %q response %v, want inherit", code, row.Authority, out["authority"])
	}
	tok, _ := out["token"].(string)
	setAccessMode(t, st, "tenant")
	p, ok := s.resolvePrincipal(context.Background(), tok)
	if !ok || !sameScopes(p.Scopes, grantFor(t, "tenant")) {
		t.Fatalf("minted inherit token after promotion: %v ok=%v, want the tenant grant", p.Scopes, ok)
	}
	rec := httptest.NewRecorder()
	s.handleListUserTokens(rec, bodyReq("GET", "/v1/_users/alice/tokens", "", tenantOp, "subject", "alice"))
	var list listUserTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].Authority != auth.AuthorityInherit {
		t.Errorf("listing = %+v, want the token marked inherit", list.Tokens)
	}
}

// A mint may ask for either mode whatever the default; anything else is
// refused and nothing is written.
func TestMintUserToken_ExplicitAuthorityOverridesTheDefault(t *testing.T) {
	for _, tc := range []struct {
		flag bool
		body string
		want string
	}{
		{false, `{"authority":"inherit"}`, auth.AuthorityInherit},
		{true, `{"authority":"frozen"}`, ""},
		{true, `{}`, auth.AuthorityInherit},
	} {
		s, st := tokenAuthServer(t, "legacy")
		s.cfg().Env.TokenAuthorityInherit = tc.flag
		seedUser(t, st, "acme", "alice", "isolated", "active")
		code, out, row := rawMint(t, s, st, tc.body)
		if code != http.StatusCreated || row.Authority != tc.want {
			// Never the whole response: a 201 carries the minted bearer.
			t.Errorf("flag=%v body %s: status %d stored %q, want %q (%v)", tc.flag, tc.body, code, row.Authority, tc.want, out["error"])
		}
	}
	s, st := tokenAuthServer(t, "legacy")
	seedUser(t, st, "acme", "alice", "isolated", "active")
	for _, body := range []string{`{"authority":"all"}`, `{"authority":"Inherit"}`, `{"scopes":"inherit"}`} {
		if code, out, _ := rawMint(t, s, st, body); code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400 (%v)", body, code, out)
		}
	}
	if rows, _ := st.OperatorTokenDefListBySubject(context.Background(), "acme", "alice"); len(rows) != 0 {
		t.Errorf("%d token row(s) written by refused mints", len(rows))
	}
}

// Every gate-issued token (password, OIDC, OAuth, external gates and the
// bridges behind them all mint through issueGateToken) follows the flag.
func TestIssueGateToken_AuthorityFollowsTheFlag(t *testing.T) {
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}
	s, st, _ := identitySessionServer(t, m)
	ctx := context.Background()
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:1", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	_, row, err := s.issueGateToken(ctx, "acme", "google:1", "google", 0, auth.Restrictions{})
	if err != nil || row.Authority != "" {
		t.Fatalf("flag off: authority %q, %v; want frozen", row.Authority, err)
	}
	s.cfg().Env.TokenAuthorityInherit = true
	tok, row, err := s.issueGateToken(ctx, "acme", "google:1", "google", 0, auth.Restrictions{})
	if err != nil || row.Authority != auth.AuthorityInherit {
		t.Fatalf("flag on: authority %q, %v; want inherit", row.Authority, err)
	}
	mode := "tenant"
	if err := st.UserUpdate(ctx, "acme", "google:1", store.UserPatch{AccessMode: &mode}); err != nil {
		t.Fatal(err)
	}
	p, ok := s.resolvePrincipal(ctx, tok)
	if !ok || !sameScopes(p.Scopes, grantFor(t, "tenant")) || p.IssuedByGate != "google" {
		t.Fatalf("issued inherit token after promotion: %+v ok=%v", p, ok)
	}
}
