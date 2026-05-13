package http

import (
	"regexp"
	"strings"
)

// runBearerRe matches the v0.8.x per-run MCP bearer template tokens
// in a header value. Two forms supported:
//
//	${run.user_bearer}              — strict (no fallback)
//	${run.user_bearer:-FALLBACK}    — POSIX-style default
//
// The lazy `.*?` for the fallback group is safe because expandEnv
// (internal/config/config.go) has already resolved any inner
// ${LOOMCYCLE_*} at yaml-load time. No nested `}` remains at request
// time; bearer tokens themselves cannot contain `}` per the
// [A-Za-z0-9._\-+/=]{16,512} charset validated at the HTTP boundary.
var runBearerRe = regexp.MustCompile(`\$\{run\.user_bearer(?::-(.*?))?\}`)

// substituteRunVars replaces ${run.user_bearer} and
// ${run.user_bearer:-FALLBACK} tokens in s with bearer (or the
// fallback when bearer is empty). It is a pure function — concurrent
// callers may invoke it on the same s without coordination.
//
// drop is true iff a bare ${run.user_bearer} token (no fallback)
// remained unresolved because bearer was empty. The caller (typically
// Client.do) should drop the entire header in that case rather than
// send a literal "${run.user_bearer}" placeholder downstream.
//
// Behaviour matrix:
//
//	s contains                          bearer=""        bearer="X"
//	-----------------------------------------------------------------
//	(no ${run.*} token)                 (s, false)       (s, false)
//	${run.user_bearer}                  ("", true)       ("X", false)
//	${run.user_bearer:-FB}              ("FB", false)    ("X", false)
//	mixed bare + fallback               (mixed, true)    ("X...X", false)
func substituteRunVars(s, bearer string) (string, bool) {
	drop := false
	out := runBearerRe.ReplaceAllStringFunc(s, func(m string) string {
		if bearer != "" {
			return bearer
		}
		// bearer empty — use the fallback if the token had one.
		if i := strings.Index(m, ":-"); i >= 0 {
			// Strip the trailing "}" to recover the fallback text.
			return m[i+2 : len(m)-1]
		}
		// Bare token, no fallback → caller drops the header.
		drop = true
		return ""
	})
	return out, drop
}

// tokenPrefix returns a triage-safe 4-char prefix + ellipsis for a
// bearer-shaped string, or "(empty)" for the empty string. Used only
// on WARN paths; never invoked on the happy path. Satisfies CLAUDE.md
// rule 4: bearer tokens never appear in logs in full.
func tokenPrefix(s string) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) <= 4 {
		return s + "…"
	}
	return s[:4] + "…"
}
