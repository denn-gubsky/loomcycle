package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// HTTP performs a single outbound HTTP request and returns the response body
// as text. Two layers of SSRF defence:
//
//  1. **Hostname allowlist** (HostAllowlist). Suffix-matched: an entry
//     "example.com" matches "example.com" and "api.example.com" but not
//     "evil-example.com". Empty allowlist refuses every call.
//
//  2. **IP-level block at connect time** via Dialer.Control. Even if the
//     hostname is allowlisted, the resolved IP is rejected if it falls in
//     RFC1918 / loopback / link-local / multicast / unspecified ranges.
//     This defeats DNS rebinding: the allowlisted hostname can resolve to
//     anything but we only TCP-connect to public addresses.
//
// Redirects re-run the allowlist check on every hop. Bodies are bounded
// in both directions; ctx is honoured throughout via the standard request
// context propagation.
type HTTP struct {
	// HostAllowlist is the suffix-matched list of permitted hostnames.
	// Required: empty allowlist rejects every call.
	HostAllowlist []string
	// MaxRequestBytes caps the request body. Default 256 KiB.
	MaxRequestBytes int64
	// MaxResponseBytes caps the response body (truncated, not erroring).
	// Default 256 KiB.
	MaxResponseBytes int64
	// Timeout is the per-request timeout. Default 30s.
	Timeout time.Duration
	// AllowPrivateIPs disables the IP-level block. Default false. Tests
	// flip this to true so they can hit httptest.NewServer (loopback).
	// Production never sets this to true.
	AllowPrivateIPs bool
	// PrivateHostAllowlist is the suffix-matched list of hostnames
	// allowed to resolve to private IPs at dial time. Default empty
	// (no exception — every private resolution refused). Operator
	// opts specific hosts in via LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST.
	// Use case: agents calling back to a localhost-bound application
	// API. Hostname is checked BEFORE DNS so dial-time still validates
	// the resolved IP against this same list.
	//
	// Matching is suffix-string against the URL's literal hostname,
	// not against resolved IPs. Listing "localhost" does NOT permit
	// a URL written as "http://127.0.0.1/" — operators wanting both
	// must list both literally. Same applies to IPv4 vs IPv6 loopback:
	// "127.0.0.1" doesn't cover "::1".
	PrivateHostAllowlist []string
}

func (h *HTTP) Name() string { return "HTTP" }
func (h *HTTP) Description() string {
	return "Make an HTTP request to an allowlisted host. Returns response body as text (truncated)."
}

