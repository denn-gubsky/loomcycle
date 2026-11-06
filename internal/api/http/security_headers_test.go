package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
)

// signInRoutes derives, from this package's own source, every route a browser
// reaches while signing in, accepting an invite or resetting a password: the
// mux patterns under /ui, /v1/_auth, /v1/_oauth and the OAuth metadata, and
// every /ui/<page> the server sends a person to (the invite and reset links,
// the post-login destinations). Read, not copied, so a route added later is
// covered by the census below without anyone listing it.
func signInRoutes(t *testing.T) (patterns, pages []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	patRe := regexp.MustCompile(`mux\.Handle(?:Func)?\("((?:GET|POST|PUT|PATCH|DELETE|OPTIONS) (?:/ui|/v1/_auth/|/v1/_oauth/|/\.well-known/oauth)[^"]*)"`)
	pageRe := regexp.MustCompile(`"(/ui/[a-z][a-z-]*)`)
	seenPat, seenPage := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range patRe.FindAllStringSubmatch(string(src), -1) {
			seenPat[m[1]] = true
		}
		for _, m := range pageRe.FindAllStringSubmatch(string(src), -1) {
			seenPage[m[1]] = true
		}
	}
	for p := range seenPat {
		patterns = append(patterns, p)
	}
	for p := range seenPage {
		pages = append(pages, p)
	}
	sort.Strings(patterns)
	sort.Strings(pages)
	// The census is only as good as the derivation: these must be in it.
	for _, must := range []string{"GET /ui/", "POST /ui/session", "GET /v1/_auth/login/{gate}", "POST /v1/_auth/invite", "POST /v1/_auth/reset", "GET /v1/_oauth/authorize"} {
		if !seenPat[must] {
			t.Fatalf("the derived routes lack %q: the source pattern changed", must)
		}
	}
	for _, must := range []string{"/ui/login", "/ui/invite", "/ui/reset", "/ui/pending"} {
		if !seenPage[must] {
			t.Fatalf("the derived pages lack %q: the source pattern changed", must)
		}
	}
	return patterns, pages
}

// TestSecurityHeaders_EverySignInRouteSendsThem: every sign-in route and page,
// whatever it answers (a page, JSON, a redirect, an error), forbids framing and
// sniffing, sends a same-origin Referer policy and a CSP with
// frame-ancestors 'none', and HSTS when the public URL is https.
func TestSecurityHeaders_EverySignInRouteSendsThem(t *testing.T) {
	patterns, pages := signInRoutes(t)
	cfg := makeBaseConfig()
	cfg.Env.PublicURL = "https://loomcycle.example"
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(8, 8, 5*time.Second), nil)
	h := srv.Mux()
	param := regexp.MustCompile(`\{[^}]+\}`)
	type req struct{ method, path string }
	var reqs []req
	for _, p := range patterns {
		method, path, _ := strings.Cut(p, " ")
		reqs = append(reqs, req{method, param.ReplaceAllString(path, "x")})
	}
	for _, p := range pages {
		reqs = append(reqs, req{"GET", p})
	}
	reqs = append(reqs, req{"GET", "/v1/docs"}, req{"GET", "/healthz"})
	for _, rq := range reqs {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rq.method, rq.path, strings.NewReader("{}")))
		hd := rec.Header()
		if hd.Get("X-Frame-Options") != "DENY" || hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") == "" {
			t.Errorf("%s %s (%d): XFO %q nosniff %q referrer %q", rq.method, rq.path, rec.Code,
				hd.Get("X-Frame-Options"), hd.Get("X-Content-Type-Options"), hd.Get("Referrer-Policy"))
		}
		if csp := hd.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s %s (%d): CSP %q lacks frame-ancestors 'none'", rq.method, rq.path, rec.Code, csp)
		}
		if hd.Get("Strict-Transport-Security") != hstsValue {
			t.Errorf("%s %s: HSTS %q with an https public URL", rq.method, rq.path, hd.Get("Strict-Transport-Security"))
		}
	}

	// No HSTS on a plain-http deployment.
	cfg2 := makeBaseConfig()
	cfg2.Env.PublicURL = "http://loomcycle.lan:8080"
	rec := httptest.NewRecorder()
	New(cfg2, &stubResolver{}, nil, concurrency.New(8, 8, 5*time.Second), nil).Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/login", nil))
	if v := rec.Header().Get("Strict-Transport-Security"); v != "" {
		t.Errorf("HSTS %q sent for an http public URL", v)
	}
}

// TestOpenAPIDocs_CSPAllowsOnlyItsOwnInlineScript: the reference console runs
// under a policy that admits its one inline boot script by hash, not inline
// script in general, and loads nothing from another origin.
func TestOpenAPIDocs_CSPAllowsOnlyItsOwnInlineScript(t *testing.T) {
	srv := New(makeBaseConfig(), &stubResolver{}, nil, concurrency.New(8, 8, 5*time.Second), nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/docs = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	script := regexp.MustCompile(`script-src ([^;]*)`).FindStringSubmatch(csp)
	if script == nil || strings.Contains(script[1], "unsafe-inline") || strings.Count(script[1], "'sha256-") != 1 {
		t.Errorf("docs script-src %q: want 'self' and exactly one hash", script)
	}
	if strings.Contains(csp, "https:") || strings.Contains(csp, "*") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("docs CSP %q admits another origin", csp)
	}
}
