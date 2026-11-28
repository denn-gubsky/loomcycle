package http

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3, P9 plan §3.3: the exposure route. The decision is the
// lifecycle's (OpenExposure, real, on a SQLite store); the dial goes to a
// test server standing for the workspace's application.

const testExposureID = "0123456789abcdef01234567"

// seenRequest is what the application received.
type seenRequest struct {
	Path, Host, Origin, Auth, Cookie, Proto string
	Header                                  http.Header
}

type exposureFixture struct {
	st   store.Store
	m    *lifecycle.Manager
	app  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
	// dialErr, when set, is what the dial answers instead of reaching app.
	dialErr error
	mux     *http.ServeMux
	lim     exposeRouteLimits
	// wsApp, when set, answers the application's /ws… paths (P9 PR 5).
	wsApp http.HandlerFunc
	// recheck, when set, is how often an open socket asks whether its
	// exposure is still open.
	recheck time.Duration
}

// testExposures is the real bookkeeping with the dial sent to the fixture's
// application.
type testExposures struct {
	*lifecycle.Manager
	f *exposureFixture
}

func (x testExposures) DialExposure(ctx context.Context, _ store.WorkspaceRow, _ int) (io.ReadWriteCloser, error) {
	if x.f.dialErr != nil {
		return nil, x.f.dialErr
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", x.f.app.Listener.Addr().String())
}

func newExposureFixture(t *testing.T) *exposureFixture {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "expose.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &exposureFixture{st: st, lim: exposeRouteLimits{MaxConnections: 32, RequestsPerSecond: 1000, BytesPerSecond: 1 << 30}}
	f.app = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{Path: r.URL.RequestURI(), Host: r.Host, Origin: r.Header.Get("Origin"),
			Auth: r.Header.Get("Authorization"), Cookie: r.Header.Get("Cookie"), Header: r.Header.Clone()})
		f.mu.Unlock()
		if f.wsApp != nil && strings.HasPrefix(r.URL.Path, "/ws") {
			f.wsApp(w, r)
			return
		}
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", "/there?x=1")
			w.WriteHeader(http.StatusFound)
			return
		case "/hostile":
			// Headers that act on the whole origin, which the core's own
			// origin would apply, beside the few a page needs.
			h := w.Header()
			for k, v := range map[string]string{
				"Clear-Site-Data": `"*"`, "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
				"Access-Control-Allow-Origin": "*", "Access-Control-Allow-Credentials": "true", "Refresh": "0; url=https://evil.example/",
				"Link": "<https://evil.example/x.js>; rel=preload", "Service-Worker-Allowed": "/",
				"Origin-Agent-Cluster": "?0", "Permissions-Policy": "camera=*", "Alt-Svc": `h3=":443"`,
				"Nel": `{"report_to":"x","max_age":1}`, "Report-To": `{"group":"x"}`, "Reporting-Endpoints": `x="https://evil.example/"`,
				"X-Frame-Options": "ALLOWALL", "Content-Security-Policy-Report-Only": "default-src *",
				"Content-Type": "text/html; charset=utf-8", "Etag": `"v1"`, "Last-Modified": "Wed, 25 Nov 2026 10:00:00 GMT",
				"Content-Disposition": "inline", "Cache-Control": "public, max-age=86400", "Trailer": "X-Late",
			} {
				h.Set(k, v)
			}
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s3cret"})
			_, _ = io.WriteString(w, "<p>page</p>")
			h.Set("X-Late", "trailer")
			return
		case "/early-hints":
			// A 1xx with headers that act on the origin, then the answer.
			h := w.Header()
			h.Set("Link", "<https://evil.example/x.js>; rel=preload")
			h.Set("Clear-Site-Data", `"*"`)
			w.WriteHeader(http.StatusEarlyHints)
			h.Del("Link")
			h.Del("Clear-Site-Data")
			h.Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "after the hints")
			return
		case "/backslash":
			// Browsers read "/\\" as "//": off-site, not a path of the
			// application's own.
			w.Header().Set("Location", `/\evil.example/x`)
			w.WriteHeader(http.StatusFound)
			return
		case "/away":
			w.Header().Set("Location", "https://example.com/elsewhere")
			w.WriteHeader(http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s3cret"})
		w.Header().Set("Content-Security-Policy", "default-src *")
		_, _ = io.WriteString(w, "hello from the workspace")
	}))
	t.Cleanup(f.app.Close)
	ctx := context.Background()
	if _, err := st.WorkspaceCreate(ctx, store.WorkspaceRow{ID: "ws_1", TenantID: "acme", UserID: "alice", Name: "web",
		WorkerID: "wk1", Engine: "host", OwnerRunID: "run1", RootRunID: "run1", State: store.WorkspaceReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: testExposureID, TenantID: "acme", UserID: "alice",
		WorkspaceID: "ws_1", Port: 8000, Kind: store.WorkspaceExposurePage}, 2); err != nil {
		t.Fatal(err)
	}
	f.m = lifecycle.New(nil, st, lifecycle.Limits{}, nil)
	f.m.SetExpose(lifecycle.ExposeLimits{Enabled: true})
	f.rebuild()
	return f
}