func (h *HTTP) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"method":  {"type": "string", "enum": ["GET","POST","PUT","PATCH","DELETE","HEAD"], "description": "HTTP method."},
			"url":     {"type": "string", "description": "Absolute URL (scheme http/https). Host must be allowlisted."},
			"headers": {"type": "object", "additionalProperties": {"type": "string"}},
			"body":    {"type": "string", "description": "Request body. Required for POST/PUT/PATCH; ignored otherwise."}
		},
		"required": ["method", "url"]
	}`)
}

func (h *HTTP) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	return h.do(ctx, args.Method, args.URL, args.Headers, args.Body)
}

// do is split out so WebFetch can call it directly with GET defaults.
func (h *HTTP) do(ctx context.Context, method, rawURL string, headers map[string]string, body string) (tools.Result, error) {
	// Refuse only when the operator floor is empty AND no permitted Pre-hook
	// contributed a per-call host grant. A permitted host-widen grant
	// (ExtraAllowedHosts — populated ONLY by operator-opted-in hooks via the
	// dispatcher's IsHostWidenPermitted gate, never by the model or the
	// runtime caller) must be able to satisfy a call even with no static
	// allowlist; otherwise hooks.permit_host_widen is dead for any deployment
	// that drives all hosts dynamically through a hook (an empty floor used to
	// short-circuit here before the per-host extras check below ever ran). The
	// per-host check below still enforces the grant, and the dial-time
	// private-IP guard still applies — so this widens nothing the operator
	// did not explicitly opt in to.
	if len(h.HostAllowlist) == 0 && len(tools.ExtraAllowedHosts(ctx)) == 0 {
		return tools.Result{Text: "HTTP tool has empty host allowlist; refusing all calls", IsError: true}, nil
	}
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return tools.Result{Text: "unsupported method: " + method, IsError: true}, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return tools.Result{Text: "invalid url: " + err.Error(), IsError: true}, nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return tools.Result{Text: "url scheme must be http or https", IsError: true}, nil
	}
	if !hostAllowed(parsed.Hostname(), h.HostAllowlist) &&
		!hostAllowedExtras(parsed.Hostname(), tools.ExtraAllowedHosts(ctx)) {
		// Operator floor + caller-narrowed list rejected, and no
		// permitted Pre-hook contributed a per-call grant covering
		// this hostname either. (Trust-boundary reminder: the
		// extras list comes ONLY from operator-opted-in hooks via
		// the dispatcher; the model and the runtime caller cannot
		// inject into it.)
		return tools.Result{Text: fmt.Sprintf("host %q not in allowlist", parsed.Hostname()), IsError: true}, nil
	}

	maxReq := h.MaxRequestBytes
	if maxReq == 0 {
		maxReq = 256 * 1024
	}
	if int64(len(body)) > maxReq {
		return tools.Result{Text: fmt.Sprintf("request body exceeds %d bytes", maxReq), IsError: true}, nil
	}
	maxResp := h.MaxResponseBytes
	if maxResp == 0 {
		maxResp = 256 * 1024
	}
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, method, rawURL, strings.NewReader(body))
	if err != nil {
		return tools.Result{Text: "build request: " + err.Error(), IsError: true}, nil
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: h.dialContext,
		},
		// Re-validate the destination on each redirect hop. Without
		// this, an allowlisted host could 302 to an internal URL and
		// the second TCP connect would happen to a non-allowlisted
		// destination — only blocked by the IP-level guard.
		//
		// Per-call host-widening (v0.8.17) ALSO applies here: the
		// extras list attached to ctx pre-dispatch covers the entire
		// tool call including any redirects within it. KNOWN
		// LIMITATION: the hook is not re-invoked mid-redirect, so a
		// redirect to a brand-new hostname not approved up front
		// will fail. Document loudly in TOOLS.md.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !hostAllowed(req.URL.Hostname(), h.HostAllowlist) &&
				!hostAllowedExtras(req.URL.Hostname(), tools.ExtraAllowedHosts(ctx)) {
				return fmt.Errorf("redirect host %q not in allowlist", req.URL.Hostname())
			}
			return nil
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return tools.Result{Text: "request: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxResp+1)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		return tools.Result{Text: "read response: " + err.Error(), IsError: true}, nil
	}
	truncated := false
	if int64(len(respBody)) > maxResp {
		respBody = respBody[:maxResp]
		truncated = true
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP %s %s -> %d\n", method, rawURL, resp.StatusCode)
	for k, v := range resp.Header {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(v, ", "))
	}
	b.WriteString("\n")
	b.Write(respBody)
	if truncated {
		fmt.Fprintf(&b, "\n[truncated at %d bytes]", maxResp)
	}
	return tools.Result{Text: b.String()}, nil
}

// dialContext is the connection-level SSRF guard. By the time we reach
// it, the hostname has been DNS-resolved to a concrete address set —
// we filter out private addresses, refuse if NONE remain, and try each
// public address in turn until one connects (mirrors stdlib Dialer's
// happy-eyeballs-like behaviour for dual-stack hosts).
//
// This shape catches two real-world cases the naive "ips[0]" approach
// misses:
//
//   - Dual-stack host returns [v6, v4]; v6 unreachable on this network.
//     Iterating across the slice lets us fall back to v4.
//   - Public host with a misconfigured DNS record that includes a
//     private IP alongside the real one (corp-DNS leak). We dial the
//     public IPs and ignore the leak rather than refusing the host.
//
// The Control hook is a belt-and-braces re-check at the syscall layer
// in case the kernel ends up dialing a different address than the IP
// we passed (rare but possible if the address gets transformed).
// dialContext is the HTTP/WebFetch tool's SSRF-blocking dialer. It delegates to
// the shared guardedDialContext (see ssrf.go) so the identical dial-time
// private-IP guard backs the mem9 backend client too — one implementation, no
// weak-copy drift. AllowPrivateIPs is the tests-only global lift;
// PrivateHostAllowlist is the operator's per-host opt-in.
func (h *HTTP) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return guardedDialContext(h.AllowPrivateIPs, h.PrivateHostAllowlist)(ctx, network, addr)
}

// hostAllowed reports whether host is permitted by the allowlist.
// Suffix match anchored on a dot boundary so "example.com" matches
// "example.com" and "api.example.com" but not "evilexample.com".
// Bare hostnames (no dots) match exactly only.
func hostAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if entry == "" {
			continue
		}
		if host == entry {
			return true
		}
		if strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// hostAllowedExtras is the matcher for per-call host-widening grants
// from permitted Pre-hooks (v0.8.17). Intentionally stricter than
// hostAllowed:
//
//   - A bare entry "acme.com" matches "acme.com" ONLY (NOT
//     "careers.acme.com"). Useful for hooks that want to approve
//     exactly the URL the model asked about, no broader.
//   - A leading-dot entry ".acme.com" suffix-matches: "acme.com" AND
//     any subdomain. The leading dot is an explicit opt-in to the
//     "operator-list-style" suffix semantics, so cautious hook
//     authors get exact-match by default.
//
// Returns false on nil / empty extras (the common path) without
// allocating.
func hostAllowedExtras(host string, extras []string) bool {
	if len(extras) == 0 {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, entry := range extras {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, ".") {
			// Leading-dot: suffix-match (operator-style).
			anchor := entry[1:]
			if anchor == "" {
				continue // ".  alone" isn't meaningful
			}
			if host == anchor || strings.HasSuffix(host, "."+anchor) {
				return true
			}
			continue
		}
		// Bare entry: exact match only.
		if host == entry {
			return true
		}
	}
	return false
}

// isPrivateIP returns true for any IP an SSRF attacker would want to reach:
// loopback (127.0.0.0/8, ::1), RFC1918 private (10/8, 172.16/12, 192.168/16),
// link-local (169.254/16, fe80::/10) — including the AWS/GCP metadata service
// at 169.254.169.254 — multicast, and unspecified (0.0.0.0, ::).
// IPv6 ULAs (fc00::/7) too.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	return false
}
