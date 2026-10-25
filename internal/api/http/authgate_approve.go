package http

// authgate_approve.go — RFC FA §3.7a: the approval queue. A gate configured
// with new_user.status: pending provisions people who can sign in but whose
// session answers no to everything; an operator lets them in here.

import (
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// handleApproveUser serves POST /v1/_users/{subject}/approve: pending →
// active. A tenant operator approves in its own tenant, an admin in any (with
// ?tenant=), and an auth:manage credential only a user at the floor (§6.1.1).
// Audited as auth_user_approved.
func (s *Server) handleApproveUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	tenant, caller, ok := s.userCrudTenant(w, r)
	if !ok {
		return
	}
	if _, ok := s.holdManagerToFloorTarget(w, ctx, caller, nil, tenant, subject, "approve", false); !ok {
		return
	}
	u, err := s.store.UserGet(ctx, tenant, subject)
	if err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return
	}
	if u.Status != store.UserStatusPending {
		writeJSONError(w, http.StatusConflict, "not_pending", "this user is not awaiting approval")
		return
	}
	active := "active"
	if err := s.store.UserUpdate(ctx, tenant, subject, store.UserPatch{Status: &active}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "user_update_failed", err.Error())
		return
	}
	p, _ := auth.PrincipalFromContext(ctx)
	s.recordAuthAudit(audit.Event{Action: "auth_user_approved", TargetTenant: tenant, TargetSubject: subject,
		ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix, SourceAddr: s.clientSource(r)})
	u.Status = active
	writeJSON(w, http.StatusOK, toWireUserRecord(u))
}
