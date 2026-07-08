package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestHTTPRefusesEmptyAllowlist(t *testing.T) {
	h := &HTTP{}
	res, err := h.Execute(context.Background(), json.RawMessage(`{"method":"GET","url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "allowlist") {
		t.Fatalf("expected allowlist refusal, got %q", res.Text)
	}
}

// A permitted Pre-hook's per-call host grant (ExtraAllowedHosts) must satisfy a
// call even when the static HostAllowlist is EMPTY — otherwise
// hooks.permit_host_widen is dead for any deployment that drives all hosts
// dynamically through a hook. Fail-before: on the pre-fix code the empty-
// allowlist guard returned "refusing all calls" before the extras were ever
// consulted, so this request was refused and this test fails.
func TestHTTPEmptyAllowlistHonoursPermittedHostWidenGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reached via hook grant")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{
		// HostAllowlist deliberately EMPTY — the operator runs no static
		// floor and relies entirely on permitted hooks to grant hosts.
		PrivateHostAllowlist: []string{host}, // let the loopback dial through
	}
	// The grant arrives ONLY via ctx, exactly as loop.dispatchOneTool sets it
	// after a permitted Pre-hook returns allow_hosts.
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{host})
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("permitted host-widen grant should satisfy an empty-allowlist call; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "reached via hook grant") {
		t.Errorf("body missing: %q", res.Text)
	}
}

// The empty-allowlist grant is host-scoped: a grant for one host does NOT open
// a different host. Guards against the fix degenerating into "any extras →
// allow anything."
func TestHTTPEmptyAllowlistGrantIsHostScoped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should not be reached")
	}))
	defer srv.Close()

	h := &HTTP{PrivateHostAllowlist: []string{mustHost(t, srv.URL)}}
	// Grant covers a DIFFERENT host than the one requested.
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{"granted.example"})
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not in allowlist") {
		t.Fatalf("expected host-not-in-allowlist refusal (grant is host-scoped); got %q", res.Text)
	}
}

func TestHTTPHostAllowlist(t *testing.T) {
	cases := []struct {
		host  string
		list  []string
		allow bool
	}{
		{"example.com", []string{"example.com"}, true},
		{"api.example.com", []string{"example.com"}, true},  // suffix match
		{"evilexample.com", []string{"example.com"}, false}, // anchored
		{"example.com.evil", []string{"example.com"}, false},
		{"example.com", nil, false},
		{"", []string{"example.com"}, false},
		{"EXAMPLE.COM", []string{"example.com"}, true}, // case-insensitive
	}
	for _, tc := range cases {
		t.Run(tc.host+"/"+strings.Join(tc.list, ","), func(t *testing.T) {
			if got := hostAllowed(tc.host, tc.list); got != tc.allow {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.host, tc.list, got, tc.allow)
			}
		})
	}
}

// SSRF defence layer 1: hostname not in allowlist → refuse before any DNS
// or TCP work. The IP guard would also catch a private resolution, but
// catching it earlier means we never resolve attacker-controlled DNS.
func TestHTTPRejectsNonAllowlistedHost(t *testing.T) {
	h := &HTTP{HostAllowlist: []string{"good.example"}}
	res, err := h.Execute(context.Background(), json.RawMessage(`{"method":"GET","url":"https://attacker.example/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not in allowlist") {
		t.Errorf("expected allowlist rejection, got %q", res.Text)
	}
}

// SSRF defence layer 2: even if the hostname IS allowlisted, an IP that
// resolves to a private range must be refused at connect time. We test
// this with an allowlisted hostname and a manual loopback URL — the
// IP-level check on 127.0.0.1 should fire.
func TestHTTPRejectsPrivateIPDespiteAllowlist(t *testing.T) {
	// AllowPrivateIPs MUST stay false here so the test exercises the guard.
	h := &HTTP{HostAllowlist: []string{"localhost"}}
	body, _ := json.Marshal(map[string]string{
		"method": "GET",
		"url":    "http://localhost:1/", // any non-routable port; we expect rejection BEFORE the dial completes
	})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected private-IP rejection, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "no public addresses") && !strings.Contains(res.Text, "private") {
		t.Errorf("rejection should mention private/public; got %q", res.Text)
	}
}

// The "*" allow-all sentinel: an operator that sets
// LOOMCYCLE_HTTP_HOST_ALLOWLIST=* accepts any hostname at the name layer.
// Pure-matcher unit test.
func TestHostAllowedWildcard(t *testing.T) {
	if !hostAllowed("anything.example.com", []string{"*"}) {
		t.Error(`"*" should allow any host`)
	}
	if !hostAllowed("random.tld", []string{"foo.com", "*"}) {
		t.Error(`"*" anywhere in the list should allow any host`)
	}
	if hostAllowed("", []string{"*"}) {
		t.Error("an empty host is never allowed, even under *")
	}
	// A literal "*" is the sentinel, never a suffix — it must NOT be treated
	// as matching only hosts ending ".*" or exactly "*".
	if !hostAllowed("deep.sub.domain.io", []string{"*"}) {
		t.Error(`"*" should match arbitrarily deep subdomains`)
	}
}

// The "*" sentinel widens ONLY the name layer (layer 1). The dial-time
// IP guard (layer 2) still refuses private/loopback IPs, so "*" means
// "all PUBLIC hosts", never "all addresses". Fail-before is impossible
// (the guard is independent) but this pins the documented public-only
// semantics against a future change that might route "*" past the dial
// guard.
func TestHTTPWildcardStillBlocksPrivateIP(t *testing.T) {
	// AllowPrivateIPs stays false so the dial guard is live.
	h := &HTTP{HostAllowlist: []string{"*"}}
	body, _ := json.Marshal(map[string]string{
		"method": "GET",
		"url":    "http://localhost:1/", // loopback → guard must fire despite *
	})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("wildcard must not lift the private-IP guard; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "no public addresses") && !strings.Contains(res.Text, "private") {
		t.Errorf("rejection should mention private/public; got %q", res.Text)
	}
}

// The "*" sentinel does let an otherwise-unlisted PUBLIC-shaped host
// through the name layer: with the IP guard disabled (as tests do for
// loopback), a request to a host that is on no explicit allowlist entry
// succeeds purely because of "*".
func TestHTTPWildcardAllowsUnlistedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reached via wildcard")
	}))
	defer srv.Close()

	// HostAllowlist is ONLY "*" — the httptest host is on no explicit entry.
	// AllowPrivateIPs lets the loopback dial complete (the name layer is what
	// we're exercising here).
	h := &HTTP{HostAllowlist: []string{"*"}, AllowPrivateIPs: true}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf(`"*" should admit an unlisted host at the name layer; got %q`, res.Text)
	}
	if !strings.Contains(res.Text, "reached via wildcard") {
		t.Errorf("expected the server body; got %q", res.Text)
	}
}

