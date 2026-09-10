package teamrun

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The team-workflow variable expander. It carries a small non-secret value
// across states — a webhook's delivery id, a timestamp, an attempt counter —
// which the threaded `Task.Input` string alone cannot do.
//
// It is DELIBERATELY NOT a typed variable bus: no ports, no [nodeId, key]
// reference graph, no value types. That is a dataflow engine's model, and here
// it would duplicate what agents already do with JSON. This is string
// substitution over a flat map with a closed token set.
//
// Modelled on internal/tools/mcp/http/substitute.go, which established the
// convention this extends: one regex per family, disjoint so order does not
// matter, POSIX `:-` fallback, and a pure function.

// varRe matches the workflow variable tokens:
//
//	${var.<name>}              — bare
//	${var.<name>:-FALLBACK}    — POSIX-style default
//
// <name> is [a-zA-Z0-9_-]{1,64}, the same charset the credentials validator
// enforces, so an operator typo like `${var.foo bar}` does not silently match.
//
// Capture groups: 1 = name, 2 = fallback (empty unless the `:-` form is used).
var varRe = regexp.MustCompile(`\$\{var\.([a-zA-Z0-9_-]{1,64})(?::-(.*?))?\}`)

// tokenRe matches the built-in tokens. A FIXED ENUM, not an expression
// language — the list stays tiny on purpose, and `${now.*}` / `${team.*}` are
// genuinely new because the runtime had no time or walk-position token at all.
var tokenRe = regexp.MustCompile(`\$\{(now\.iso8601|now\.unix|now\.date|team\.state|team\.iteration)\}`)

// placeholderDelims is what a resolved variable value may never contain. See
// Expand for why this is a security rule rather than hygiene.
const (
	openDelim  = "{{"
	closeDelim = "}}"
)

// Env is everything the expander may read. Pure input: Expand never reads a
// clock, a store, or a context.
type Env struct {
	Vars      map[string]string
	Now       time.Time
	State     string
	Iteration int
}

// Expand substitutes ${var.*} and the built-in ${now.*} / ${team.*} tokens in s.
//
// NON-SECRET POSTURE: an unresolved bare token substitutes to EMPTY and the
// surrounding text is kept. It never drops its container the way
// ${run.user_bearer} does — a variable is non-secret by construction (see
// teamgraph.Validate, which refuses to bind one from the credentials namespace),
// so a missing one is a blank, not a leak.
//
// A REFUSED VALUE IS A SECURITY BOUNDARY, NOT HYGIENE. A value containing `{{`
// or `}}` is dropped (empty, and named in `refused`) rather than substituted.
// Variables are bound from attacker-influenceable sources — an inbound webhook
// body is explicitly UNTRUSTED, and a channel message can be agent-written —
// and the composed prompt is later scanned for `{{…}}` placeholders that the
// runtime resolves UNDER ITS OWN AUTHORITY, ungated by the agent's tools or
// memory scopes. Letting a variable value carry those delimiters would let
// whoever controls the payload synthesise a placeholder the runtime then
// executes: an ungated read primitive reachable from an untrusted string.
//
// This is one half of the defence. The other is that placeholder arguments
// resolve their variables INSIDE the matched placeholder rather than in a
// pre-pass over the whole template, so no substitution can introduce a NEW
// placeholder for a later pass to find. Belt and braces on purpose — this half
// survives someone later reordering the passes for a good-looking reason.
//
// Pure and concurrent-safe: reads env, returns new strings.
func Expand(s string, env Env) (out string, refused []string) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	out = varRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := varRe.FindStringSubmatch(m)
		name, fallback := sub[1], sub[2]
		v, ok := env.Vars[name]
		if ok && v != "" {
			if strings.Contains(v, openDelim) || strings.Contains(v, closeDelim) {
				refused = append(refused, name)
				return ""
			}
			return v
		}
		if strings.Contains(m, ":-") {
			return fallback
		}
		return "" // bare + unresolved → empty, never dropped
	})
	out = tokenRe.ReplaceAllStringFunc(out, func(m string) string {
		switch m {
		case "${now.iso8601}":
			return env.Now.UTC().Format(time.RFC3339)
		case "${now.unix}":
			return strconv.FormatInt(env.Now.Unix(), 10)
		case "${now.date}":
			return env.Now.UTC().Format("2006-01-02")
		case "${team.state}":
			return env.State
		case "${team.iteration}":
			return strconv.Itoa(env.Iteration)
		}
		return ""
	})
	return out, refused
}
