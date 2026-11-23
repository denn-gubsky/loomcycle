package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// The token every credentialed test sends. A test that finds it anywhere but
// the upstream's own Authorization header fails.
const secretToken = "tok-3c1f9a-never-leaves-the-core"

// fakeCreds resolves one credential name the way credential.Engine does: a
// restricted credential resolves only for a target under its list.
type fakeCreds struct {
	name     string
	value    string
	username string
	allowed  *[]string
	calls    atomic.Int32
}

func (f *fakeCreds) ResolveFor(_ context.Context, _, _, _, name, target string) (credential.Resolved, bool, error) {
	f.calls.Add(1)
	if name != f.name {
		return credential.Resolved{}, false, nil
	}
	if f.allowed != nil && !credential.URLAllowed(f.allowed, target) {
		return credential.Resolved{}, false, credential.ErrURLNotAllowed
	}
	return credential.Resolved{Value: f.value, Username: f.username, AllowedURLs: f.allowed}, true, nil
}

// upstream is a TLS server that records what reached it.
type upstream struct {
	*httptest.Server
	hits    atomic.Int32
	mu      sync.Mutex
	lastReq *http.Request
	handler http.HandlerFunc
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{handler: h}
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.mu.Lock()
		u.lastReq = r.Clone(context.Background())
		u.mu.Unlock()
		if u.handler != nil {
			u.handler(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastReq
}

// routeURL is the upstream's address as a route URL.
func (u *upstream) routeURL(path string) string { return u.URL + "/" + path }

// testRoots trusts the certificate every httptest TLS server shares.
func testRoots(t *testing.T) *x509.CertPool {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

func newTestProxy(t *testing.T, creds Credentials, opt func(*Options)) (*Proxy, *[]Line) {
	t.Helper()
	var mu sync.Mutex
	lines := &[]Line{}
	o := Options{
		Credentials:          creds,
		PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig:            &tls.Config{RootCAs: testRoots(t), MinVersion: tls.VersionTLS12},
		Audit: func(l Line) {
			mu.Lock()
			defer mu.Unlock()
			*lines = append(*lines, l)
		},
	}
	if opt != nil {
		opt(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, lines
}

func grantOf(routes ...Route) Grant {
	return Grant{TenantID: "t1", AgentName: "a1", UserID: "u1", RunID: "run1", WorkspaceID: "ws1", Routes: routes}
}

func do(t *testing.T, p *Proxy, g Grant, r Request) (*Response, error) {
	t.Helper()
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	resp, err := p.Do(context.Background(), g, r)
	if err == nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

func readAll(t *testing.T, resp *Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return string(b)
}

func wantRefusal(t *testing.T, err error, status int) *Refusal {
	t.Helper()
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v, want a *Refusal", err)
	}
	if ref.Status != status {
		t.Fatalf("refusal status = %d (%s), want %d", ref.Status, ref.Reason, status)
	}
	return ref
}

func TestProxy_RefusesARouteTheWorkspaceWasNotGranted(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "api", URL: up.routeURL("")})
	_, err := do(t, p, g, Request{Route: "other", Path: "x"})
	wantRefusal(t, err, http.StatusForbidden)
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called %d times", up.hits.Load())
	}
}

func TestProxy_RefusesAMethodTheRouteDoesNotList(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "ro", URL: up.routeURL("")}, Route{Name: "rw", URL: up.routeURL(""), Methods: []string{"GET", "POST"}})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, "CONNECT", "get", "TRACE"} {
		_, err := do(t, p, g, Request{Route: "ro", Method: m, Path: "x"})
		wantRefusal(t, err, http.StatusForbidden)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called %d times", up.hits.Load())
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if _, err := do(t, p, g, Request{Route: "ro", Method: m, Path: "x"}); err != nil {
			t.Fatalf("%s on a read-only route: %v", m, err)
		}
	}
	if _, err := do(t, p, g, Request{Route: "rw", Method: http.MethodPost, Path: "x", Body: strings.NewReader("{}"), ContentLength: 2}); err != nil {
		t.Fatalf("POST on a route listing it: %v", err)
	}
}