// rebuild makes the route's mux again with the fixture's limits.
func (f *exposureFixture) rebuild() {
	f.mux = http.NewServeMux()
	h := newExposureHandler(testExposures{Manager: f.m, f: f}, f.lim)
	if f.recheck > 0 {
		h.socketRecheck = f.recheck
	}
	withRequestPrincipal(h)
	f.mux.Handle(exposureRoutePattern, h)
}

// as is a request with a bearer header, authenticated as p (nil p: no
// principal, as open mode leaves it).
func (f *exposureFixture) as(p *auth.Principal, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "board.example"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer lc_test")
	}
	if p != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), *p))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *exposureFixture) seenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

var (
	alice      = &auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"runs:read"}}
	bob        = &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{"runs:read"}}
	aliceElse  = &auth.Principal{TenantID: "other", Subject: "alice", Scopes: []string{"runs:read"}}
	adminBob   = &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{"substrate:admin"}}
	legacyRoot = &auth.Principal{TenantID: "default", Subject: "alice", Legacy: true}
	pagePath   = exposurePrefix + testExposureID + "/index.html"
)

// wantSandboxed asserts the headers every answer of the route carries.
func wantSandboxed(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts allow-forms") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("%s: Content-Security-Policy = %q, want the sandbox policy", what, csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: no X-Content-Type-Options: nosniff", what)
	}
	if len(w.Header().Values("Set-Cookie")) != 0 {
		t.Errorf("%s: Set-Cookie reached the client: %v", what, w.Header().Values("Set-Cookie"))
	}
}

// Only the workspace's own user, in its tenant, reaches the page (RFC GW
// D22, D-P9-8): another user of the tenant, the same subject in another
// tenant, an admin, the legacy token and a caller with no principal all get
// the answer an unknown id gets, and the application sees none of them.
func TestExposureRoute_RefusesEveryCallerButTheWorkspacesOwnUser(t *testing.T) {
	f := newExposureFixture(t)
	unknown := f.as(alice, "GET", exposurePrefix+"ffffffffffffffffffffffff/index.html", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("an unknown id = %d, want 404", unknown.Code)
	}
	for name, p := range map[string]*auth.Principal{"another user": bob, "another tenant": aliceElse,
		"an admin": adminBob, "the legacy token": legacyRoot, "no principal": nil} {
		w := f.as(p, "GET", pagePath, nil)
		if w.Code != unknown.Code || w.Body.String() != unknown.Body.String() {
			t.Errorf("%s: %d %q, want the unknown id's %d %q", name, w.Code, w.Body.String(), unknown.Code, unknown.Body.String())
		}
		wantSandboxed(t, name, w)
	}
	if n := f.seenCount(); n != 0 {
		t.Fatalf("the application received %d requests from refused callers", n)
	}
	if w := f.as(alice, "GET", pagePath, nil); w.Code != http.StatusOK || w.Body.String() != "hello from the workspace" {
		t.Errorf("the workspace's own user = %d %q", w.Code, w.Body.String())
	}
}

// The route takes the bearer in the Authorization header only: a request a
// Web UI cookie authenticated, and one with no header, are refused, so a
// browser sent to the address runs nothing (RFC GW §5.3).
func TestExposureRoute_RefusesACookieOrAMissingBearerHeader(t *testing.T) {
	f := newExposureFixture(t)
	r := httptest.NewRequest("GET", pagePath, nil)
	r = r.WithContext(withUIAuth(auth.WithPrincipal(r.Context(), *alice), uiAuth{}))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a cookie-authenticated request = %d, want 401", w.Code)
	}
	wantSandboxed(t, "cookie", w)
	r = httptest.NewRequest("GET", pagePath, nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), *alice))
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no Authorization header = %d, want 401", w.Code)
	}
	if n := f.seenCount(); n != 0 {
		t.Errorf("the application received %d requests", n)
	}
}