// Direct unit test on dialContext with a public-shaped hostname that
// resolves entirely to private IPs — proves the guard is what stops
// the dial, not the layer-1 allowlist exact-match path. Catches the
// regression the reviewer flagged: a future refactor of hostAllowed
// could break this without TestHTTPRejectsPrivateIPDespiteAllowlist
// noticing, because that test relies on "localhost"-as-allowlisted.
//
// We don't need a custom resolver: net.DefaultResolver.LookupIPAddr
// resolves "localhost" to loopback addresses on every standard system,
// and "localhost" is private under our isPrivateIP rule.
func TestHTTPDialContextRefusesAllPrivateResolution(t *testing.T) {
	h := &HTTP{HostAllowlist: []string{"localhost"}, AllowPrivateIPs: false}
	_, err := h.dialContext(context.Background(), "tcp", "localhost:1")
	if err == nil {
		t.Fatal("expected refusal; localhost should resolve only to private IPs")
	}
	if !strings.Contains(err.Error(), "no public addresses") {
		t.Errorf("expected 'no public addresses' error, got %v", err)
	}
}

// PrivateHostAllowlist: hosts on the list bypass the IP-private check
// at dial time. Use case: agent calling back to localhost-bound app
// API. Must NOT also disable the check for other hosts.
//
// We use httptest.NewServer (loopback) — when "localhost" is in the
// PrivateHostAllowlist, the dial proceeds. When it's NOT, the dial
// is blocked by the standard private-IP rejection.
func TestHTTPPrivateHostAllowlistPermitsListedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok via localhost")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{
		HostAllowlist:        []string{host},
		PrivateHostAllowlist: []string{host},
		// AllowPrivateIPs stays false — we're testing the per-host
		// exception, not the global tests-only flag.
	}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("listed private host should be reachable; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "ok via localhost") {
		t.Errorf("body missing: %q", res.Text)
	}
}