func TestProxy_RefusesAPathThatCouldLeaveTheRoute(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "api", URL: up.routeURL("v2/")})
	for _, path := range []string{
		"..", "../admin", "a/../../admin", "./x", "a/./b",
		"%2e%2e/admin", "a/%2E%2E/b", "a%2fb", "a%2F..%2Fb", "a%5cb", "a%5C..", "a\\b",
		"/abs", "//evil.example/x", "https://evil.example/x", "a\x00b", "a\nb", "a\rb", "a\tb",
		"%252e%252e/admin", "a%25b", "a#frag", "a//b",
	} {
		_, err := do(t, p, g, Request{Route: "api", Path: path})
		if err == nil {
			t.Errorf("path %q: no refusal", path)
			continue
		}
		wantRefusal(t, err, http.StatusForbidden)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called %d times", up.hits.Load())
	}
	resp, err := do(t, p, g, Request{Route: "api", Path: "simple/numpy/?page=2&q=a%20b"})
	if err != nil {
		t.Fatalf("a clean path: %v", err)
	}
	_ = readAll(t, resp)
	if got := up.last().URL.RequestURI(); got != "/v2/simple/numpy/?page=2&q=a%20b" {
		t.Fatalf("upstream path = %q", got)
	}
}

func TestProxy_SendsTheCredentialAndDropsTheWorkersOwn(t *testing.T) {
	up := newUpstream(t, nil)
	creds := &fakeCreds{name: "crm", value: secretToken, allowed: &[]string{up.URL}}
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(Route{Name: "crm", URL: up.routeURL("api/"), Credential: "crm", Methods: []string{"GET", "POST"}})
	h := http.Header{}
	h.Set("Authorization", "Bearer forged")
	h.Set("Cookie", "session=stolen")
	h.Set("Host", "evil.example")
	h.Set("X-Forwarded-For", "10.0.0.1")
	h.Set("Forwarded", "for=10.0.0.1")
	h.Set("Proxy-Authorization", "Basic Zm9vOmJhcg==")
	h.Set("Connection", "X-Hop")
	h.Set("X-Hop", "1")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("X-Api-Version", "7")
	resp, err := do(t, p, g, Request{Route: "crm", Path: "contacts", Header: h})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = readAll(t, resp)
	r := up.last()
	if got := r.Header.Get("Authorization"); got != "Bearer "+secretToken {
		t.Fatalf("upstream Authorization = %q", got)
	}
	for _, k := range []string{"Cookie", "X-Forwarded-For", "Forwarded", "Proxy-Authorization", "X-Hop", "Keep-Alive"} {
		if v := r.Header.Get(k); v != "" {
			t.Errorf("upstream got %s: %q", k, v)
		}
	}
	if r.Host != strings.TrimPrefix(up.URL, "https://") {
		t.Errorf("upstream Host = %q", r.Host)
	}
	if r.Header.Get("X-Api-Version") != "7" {
		t.Errorf("an ordinary header was dropped")
	}
}

func TestProxy_SendsBasicAndNamedHeaderAuth(t *testing.T) {
	up := newUpstream(t, nil)
	creds := &fakeCreds{name: "c", value: secretToken, username: "svc", allowed: &[]string{up.URL}}
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(
		Route{Name: "b", URL: up.routeURL(""), Credential: "c", Auth: AuthBasic},
		Route{Name: "h", URL: up.routeURL(""), Credential: "c", Auth: AuthHeader, Header: "X-Api-Key"},
	)
	resp, err := do(t, p, g, Request{Route: "b", Path: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, resp)
	if u, pw, ok := up.last().BasicAuth(); !ok || u != "svc" || pw != secretToken {
		t.Fatalf("basic auth = %q %q %v", u, pw, ok)
	}
	h := http.Header{}
	h.Set("X-Api-Key", "forged")
	resp, err = do(t, p, g, Request{Route: "h", Path: "x", Header: h})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, resp)
	if got := up.last().Header.Values("X-Api-Key"); len(got) != 1 || got[0] != secretToken {
		t.Fatalf("X-Api-Key = %q", got)
	}
}

