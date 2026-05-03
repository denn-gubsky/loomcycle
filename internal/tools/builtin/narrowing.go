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
			ws.AllowedHosts = intersectHosts(v.AllowedHosts, callerAllowed)
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
// permitted by the operator's static list. operator==nil (operator has
// no static list set, e.g. test config) means the caller's list passes
// through as-is. caller==nil means no narrowing — but NarrowHosts
// short-circuits before calling this in that case; we never reach
// here with caller==nil.
//
// The check uses hostAllowed in BOTH directions: an operator entry of
// "example.com" allows a caller entry of "api.example.com" (more
// specific narrowing is fine). A caller entry of "example.org" is
// rejected if the operator only has "example.com".
func intersectHosts(operator, caller []string) []string {
	if len(operator) == 0 {
		return append([]string(nil), caller...)
	}
	out := make([]string, 0, len(caller))
	for _, c := range caller {
		if hostAllowed(c, operator) {
			out = append(out, c)
		}
	}
	return out
}