// What crosses (RFC GW §5.3): the prefix is removed, Host and Origin pass as
// sent, the bearer and cookies never reach the application, Set-Cookie
// never reaches the client, a redirect lands under the prefix, and every
// answer carries the sandbox policy in place of the application's own.
func TestExposureRoute_CarriesTheRequestUnderTheSandboxPolicy(t *testing.T) {
	f := newExposureFixture(t)
	w := f.as(alice, "GET", exposurePrefix+testExposureID+"/a/b%20c?q=1", map[string]string{
		"Origin": "https://board.example", "Cookie": "lc_session=abc", "Authorization": "Bearer lc_secret_value",
		"Proxy-Authorization": "Basic x"})
	if w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body.String())
	}
	wantSandboxed(t, "a page", w)
	if strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src *") {
		t.Errorf("the application's own policy reached the client: %q", w.Header().Get("Content-Security-Policy"))
	}
	got := f.seen[0]
	if got.Path != "/a/b%20c?q=1" || got.Host != "board.example" || got.Origin != "https://board.example" {
		t.Errorf("the application saw path %q host %q origin %q", got.Path, got.Host, got.Origin)
	}
	if got.Auth != "" || got.Cookie != "" || got.Header.Get("Proxy-Authorization") != "" {
		t.Errorf("a credential reached the application: auth %q cookie %q", got.Auth, got.Cookie)
	}
	for k, vs := range got.Header {
		for _, v := range vs {
			if strings.Contains(v, "lc_secret_value") || strings.Contains(v, "lc_test") {
				t.Errorf("the bearer reached the application in %s", k)
			}
		}
	}
	if w := f.as(alice, "GET", exposurePrefix+testExposureID+"/redirect", nil); w.Header().Get("Location") != exposurePrefix+testExposureID+"/there?x=1" {
		t.Errorf("a redirect's Location = %q, want it under the prefix", w.Header().Get("Location"))
	}
	if w := f.as(alice, "GET", exposurePrefix+testExposureID+"/away", nil); w.Header().Get("Location") != "https://example.com/elsewhere" {
		t.Errorf("a redirect elsewhere = %q, want it unchanged", w.Header().Get("Location"))
	}
}

// An exposure closes with unexpose, with its workspace's release, at its
// idle timeout and when the operator turns workspace.expose off: each is the
// unknown id's answer, and nothing reaches the application.
func TestExposureRoute_ClosedByUnexposeReleaseIdleAndTheSwitch(t *testing.T) {
	ctx := context.Background()
	for name, close := range map[string]func(f *exposureFixture){
		"unexpose": func(f *exposureFixture) {
			_, _ = f.st.WorkspaceExposureClose(ctx, "acme", testExposureID, store.WorkspaceExposureClosedUnexposed, time.Now())
		},
		"release": func(f *exposureFixture) {
			_, _ = f.st.WorkspaceSetState(ctx, "acme", "ws_1", store.WorkspaceReady, store.WorkspaceReleasing)
		},
		"idle": func(f *exposureFixture) {
			_ = f.st.WorkspaceExposureTouch(ctx, "acme", testExposureID, time.Now().Add(-time.Hour))
		},
		"switch off": func(f *exposureFixture) { f.m.SetExpose(lifecycle.ExposeLimits{Enabled: false}) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newExposureFixture(t)
			if w := f.as(alice, "GET", pagePath, nil); w.Code != http.StatusOK {
				t.Fatalf("before = %d", w.Code)
			}
			close(f)
			if w := f.as(alice, "GET", pagePath, nil); w.Code != http.StatusNotFound {
				t.Errorf("after %s = %d, want 404", name, w.Code)
			}
			if n := f.seenCount(); n != 1 {
				t.Errorf("the application received %d requests, want only the one before", n)
			}
		})
	}
}