func TestProxy_RefusesAnUnrestrictedCredential(t *testing.T) {
	up := newUpstream(t, nil)
	creds := &fakeCreds{name: "c", value: secretToken} // no allowed_urls
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(Route{Name: "r", URL: up.routeURL(""), Credential: "c"})
	_, err := do(t, p, g, Request{Route: "r", Path: "x"})
	ref := wantRefusal(t, err, http.StatusForbidden)
	if strings.Contains(ref.Reason, secretToken) {
		t.Fatalf("the reason carries the token")
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called")
	}
}

func TestProxy_RefusesACredentialThatDoesNotCoverTheRoute(t *testing.T) {
	up := newUpstream(t, nil)
	creds := &fakeCreds{name: "c", value: secretToken, allowed: &[]string{"https://api.other.example"}}
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(Route{Name: "r", URL: up.routeURL(""), Credential: "c"})
	_, err := do(t, p, g, Request{Route: "r", Path: "x"})
	wantRefusal(t, err, http.StatusForbidden)
	missing := grantOf(Route{Name: "r", URL: up.routeURL(""), Credential: "absent"})
	_, err = do(t, p, missing, Request{Route: "r", Path: "x"})
	wantRefusal(t, err, http.StatusForbidden)
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called")
	}
}

func TestProxy_RelaysARedirectToAHeldRouteRewrittenAndRefusesAnyOther(t *testing.T) {
	other := newUpstream(t, nil)
	var target string
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	})
	creds := &fakeCreds{name: "c", value: secretToken, allowed: &[]string{up.URL}}
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(
		Route{Name: "a", URL: up.routeURL(""), Credential: "c"},
		Route{Name: "files", URL: other.routeURL("dl/")},
	)
	target = other.routeURL("dl/pkg-1.0.tar.gz?sig=1")
	resp, err := do(t, p, g, Request{Route: "a", Path: "x"})
	if err != nil {
		t.Fatalf("redirect to a held route: %v", err)
	}
	if resp.Status != http.StatusFound {
		t.Fatalf("status = %d", resp.Status)
	}
	if got := resp.Header.Get("Location"); got != "http://egress/files/pkg-1.0.tar.gz?sig=1" {
		t.Fatalf("Location = %q", got)
	}
	_ = readAll(t, resp)

	target = "https://evil.example/steal"
	_, err = do(t, p, g, Request{Route: "a", Path: "x"})
	ref := wantRefusal(t, err, http.StatusBadGateway)
	if !strings.Contains(ref.Reason, "evil.example") {
		t.Fatalf("reason %q does not name the target host", ref.Reason)
	}
	// Under the same host but outside every held route's path.
	target = other.routeURL("private/x")
	_, err = do(t, p, g, Request{Route: "a", Path: "x"})
	wantRefusal(t, err, http.StatusBadGateway)
	if other.hits.Load() != 0 {
		t.Fatalf("the core followed a redirect itself (%d hits)", other.hits.Load())
	}
}

func TestProxy_DropsAuthenticationAndHopByHopHeadersFromTheAnswer(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "s=1")
		w.Header().Add("Set-Cookie", "t=2")
		w.Header().Set("WWW-Authenticate", `Bearer realm="x"`)
		w.Header().Set("Proxy-Authenticate", "Basic")
		w.Header().Set("Authentication-Info", "nextnonce=1")
		w.Header().Set("X-Api-Key", "echoed")
		w.Header().Set("Connection", "X-Hop-Back")
		w.Header().Set("X-Hop-Back", "1")
		w.Header().Set("X-Request-Id", "r1")
		w.WriteHeader(http.StatusUnauthorized)
	})
	creds := &fakeCreds{name: "c", value: secretToken, allowed: &[]string{up.URL}}
	p, _ := newTestProxy(t, creds, nil)
	g := grantOf(Route{Name: "h", URL: up.routeURL(""), Credential: "c", Auth: AuthHeader, Header: "X-Api-Key"})
	resp, err := do(t, p, g, Request{Route: "h", Path: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.Status)
	}
	for _, k := range []string{"Set-Cookie", "WWW-Authenticate", "Proxy-Authenticate", "Authentication-Info", "X-Api-Key", "X-Hop-Back", "Connection"} {
		if v := resp.Header.Values(k); len(v) > 0 {
			t.Errorf("answer kept %s: %q", k, v)
		}
	}
	if resp.Header.Get("X-Request-Id") != "r1" {
		t.Errorf("an ordinary header was dropped")
	}
}

