package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P5 — the OAuth gate end to end through P1's redirect-login routes,
// against a GitHub Enterprise-shaped provider.

const oauthTestRedirect = "https://lc.example/v1/_auth/complete/gh"

type fakeGitHub struct {
	mu    sync.Mutex
	used  map[string]bool
	srv   *httptest.Server
	email map[string]any
	orgs  []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{used: map[string]bool{}, orgs: []string{"acme", "random-org"},
		email: map[string]any{"email": "alice@acme.example", "primary": true, "verified": true}}
	mux := http.NewServeMux()
	// The person "signs in" and the provider sends the browser back.
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "pkce required", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=c-"+q.Get("state")[:8]+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		code := r.PostForm.Get("code")
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(code, "c-") || f.used[code] || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		f.used[code] = true
		_, _ = w.Write([]byte(`{"access_token":"gho_x","token_type":"bearer"}`))
	})
	api := func(path string, body func() any) {
		mux.HandleFunc("GET /api/v3"+path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_x" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(body())
		})
	}
	api("/user", func() any { return map[string]any{"id": 4242, "login": "alice", "name": "Alice"} })
	api("/user/emails", func() any { return []any{f.email} })
	api("/user/memberships/orgs", func() any {
		var out []any
		for _, o := range f.orgs {
			out = append(out, map[string]any{"state": "active", "organization": map[string]any{"login": o}})
		}
		return out
	})
	api("/user/teams", func() any {
		return []any{map[string]any{"slug": "platform", "organization": map[string]any{"login": "acme"}},
			map[string]any{"slug": "admins", "organization": map[string]any{"login": "random-org"}}}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newOAuthFixture is newOIDCFixture's shape with an OAuth gate "gh".
func newOAuthFixture(t *testing.T, m config.AuthIdentityMap, allowed []string) (*oidcFixture, *fakeGitHub) {
	t.Helper()
	gh := newFakeGitHub(t)
	s, st, sink := sessionTestServer(t, "legacy")
	g := identityGateCfg("gh", m)
	g.Kind = config.AuthGateKindOAuth
	g.OAuth = &config.AuthOAuthGate{Preset: config.OAuthPresetGitHub, ClientID: "cid", ClientSecret: "csecret",
		BaseURL: gh.srv.URL, AllowedOrgs: allowed, TrustEmail: true, RedirectURI: oauthTestRedirect}
	s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, g)
	s.cfg().Env.PublicURL = "https://lc.example"
	s.oidcHTTP = gh.srv.Client()
	return &oidcFixture{s: s, st: st, sink: sink}, gh
}

func (f *oidcFixture) oauthLogin(t *testing.T) (*httptest.ResponseRecorder, string, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_auth/login/gh", nil)
	r.SetPathValue("gate", "gh")
	r.Header.Set("User-Agent", testUA)
	rec := httptest.NewRecorder()
	f.s.handleRedirectLogin(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	lc := cookieNamed(rec, loginCookiePlain)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb := strings.TrimPrefix(resp.Header.Get("Location"), oauthTestRedirect)
	return f.oauthComplete(t, cb, lc), cb, lc
}

func (f *oidcFixture) oauthComplete(t *testing.T, query string, lc *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/_auth/complete/gh"+query, nil)
	r.SetPathValue("gate", "gh")
	r.Header.Set("User-Agent", testUA)
	if lc != nil {
		r.AddCookie(lc)
	}
	rec := httptest.NewRecorder()
	f.s.handleRedirectComplete(rec, r)
	return rec
}

// A GitHub login provisions the user under its immutable id, links the
// identity, starts a session and syncs only the allowed org's groups.
func TestOAuthLogin_GitHubProvisionsSyncsAllowedOrgGroupsAndStartsASession(t *testing.T) {
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto, Trusted: true, SyncGroups: true, GroupsProvision: true}
	f, _ := newOAuthFixture(t, m, []string{"acme"})
	rec, _, _ := f.oauthLogin(t)
	sc := cookieNamed(rec, sessionCookiePlain)
	if rec.Code != http.StatusSeeOther || sc == nil {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	code, me := meOf(t, f.s, sc)
	if code != http.StatusOK || me["subject"] != "gh:4242" || me["tenant_id"] != "acme" {
		t.Fatalf("me = %d %v", code, me)
	}
	ctx := context.Background()
	id, err := f.st.UserIdentityListSubject(ctx, "acme", "gh:4242")
	if err != nil || len(id) != 1 || id[0].Email != "alice@acme.example" || !id[0].EmailVerified {
		t.Fatalf("identity = %+v %v", id, err)
	}
	groups, err := f.st.GroupsOfSubject(ctx, "acme", "gh:4242")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.Contains(n, "random-org") {
			t.Fatalf("an untrusted org became a group: %v", names)
		}
	}
	if !containsGate(names, "acme") || !containsGate(names, "acme/platform") {
		t.Fatalf("groups = %v, want acme and acme/platform", names)
	}
}

// An unverified primary e-mail is never recorded on the identity.
func TestOAuthLogin_UnverifiedEmailIsNotRecorded(t *testing.T) {
	f, gh := newOAuthFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	gh.email = map[string]any{"email": "spoof@acme.example", "primary": true, "verified": false}
	if rec, _, _ := f.oauthLogin(t); cookieNamed(rec, sessionCookiePlain) == nil {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	id, _ := f.st.UserIdentityListSubject(context.Background(), "acme", "gh:4242")
	if len(id) != 1 || id[0].Email != "" || id[0].EmailVerified {
		t.Fatalf("identity = %+v", id)
	}
}

// The callback without the browser's login cookie, and a replayed callback,
// are refused and create no session.
func TestOAuthLogin_ForeignOrReplayedCallbackIsRefused(t *testing.T) {
	f, _ := newOAuthFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	rec, cb, lc := f.oauthLogin(t)
	if cookieNamed(rec, sessionCookiePlain) == nil {
		t.Fatal("precondition: the first callback signs in")
	}
	for name, c := range map[string]*http.Cookie{"replayed": lc, "no login cookie": nil} {
		again := f.oauthComplete(t, cb, c)
		if sc := cookieNamed(again, sessionCookiePlain); sc != nil && sc.Value != "" {
			t.Fatalf("%s callback started a session", name)
		}
		if loginErrorCode(t, again) == "" {
			t.Fatalf("%s callback: no error code", name)
		}
	}
}

// A tenant under a gate policy that does not list the OAuth gate refuses the
// session, whatever the provider said.
func TestOAuthLogin_TenantPolicyWithoutTheGateRefusesTheSession(t *testing.T) {
	f, _ := newOAuthFixture(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}, nil)
	f.s.cfg().ResolvedAuth.TenantPolicies = map[string][]string{"acme": {"local"}}
	rec, _, _ := f.oauthLogin(t)
	if sc := cookieNamed(rec, sessionCookiePlain); sc != nil && sc.Value != "" {
		t.Fatal("a session was established through a gate the tenant policy does not list")
	}
	if n := sessionRows(t, f.st); n != 0 {
		t.Fatalf("%d session rows", n)
	}
	_ = store.UISessionRow{}
}
