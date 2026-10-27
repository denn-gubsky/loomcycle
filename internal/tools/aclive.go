package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RFC EY §3.7, Q-B — live re-evaluation of the caller's ACL inside a run
// (LOOMCYCLE_ACL_LIVE=1). A run's set is otherwise frozen at its start: a
// binding removed while a long interactive run is in flight reaches it only
// when it ends. With a LiveACL on the run's policy, the run re-reads its
// caller's set at every iteration boundary (before the model call) and
// before every tool call, and holds its policy to
//
//	current ∩ start
//
// (authz.Effective.Within): a tool, volume write, keyspace or skill the
// current rules refuse is withdrawn on the next call, and nothing the run
// started without comes back, whatever a binding added since grants. A
// binding restored after a mid-run removal gives back what it took, up to
// the start set.
//
// The re-read is the resolver's cached lookup (aclres.ResolveSubject): the
// compiled set of an unchanged acl_version is the same pointer, so an
// unchanged set costs a map read and changes nothing. A tool call already
// running when the set changes finishes; the next call is asked again.

// LiveResolveFunc re-resolves the CURRENT set of the subject a set was
// compiled for (aclres.Resolver.ResolveSubject).
type LiveResolveFunc func(ctx context.Context, subj authz.Subject) (*authz.Effective, error)

// LiveACL is one run's live view of its originating caller's set. Each run
// owns one (a child builds its own from its parent's set at spawn, so each
// run's transcript reports its own narrowing); it is safe for concurrent
// use by a run's parallel tool calls.
type LiveACL struct {
	subject authz.Subject
	resolve LiveResolveFunc
	state   atomic.Pointer[liveState]
}

// liveState is one reading of the caller's set. It is replaced, never
// changed, except for its memo.
type liveState struct {
	// latest is the caller's set as last read.
	latest *authz.Effective
	// fault is the error of the last re-read when it failed; latest is then
	// the reading before it.
	fault error

	mu   sync.Mutex
	memo map[*authz.Effective]*authz.Effective // a run's start set → latest held to it
}

// within is latest held to start, the same pointer for the same start so a
// set compared by identity (the shadow record, a cache key) stays stable.
func (st *liveState) within(start *authz.Effective) *authz.Effective {
	st.mu.Lock()
	defer st.mu.Unlock()
	if eff, ok := st.memo[start]; ok {
		return eff
	}
	if st.memo == nil {
		st.memo = map[*authz.Effective]*authz.Effective{}
	}
	eff := st.latest.Within(start)
	st.memo[start] = eff
	return eff
}

// NewLiveACL returns the live view of start's subject, reading its current
// set with resolve. nil when there is nothing to follow (no set, or a set
// with no subject to resolve again).
func NewLiveACL(start *authz.Effective, resolve LiveResolveFunc) *LiveACL {
	if start == nil || resolve == nil || start.Subject.ID == "" {
		return nil
	}
	l := &LiveACL{subject: start.Subject, resolve: resolve}
	l.state.Store(&liveState{latest: start})
	return l
}

// rawPolicy is the policy ctx carries, before any live narrowing.
func rawPolicy(ctx context.Context) RunPolicy {
	p, _ := ctx.Value(ctxKeyRunPolicy{}).(RunPolicy)
	return p
}

// applyLive is p narrowed by its caller's current set held to p's own
// (PolicyOf calls it). It reads the last reading only; RefreshLiveACL is
// what re-reads.
func applyLive(ctx context.Context, p RunPolicy) RunPolicy {
	l := p.LiveACL
	if l == nil || p.ACL == nil {
		return p
	}
	st := l.state.Load()
	if st.latest == p.ACL || !sameSubject(st.latest.Subject, p.ACL.Subject) {
		return p
	}
	return narrowPolicyByACL(ctx, p, st.within(p.ACL))
}

// sameSubject: the set a run carries is its live view's caller. A policy
// that carries another subject's set (a walk stamped by a trigger owner) is
// not narrowed by this caller's.
func sameSubject(a, b authz.Subject) bool {
	return a.TenantID == b.TenantID && a.ID == b.ID && a.TokenDefID == b.TokenDefID && a.Class == b.Class
}

