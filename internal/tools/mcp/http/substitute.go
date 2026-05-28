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

// runCredRe matches the v1.x per-tool credentials template tokens
// (RFC F). Two forms supported, mirroring the user_bearer shape:
//
//	${run.credentials.<name>}              — strict (no fallback)
//	${run.credentials.<name>:-FALLBACK}    — POSIX-style default
//
// <name> matches the same [a-zA-Z0-9_-]{1,64} the wire-entry-point
// validator enforces. Operators using keys outside that charset
// fail validation upstream; the regex stays strict here so an
// operator-typo'd `${run.credentials.foo bar}` in their yaml
// doesn't silently match.
//
// Capture groups:
//
//	1: credential name
//	2: fallback (when `:-` form is used; empty otherwise)
var runCredRe = regexp.MustCompile(`\$\{run\.credentials\.([a-zA-Z0-9_-]{1,64})(?::-(.*?))?\}`)

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

// substituteCredentialRefs replaces ${run.credentials.<name>} and
// ${run.credentials.<name>:-FALLBACK} tokens in s using values from
// the creds map. RFC F per-tool credentials substitution; sibling
// to substituteRunVars (which handles the legacy ${run.user_bearer}
// single-bearer form).
//
// drop is true iff at least one bare ${run.credentials.<name>}
// (no fallback) remained unresolved because creds[<name>] was
// empty or absent. The caller drops the entire header in that case
// rather than sending a literal placeholder downstream — same
// posture as substituteRunVars.
//
// missing accumulates the names of unresolved bare credentials so
// the caller can emit a single triage log line per outbound request
// instead of one per substitution. Empty when no bare credential
// went unresolved.
//
// Behaviour matrix (for one ${run.credentials.X} token):
//
//	creds[X]=""    creds[X]="V"
//	${...X}             drop+missing+="X"  ("V", drop unchanged)
//	${...X:-FB}         ("FB", -)          ("V", -)
//	X absent, ${...X}   drop+missing+="X"  (n/a)
//	X absent, ${...X:-FB} ("FB", -)        (n/a)
//
// Concurrent callers may invoke this on the same s; the function
// reads creds only — never mutates.
func substituteCredentialRefs(s string, creds map[string]string) (out string, drop bool, missing []string) {
	out = runCredRe.ReplaceAllStringFunc(s, func(m string) string {
		// Re-match to get the capture groups.
		sub := runCredRe.FindStringSubmatch(m)
		// sub[0] = whole match; sub[1] = name; sub[2] = fallback (if any).
		name := sub[1]
		value, ok := creds[name]
		if ok && value != "" {
			return value
		}
		// Resolve to fallback when the `:-` form is present.
		if strings.Contains(m, ":-") {
			return sub[2]
		}
		// Bare token, no fallback, missing/empty value → caller drops.
		drop = true
		missing = append(missing, name)
		return ""
	})
	return out, drop, missing
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
