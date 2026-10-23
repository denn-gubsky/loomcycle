package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EY §3.7 — a run's capability is its agent definition's grants
// INTERSECTED with the originating principal's ACL. The entry points call
// these at run start on what the definition resolved to; a sub-agent's own
// definition is narrowed against the same set (RunPolicy.ACL inherits).
//
// Every helper is mode-aware through authz.Evaluate: compat drops only what
// a rule explicitly denies, enforce keeps only what a rule allows, shadow
// drops nothing. An enforce-mode run with no compiled set keeps nothing: the
// gate that admitted the run should already have refused it.

// aclPermits is the one question: may the originating principal do action
// on the resource, under the mode stamped on ctx.
func aclPermits(ctx context.Context, eff *authz.Effective, action string, res authz.Resource) bool {
	r := authz.Evaluate(ctx, eff, action, res, true)
	return r.Allowed && !r.Unavailable
}

// NarrowToolsByACL returns the tools the principal may invoke and the names
// it may not. A removed tool is withheld from the model's schema, not merely
// refused on call (owner decision D6).
func NarrowToolsByACL(ctx context.Context, eff *authz.Effective, in []Tool) ([]Tool, []string) {
	if eff == nil && authz.ModeFrom(ctx) != authz.ModeEnforce {
		return in, nil
	}
	kept := make([]Tool, 0, len(in))
	var removed []string
	for _, t := range in {
		if aclPermits(ctx, eff, "invoke", authz.ResourceForTool(t.Name())) {
			kept = append(kept, t)
		} else {
			removed = append(removed, t.Name())
		}
	}
	return kept, removed
}

// NarrowVolumesByACL keeps each binding the principal may read, downgrades
// to read-only one it may read but not write, and drops one it may not
// read. An inactive policy is returned as is.
func NarrowVolumesByACL(ctx context.Context, eff *authz.Effective, v VolumePolicyValue) VolumePolicyValue {
	if !v.Active || (eff == nil && authz.ModeFrom(ctx) != authz.ModeEnforce) {
		return v
	}
	out := VolumePolicyValue{Active: true}
	for _, b := range v.Bindings {
		res := authz.Resource{Type: "volume", Name: b.Name}
		if !aclPermits(ctx, eff, "read", res) && !aclPermits(ctx, eff, "write", res) {
			continue // write implies read (ActionForVolumeMode); neither → gone
		}
		nb := b
		if !nb.ReadOnly && !aclPermits(ctx, eff, "write", res) {
			nb.ReadOnly = true
		}
		out.Bindings = append(out.Bindings, nb)
	}
	return out
}

// NarrowScopesByACL keeps the scopes of a keyspace type (memory, sqlmem,
// history) the principal may read; a scope it may not read is dropped
// whole. A write-only denial is not expressible in the policy values yet
// and is left to a later per-call check.
func NarrowScopesByACL(ctx context.Context, eff *authz.Effective, typ string, scopes []string) []string {
	if len(scopes) == 0 || (eff == nil && authz.ModeFrom(ctx) != authz.ModeEnforce) {
		return scopes
	}
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if aclPermits(ctx, eff, "read", authz.Resource{Type: typ, Name: scopeResourceName(sc)}) {
			out = append(out, sc)
		}
	}
	return out
}

// scopeResourceName maps a policy scope word to the catalog's name for it:
// the history policy's `self` is the catalog's `self`, the memory tool's
// scopes are their own names, and a legacy alias is passed through.
func scopeResourceName(scope string) string { return scope }

// NarrowSkillsByACL appends the principal's unconditional skill denies to
// the agent's `skills:` pattern list as `-glob` entries, which the skill
// matcher already honours. An enforce-mode principal with no skill allow
// at all denies every skill.
func NarrowSkillsByACL(ctx context.Context, eff *authz.Effective, sp SkillPolicyValue) SkillPolicyValue {
	if eff == nil {
		if authz.ModeFrom(ctx) == authz.ModeEnforce {
			return SkillPolicyValue{Patterns: []string{"-*"}, Granted: sp.Granted}
		}
		return sp
	}
	out := SkillPolicyValue{Granted: sp.Granted, Patterns: append([]string(nil), sp.Patterns...)}
	if authz.ModeFrom(ctx) == authz.ModeEnforce && !aclPermits(ctx, eff, "invoke", authz.Resource{Type: "skill", Name: authz.AnyInstance}) {
		out.Patterns = append(out.Patterns, "-*")
		return out
	}
	for _, pat := range eff.DenyPatterns("skill", "invoke") {
		if pat == "*" || pat == "**" {
			out.Patterns = append(out.Patterns, "-*")
			continue
		}
		out.Patterns = append(out.Patterns, "-"+pat)
	}
	return out
}