// Asymmetric companion to TestHTTPPrivateHostAllowlistPermitsListedHost.
// The earlier test puts the SAME entry on both HostAllowlist and
// PrivateHostAllowlist — could pass for the wrong reason if the dial
// path mistakenly consulted HostAllowlist. Here HostAllowlist is the
// only thing letting the URL through layer-1; PrivateHostAllowlist
// has a non-matching entry. The dial layer should still reject the
// private IP because the exemption doesn't apply to this host.
func TestHTTPPrivateHostAllowlistAsymmetricLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should not be reached")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{
		HostAllowlist:        []string{host},                  // 127.0.0.1 — passes layer 1
		PrivateHostAllowlist: []string{"some.unrelated.host"}, // does NOT match host
	}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected refusal — host not in PrivateHostAllowlist; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "no public addresses") {
		t.Errorf("expected 'no public addresses' rejection (private-IP guard fires); got %q", res.Text)
	}
}

// Inverse: a host NOT in PrivateHostAllowlist resolving to private IP
// is still refused. The exception is scoped to listed hosts only.
func TestHTTPPrivateHostAllowlistDoesNotLeakToOtherHosts(t *testing.T) {
	h := &HTTP{
		HostAllowlist:        []string{"localhost", "127.0.0.1"},
		PrivateHostAllowlist: []string{"my-app"}, // arbitrary unrelated host
	}
	// Dial localhost — NOT in PrivateHostAllowlist. Should be refused
	// the same way as without the exception list.
	_, err := h.dialContext(context.Background(), "tcp", "localhost:1")
	if err == nil {
		t.Fatal("expected refusal for unlisted private host")
	}
	if !strings.Contains(err.Error(), "no public addresses") {
		t.Errorf("expected 'no public addresses' error, got %v", err)
	}
}

// Suffix-match parity: PrivateHostAllowlist uses the same suffix
// matching as HostAllowlist. Listing "myapp.local" permits
// "api.myapp.local" too, anchored at a dot boundary.
func TestHTTPPrivateHostAllowlistSuffixMatch(t *testing.T) {
	cases := []struct {
		host  string
		list  []string
		match bool
	}{
		{"myapp.local", []string{"myapp.local"}, true},
		{"api.myapp.local", []string{"myapp.local"}, true},
		{"evilmyapp.local", []string{"myapp.local"}, false},
		{"myapp.local.evil", []string{"myapp.local"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := hostAllowed(tc.host, tc.list); got != tc.match {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.host, tc.list, got, tc.match)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.169.254", true}, // EC2/GCP metadata
		{"0.0.0.0", true},
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false}, // Cloudflare public DNS v6
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isPrivateIP(net.ParseIP(tc.ip)); got != tc.private {
				t.Errorf("isPrivateIP(%s) = %v, want %v", tc.ip, got, tc.private)
			}
		})
	}
}

// Successful round-trip against an allowed httptest server (loopback,
// so AllowPrivateIPs must be set — production code never sets this).
func TestHTTPSuccessfulRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from server")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{HostAllowlist: []string{host}, AllowPrivateIPs: true}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "hello from server") {
		t.Errorf("missing body; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "-> 200") {
		t.Errorf("missing status line; got %q", res.Text)
	}
}

// Response truncation: a server that returns more than MaxResponseBytes
// gets truncated and the result text says so. Without truncation, a
// hostile server could blow the model's context window.
func TestHTTPTruncatesLargeResponse(t *testing.T) {
	big := strings.Repeat("x", 10_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, big)
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{HostAllowlist: []string{host}, AllowPrivateIPs: true, MaxResponseBytes: 100}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[truncated at 100 bytes]") {
		t.Errorf("missing truncation marker; got %q", res.Text)
	}
}

