package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// RFC EY P2 — the HTTP transport's access-control gate. It runs AFTER the
// scope gate in authMiddleware and asks the engine the route's type-level
// question (authz.HTTPSurface); the run intakes then ask the instance
// question for the agent (admitPrincipalAgent). What the answer means is the
// rollout mode's call (authz.Evaluate): compat honours an explicit deny on
// top of the scope answer, shadow logs a divergence, enforce decides.

// SetACLResolver wires the RFC EY resolver. Nil leaves every gate on the
// scope answer alone (the pre-EY behaviour).
func (s *Server) SetACLResolver(r *aclres.Resolver) {
	s.aclResolver = r
	if r != nil {
		// The config-declared lists, groups and bindings (RFC EY §9), read
		// from the live config on every compile so a reload is honoured.
		r.SetStatic(func() *authz.Static {
			if c := s.cfg(); c != nil {
				return c.ResolvedACL
			}
			return nil
		})
	}
}

// ACLResolver returns the wired resolver (nil when none).
func (s *Server) ACLResolver() *aclres.Resolver { return s.aclResolver }

// aclMode is the rollout mode: the resolver's, or compat when none is wired.
func (s *Server) aclMode() authz.Mode {
	if s.aclResolver == nil {
		return authz.ModeCompat
	}
	return s.aclResolver.Mode()
}

// enrichPrincipal fills the principal's groups and compiled set. A store
// fault leaves Effective nil and logs: compat and shadow then decide on the
// scope answer, enforce refuses at the gate (authz.Evaluate).
func (s *Server) enrichPrincipal(ctx context.Context, p auth.Principal) auth.Principal {
	if s.aclResolver == nil {
		return p
	}
	out, err := s.aclResolver.Resolve(ctx, p)
	if err != nil {
		slog.Error("authz: resolving the set failed", "tenant", p.TenantID, "subject", p.Subject, "mode", s.aclMode(), "err", err)
		return p
	}
	return out
}

// aclGateHTTP asks the route's type-level question. It returns false after
// writing the refusal, else the ctx the handler runs under, which records
// the question it answered (withRouteGate) so an operation behind the route
// that holds the same question itself does not ask it twice.
// legacyAllowed is always true here: the scope gate already admitted the
// request.
func (s *Server) aclGateHTTP(w http.ResponseWriter, r *http.Request, p auth.Principal) (context.Context, bool) {
	ctx := r.Context()
	surface, governed := authz.HTTPSurface(r.Method, r.URL.Path)
	if !governed {
		return ctx, true
	}
	res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
	if !s.applyGate(ctx, w, p, surface, res) {
		return ctx, false
	}
	return withRouteGate(ctx, surface), true
}

// applyGate turns a gate result into a response. It is shared by the route
// gate and the instance checks that can write their own refusal.
func (s *Server) applyGate(ctx context.Context, w http.ResponseWriter, p auth.Principal, surface authz.Surface, res authz.GateResult) bool {
	if r := s.gateReply(ctx, p, surface, res); r != nil {
		writeACLReply(w, *r)
		return false
	}
	return true
}

// gateReply is applyGate's decision as a reply, for a caller that answers
// on a transport other than HTTP: the refusal (after recording it), or nil
// when the request may proceed (a shadow divergence is recorded and let
// through).
func (s *Server) gateReply(ctx context.Context, p auth.Principal, surface authz.Surface, res authz.GateResult) *aclReply {
	switch {
	case res.Unavailable:
		s.recordACLEvent(ctx, p, "acl_unavailable", surface, res.Decision)
		r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "access control could not be evaluated; retry")
		return &r
	case res.Diverged:
		s.recordACLEvent(ctx, p, "acl_shadow_diverged", surface, res.Decision)
		return nil
	case !res.Allowed:
		s.recordACLEvent(ctx, p, "acl_denied", surface, res.Decision)
		r := aclReply{http.StatusForbidden, aclDeniedBody(surface, res.Decision)}
		return &r
	}
	return nil
}

// aclDeniedReply is the refusal body: the action, the resource and the rules
// that decided. The reasons are the caller's own decision, so they are shown.
type aclDeniedReply struct {
	Code     string         `json:"code"`
	Error    string         `json:"error"`
	Action   string         `json:"action"`
	Resource string         `json:"resource"`
	Reasons  []authz.Reason `json:"reasons,omitempty"`
}