// narrowPolicyByACL applies the run-start narrowings again, with eff: tools
// it may not invoke leave the tool list (and are added to ACLRemovedTools),
// volumes, keyspaces and skills narrow as NarrowVolumesByACL,
// NarrowScopesByACL and NarrowSkillsByACL do, and eff becomes the set every
// per-call question is asked of. Every slice is replaced, never written
// through (p is a shallow copy of the ctx's policy).
func narrowPolicyByACL(ctx context.Context, p RunPolicy, eff *authz.Effective) RunPolicy {
	var removed []string
	kept := make([]string, 0, len(p.Tools))
	for _, name := range p.Tools {
		if aclPermits(ctx, eff, "invoke", authz.ResourceForTool(name)) {
			kept = append(kept, name)
		} else {
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		p.Tools = kept
		p.ACLRemovedTools = appendNew(p.ACLRemovedTools, removed)
	}
	p.Volumes = NarrowVolumesByACL(ctx, eff, p.Volumes)
	p.Memory.AllowedScopes = NarrowScopesByACL(ctx, eff, "memory", p.Memory.AllowedScopes)
	p.Memory.ReadOnlyScopes = appendNew(p.Memory.ReadOnlyScopes, ReadOnlyScopesByACL(ctx, eff, "memory", p.Memory.AllowedScopes))
	p.SqlMem.AllowedScopes = NarrowScopesByACL(ctx, eff, "sqlmem", p.SqlMem.AllowedScopes)
	p.SqlMem.ReadOnlyScopes = appendNew(p.SqlMem.ReadOnlyScopes, ReadOnlyScopesByACL(ctx, eff, "sqlmem", p.SqlMem.AllowedScopes))
	p.History.Scopes = NarrowScopesByACL(ctx, eff, "history", p.History.Scopes)
	// The start set's skill denies are already on the patterns, and eff
	// repeats them (it is held to that set): append only the new ones.
	sk := NarrowSkillsByACL(ctx, eff, p.Skill)
	if len(sk.Patterns) >= len(p.Skill.Patterns) {
		sk.Patterns = appendNew(append([]string(nil), p.Skill.Patterns...), sk.Patterns[len(p.Skill.Patterns):])
	}
	p.Skill = sk
	p.ACL = eff
	return p
}

// appendNew is base with each of add not already in it, in order; base is
// copied, never appended to in place.
func appendNew(base, add []string) []string {
	seen := make(map[string]bool, len(base)+len(add))
	out := append([]string(nil), base...)
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RefreshLiveACL re-reads the caller's set of the run on ctx when it has a
// live view, and reports what the change took from (or gave back to) this
// run. No live view (LOOMCYCLE_ACL_LIVE off, no set): nothing.
func RefreshLiveACL(ctx context.Context) {
	raw := rawPolicy(ctx)
	if raw.LiveACL == nil {
		return
	}
	raw.LiveACL.refresh(ctx, raw)
}

func (l *LiveACL) refresh(ctx context.Context, raw RunPolicy) {
	old := l.state.Load()
	eff, err := l.resolve(ctx, l.subject)
	if err != nil {
		if ctx.Err() != nil {
			return // the call is being cancelled, not the rules unreadable
		}
		if old.fault == nil && l.state.CompareAndSwap(old, &liveState{latest: old.latest, fault: err}) {
			// Compat and shadow keep the last reading (the legacy posture: fail
			// open to what was known); enforce refuses every tool call until
			// a re-read succeeds (liveToolRefusal).
			slog.WarnContext(ctx, "authz: live ACL re-read failed; keeping the last reading (enforce refuses tool calls until it succeeds)",
				"tenant", l.subject.TenantID, "subject", l.subject.ID, "run_id", RunID(ctx), "mode", string(authz.ModeFrom(ctx)), "err", err)
		}
		return
	}
	if eff == nil || (eff == old.latest && old.fault == nil) {
		return
	}
	next := &liveState{latest: eff}
	if !l.state.CompareAndSwap(old, next) {
		return // a parallel call read it first and reports it
	}
	if old.fault != nil {
		slog.InfoContext(ctx, "authz: live ACL re-read recovered", "tenant", l.subject.TenantID, "subject", l.subject.ID, "run_id", RunID(ctx))
	}
	if eff == old.latest {
		return
	}
	reportLiveChange(ctx, raw, old, next)
}

// reportLiveChange tells the run's transcript what the new reading took
// from its policy — the RFC DE inert-grant event with gate `acl`, as the run
// start reports a tool its caller may not use — and logs what it gave back.
func reportLiveChange(ctx context.Context, raw RunPolicy, old, next *liveState) {
	before, after := livePolicyFor(ctx, raw, old), livePolicyFor(ctx, raw, next)
	emit := EventEmitter(ctx)
	version := next.latest.Version
	inert := func(tool, msg string) {
		emit(providers.Event{
			Type:            providers.EventCapabilityInert,
			Text:            msg,
			CapabilityInert: &providers.CapabilityInertInfo{Tool: tool, Gate: "acl", Message: msg},
		})
	}
	lost, regained := diffNames(before.Tools, after.Tools)
	for _, name := range lost {
		inert(name, fmt.Sprintf("tool %s is no longer allowed by the caller's access-control rules (changed during the run, acl_version %d); it is withheld from the rest of this run", name, version))
	}
	var narrowed []string
	for _, b := range before.Volumes.Bindings {
		switch nb, ok := findBinding(after.Volumes.Bindings, b.Name); {
		case !ok:
			narrowed = append(narrowed, "volume "+b.Name+" dropped")
		case !b.ReadOnly && nb.ReadOnly:
			narrowed = append(narrowed, "volume "+b.Name+" read-only")
		}
	}
	for _, ks := range []struct {
		typ           string
		before, after []string
	}{{"memory", before.Memory.AllowedScopes, after.Memory.AllowedScopes},
		{"sqlmem", before.SqlMem.AllowedScopes, after.SqlMem.AllowedScopes},
		{"history", before.History.Scopes, after.History.Scopes}} {
		gone, _ := diffNames(ks.before, ks.after)
		for _, sc := range gone {
			narrowed = append(narrowed, ks.typ+" scope "+sc+" dropped")
		}
	}
	for _, ks := range []struct {
		typ           string
		before, after []string
	}{{"memory", before.Memory.ReadOnlyScopes, after.Memory.ReadOnlyScopes},
		{"sqlmem", before.SqlMem.ReadOnlyScopes, after.SqlMem.ReadOnlyScopes}} {
		ro, _ := diffNames(ks.after, ks.before)
		for _, sc := range ro {
			narrowed = append(narrowed, ks.typ+" scope "+sc+" read-only")
		}
	}
	if newSkill, _ := diffNames(after.Skill.Patterns, before.Skill.Patterns); len(newSkill) > 0 {
		narrowed = append(narrowed, "skills "+strings.Join(newSkill, ", "))
	}
	if len(narrowed) > 0 {
		inert("", fmt.Sprintf("the caller's access-control rules changed during the run (acl_version %d): %s", version, strings.Join(narrowed, "; ")))
	}
	if len(regained) > 0 {
		slog.InfoContext(ctx, "authz: live ACL gave tools back to the run (within its start set)",
			"run_id", RunID(ctx), "acl_version", version, "tools", regained)
	}
}

// livePolicyFor is raw narrowed by one reading, as applyLive would.
func livePolicyFor(ctx context.Context, raw RunPolicy, st *liveState) RunPolicy {
	if raw.ACL == nil || st.latest == raw.ACL || !sameSubject(st.latest.Subject, raw.ACL.Subject) {
		return raw
	}
	return narrowPolicyByACL(ctx, raw, st.within(raw.ACL))
}

// diffNames is what a has and b lacks, and what b has and a lacks.
func diffNames(a, b []string) (onlyA, onlyB []string) {
	inA := make(map[string]bool, len(a))
	for _, s := range a {
		inA[s] = true
	}
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	return onlyA, onlyB
}

func findBinding(bs []VolumeBinding, name string) (VolumeBinding, bool) {
	for _, b := range bs {
		if b.Name == name {
			return b, true
		}
	}
	return VolumeBinding{}, false
}

// LiveToolSpecs is specs without the tools the run's caller may no longer
// invoke, after re-reading the caller's set — the schema the next model call
// is given (owner decision D6: a refused tool is withheld, not shown and
// refused). specs itself when nothing is withheld, and always with no live
// view.
func LiveToolSpecs(ctx context.Context, specs []providers.ToolSpec) []providers.ToolSpec {
	raw := rawPolicy(ctx)
	if raw.LiveACL == nil {
		return specs
	}
	raw.LiveACL.refresh(ctx, raw)
	eff := applyLive(ctx, raw).ACL
	if eff == raw.ACL || authz.ModeFrom(ctx) == authz.ModeShadow {
		return specs
	}
	out := make([]providers.ToolSpec, 0, len(specs))
	for _, sp := range specs {
		if aclPermits(ctx, eff, "invoke", authz.ResourceForTool(sp.Name)) {
			out = append(out, sp)
		}
	}
	if len(out) == len(specs) {
		return specs
	}
	return out
}

// liveToolRefusal is the refusal of a call to a tool the run's caller may no
// longer invoke, asked after re-reading the caller's set: acl_denied (or
// acl_unavailable when enforce mode cannot read the set), recorded like any
// other in-run ACL refusal. A call the start set already answered — nothing
// changed — is not asked again. Shadow mode refuses nothing, live as at the
// start.
func liveToolRefusal(ctx context.Context, name string) (Result, bool) {
	raw := rawPolicy(ctx)
	if raw.LiveACL == nil {
		return Result{}, false
	}
	raw.LiveACL.refresh(ctx, raw)
	mode := authz.ModeFrom(ctx)
	if mode == authz.ModeShadow {
		return Result{}, false
	}
	res := authz.ResourceForTool(name)
	surface := authz.Surface{Action: "invoke", Type: res.Type, Name: res.Name}
	if st := raw.LiveACL.state.Load(); st.fault != nil && mode == authz.ModeEnforce {
		recordDefOp(ctx, st.latest, "acl_unavailable", surface, authz.Decision{})
		msg := "the caller's access-control rules could not be read again during the run; retry"
		return aclRefusalResult(aclRefusal{Code: "acl_unavailable", Error: msg},
			&ErrorInfo{Category: CategoryTransient, Retryable: true, Description: msg}), true
	}
	eff := applyLive(ctx, raw).ACL
	if eff == raw.ACL {
		return Result{}, false
	}
	return judgeDefOp(ctx, eff, surface)
}
