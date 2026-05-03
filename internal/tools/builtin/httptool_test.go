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
