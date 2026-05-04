package builtin

import (
	"net"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// NarrowHosts returns a copy of the tool slice where every HTTP,
// WebFetch, and WebSearch instance is value-copied with its host
// allowlist resolved per callerAllowed and the policy mode. Other
// tools pass through unchanged.
//
// Two policy modes (callerAuthoritative selects):
//
// MODE INTERSECT (callerAuthoritative=false, today's default):
//
//	The operator's static list is the floor. Caller can SHRINK,
//	never widen. Empty operator list = deny-all (caller cannot lift
//	it; this was the BLOCKING fix from feature-tools review). Suffix
//	matching means caller "api.example.com" passes operator
//	"example.com" but caller "example.com" is rejected if operator
//	only has "api.example.com".
//	  - callerAllowed nil    → no narrowing; pass tools through.
//	  - callerAllowed []     → deny-all (empty intersection).
//	  - callerAllowed [hosts] → intersect with operator's list.
//
// MODE CALLER_AUTHORITATIVE (callerAuthoritative=true, opt-in via
// LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1):
//
//	Trust-the-caller: caller's list replaces operator's. IP-level
//	guard at dial time still blocks RFC1918/loopback/etc.; this
//	flag only controls hostname policy.
//	  - callerAllowed nil    → fall back to operator's static list (option iii)
//	  - callerAllowed []     → fall back to operator's static list (option iii)
//	  - callerAllowed [hosts] → use caller's list as the sole policy.
//
// Loopback aliases (localhost, 127.0.0.1, ::1, *.localhost, etc.)
// are stripped from caller's list before either mode evaluates.
// Operator's list is stripped at config-load time. The IP-level
// connect guard is the actual security gate; the strip is
// belt-and-braces so no operator/caller can confuse themselves into
// thinking "localhost" is a valid allowlist entry.
//
// wsFilterMode applies only to WebSearch:
//   - WebSearchFilterDrop (default when narrowing applies) drops
//     Brave results whose host isn't in the effective list.
//   - WebSearchFilterKeep returns Brave's results unchanged; the
//     caller filters downstream.
func NarrowHosts(in []tools.Tool, callerAllowed []string, wsFilterMode string, callerAuthoritative bool) []tools.Tool {
	// Strip loopback aliases from caller's list before any mode logic.
	// nil stays nil (the "no narrowing" signal); a list with only
	// loopback aliases shrinks to an empty list (deny-all in INTERSECT,
	// fall-back-to-operator in CALLER_AUTHORITATIVE).
	if callerAllowed != nil {
		callerAllowed = StripLocalhostAliases(callerAllowed)
	}

	if callerAuthoritative {
		// Option (iii): nil OR empty → fall back to operator's static.
		// Pass tools through unchanged so each tool's existing
		// HostAllowlist (already loopback-stripped at startup) applies.
		if len(callerAllowed) == 0 {
			return in
		}
		// Caller's list is the sole authority — REPLACE every tool's
		// HostAllowlist with it. No intersection with operator.
		return replaceHostsInTools(in, callerAllowed, wsFilterMode)
	}

	// MODE INTERSECT: today's default.
	if callerAllowed == nil {
		return in
	}
	// Find HTTP's operator floor — used as the floor for WebSearch too,
	// since WebFetch (which shares HTTP) is what the model actually
	// uses to follow up on search results. Showing the model URLs it
	// can't fetch is wasteful AND misleading. If no HTTP tool is in
	// the run's slice, WebSearch has no floor and can only narrow as
	// far as the caller's list.
	var httpFloor []string
	for _, t := range in {
		if h, ok := t.(*HTTP); ok {
			httpFloor = h.HostAllowlist
			break
		}
	}
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		switch v := t.(type) {
		case *HTTP:
			out = append(out, narrowHTTP(v, callerAllowed))
		case *WebFetch:
			// WebFetch wraps an HTTP backend; narrow that one instead.
			wf := *v
			if wf.HTTP != nil {
				wf.HTTP = narrowHTTP(wf.HTTP, callerAllowed)
			}
			out = append(out, &wf)
		case *WebSearch:
			ws := *v
			// WebSearch's floor is HTTP's allowlist (see comment above).
			// If HTTP isn't in the run, the floor is nil → empty result
			// (model can't fetch anything anyway).
			ws.AllowedHosts = intersectHosts(httpFloor, callerAllowed)
			ws.FilterMode = wsFilterMode
			if ws.FilterMode == "" {
				ws.FilterMode = WebSearchFilterDrop
			}
			out = append(out, &ws)
		default:
			out = append(out, t)
		}
	}
	return out
}

