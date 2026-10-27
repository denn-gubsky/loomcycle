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
// whole. A scope it may read but not write stays, and ReadOnlyScopesByACL
// names it.
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

// ReadOnlyScopesByACL returns the scopes of a writable keyspace type (memory,
// sqlmem) the principal may not write, out of the scopes NarrowScopesByACL
// kept — the policy value's ReadOnlyScopes. Mode-aware like the rest: compat
// lists a scope only under an explicit write deny, enforce lists every scope
// no rule lets it write, shadow lists none (ShadowACLNarrowing reports them).
// Unlike a volume, write here does not imply read: a scope the principal may
// write but not read was already dropped whole.
func ReadOnlyScopesByACL(ctx context.Context, eff *authz.Effective, typ string, scopes []string) []string {
	if len(scopes) == 0 || (eff == nil && authz.ModeFrom(ctx) != authz.ModeEnforce) {
		return nil
	}
	var out []string
	for _, sc := range scopes {
		if !aclPermits(ctx, eff, "write", authz.Resource{Type: typ, Name: scopeResourceName(sc)}) {
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
// at all denies every skill. Shadow mode appends nothing, as it narrows
// nothing else; ShadowACLNarrowing names what enforce would append.
func NarrowSkillsByACL(ctx context.Context, eff *authz.Effective, sp SkillPolicyValue) SkillPolicyValue {
	if eff == nil {
		if authz.ModeFrom(ctx) == authz.ModeEnforce {
			return SkillPolicyValue{Patterns: []string{"-*"}, Granted: sp.Granted}
		}
		return sp
	}
	if authz.ModeFrom(ctx) == authz.ModeShadow {
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

// ACLShadowDivergence is one narrowing enforce mode would apply to a run at
// its start that shadow mode left in place: the question and the engine's
// answer to it.
type ACLShadowDivergence struct {
	Surface  authz.Surface
	Decision authz.Decision
}

// ShadowACLNarrowing names what the narrowing helpers above would take from
// a run's policy under enforce — each tool withheld, each volume made
// read-only or dropped, each memory, SQL-memory or history scope dropped,
// each memory or SQL-memory scope made read-only, and
// each skill deny it would append to the agent's patterns (every skill when
// no rule allows one) — given the policy shadow mode built,
// which took nothing. Outside shadow mode, or with no compiled set, it
// returns nil: compat and enforce applied their answer already, and a run
// with no set has none to compare.
//
// "Would take" is the enforce answer, so a question no rule allows counts
// as well as a rule's deny (enforce treats both as a refusal).
func ShadowACLNarrowing(ctx context.Context, p RunPolicy) []ACLShadowDivergence {
	eff := p.ACL
	if eff == nil || authz.ModeFrom(ctx) != authz.ModeShadow {
		return nil
	}
	enforce := authz.WithMode(ctx, authz.ModeEnforce)
	refused := func(action string, res authz.Resource) (authz.Decision, bool) {
		r := authz.Evaluate(enforce, eff, action, res, true)
		return r.Decision, !r.Allowed
	}
	var out []ACLShadowDivergence
	add := func(action string, res authz.Resource, d authz.Decision) {
		out = append(out, ACLShadowDivergence{Surface: authz.Surface{Action: action, Type: res.Type, Name: res.Name}, Decision: d})
	}
	for _, name := range p.Tools {
		res := authz.ResourceForTool(name)
		if d, no := refused("invoke", res); no {
			add("invoke", res, d)
		}
	}
	if p.Volumes.Active {
		for _, b := range p.Volumes.Bindings {
			res := authz.Resource{Type: "volume", Name: b.Name}
			// As NarrowVolumesByACL: write implies read, so a binding goes
			// only when neither is allowed (recorded as the read refused),
			// and a read-write one becomes read-only when write is refused.
			rd, readNo := refused("read", res)
			wd, writeNo := refused("write", res)
			switch {
			case readNo && writeNo:
				add("read", res, rd)
			case writeNo && !b.ReadOnly:
				add("write", res, wd)
			}
		}
	}
	for _, ks := range []struct {
		typ      string
		scopes   []string
		writable bool
	}{{"memory", p.Memory.AllowedScopes, true}, {"sqlmem", p.SqlMem.AllowedScopes, true}, {"history", p.History.Scopes, false}} {
		for _, sc := range ks.scopes {
			res := authz.Resource{Type: ks.typ, Name: scopeResourceName(sc)}
			// As NarrowScopesByACL then ReadOnlyScopesByACL: an unreadable
			// scope is dropped (recorded as the read refused); a readable one
			// that may not be written becomes read-only.
			if d, no := refused("read", res); no {
				add("read", res, d)
			} else if d, no := refused("write", res); ks.writable && no {
				add("write", res, d)
			}
		}
	}
	if p.Skill.Granted {
		// As NarrowSkillsByACL under enforce: no skill allowed at all appends
		// "-*" alone; otherwise each unconditional deny is appended as its
		// own pattern, named here by its selector as written.
		res := authz.Resource{Type: "skill", Name: authz.AnyInstance}
		if d, no := refused("invoke", res); no {
			add("invoke", res, d)
		} else {
			for _, pat := range eff.DenyPatterns("skill", "invoke") {
				add("invoke", authz.Resource{Type: "skill", Name: pat}, skillDenyDecision(enforce, eff, pat))
			}
		}
	}
	return out
}

// skillDenyDecision is the engine's answer for a skill deny pattern asked as
// a name: a selector matches its own text, so the unconditional deny that
// wrote it decides, and its rule is named. Should the engine answer
// otherwise, the deny is still reported, with no rule named.
func skillDenyDecision(ctx context.Context, eff *authz.Effective, pat string) authz.Decision {
	r := authz.Evaluate(ctx, eff, "invoke", authz.Resource{Type: "skill", Name: pat}, true)
	if r.Decision.Outcome == authz.Denied {
		return r.Decision
	}
	return authz.Decision{Outcome: authz.Denied}
}