// pypiPage is a simple-index page as PyPI serves it, with a hash beside each
// link.
const pypiPage = `<a href="https://files.pythonhosted.org/packages/ab/cd/pkg-1.0.tar.gz#sha256=0123">pkg-1.0.tar.gz</a>`

// pypiProxy serves pypiPage as if from https://pypi.org/: the proxy's dial is
// pointed at a local server.
func pypiProxy(t *testing.T) (*Proxy, *upstream) {
	t.Helper()
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, pypiPage)
	})
	addr := up.Listener.Addr().String()
	p, _ := newTestProxy(t, nil, func(o *Options) {
		// httptest's certificate names example.com, not pypi.org.
		o.TLSConfig.ServerName = "example.com"
		o.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	})
	return p, up
}

func TestProxy_RewritesPyPILinksOnlyOnAnOperatorRouteWithTheFilesRouteHeld(t *testing.T) {
	p, _ := pypiProxy(t)
	files := Route{Name: "pypi-files", URL: "https://files.pythonhosted.org/"}
	cases := []struct {
		name    string
		grant   Grant
		rewrite bool
	}{
		{"operator route and the files route", grantOf(Route{Name: "pypi", URL: "https://pypi.org/"}, files), true},
		{"no files route", grantOf(Route{Name: "pypi", URL: "https://pypi.org/"}), false},
		{"a tenant's route on pypi.org", grantOf(Route{Name: "pypi", URL: "https://pypi.org/", Tenant: true}, files), false},
		{"a tenant's files route", grantOf(Route{Name: "pypi", URL: "https://pypi.org/"}, Route{Name: "pypi-files", URL: "https://files.pythonhosted.org/", Tenant: true}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := do(t, p, c.grant, Request{Route: "pypi", Path: "simple/pkg/"})
			if err != nil {
				t.Fatal(err)
			}
			body := readAll(t, resp)
			want := pypiPage
			if c.rewrite {
				want = `<a href="http://egress/pypi-files/packages/ab/cd/pkg-1.0.tar.gz#sha256=0123">pkg-1.0.tar.gz</a>`
			}
			if body != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
	}
}

func TestProxy_RefusesOverTheRateLimit(t *testing.T) {
	up := newUpstream(t, nil)
	now := time.Unix(1000, 0)
	p, _ := newTestProxy(t, nil, func(o *Options) {
		o.Limits = Limits{RatePerMinute: 2, MaxBytes: 1 << 20}
		o.Now = func() time.Time { return now }
	})
	g := grantOf(Route{Name: "r", URL: up.routeURL("")})
	for i := 0; i < 2; i++ {
		resp, err := do(t, p, g, Request{Route: "r", Path: "x"})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = readAll(t, resp)
	}
	_, err := do(t, p, g, Request{Route: "r", Path: "x"})
	wantRefusal(t, err, http.StatusTooManyRequests)
	// Another workspace has its own budget.
	other := g
	other.WorkspaceID = "ws2"
	if _, err := do(t, p, other, Request{Route: "r", Path: "x"}); err != nil {
		t.Fatalf("another workspace: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := do(t, p, g, Request{Route: "r", Path: "x"}); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
	if hits := up.hits.Load(); hits != 4 {
		t.Fatalf("upstream hits = %d, want 4", hits)
	}
}

func TestProxy_CutsAnAnswerAtTheByteBudget(t *testing.T) {
	big := strings.Repeat("x", 4096)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, big) })
	p, _ := newTestProxy(t, nil, func(o *Options) { o.Limits = Limits{RatePerMinute: 100, MaxBytes: 1000} })
	g := grantOf(Route{Name: "r", URL: up.routeURL("")})
	resp, err := do(t, p, g, Request{Route: "r", Path: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if err == nil || !errors.Is(err, ErrByteBudget) {
		t.Fatalf("read err = %v, want ErrByteBudget", err)
	}
	if len(b) > 1000 {
		t.Fatalf("read %d bytes past a budget of 1000", len(b))
	}
	_ = resp.Body.Close()
	_, err = do(t, p, g, Request{Route: "r", Path: "x"})
	wantRefusal(t, err, http.StatusTooManyRequests)
	p.Forget("ws1")
	if _, err := do(t, p, g, Request{Route: "r", Path: "x"}); err != nil {
		t.Fatalf("after Forget: %v", err)
	}
}

func TestProxy_AuditsCredentialedAndTenantRoutesOnlyWithoutQueryOrToken(t *testing.T) {
	up := newUpstream(t, nil)
	creds := &fakeCreds{name: "c", value: secretToken, allowed: &[]string{up.URL}}
	p, lines := newTestProxy(t, creds, nil)
	g := grantOf(
		Route{Name: "pub", URL: up.routeURL("")},
		Route{Name: "cred", URL: up.routeURL(""), Credential: "c"},
		Route{Name: "ten", URL: up.routeURL(""), Tenant: true},
	)
	for _, r := range []string{"pub", "cred", "ten"} {
		resp, err := do(t, p, g, Request{Route: r, Path: "items?api_key=" + secretToken})
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		_ = readAll(t, resp)
	}
	// A refusal on a credentialed route is audited too.
	_, _ = do(t, p, g, Request{Route: "cred", Path: "../x"})
	if len(*lines) != 3 {
		t.Fatalf("audit lines = %d (%+v), want 3", len(*lines), *lines)
	}
	routes := map[string]bool{}
	for _, l := range *lines {
		routes[l.Route] = true
		raw, _ := json.Marshal(l)
		if strings.Contains(string(raw), secretToken) || strings.Contains(string(raw), "api_key") {
			t.Fatalf("audit line carries the query or the token: %s", raw)
		}
		if l.WorkspaceID != "ws1" || l.TenantID != "t1" || l.RunID != "run1" {
			t.Fatalf("audit line identity: %+v", l)
		}
	}
	if routes["pub"] || !routes["cred"] || !routes["ten"] {
		t.Fatalf("audited routes = %v", routes)
	}
	ok := (*lines)[0]
	if ok.Path != "items" || ok.Method != http.MethodGet || ok.Status != http.StatusOK || ok.Bytes != 2 {
		t.Fatalf("first line = %+v", ok)
	}
}

func TestProxy_NoErrorCarriesTheToken(t *testing.T) {
	up := newUpstream(t, nil)
	url := up.routeURL("")
	creds := &fakeCreds{name: "c", value: secretToken, allowed: &[]string{up.URL}}
	p, _ := newTestProxy(t, creds, nil)
	up.Close() // every request now fails at the network
	g := grantOf(Route{Name: "r", URL: url, Credential: "c"})
	_, err := do(t, p, g, Request{Route: "r", Path: "x"})
	if err == nil {
		t.Fatal("no error from a closed upstream")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error carries the token: %v", err)
	}
}

func TestProxy_RefusesAPrivateAddressTheOperatorDidNotVouchFor(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, func(o *Options) { o.PrivateHostAllowlist = nil })
	g := grantOf(Route{Name: "r", URL: up.routeURL("")})
	if _, err := do(t, p, g, Request{Route: "r", Path: "x"}); err == nil {
		t.Fatal("a route to 127.0.0.1 was served without the allowlist")
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called")
	}
}

func TestProxy_RefusesARouteThatIsNotHTTPS(t *testing.T) {
	p, _ := newTestProxy(t, nil, nil)
	for _, u := range []string{"http://example.com/", "https://example.com", "https://user@example.com/", "https://example.com/?q=1", "ftp://example.com/"} {
		_, err := do(t, p, grantOf(Route{Name: "r", URL: u}), Request{Route: "r", Path: "x"})
		wantRefusal(t, err, http.StatusForbidden)
	}
}

func TestProxy_DropsTheSessionsOwnCredentialOnARouteWithNone(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "pub", URL: up.routeURL("")})
	// Keys as a worker might write them, not in canonical form.
	h := http.Header{"authorization": {"Bearer from-the-session"}, "cookie": {"a=b"}, "proxy-authorization": {"x"}, "x-forwarded-host": {"evil"}}
	resp, err := do(t, p, g, Request{Route: "pub", Path: "x", Header: h})
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, resp)
	r := up.last()
	for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Forwarded-Host"} {
		if v := r.Header.Get(k); v != "" {
			t.Errorf("upstream got %s: %q", k, v)
		}
	}
}