// A port no server of the workspace listens on itself (the worker's #2917
// check) is a 502 with the sandbox policy, naming no host or path.
func TestExposureRoute_APortNotServedIsABadGateway(t *testing.T) {
	f := newExposureFixture(t)
	f.dialErr = &dispatch.OpError{Class: workspace.ClassPortNotServed, Detail: "port 8000 is not listened on by a server of this workspace itself"}
	w := f.as(alice, "GET", pagePath, nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no server") {
		t.Errorf("= %d %q, want 502 naming no server", w.Code, w.Body.String())
	}
	wantSandboxed(t, "502", w)
}

// An upgrade to anything but WebSocket (h2c) is refused, and reaches no
// application.
func TestExposureRoute_RefusesAnUpgradeThatIsNotWebSocket(t *testing.T) {
	f := newExposureFixture(t)
	w := f.as(alice, "GET", pagePath, map[string]string{"Connection": "Upgrade", "Upgrade": "h2c"})
	if w.Code != http.StatusNotImplemented {
		t.Errorf("an h2c upgrade = %d, want 501", w.Code)
	}
	if n := f.seenCount(); n != 0 {
		t.Errorf("the application received %d requests", n)
	}
}

// The workspace's request rate is bounded (workspace.expose.requests_per_second).
func TestExposureRoute_RefusesPastTheRequestRate(t *testing.T) {
	f := newExposureFixture(t)
	f.lim.RequestsPerSecond = 1
	f.rebuild()
	codes := map[int]int{}
	for range 5 {
		codes[f.as(alice, "GET", pagePath, nil).Code]++
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusOK] == 0 {
		t.Errorf("5 quick requests at 1/s gave %v, want some 200 and some 429", codes)
	}
}

// Only the headers a page needs reach the client (the security review of
// #2931): an exposed server's answer is forwarded by allowlist, so a header
// that acts on the whole origin the core serves from — clearing its
// storage, pinning HSTS, opening CORS, a preload or a refresh, a service
// worker's scope, reporting — never applies there. Cache-Control is the
// runtime's, private and never stored; trailers are dropped too.
func TestExposureRoute_ForwardsOnlyTheHeadersAPageNeeds(t *testing.T) {
	f := newExposureFixture(t)
	w := f.as(alice, "GET", exposurePrefix+testExposureID+"/hostile", nil)
	if w.Code != http.StatusOK || w.Body.String() != "<p>page</p>" {
		t.Fatalf("= %d %q", w.Code, w.Body.String())
	}
	res := w.Result()
	for _, h := range []string{"Clear-Site-Data", "Set-Cookie", "Strict-Transport-Security", "Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials", "Refresh", "Link", "Service-Worker-Allowed", "Origin-Agent-Cluster",
		"Permissions-Policy", "Alt-Svc", "Nel", "Report-To", "Reporting-Endpoints", "X-Frame-Options",
		"Content-Security-Policy-Report-Only", "Trailer", "X-Late"} {
		if v := res.Header.Values(h); len(v) != 0 {
			t.Errorf("%s reached the client: %v", h, v)
		}
		if v := res.Trailer.Values(h); len(v) != 0 {
			t.Errorf("trailer %s reached the client: %v", h, v)
		}
	}
	for h, want := range map[string]string{"Content-Type": "text/html; charset=utf-8", "Etag": `"v1"`,
		"Last-Modified": "Wed, 25 Nov 2026 10:00:00 GMT", "Content-Disposition": "inline", "Cache-Control": "private, no-store"} {
		if got := res.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	wantSandboxed(t, "a hostile answer", w)
}

// A 1xx the application sends never reaches the client (the security review
// of #2931): ReverseProxy relays it through its own trace, past
// ModifyResponse, so its headers would skip the allowlist, and its clear of
// the header map would strip the sandbox policy from the final answer.
func TestExposureRoute_DropsAnInformationalAnswerAndKeepsThePolicy(t *testing.T) {
	f := newExposureFixture(t)
	w := f.as(alice, "GET", exposurePrefix+testExposureID+"/early-hints", nil)
	if w.Code != http.StatusOK || w.Body.String() != "after the hints" {
		t.Fatalf("= %d %q, want the final 200 and no 1xx", w.Code, w.Body.String())
	}
	for _, h := range []string{"Link", "Clear-Site-Data"} {
		if v := w.Result().Header.Values(h); len(v) != 0 {
			t.Errorf("%s from a 1xx reached the client: %v", h, v)
		}
	}
	wantSandboxed(t, "the answer after a 1xx", w)
	if w.Result().Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("Cache-Control after a 1xx = %q", w.Result().Header.Get("Cache-Control"))
	}

	// Over a real connection, where a relayed 1xx would reach the client
	// and its clear would strip the final answer's policy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), *alice)))
	}))
	defer srv.Close()
	var infos []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		infos = append(infos, code)
		return nil
	}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET",
		srv.URL+exposurePrefix+testExposureID+"/early-hints", nil)
	req.Header.Set("Authorization", "Bearer lc_test")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(infos) != 0 || resp.StatusCode != http.StatusOK {
		t.Errorf("the client saw 1xx %v and final %d, want no 1xx and 200", infos, resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts allow-forms") || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Link") != "" || resp.Header.Get("Clear-Site-Data") != "" {
		t.Errorf("the final answer over a connection: CSP %q nosniff %q Link %q Clear-Site-Data %q", csp,
			resp.Header.Get("X-Content-Type-Options"), resp.Header.Get("Link"), resp.Header.Get("Clear-Site-Data"))
	}
}