// Redirect chain: if an allowed host 302s to a non-allowlisted host,
// the redirect must be refused. This is the second SSRF surface — a
// model could be tricked into following a redirect to an internal URL.
func TestHTTPRefusesRedirectToNonAllowlistedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://attacker.example/", http.StatusFound)
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	h := &HTTP{HostAllowlist: []string{host}, AllowPrivateIPs: true}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected redirect rejection, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "redirect host") {
		t.Errorf("expected redirect-host error; got %q", res.Text)
	}
}

func TestHTTPRejectsOversizedRequestBody(t *testing.T) {
	h := &HTTP{HostAllowlist: []string{"x.example"}, MaxRequestBytes: 8}
	body, _ := json.Marshal(map[string]string{"method": "POST", "url": "https://x.example/", "body": "this is much more than eight bytes"})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "exceeds") {
		t.Errorf("expected body-size rejection, got %q", res.Text)
	}
}

func TestHTTPRejectsNonHTTPScheme(t *testing.T) {
	h := &HTTP{HostAllowlist: []string{"example"}}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": "file:///etc/passwd"})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "scheme") {
		t.Errorf("expected scheme rejection, got %q", res.Text)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Hostname()
}

// TestHTTPAllowsHostFromCtxExtras: a host NOT in the operator's
// allowlist but PRESENT in the ctx-attached extras (placed there by a
// permitted Pre-hook earlier in dispatch) must succeed. This is the
// end-to-end of the v0.8.17 per-call host-widening capability — proves
// the gate consults ctx-extras after the static-list check fails.
func TestHTTPAllowsHostFromCtxExtras(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok via extras")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	// Operator's static allowlist deliberately does NOT include `host`.
	// A throwaway entry keeps the deny-all branch out of the way so
	// we're specifically testing the "operator-floor rejects, extras
	// approves" branch.
	h := &HTTP{
		HostAllowlist:        []string{"some-other-host.example"},
		PrivateHostAllowlist: []string{host}, // dial-layer exception so loopback succeeds
		AllowPrivateIPs:      false,
	}

	body, _ := json.Marshal(map[string]string{"method": "GET", "url": srv.URL})
	// Attach the host as a per-call extra (mirrors what
	// loop.dispatchOneTool does after a permitted Pre-hook).
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{host})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("host in ctx-extras should be reachable; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "ok via extras") {
		t.Errorf("response body unexpected: %q", res.Text)
	}
}

// TestHTTPCtxExtrasEmptyDoesNotWiden: a ctx with NO extras attached
// must NOT widen — the host stays blocked. Catches a regression where
// a future refactor accidentally treats nil/empty extras as "allow
// everything" (an easy reversal-of-meaning bug).
func TestHTTPCtxExtrasEmptyDoesNotWiden(t *testing.T) {
	h := &HTTP{HostAllowlist: []string{"only.allowed.example"}}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": "https://unknown.example/"})
	res, err := h.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected refusal — empty ctx-extras must not widen; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "not in allowlist") {
		t.Errorf("expected 'not in allowlist' error; got %q", res.Text)
	}
}

// TestHTTPCtxExtrasIPBlockNotBypassable: even if extras include a
// hostname that resolves to a private IP (e.g. "localhost"), the
// dial-time private-IP block still fires when PrivateHostAllowlist
// does NOT opt that host in. Pins the design: a Pre-hook can widen
// at the hostname layer, but cannot bypass the SSRF-defense at the
// IP layer — that's a separate, orthogonal trust boundary.
func TestHTTPCtxExtrasIPBlockNotBypassable(t *testing.T) {
	h := &HTTP{
		HostAllowlist: []string{"some-other-host.example"},
		// PrivateHostAllowlist deliberately empty — no IP-layer exemption.
	}
	body, _ := json.Marshal(map[string]string{"method": "GET", "url": "http://localhost:1/"})
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{"localhost"})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected dial-layer refusal even with extras; got %q", res.Text)
	}
	// The error surface depends on resolver/dial behavior; what matters
	// is that the call DID NOT succeed. Either "no public addresses"
	// (private-IP guard) or a connect failure on the bogus port — both
	// are acceptable. We just assert the request was rejected.
	if strings.Contains(res.Text, "HTTP GET") && !strings.Contains(res.Text, "-> 4") && !strings.Contains(res.Text, "-> 5") {
		t.Errorf("response looked like a successful round-trip; got %q", res.Text)
	}
}

