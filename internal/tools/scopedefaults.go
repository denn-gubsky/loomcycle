package tools

import "context"

// DenyAllScopes is the explicit "grant nothing" value for a capability-scope
// list. It exists because absence and emptiness cannot be told apart on the
// wire: the substrate's agent shape tags every scope list `omitempty`, so a
// stored `memory_scopes: []` serialises to nothing and reads back as nil,
// indistinguishable from an operator who never set the field.
//
// A VALUE survives that round-trip where a presence distinction cannot. The
// spelling matches the house idiom for the same problem elsewhere — `skills:
// [-*]` is how an agent is denied every skill.
const DenyAllScopes = "-*"

// deniedAll reports whether a declared list is an explicit refusal.
func deniedAll(declared []string) bool {
	for _, s := range declared {
		if s == DenyAllScopes {
			return true
		}
	}
	return false
}

// cloneScopes copies a declared list before handing it out.
//
// The resolvers are given the LIVE slice off a shared config.AgentDef, and the
// result is stored in a policy value that outlives the call. Returning the
// definition's own slice means a later append by any consumer writes into the
// definition's backing array — and when the append fits in spare capacity it
// does so INVISIBLY: the definition's length-view still compares equal, so a
// test that diffs the slice sees nothing while the array behind it has changed.
func cloneScopes(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// EffectiveMemoryScopes resolves the Memory tool's scope allowlist for a run.
//
// WHY THIS EXISTS AT ALL. `tools` and `memory_scopes` are two independent
// grants, and an empty second one silently voids the first: an agent holding
// Memory with no scope list is refused at call time, mid-task, with nothing
// having said so beforehand. The default closes the common case — a caller
// reaching their OWN data — while leaving every grant that confers authority
// over someone else default-deny.
//
// WHY IT RESOLVES HERE AND NOT IN THE DEFINITION. memory_scopes is
// content-identifying: it is hashed into the agent's content_sha256 with
// `omitempty`, so an empty list is ABSENT from the hashed JSON. Writing a
// default into the definition would turn `[]` into `["user"]`, change the hash,
// and fork every affected agent on upgrade. Resolving downstream of the hash
// leaves every stored definition byte-identical.
//
// The rules, in order:
//
//   - an explicit DenyAllScopes entry means the operator said no — honoured,
//     and the ONLY way to express it now that the default exists;
//   - a non-empty list is used verbatim, defaults never widen it;
//   - otherwise the default is the caller's own data: `user`, plus `tenant`
//     when the run is a non-isolated member of one.
//
// The `user` default is gated on the run actually carrying a user id, because
// the `user` scope resolves its scope_id from exactly that — defaulting a
// userless run to `user` would hand back a grant that cannot resolve, which is
// the same silent-uselessness this function exists to remove. Such a run stays
// default-deny and is reported by the inert-grant warning instead.
//
// `tenant` mirrors ConfineIsolatedScope rather than relying on it. That gate
// refuses tenant/global for an isolated run independently, so naming `tenant`
// here for an isolated caller would be honest about intent and wrong about
// effect — the list this returns is the list that will actually be honoured.
func EffectiveMemoryScopes(ctx context.Context, declared []string) []string {
	if deniedAll(declared) {
		return nil
	}
	if len(declared) > 0 {
		return cloneScopes(declared)
	}
	id := RunIdentity(ctx)
	if id.UserID == "" {
		return nil
	}
	scopes := []string{"user"}
	if id.TenantID != "" && !id.Isolated {
		scopes = append(scopes, "tenant")
	}
	return scopes
}

// EffectiveHistoryScopes resolves the History tool's owner-scope gate for a run.
//
// Same shape and the same reasons as EffectiveMemoryScopes. The default is
// `user` alone: a user always has access to their own chats, and that is the
// whole of what they own. `self`, `tenant` and `global` all reach beyond it —
// `self` is the AGENT's chats across users, not the caller's — so none of them
// is defaulted.
func EffectiveHistoryScopes(ctx context.Context, declared []string) []string {
	if deniedAll(declared) {
		return nil
	}
	if len(declared) > 0 {
		return cloneScopes(declared)
	}
	if RunIdentity(ctx).UserID == "" {
		return nil
	}
	return []string{"user"}
}