// underPrefix moves a redirect under the prefix only when it is a path of
// the application's own: "/x". A redirect whose second character is "/" or
// "\\" (which browsers read as "//"), or that holds a control character
// (stripped by browsers, so "/\t/evil" is "//evil"), is off-site, and is
// left as it is, never put under the prefix (CodeQL go/bad-redirect-check).
func TestExposureRoute_UnderPrefixMovesOnlyTheApplicationsOwnPaths(t *testing.T) {
	const prefix = exposurePrefix + testExposureID
	for loc, want := range map[string]string{
		"/x":                       prefix + "/x",
		"/a/b?q=1":                 prefix + "/a/b?q=1",
		"/":                        prefix + "/",
		"//evil.example/x":         "//evil.example/x",
		`/\evil.example/x`:         `/\evil.example/x`,
		"/\t/evil.example":         "/\t/evil.example",
		"https://example.com/else": "https://example.com/else",
		"relative/path":            "relative/path",
	} {
		if got := underPrefix(loc, prefix); got != want {
			t.Errorf("underPrefix(%q) = %q, want %q", loc, got, want)
		}
	}
	f := newExposureFixture(t)
	w := f.as(alice, "GET", prefix+"/backslash", nil)
	if got := w.Header().Get("Location"); got != `/\evil.example/x` {
		t.Errorf("a redirect to /\\evil.example = %q, want it left as it is, not under the prefix", got)
	}
}

// Every answer of the route, whoever made it, is sandboxed without
// allow-same-origin and carries nosniff: a page is model-written active
// content served from the API's origin, and the sandbox gives it an opaque
// origin instead. This is the guard CodeQL go/reflected-xss alerts 422 and
// 423 were dismissed on: it fails if allow-same-origin is added to the
// policy or sandbox is dropped.
func TestExposure_EveryAnswerIsSandboxedWithoutSameOrigin(t *testing.T) {
	f := newExposureFixture(t)
	cookie := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", pagePath, nil)
		r = r.WithContext(withUIAuth(auth.WithPrincipal(r.Context(), *alice), uiAuth{}))
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		return w
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"a 200 page":            f.as(alice, "GET", pagePath, nil),
		"the core's 404":        f.as(alice, "GET", exposurePrefix+"ffffffffffffffffffffffff/x", nil),
		"the core's refusal":    cookie(),
		"a redirect":            f.as(alice, "GET", exposurePrefix+testExposureID+"/redirect", nil),
		"an answer after a 1xx": f.as(alice, "GET", exposurePrefix+testExposureID+"/early-hints", nil),
	} {
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
			t.Errorf("%s: Content-Security-Policy = %q, want a sandbox without allow-same-origin", name, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no X-Content-Type-Options: nosniff", name)
		}
	}
}

// withRequestPrincipal gives a test handler the credential re-check as the
// request's own principal, unchanging; the tests that end a credential go
// through the real server (workspace_exposure_ws_test.go).
func withRequestPrincipal(h *exposureHandler) *exposureHandler {
	h.reauth = func(r *http.Request) (auth.Principal, error) {
		if p, ok := auth.PrincipalFromContext(r.Context()); ok {
			return p, nil
		}
		return auth.Principal{}, errors.New("no principal")
	}
	h.expiry = func(*http.Request) time.Time { return time.Time{} }
	return h
}