// An upstream may read a path differently from the proxy's check: a matrix
// parameter after ";" is stripped by Tomcat and Spring, an over-long UTF-8
// encoding of "." is decoded by some servers, a double encoding by others.
// Each must be refused, so the segment the check passed is the segment the
// upstream reads.
func TestProxy_RefusesAPathAnUpstreamCouldReadAsAnotherPath(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "api", URL: up.routeURL("v2/")})
	for _, path := range []string{
		"..;x", "a/..;/admin", "..;/admin", "a;b", "..%3bx", "a%3Bb",
		"%2e%2e", "%2e%2e/admin", "%252e%252e/admin", "a%2fb", "a%2F..",
		"%c0%ae%c0%ae/admin", "%c0%ae", "%e0%80%ae", "%ff", "a%80b",
		"a\\b", "a%5cb", "%00", "a%0ab", "a%7fb",
		"a b", "a\"b", "a<b", "a>b", "a^b", "a`b", "a{b", "a|b", "a}b", "a[b", "a]b",
	} {
		_, err := do(t, p, g, Request{Route: "api", Path: path})
		if err == nil {
			t.Errorf("path %q: no refusal", path)
			continue
		}
		wantRefusal(t, err, http.StatusForbidden)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream was called %d times", up.hits.Load())
	}
	// What registries and APIs send still passes: unreserved characters,
	// the vetted sub-delims, ":" and "@", and a UTF-8 percent-encoding.
	for _, path := range []string{
		"simple/numpy/", "packages/ab/cd/numpy-2.1.0-cp312-manylinux_2_17_x86_64.whl",
		"@types/node", "a:b", "a~b_c.d-e", "x!$&'()*+,=y", "caf%C3%A9", "a%20b",
	} {
		resp, err := do(t, p, g, Request{Route: "api", Path: path})
		if err != nil {
			t.Errorf("path %q: %v", path, err)
			continue
		}
		_ = readAll(t, resp)
	}
}

