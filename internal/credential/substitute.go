package credential

import (
	"context"
	"regexp"
)

// credTokenRe matches a $cred:<name> reference — the RFC AR binding syntax used
// in an MCPServerDef's env / headers / args (and other Def fields). <name> uses
// the same charset the credentialdef tool validates.
var credTokenRe = regexp.MustCompile(`\$cred:([A-Za-z0-9_-]{1,128})`)

// HasRef reports whether s contains any $cred:<name> token (a cheap pre-check
// so callers skip the resolve path entirely when there's nothing to bind).
func HasRef(s string) bool { return credTokenRe.MatchString(s) }

// RefNames returns the <name> of every $cred:<name> token in s (nil when none).
// The resolver fast-path uses it to report refs as unresolved WITHOUT a store
// read when the engine can't resolve anything (CanResolve()==false) — so the
// caller still drops those headers/env entries instead of sending a literal
// "$cred:foo" downstream.
func RefNames(s string) []string {
	ms := credTokenRe.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m[1])
	}
	return out
}

// Substitute replaces every $cred:<name> token in s with the resolved plaintext
// credential for the given run identity (scope precedence agent > user > tenant,
// so a user's own token shadows the tenant default). This is the runtime-side
// bind: the plaintext is injected into an outbound MCP request header / child
// process env — never into the agent transcript.
//
//   - register, if non-nil, is called with each resolved plaintext so the caller
//     can add it to a redactor (defence against a downstream echo).
//   - unresolved names (no such credential in any visible scope) are LEFT as the
//     literal token and returned in `unresolved` — the caller drops the header /
//     env entry rather than sending a literal "$cred:foo" downstream.
//   - a hard resolve error (store fault / decrypt failure) aborts and is
//     returned; the partially-substituted string is discarded by the caller.
func (e *Engine) Substitute(ctx context.Context, tenantID, agentName, userID, s string, register func(string)) (out string, unresolved []string, err error) {
	if !credTokenRe.MatchString(s) {
		return s, nil, nil
	}
	var firstErr error
	out = credTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		name := credTokenRe.FindStringSubmatch(m)[1]
		res, found, rerr := e.Resolve(ctx, tenantID, agentName, userID, name)
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return m
		}
		if !found {
			unresolved = append(unresolved, name)
			return m
		}
		if register != nil {
			register(res.Value)
		}
		return res.Value
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	return out, unresolved, nil
}