func aclDeniedBody(surface authz.Surface, d authz.Decision) aclDeniedReply {
	return aclDeniedReply{"acl_denied", aclDeniedText(surface, d), surface.Action, surface.Type + ":" + surface.Name, d.Reasons}
}

// writeACLDenied writes the refusal.
func writeACLDenied(w http.ResponseWriter, surface authz.Surface, d authz.Decision) {
	writeJSONErrorBody(w, http.StatusForbidden, aclDeniedBody(surface, d))
}

func aclDeniedText(surface authz.Surface, d authz.Decision) string {
	if d.Outcome == authz.Denied && len(d.Reasons) > 0 {
		return fmt.Sprintf("%s %s:%s is denied by %s rule %d", surface.Action, surface.Type, surface.Name, d.Reasons[0].ACL, d.Reasons[0].RuleIndex)
	}
	return fmt.Sprintf("no rule allows %s %s:%s", surface.Action, surface.Type, surface.Name)
}

// errACLDenied is the instance-check form, carried through the run intakes
// like runner.ErrAgentNotAllowed and written by writeAgentNotAllowed's
// sibling below.
type errACLDenied struct {
	surface  authz.Surface
	decision authz.Decision
}

func (e *errACLDenied) Error() string { return "acl_denied: " + aclDeniedText(e.surface, e.decision) }

// Is lets the existing intake error paths that test for ErrAgentNotAllowed
// treat an ACL refusal of a run the same way (a 403 with its own code).
func (e *errACLDenied) Is(target error) bool { return target == runner.ErrAgentNotAllowed }

// aclAdmitAgent is the instance question at a run intake: may the principal
// run this agent. Called from admitPrincipalAgent, which every row-creating
// intake calls, so the census test covers it.
func (s *Server) aclAdmitAgent(ctx context.Context, p auth.Principal, ok bool, agent string) error {
	if !ok {
		return nil // no principal: open mode or a trigger firing under a definition
	}
	surface := authz.Surface{Action: "run", Type: "agent", Name: agent}
	res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
	switch {
	case res.Unavailable:
		s.recordACLEvent(ctx, p, "acl_unavailable", surface, res.Decision)
		return &errACLDenied{surface: surface, decision: res.Decision}
	case res.Diverged:
		s.recordACLEvent(ctx, p, "acl_shadow_diverged", surface, res.Decision)
	case !res.Allowed:
		s.recordACLEvent(ctx, p, "acl_denied", surface, res.Decision)
		return &errACLDenied{surface: surface, decision: res.Decision}
	}
	return nil
}

// recordACLEvent writes a refusal or a shadow divergence to the audit sink
// (best-effort) and the log. Names and ids only, never a bearer.
func (s *Server) recordACLEvent(ctx context.Context, p auth.Principal, action string, surface authz.Surface, d authz.Decision) {
	reasons := make([]string, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		reasons = append(reasons, fmt.Sprintf("%s#%d:%s", r.ACL, r.RuleIndex, r.Effect))
	}
	slog.Warn("authz: "+action, "tenant", p.TenantID, "subject", p.Subject, "action", surface.Action,
		"resource", surface.Type+":"+surface.Name, "outcome", d.Outcome.String(), "rules", reasons, "transport", authz.TransportFrom(ctx))
	if s.tokenAudit == nil {
		return
	}
	ev := audit.Event{
		Action: action, ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix,
		TargetName: surface.Type + ":" + surface.Name, AclAction: surface.Action, AclOutcome: d.Outcome.String(), AclRules: reasons,
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("authz: audit write failed", "err", err)
	}
}

// principalACL is the compiled set of the principal on ctx, or nil.
func principalACL(ctx context.Context) *authz.Effective {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	return p.Effective
}

// recordedCaller is the caller of a run that outlived its request — a queued
// run started after a restart — whose set was compiled again from the run's
// record (recordedCallerACL). It stands in for the principal's set only: no
// principal is put on ctx, so nothing else the run does reads scopes it
// never had.
type recordedCaller struct {
	origin aclOriginRecord
	eff    *authz.Effective
}

type ctxKeyRecordedCaller struct{}

func withRecordedCaller(ctx context.Context, o aclOriginRecord, eff *authz.Effective) context.Context {
	return context.WithValue(ctx, ctxKeyRecordedCaller{}, recordedCaller{origin: o, eff: eff})
}

