// varinject.go — the ${var.*} / ${now.*} / ${team.*} half of the single pass.
//
// WHY IT LIVES HERE, beside the {{...}} families rather than beside the walk
// that supplies the values. Variables are substituted into the SAME text the
// placeholder families are expanded in, and the ordering between them is a
// security boundary, not a preference:
//
// If variables were resolved in an earlier pass — as they were until this file
// existed — a value could introduce a `{{` that the LATER placeholder pass then
// reads as an operator-authored placeholder. Variables bind from
// attacker-influenceable sources (an inbound webhook body is explicitly
// UNTRUSTED; a channel message can be agent-written), and the runtime resolves
// placeholders under its OWN authority, ungated by the agent's tools or memory
// scopes. That is an ungated read primitive reachable from an untrusted string.
//
// Making them ALTERNATIVES OF ONE PASS removes the ordering entirely. A
// substitution's output is never rescanned within a single
// ReplaceAllStringFunc, so a variable cannot produce a placeholder and a
// placeholder body cannot produce a variable — by construction, not by charset
// checks that the next person to touch this could relax.
package memory

import (
	"regexp"
	"strings"
)

// varPlaceholderPattern matches the workflow variable tokens:
//
//	${var.<name>}   ${var.<name>:-FALLBACK}   ${now.date}   ${team.state}
//
// The namespace is a CLOSED set — var / now / team — so this can never match
// `${run.tenant_id}`, `${HOME}`, or any other `${...}` an operator has written
// in a prompt for their own reasons. Combined with the caller-supplied Values
// map being nil for every non-team run, a prompt that does not use workflow
// variables is byte-identical to before this existed.
//
// The FALLBACK charset excludes `{` and `}`: operator-authored text that cannot
// introduce a placeholder delimiter either, so the guarantee holds on both
// halves of the token.
const varPlaceholderPattern = `\$\{((?:var|now|team)\.[a-zA-Z0-9_-]{1,64})(?::-([^{}]*?))?\}`

var varPlaceholderRe = regexp.MustCompile(varPlaceholderPattern)

// expandVarPlaceholder renders one ${...} match from values.
//
// NON-SECRET POSTURE: an unresolved bare token substitutes to EMPTY and keeps
// its surroundings. It never drops its container the way ${run.user_bearer}
// does — a workflow variable cannot be a secret (teamgraph.Validate refuses to
// bind one from the credentials namespace), so a blank is harmless where
// dropping would silently strip operator-authored text.
//
// A value carrying `{{` or `}}` is REFUSED — dropped, and named in refused.
// This is belt to the single pass's braces: the pass already makes it
// impossible for a substituted value to be re-read as a placeholder, and this
// survives someone later reintroducing an ordering for a good-looking reason.
func expandVarPlaceholder(match string, values map[string]string, refused *[]string) string {
	sub := varPlaceholderRe.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	name, fallback := sub[1], sub[2]
	v, ok := values[name]
	if ok && v != "" {
		if strings.Contains(v, "{{") || strings.Contains(v, "}}") {
			*refused = append(*refused, name)
			return ""
		}
		return v
	}
	if strings.Contains(match, ":-") {
		return fallback
	}
	return ""
}
