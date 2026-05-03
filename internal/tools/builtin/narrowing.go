package builtin

import (
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// NarrowHosts returns a copy of the tool slice where every HTTP,
// WebFetch, and WebSearch instance is value-copied with its host
// allowlist intersected against callerAllowed. Other tools pass
// through unchanged.
//
// The intersection is suffix-anchored via hostAllowed (entry
// "example.com" matches "example.com" and "api.example.com").
// Callers can SHRINK the operator's static allowlist this way; they
// cannot widen it, because every entry in callerAllowed is checked
// against the operator list before being kept.
//
// Important: when an operator-level allowlist is empty (HTTP's
// HostAllowlist is nil/empty), that's already a deny-all signal at
// the HTTP layer. Narrowing must preserve it — the caller cannot
// supply allowed_hosts:["evil.com"] and bypass the operator's
// deny-all default. intersectHosts enforces this.
//
// callerAllowed semantics, set at the HTTP layer:
//   - nil          — no narrowing; pass tools through untouched.
//   - []           — deny all; the wrapped tools have an empty
//     allowlist and refuse every call.
//   - [hosts...]   — intersect with the operator's list.
//
// wsFilterMode applies only to WebSearch:
//   - WebSearchFilterDrop (default when callerAllowed is non-nil)
//     drops Brave results whose host isn't in the intersected list.
//   - WebSearchFilterKeep returns Brave's results unchanged; the
//     caller filters downstream.
func NarrowHosts(in []tools.Tool, callerAllowed []string, wsFilterMode string) []tools.Tool {
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

// narrowHTTP value-copies an HTTP and replaces its allowlist with the
// intersection. All other config (timeouts, byte caps, AllowPrivateIPs)
// carries over unchanged.
func narrowHTTP(orig *HTTP, callerAllowed []string) *HTTP {
	narrowed := *orig
	narrowed.HostAllowlist = intersectHosts(orig.HostAllowlist, callerAllowed)
	return &narrowed
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
