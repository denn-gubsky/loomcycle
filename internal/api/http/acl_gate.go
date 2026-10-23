package http

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

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
func (s *Server) SetACLResolver(r *aclres.Resolver) { s.aclResolver = r }

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
		log.Printf("authz: resolving the set for tenant=%q subject=%q failed (mode %s): %v", p.TenantID, p.Subject, s.aclMode(), err)
		return p
	}
	return out
}

// aclGateHTTP asks the route's type-level question. It returns false after
// writing the refusal. legacyAllowed is always true here: the scope gate
// already admitted the request.
func (s *Server) aclGateHTTP(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	surface, governed := authz.HTTPSurface(r.Method, r.URL.Path)
	if !governed {
		return true
	}
	ctx := r.Context()
	res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
	return s.applyGate(ctx, w, p, surface, res)
}

// applyGate turns a gate result into a response. It is shared by the route
// gate and the instance checks that can write their own refusal.
func (s *Server) applyGate(ctx context.Context, w http.ResponseWriter, p auth.Principal, surface authz.Surface, res authz.GateResult) bool {
	switch {
	case res.Unavailable:
		s.recordACLEvent(ctx, p, "acl_unavailable", surface, res.Decision)
		writeJSONError(w, http.StatusServiceUnavailable, "acl_unavailable", "access control could not be evaluated; retry")
		return false
	case res.Diverged:
		s.recordACLEvent(ctx, p, "acl_shadow_diverged", surface, res.Decision)
		return true
	case !res.Allowed:
		s.recordACLEvent(ctx, p, "acl_denied", surface, res.Decision)
		writeACLDenied(w, surface, res.Decision)
		return false
	}
	return true
}

// writeACLDenied is the refusal: the action, the resource and the rules that
// decided. The reasons are the caller's own decision, so they are shown.
func writeACLDenied(w http.ResponseWriter, surface authz.Surface, d authz.Decision) {
	writeJSONErrorBody(w, http.StatusForbidden, struct {
		Code     string         `json:"code"`
		Error    string         `json:"error"`
		Action   string         `json:"action"`
		Resource string         `json:"resource"`
		Reasons  []authz.Reason `json:"reasons,omitempty"`
	}{"acl_denied", aclDeniedText(surface, d), surface.Action, surface.Type + ":" + surface.Name, d.Reasons})
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
	log.Printf("authz: %s tenant=%q subject=%q action=%s resource=%s:%s outcome=%s rules=[%s] transport=%s",
		action, p.TenantID, p.Subject, surface.Action, surface.Type, surface.Name, d.Outcome, strings.Join(reasons, ","), authz.TransportFrom(ctx))
	if s.tokenAudit == nil {
		return
	}
	ev := audit.Event{
		Action: action, ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix,
		TargetName: surface.Type + ":" + surface.Name, AclAction: surface.Action, AclOutcome: d.Outcome.String(), AclRules: reasons,
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		log.Printf("authz: audit write failed: %v", err)
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