// TestHTTPRedirectConsultsCtxExtras: initial URL on the operator
// allowlist, redirect target in the ctx-extras. CheckRedirect must
// allow the hop because the extras attached at dispatch cover the
// entire tool call.
func TestHTTPRedirectConsultsCtxExtras(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok at redirect target")
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	sourceHost := mustHost(t, source.URL)
	targetHost := mustHost(t, target.URL)
	h := &HTTP{
		HostAllowlist:        []string{sourceHost},
		PrivateHostAllowlist: []string{sourceHost, targetHost},
		AllowPrivateIPs:      false,
	}

	body, _ := json.Marshal(map[string]string{"method": "GET", "url": source.URL})
	// Source is in the static list; target is only in the per-call extras.
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{targetHost})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected redirect to extras-approved host to succeed; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "ok at redirect target") {
		t.Errorf("redirect did not land at target; body = %q", res.Text)
	}
}

// TestHTTPRedirectToUnknownHostStillBlocked: a redirect to a host
// covered by NEITHER the static list NOR the extras still fails.
// Pins the "extras don't blanket-approve" property — they cover
// exactly the hostnames the hook named, no more.
func TestHTTPRedirectToUnknownHostStillBlocked(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://attacker.example/", http.StatusFound)
	}))
	defer source.Close()

	sourceHost := mustHost(t, source.URL)
	h := &HTTP{
		HostAllowlist:   []string{sourceHost},
		AllowPrivateIPs: true,
	}

	body, _ := json.Marshal(map[string]string{"method": "GET", "url": source.URL})
	// Extras approve a different host than the redirect target.
	ctx := tools.WithExtraAllowedHosts(context.Background(), []string{"acme.com"})
	res, err := h.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected redirect rejection (target not in static OR extras); got %q", res.Text)
	}
	if !strings.Contains(res.Text, "redirect host") && !strings.Contains(res.Text, "not in allowlist") {
		t.Errorf("expected redirect-host error; got %q", res.Text)
	}
}

// TestHostAllowedExtras_LeadingDotSuffix unit-tests the matcher
// semantics: dotless entries are exact-match; leading-dot entries
// are suffix-match. This is intentionally stricter than the
// operator allowlist's hostAllowed() which is suffix-match always.
func TestHostAllowedExtras_LeadingDotSuffix(t *testing.T) {
	cases := []struct {
		host    string
		extras  []string
		want    bool
		comment string
	}{
		// Dotless: exact match only.
		{"acme.com", []string{"acme.com"}, true, "exact"},
		{"careers.acme.com", []string{"acme.com"}, false, "subdomain of dotless entry should NOT match"},
		{"acme.co", []string{"acme.com"}, false, "different host"},
		// Leading-dot: suffix-match including the bare anchor.
		{"acme.com", []string{".acme.com"}, true, "anchor of leading-dot matches"},
		{"careers.acme.com", []string{".acme.com"}, true, "subdomain of leading-dot matches"},
		{"evilacme.com", []string{".acme.com"}, false, "non-dot-boundary suffix must NOT match"},
		// Empty / edge cases.
		{"acme.com", []string{}, false, "empty extras"},
		{"acme.com", nil, false, "nil extras"},
		{"acme.com", []string{"", "."}, false, "empty + bare-dot entries are no-ops"},
		// Trailing dot on host (FQDN form) normalised.
		{"acme.com.", []string{"acme.com"}, true, "trailing-dot host normalises"},
		// Case-insensitivity.
		{"ACME.COM", []string{"acme.com"}, true, "case-insensitive host"},
		{"acme.com", []string{"ACME.COM"}, true, "case-insensitive entry"},
		// Empty host never matches.
		{"", []string{"acme.com"}, false, "empty host"},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			got := hostAllowedExtras(tc.host, tc.extras)
			if got != tc.want {
				t.Errorf("hostAllowedExtras(%q, %v) = %v, want %v (%s)",
					tc.host, tc.extras, got, tc.want, tc.comment)
			}
		})
	}
}
