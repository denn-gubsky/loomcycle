package http

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FY Part A — whose scope an off-run browse acts on.
//
// The Path, Document and memory routes let a caller name a subject other than
// its own (?scope_id=, the memory routes' {scope_id}, the memory search's
// scope_id): the operator's way to inspect what an agent or a colleague holds.
// Those routes are also open to tenant members (RFC CB), so the subject a
// request names is resolved here, once, for every one of them
// (TestBrowseSubject_EveryScopeIDReaderGoesThroughTheHelper holds that):
//
//   - operator plane — admin, the legacy token, open mode, and substrate:tenant
//     in its own tenant: any subject, as before (an admin also any tenant);
//   - member (not isolated): its own subject; another subject's user scope only
//     when its access-control set ALLOWS `browse <type>:user` with that subject
//     as the owner (the member baseline allows it for the owner itself only,
//     so a tenant rule is what widens it); an agent's scope only for an agent
//     it may run. No set (no resolver, or it could not be read): its own only.
//     A browse is a READ: a request that may change anything, on a subject
//     other than the member's own, is refused whatever the rules allow — the
//     member baseline grants write and delete on every user scope it reaches,
//     so a browse grant would otherwise become write access;
//   - isolated member: its own subject only, and the isolation bit stays on the
//     identity, which the tools read to refuse it the tenant keyspace.
//
// A request refused here is answered with one not-found whatever subject it
// named, so the answer never says whether that subject exists.

// browseSubject is who an off-run browse acts as.
type browseSubject struct {
	// own: no subject or tenant was named, or a non-operator named nothing it
	// could use — the operator-trust ctx of the caller itself.
	own bool
	// tenantID / userID are the tenant and the subject a scope=user op keys
	// on; agentName the name a scope=agent op keys on ("" keeps the synthetic
	// operator name the caller's own ctx carries).
	tenantID, userID, agentName string
	isolated                    bool
}

// browseSubjectFor resolves the subject (reqScopeID) and tenant (reqTenant) a
// request names, each "" for none, for a browse of typ ("document" or
// "memory") touching scopes ("user", "agent", "tenant"; none when the caller
// cannot say); writes says the request may change something. It reports
// false when the request must be refused: it names a subject the caller may
// not browse on one of the scopes it touches, or one it may browse on none of
// them, or it is a member's write naming a subject other than its own. A named
// subject is set only on the planes the caller may browse it on, so a scope
// the caller could not name in advance still never reaches another subject's
// data.
func (s *Server) browseSubjectFor(ctx context.Context, typ, reqScopeID, reqTenant string, writes bool, scopes ...string) (browseSubject, bool) {
	reqScopeID, reqTenant = strings.TrimSpace(reqScopeID), strings.TrimSpace(reqTenant)
	p, ok := auth.PrincipalFromContext(ctx)
	isolated := auth.IsIsolated(p, ok)
	admin := !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin)
	operator := admin || auth.HasScope(p.Scopes, auth.ScopeTenant)

	if operator {
		if reqScopeID == "" && reqTenant == "" {
			return browseSubject{own: true}, true
		}
		b := browseSubject{}
		if ok {
			b.tenantID, b.userID = p.TenantID, p.Subject
		}
		// Only an admin focuses another tenant; a tenant operator's ?tenant=
		// is ignored, never widening.
		if admin && reqTenant != "" {
			b.tenantID = reqTenant
		}
		if reqScopeID != "" {
			b.userID = reqScopeID
		}
		b.agentName = b.userID
		return b, true
	}

	// A member: its tenant is its own whatever it names, so ?tenant= alone
	// changes nothing.
	b := browseSubject{own: true, isolated: isolated}
	if reqScopeID == "" {
		return b, true
	}
	if writes && reqScopeID != p.Subject {
		return browseSubject{}, false
	}
	userOK := reqScopeID == p.Subject ||
		(!isolated && authz.Grants(ctx, p.Effective, "browse", authz.Resource{Type: typ, Name: "user", Owner: reqScopeID}))
	agentOK := !isolated && s.browseMayRunAgent(ctx, p, reqScopeID)
	if !userOK && !agentOK {
		return browseSubject{}, false
	}
	for _, sc := range scopes {
		if (sc == "user" && !userOK) || (sc == "agent" && !agentOK) {
			return browseSubject{}, false
		}
	}
	b = browseSubject{tenantID: p.TenantID, userID: p.Subject, isolated: isolated}
	if userOK {
		b.userID = reqScopeID
	}
	if agentOK {
		b.agentName = reqScopeID
	}
	return b, true
}

// browseMayRunAgent is the agent-scope browse question: may the member run
// the agent, as the run plane decides it at a run's intake — the token's own
// agent list, then `run agent:<name>` under the rollout mode. A member's run
// of that agent reads and writes the agent's scope anyway, so browsing it adds
// no reach; with no set in compat mode the run plane admits the run, and so
// this admits the browse.
func (s *Server) browseMayRunAgent(ctx context.Context, p auth.Principal, name string) bool {
	if !auth.MayStartAgent(p, true, name) {
		return false
	}
	res := authz.EvaluateSurface(ctx, p.Effective, authz.Surface{Action: "run", Type: "agent", Name: name}, true)
	return res.Allowed && !res.Unavailable
}

// apply builds the ctx the browse runs under: the operator-trust ctx of the
// caller (substrateAdminUserCtx) with the resolved identity, the isolation bit
// kept.
func (b browseSubject) apply(ctx context.Context) context.Context {
	base := substrateAdminUserCtx(ctx)
	if b.own {
		if !b.isolated {
			return base
		}
		id := tools.RunIdentity(base)
		id.Isolated = true
		return tools.WithRunIdentity(base, id)
	}
	ctx = tools.WithRunIdentity(base, tools.RunIdentityValue{
		UserID:   b.userID,
		AgentID:  substrateAdminAgentID,
		TenantID: b.tenantID,
		Isolated: b.isolated,
	})
	if b.agentName != "" {
		ctx = tools.WithAgentName(ctx, b.agentName)
	}
	return ctx
}

// errBrowseRefused is a browse browseSubjectFor refused, for a caller that
// answers it later (the document feed's re-check ends its stream).
var errBrowseRefused = errors.New("browse refused")

// writeBrowseNotFound is the refusal of a browse the caller may not make: the
// same answer for every subject it could have named.
func writeBrowseNotFound(w http.ResponseWriter) {
	writeJSONError(w, http.StatusNotFound, "not_found", "not found")
}