// npm asks for a scoped package as @scope%2fname in one segment. The owner
// ruled (O15, 2026-11-23) that this exact form passes: %2f only in the FIRST
// segment, only when it starts with @, and the scope and the name each pass
// the segment rules once split. Every other encoded slash stays refused.
func TestProxy_PassesAnNpmScopedNameOnlyInItsOwnForm(t *testing.T) {
	up := newUpstream(t, nil)
	p, _ := newTestProxy(t, nil, nil)
	g := grantOf(Route{Name: "npm", URL: up.routeURL("")})
	for _, c := range []struct{ path, want string }{
		{"@types%2fnode", "/@types%2fnode"},
		{"@types%2Fnode", "/@types%2Fnode"},
		{"@babel%2fcore/-/core-7.0.0.tgz", "/@babel%2fcore/-/core-7.0.0.tgz"},
	} {
		resp, err := do(t, p, g, Request{Route: "npm", Path: c.path})
		if err != nil {
			t.Errorf("%q: %v", c.path, err)
			continue
		}
		_ = readAll(t, resp)
		if got := up.last().RequestURI; got != c.want {
			t.Errorf("%q reached upstream as %q, want %q", c.path, got, c.want)
		}
	}
	hits := up.hits.Load()
	for _, path := range []string{
		"a/@types%2fnode", "x/%2fy", "@types/node%2fx", // a later segment
		"types%2fnode", "a%2fb", "%2f@x", // a first segment not starting with @
		"@a%2f..", "@a%2F..", "@%2f", "@%2fnode", "@a%2f", "@a%2f.", "@..%2fx", "@a%2fb%2fc", "@a%2f%2e%2e",
		"@a%5cb", "@a%2f%252e",
	} {
		_, err := do(t, p, g, Request{Route: "npm", Path: path})
		if err == nil {
			t.Errorf("%q: no refusal", path)
			continue
		}
		wantRefusal(t, err, http.StatusForbidden)
	}
	if up.hits.Load() != hits {
		t.Fatalf("a refused path reached upstream")
	}
}
