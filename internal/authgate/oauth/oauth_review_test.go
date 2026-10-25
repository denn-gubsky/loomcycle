package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A numeric user id beyond 2^53 must not round: two people whose ids differ
// only past float64 precision would otherwise share a subject.
func TestOAuthGeneric_LargeNumericIDsStayDistinct(t *testing.T) {
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g, err := New(Config{Name: "sso", Preset: PresetGeneric, ClientID: "c", ClientSecret: "s",
		AuthURL: srv.URL + "/a", TokenURL: srv.URL + "/t", UserinfoURL: srv.URL + "/userinfo"})
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for _, id := range []string{"9007199254740993", "9007199254740992"} {
		body = `{"sub":` + id + `}`
		p, err := g.genericProfile(context.Background(), "tok")
		if err != nil {
			t.Fatal(err)
		}
		if p.id != id {
			t.Fatalf("id %s read as %q", id, p.id)
		}
		subjects[p.id] = true
	}
	if len(subjects) != 2 {
		t.Fatal("two large ids collided")
	}
	for _, bad := range []string{`{"sub":1.5}`, `{"sub":1e30}`, `{"sub":-0.5}`} {
		body = bad
		if p, _ := g.genericProfile(context.Background(), "tok"); p.id != "" {
			t.Fatalf("%s accepted as id %q", bad, p.id)
		}
	}
}

// fakeGitLab answers /api/v4/user (account confirmed) and /api/v4/user/emails.
func fakeGitLab(t *testing.T, emails []map[string]any) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "al", "name": "Al", "email": "al@acme.example",
			"confirmed_at": "2020-01-01T00:00:00Z", "state": "active"})
	})
	mux.HandleFunc("GET /api/v4/user/emails", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(emails) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// GitLab's account-level confirmed_at does not say the CURRENT primary
// address was confirmed: only /user/emails with the address's own
// confirmed_at does.
func TestOAuthGitLab_PrimaryEmailNeedsItsOwnConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		emails []map[string]any
		want   string
	}{
		{"confirmed address", []map[string]any{{"email": "al@acme.example", "confirmed_at": "2021-02-02T00:00:00Z"}}, "al@acme.example"},
		{"unconfirmed address", []map[string]any{{"email": "al@acme.example", "confirmed_at": nil}}, ""},
		{"not listed", []map[string]any{{"email": "other@acme.example", "confirmed_at": "2021-02-02T00:00:00Z"}}, ""},
	} {
		srv := fakeGitLab(t, tc.emails)
		g, err := New(Config{Name: "gl", Preset: PresetGitLab, ClientID: "c", ClientSecret: "s", BaseURL: srv.URL, TrustEmail: true})
		if err != nil {
			t.Fatal(err)
		}
		p, err := g.gitlabProfile(context.Background(), srv.URL, "tok")
		if err != nil {
			t.Fatal(err)
		}
		if p.email != tc.want {
			t.Fatalf("%s: email = %q, want %q", tc.name, p.email, tc.want)
		}
	}
}

// trust_email off (the default for self-managed and generic providers): no
// e-mail is asserted at all, however the provider labels it.
func TestOAuth_TrustEmailOffAssertsNoEmail(t *testing.T) {
	srv := fakeGitLab(t, []map[string]any{{"email": "al@acme.example", "confirmed_at": "2021-02-02T00:00:00Z"}})
	g, err := New(Config{Name: "gl", Preset: PresetGitLab, ClientID: "c", ClientSecret: "s", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.gitlabProfile(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	a, err := g.assertion(p)
	if err != nil || a.Email != "" || a.EmailVerified {
		t.Fatalf("assertion = %+v %v", a, err)
	}
}
