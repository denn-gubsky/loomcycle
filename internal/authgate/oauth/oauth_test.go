package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/codeflow"
)

// fakeForge is a GitHub Enterprise-shaped provider: the OAuth endpoints and the
// /api/v3 user, e-mail, org-membership and team APIs.
type fakeForge struct {
	mu        sync.Mutex
	usedCodes map[string]bool
	verifier  string // the PKCE verifier the token request carried
	emails    []map[string]any
	orgs      []string
	teams     [][2]string // org, team slug
	apiCalls  []string
}

func newFakeForge() *fakeForge {
	return &fakeForge{
		usedCodes: map[string]bool{},
		emails: []map[string]any{
			{"email": "typed@example.com", "primary": false, "verified": false},
			{"email": "alice@acme.example", "primary": true, "verified": true},
		},
		orgs:  []string{"acme", "random-org"},
		teams: [][2]string{{"acme", "platform"}, {"random-org", "admins"}},
	}
}

func (f *fakeForge) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		code := r.PostForm.Get("code")
		f.verifier = r.PostForm.Get("code_verifier")
		// A code works once (RFC 6749 §4.1.2), and only with its verifier.
		if code != "good-code" || f.usedCodes[code] || f.verifier == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		f.usedCodes[code] = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_test_access","token_type":"bearer","scope":"read:user"}`))
	})
	api := func(path string, body func() any) {
		mux.HandleFunc("GET /api/v3"+path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_test_access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			f.apiCalls = append(f.apiCalls, path)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(body())
		})
	}
	api("/user", func() any {
		// The public profile e-mail is set and unverified: it must never be used.
		return map[string]any{"id": 4242, "login": "alice", "name": "Alice", "email": "public@evil.example"}
	})
	api("/user/emails", func() any { return f.emails })
	api("/user/memberships/orgs", func() any {
		var out []map[string]any
		for _, o := range f.orgs {
			out = append(out, map[string]any{"state": "active", "organization": map[string]any{"login": o}})
		}
		return out
	})
	api("/user/teams", func() any {
		var out []map[string]any
		for _, tm := range f.teams {
			out = append(out, map[string]any{"slug": tm[1], "organization": map[string]any{"login": tm[0]}})
		}
		return out
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func githubGate(t *testing.T, base string, allowed []string, requireOrg bool) *Gate {
	t.Helper()
	g, err := New(Config{Name: "gh", Preset: PresetGitHub, ClientID: "cid", ClientSecret: "csecret",
		BaseURL: base, AllowedOrgs: allowed, RequireOrg: requireOrg, TrustEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func startLogin(t *testing.T) (authgate.LoginState, string) {
	t.Helper()
	l, err := codeflow.NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	return codeflow.State(l.Secret, l.State, "https://lc.example/v1/_auth/complete/gh", false, time.Now()), l.State
}

func callback(state, code string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/_auth/complete/gh?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
}

func TestOAuthGitHub_UsesOnlyTheVerifiedPrimaryEmailAndTheNumericID(t *testing.T) {
	f := newFakeForge()
	srv := f.server(t)
	g := githubGate(t, srv.URL, nil, false)
	ls, state := startLogin(t)
	a, err := g.Complete(context.Background(), callback(state, "good-code"), ls)
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject != "4242" || a.Issuer != srv.URL || a.Email != "alice@acme.example" || !a.EmailVerified || a.DisplayName != "Alice" {
		t.Fatalf("assertion = %+v", a)
	}
	// PKCE: the verifier the token request carried is the login's.
	if f.verifier != ls.CodeVerifier {
		t.Fatal("the code exchange did not carry the login's PKCE verifier")
	}
	// Without a verified primary, there is no e-mail at all — never the
	// public profile address, never an unverified one.
	f.emails = []map[string]any{
		{"email": "unverified-primary@acme.example", "primary": true, "verified": false},
		{"email": "verified-secondary@acme.example", "primary": false, "verified": true},
	}
	f.usedCodes = map[string]bool{}
	ls, state = startLogin(t)
	a, err = g.Complete(context.Background(), callback(state, "good-code"), ls)
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "" || a.EmailVerified || a.Claims["email"] != nil {
		t.Fatalf("an unverified GitHub e-mail was used: %+v", a)
	}
}

// An organisation the gate is not configured to trust never becomes a group,
// and with no allowlist the gate asserts no groups (and does not even read
// the memberships).
func TestOAuthGitHub_OnlyAllowedOrgsBecomeGroups(t *testing.T) {
	f := newFakeForge()
	srv := f.server(t)
	ls, state := startLogin(t)
	a, err := githubGate(t, srv.URL, []string{"ACME"}, false).Complete(context.Background(), callback(state, "good-code"), ls)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "acme/platform"}; !reflect.DeepEqual(a.Groups, want) {
		t.Fatalf("groups = %v, want %v", a.Groups, want)
	}
	for _, grp := range a.Groups {
		if strings.HasPrefix(grp, "random-org") {
			t.Fatal("an untrusted org became a group")
		}
	}

	f.usedCodes, f.apiCalls = map[string]bool{}, nil
	ls, state = startLogin(t)
	a, err = githubGate(t, srv.URL, nil, false).Complete(context.Background(), callback(state, "good-code"), ls)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Groups) != 0 {
		t.Fatalf("a gate with no allowlist asserted groups %v", a.Groups)
	}
	for _, c := range f.apiCalls {
		if strings.Contains(c, "memberships") || strings.Contains(c, "teams") {
			t.Fatalf("read %s with no allowlist", c)
		}
	}
}

func TestOAuthGitHub_RequireOrgRefusesNonMembers(t *testing.T) {
	f := newFakeForge()
	f.orgs, f.teams = []string{"random-org"}, nil
	srv := f.server(t)
	ls, state := startLogin(t)
	_, err := githubGate(t, srv.URL, []string{"acme"}, true).Complete(context.Background(), callback(state, "good-code"), ls)
	if !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("non-member: err = %v", err)
	}
}

// OAuth state mismatch and a replayed code are refused; a mismatched state
// never reaches the token endpoint.
func TestOAuth_StateMismatchAndReplayedCodeAreRefused(t *testing.T) {
	f := newFakeForge()
	srv := f.server(t)
	g := githubGate(t, srv.URL, nil, false)
	ls, _ := startLogin(t)
	other, _ := startLogin(t)
	if _, err := g.Complete(context.Background(), callback(other.State, "good-code"), ls); !errors.Is(err, codeflow.ErrStateMismatch) {
		t.Fatalf("state mismatch: err = %v", err)
	}
	if _, err := g.Complete(context.Background(), callback("", "good-code"), ls); !errors.Is(err, codeflow.ErrStateMismatch) {
		t.Fatalf("missing state: err = %v", err)
	}
	if f.usedCodes["good-code"] {
		t.Fatal("a callback with the wrong state reached the token endpoint")
	}
	if _, err := g.Complete(context.Background(), callback(ls.State, "good-code"), ls); err != nil {
		t.Fatalf("the real callback: %v", err)
	}
	if _, err := g.Complete(context.Background(), callback(ls.State, "good-code"), ls); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replayed code: err = %v", err)
	}
}

func TestOAuth_BeginRedirectsWithPKCEAndState(t *testing.T) {
	g := githubGate(t, "", nil, false)
	ls, state := startLogin(t)
	rec := httptest.NewRecorder()
	if err := g.Begin(context.Background(), rec, httptest.NewRequest(http.MethodGet, "/v1/_auth/login/gh", nil), ls); err != nil {
		t.Fatal(err)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "github.com" || loc.Path != "/login/oauth/authorize" {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	q := loc.Query()
	sum := sha256.Sum256([]byte(ls.CodeVerifier))
	if q.Get("state") != state || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("client_id") != "cid" ||
		!strings.Contains(q.Get("scope"), "read:org") || q.Get("redirect_uri") != ls.RedirectURI {
		t.Fatalf("authorize query = %v", q)
	}
	if strings.Contains(loc.String(), "csecret") {
		t.Fatal("the client secret reached the browser")
	}
}

func TestOAuthGeneric_EmailNeedsAJSONTrueVerifiedClaim(t *testing.T) {
	var info map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(info) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g, err := New(Config{Name: "sso", Preset: PresetGeneric, ClientID: "c", ClientSecret: "s",
		AuthURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", UserinfoURL: srv.URL + "/userinfo",
		Claims: ClaimMap{Groups: "roles"}, AllowedOrgs: []string{"staff"}, TrustEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		verified any
		want     string
	}{{true, "a@x.example"}, {"true", ""}, {nil, ""}, {false, ""}} {
		info = map[string]any{"sub": "u1", "email": "a@x.example", "roles": []any{"staff", "root"}}
		if tc.verified != nil {
			info["email_verified"] = tc.verified
		}
		p, err := g.genericProfile(context.Background(), "tok")
		if err != nil {
			t.Fatal(err)
		}
		a, err := g.assertion(p)
		if err != nil || a.Email != tc.want || a.Subject != "u1" {
			t.Fatalf("email_verified=%v: assertion %+v err %v", tc.verified, a, err)
		}
		if !reflect.DeepEqual(a.Groups, []string{"staff"}) {
			t.Fatalf("groups = %v (an unlisted role became a group)", a.Groups)
		}
	}
}

func TestOAuthNew_RefusesInsecureOrIncompleteProviders(t *testing.T) {
	for _, cfg := range []Config{
		{Preset: PresetGitHub, ClientID: "c"},                                                   // no secret
		{Preset: PresetGitHub, ClientID: "c", ClientSecret: "s", BaseURL: "http://ghe.example"}, // plain http off loopback
		{Preset: PresetGitea, ClientID: "c", ClientSecret: "s"},                                 // gitea needs base_url
		{Preset: PresetGeneric, ClientID: "c", ClientSecret: "s", AuthURL: "https://a/x"},       // missing endpoints
		{Preset: "okta", ClientID: "c", ClientSecret: "s"},
		{Preset: PresetGitHub, ClientID: "c", ClientSecret: "s", RequireOrg: true}, // require_org with no orgs
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) accepted", cfg)
		}
	}
}