// replaceHostsInTools is the caller-authoritative-mode helper: it
// value-copies HTTP/WebFetch/WebSearch instances with HostAllowlist
// directly REPLACED by hosts (no intersection). Other tools pass
// through. hosts is assumed already loopback-stripped.
func replaceHostsInTools(in []tools.Tool, hosts []string, wsFilterMode string) []tools.Tool {
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		switch v := t.(type) {
		case *HTTP:
			h := *v
			h.HostAllowlist = append([]string(nil), hosts...)
			out = append(out, &h)
		case *WebFetch:
			wf := *v
			if wf.HTTP != nil {
				h := *wf.HTTP
				h.HostAllowlist = append([]string(nil), hosts...)
				wf.HTTP = &h
			}
			out = append(out, &wf)
		case *WebSearch:
			ws := *v
			ws.AllowedHosts = append([]string(nil), hosts...)
			ws.FilterMode = wsFilterMode
			if ws.FilterMode == "" {
				ws.FilterMode = WebSearchFilterDrop
			}
			out = append(out, &ws)
		default:
			out = append(out, t)
		}
	}
	return out
}

// narrowHTTP value-copies an HTTP and replaces its allowlist with the
// intersection. All other config (timeouts, byte caps, AllowPrivateIPs)
// carries over unchanged.
func narrowHTTP(orig *HTTP, callerAllowed []string) *HTTP {
	narrowed := *orig
	narrowed.HostAllowlist = intersectHosts(orig.HostAllowlist, callerAllowed)
	return &narrowed
}

// StripLocalhostAliases drops loopback-aliasing entries from a host
// allowlist. Belt-and-braces: the IP-level connect guard rejects
// loopback IPs at dial time too, but stripping at allowlist-parse
// time means operators never see "localhost" in their effective list
// and falsely conclude it's reachable through the main allowlist.
//
// What's stripped (case-insensitive, trailing dot ignored):
//   - "localhost" and any host whose final label is "localhost"
//     (RFC 6761 reserves the entire .localhost TLD as loopback)
//   - "127.0.0.1", "0.0.0.0" — IPv4 loopback / unspecified
//   - "::1", "[::]", "[::1]" — IPv6 loopback / unspecified literals
//
// Does NOT apply to LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST — that env
// var's whole purpose is to permit specific loopback hosts, so the
// strip would be self-defeating.
func StripLocalhostAliases(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, h := range in {
		// Try host:port split first so "localhost:3000" or "[::1]:443"
		// also trip the strip. net.SplitHostPort succeeds on
		// well-formed host:port strings; on failure we fall back to
		// the bare-string path (allowlist entries usually omit ports).
		hostPart := h
		if hp, _, err := net.SplitHostPort(h); err == nil {
			hostPart = hp
		}
		n := strings.ToLower(strings.TrimSuffix(hostPart, "."))
		if n == "localhost" || strings.HasSuffix(n, ".localhost") {
			continue
		}
		switch n {
		case "127.0.0.1", "0.0.0.0", "::1", "[::]", "[::1]":
			continue
		}
		out = append(out, h)
	}
	return out
}

// intersectHosts returns the entries from callerAllowed that are
// permitted by the operator's static list. caller==nil means no
// narrowing — but NarrowHosts short-circuits before calling this in
// that case; we never reach here with caller==nil.
//
// **Empty operator list = deny-all, not "anything goes".** The HTTP
// tool itself treats nil/empty HostAllowlist as deny-all, so we must
// preserve that here: a caller cannot supply `allowed_hosts: ["evil"]`
// and have it slip through just because the operator hasn't configured
// a static allowlist yet. Callers can only ever intersect; they can
// never override an unset operator policy.
//
// For non-empty operator lists: hostAllowed checks each caller entry
// against the operator's set with suffix-anchored match. An operator
// entry "example.com" permits a caller entry "api.example.com" (more
// specific narrowing). A caller entry "example.com" is REJECTED if
// the operator only has "api.example.com" — caller cannot widen.
func intersectHosts(operator, caller []string) []string {
	if len(operator) == 0 {
		// Operator's deny-all stands; caller cannot lift it.
		return []string{}
	}
	out := make([]string, 0, len(caller))
	for _, c := range caller {
		if hostAllowed(c, operator) {
			out = append(out, c)
		}
	}
	return out
}