func recordedCallerFrom(ctx context.Context) (recordedCaller, bool) {
	rc, ok := ctx.Value(ctxKeyRecordedCaller{}).(recordedCaller)
	return rc, ok && rc.eff != nil
}

// runCallerACL is the set a run starting on ctx is narrowed by: its
// principal's, or with no principal the recorded caller's; nil when neither.
func runCallerACL(ctx context.Context) *authz.Effective {
	if eff := principalACL(ctx); eff != nil {
		return eff
	}
	if _, ok := auth.PrincipalFromContext(ctx); ok {
		return nil
	}
	if rc, ok := recordedCallerFrom(ctx); ok {
		return rc.eff
	}
	return nil
}

// triggerOwnerCtx is ctx for a run a trigger fires (RFC EZ §1.5): with owner
// set and no caller on ctx, the owner's CURRENT set compiled and carried as a
// recorded caller — so RunOnce narrows the run by it (runCallerACL), records
// it on the run for a resume (acl_origin), and asks `run agent:<agent>` for
// the owner (admitPrincipalAgent), exactly as for a queued run restarted
// after a restart. The transport is the trigger's own, stamped on ctx by the
// surface that fired it.
//
// No owner (a yaml trigger, a definition written with no set in force), or a
// principal or recorded caller already on ctx: ctx as it is. No resolver
// wired: ctx as it is too — access control is not configured, so a live
// request carries no set either (enrichPrincipal); every boot wires one. A set
// that cannot be compiled refuses the run with ErrACLUnavailable — logged and
// audited by recordedCallerACL — never runs it without the set.
func (s *Server) triggerOwnerCtx(ctx context.Context, owner *authz.SubjectRef, agent string) (context.Context, error) {
	if owner == nil || s.aclResolver == nil {
		return ctx, nil
	}
	if _, ok := auth.PrincipalFromContext(ctx); ok {
		return ctx, nil
	}
	if _, ok := recordedCallerFrom(ctx); ok {
		return ctx, nil
	}
	o := aclOriginRecord{TenantID: owner.TenantID, Subject: owner.Subject, TokenDefID: owner.TokenDefID, Class: owner.Class, Transport: authz.TransportFrom(ctx)}
	ctx, eff, err := s.recordedCallerACL(ctx, &o, agent)
	if err != nil {
		return ctx, fmt.Errorf("%w: the access-control rules of the owner of the trigger that fired %q (tenant %q, subject %q) could not be read (%v); the run is refused rather than run without them",
			runner.ErrACLUnavailable, agent, owner.TenantID, owner.Subject, err)
	}
	return withRecordedCaller(ctx, o, eff), nil
}

// aclAdmitSpawn is the `spawn agent:<name>` question a parent asks before it
// starts a child, against the ORIGINATING principal's set the parent policy
// carries. No set (open mode, a trigger's run): nothing to ask.
func (s *Server) aclAdmitSpawn(ctx context.Context, eff *authz.Effective, child string) error {
	surface := authz.Surface{Action: "spawn", Type: "agent", Name: child}
	res := authz.EvaluateSurface(ctx, eff, surface, true)
	if res.Unavailable || !res.Allowed {
		p, _ := auth.PrincipalFromContext(ctx)
		if eff != nil {
			p.TenantID, p.Subject = eff.Subject.TenantID, eff.Subject.ID
		}
		s.recordACLEvent(ctx, p, "acl_denied", surface, res.Decision)
		return &errACLDenied{surface: surface, decision: res.Decision}
	}
	return nil
}

// emitACLRemovedTools reports, once at run start, each tool the definition
// granted and the caller's ACL removed — the RFC DE inert-grant event with
// the gate named `acl`, so "the agent didn't use Bash" reads as "the caller
// may not".
func emitACLRemovedTools(removed []string, emit func(providers.Event)) {
	for _, name := range removed {
		msg := fmt.Sprintf("tool %s is granted by the agent definition but the caller's access-control rules do not allow invoking it; it is withheld from this run", name)
		emit(providers.Event{
			Type:            providers.EventCapabilityInert,
			Text:            msg,
			CapabilityInert: &providers.CapabilityInertInfo{Tool: name, Gate: "acl", Message: msg},
		})
	}
}
