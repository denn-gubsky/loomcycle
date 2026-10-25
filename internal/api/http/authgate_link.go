package http

// authgate_link.go — RFC FA §3.7d / D14: explicit account linking.
//
//	GET    /v1/_users/{subject}/identities          the user's sign-in identities (self, or a tenant operator / admin)
//	POST   /v1/_users/{subject}/identities {gate}    start linking a provider account (the user itself, from a Web UI session)
//	DELETE /v1/_users/{subject}/identities/{gate}    unlink (self, or a tenant operator / admin)
//
// Linking is never inferred (an e-mail match is how accounts get taken
// over). It takes two fresh authentications in one browser session: the
// session that asks must have authenticated within auth.session.reauth_window
// (the first gate), and the provider of the gate being linked is asked to
// authenticate the person again (prompt=login, max_age=0) and must report an
// auth_time inside this login (the second gate). The link state is bound to
// the asking session and to this browser's login cookie; the new identity
// must also map, through the gate's identity map, to the user's own tenant.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/identity"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

type wireUserIdentity struct {
	Gate            string     `json:"gate"`
	Issuer          string     `json:"issuer,omitempty"`
	ExternalSubject string     `json:"external_subject"`
	Email           string     `json:"email,omitempty"`
	EmailVerified   bool       `json:"email_verified"`
	CreatedAt       time.Time  `json:"created_at"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
}

// identitiesTarget resolves whose identities a request is about: the caller
// itself (any authenticated principal with that subject), or another user of
// the tenant for a tenant operator or an admin. Writes the refusal.
func (s *Server) identitiesTarget(w http.ResponseWriter, r *http.Request) (tenant, subject string, self, ok bool) {
	subject = strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return "", "", false, false
	}
	p, has := auth.PrincipalFromContext(r.Context())
	if has && !p.Legacy && p.Subject == subject && !auth.IsAuthManager(p) {
		return p.TenantID, subject, true, true
	}
	switch callerOf(r.Context()) {
	case gateCallerAdmin, gateCallerTenant:
		t, _, ok := s.userCrudTenant(w, r)
		return t, subject, false, ok
	}
	writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
	return "", "", false, false
}

// handleListUserIdentities serves GET /v1/_users/{subject}/identities.
func (s *Server) handleListUserIdentities(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	tenant, subject, _, ok := s.identitiesTarget(w, r)
	if !ok {
		return
	}
	if _, err := s.store.UserGet(r.Context(), tenant, subject); err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return
	}
	ids, err := s.store.UserIdentityListSubject(r.Context(), tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "identity_list_failed", err.Error())
		return
	}
	out := make([]wireUserIdentity, 0, len(ids))
	for _, id := range ids {
		wi := wireUserIdentity{Gate: id.Gate, Issuer: id.Issuer, ExternalSubject: id.ExternalSubject, Email: id.Email,
			EmailVerified: id.EmailVerified, CreatedAt: id.CreatedAt.UTC()}
		if !id.LastLoginAt.IsZero() {
			t := id.LastLoginAt.UTC()
			wi.LastLoginAt = &t
		}
		out = append(out, wi)
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": subject, "identities": out})
}

type linkIdentityRequest struct {
	Gate string `json:"gate"`
}

// handleLinkUserIdentity serves POST /v1/_users/{subject}/identities: the
// user, signed in through one gate within the re-authentication window,
// starts a fresh login through another to attach it. Answers
// {"redirect": <provider URL>}; the page navigates there.
func (s *Server) handleLinkUserIdentity(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	ctx := r.Context()
	ua, fromSession := uiAuthFrom(ctx)
	p, _ := auth.PrincipalFromContext(ctx)
	subject := strings.TrimSpace(r.PathValue("subject"))
	// Only the person, in a browser session of their own: an operator cannot
	// authenticate as them at two gates, and a bearer proves no re-auth.
	if !fromSession || ua.session == nil || ua.pending || p.Subject != subject || p.TenantID != ua.session.TenantID {
		writeJSONError(w, http.StatusForbidden, "link_needs_own_session", "link an account from your own signed-in Web UI session")
		return
	}
	var req linkIdentityRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	gate, cfg, ok := s.redirectGateNamed(strings.TrimSpace(req.Gate))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "gate_not_found", "no sign-in gate of that name to link")
		return
	}
	if cfg.Name == ua.session.Gate {
		writeJSONError(w, http.StatusConflict, "same_gate", "you are signed in through that gate already")
		return
	}
	window := s.cfg().ResolvedAuth.ReauthWindow
	if window <= 0 {
		window = config.DefaultSessionReauthWindow
	}
	if ua.session.AuthTime.IsZero() || time.Since(ua.session.AuthTime) > window {
		writeJSONError(w, http.StatusForbidden, "reauth_required",
			"sign in again first: linking an account needs a recent sign-in through the gate you are using")
		return
	}
	if existing, err := s.sessionIdentityRow(ctx, p.TenantID, subject, cfg.Name); err == nil && existing.Gate != "" {
		writeJSONError(w, http.StatusConflict, "already_linked", "an account of that gate is linked already; unlink it first")
		return
	} else if err != nil && !isNotFound(err) {
		writeJSONError(w, http.StatusInternalServerError, "identity_lookup_failed", err.Error())
		return
	}
	if !s.loginStartAllowed(ctx, r) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many sign-in attempts; wait and try again")
		return
	}
	u, err := s.beginRedirectLogin(w, r, cfg, gate, loginStart{purpose: store.LoginPurposeLink, returnTo: "/ui/account",
		reauth: true, linkTenant: p.TenantID, linkSubject: subject, linkSession: ua.session.SessionHash})
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "gate_unavailable", "that sign-in method is unavailable right now")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": u})
}

// completeLink finishes a link login: the asking session is still live and
// its own, the provider re-authenticated the person (the gate checked
// auth_time against this login), the identity maps to the user's tenant and
// belongs to nobody yet.
func (s *Server) completeLink(w http.ResponseWriter, r *http.Request, cfg config.AuthGate, row store.AuthLoginStateRow, a authgate.Assertion) {
	ctx := r.Context()
	refuse := func(reason string) {
		s.recordAuthEvent(audit.Event{Action: "auth_link_refused", AuthGate: cfg.Name, AuthReason: reason,
			TargetTenant: row.LinkTenant, TargetSubject: row.LinkSubject, SourceAddr: s.clientSource(r)})
		http.Redirect(w, r, "/ui/account?link_error=link_failed", http.StatusSeeOther)
	}
	if !row.Reauth {
		refuse("link without re-authentication")
		return
	}
	sess, err := s.store.UISessionGet(ctx, row.LinkSessionHash)
	if err != nil || !sess.RevokedAt.IsZero() || !time.Now().Before(sessionExpiresAt(sess, s.cfg().ResolvedAuth)) ||
		sess.TenantID != row.LinkTenant || sess.Subject != row.LinkSubject {
		refuse("the session that asked has ended")
		return
	}
	u, err := s.store.UserGet(ctx, row.LinkTenant, row.LinkSubject)
	if err != nil || u.Status != "active" {
		refuse("the user is not active")
		return
	}
	tenant, err := identity.Tenant(cfg.Identity, a)
	if err != nil || tenant != row.LinkTenant {
		refuse("the identity does not map to the user's tenant")
		return
	}
	ext, err := identity.ExternalSubject(a)
	if err != nil {
		refuse("no usable subject")
		return
	}
	err = s.store.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: row.LinkTenant, Subject: row.LinkSubject, Gate: cfg.Name,
		Issuer: a.Issuer, ExternalSubject: ext, Email: a.Email, EmailVerified: a.EmailVerified, CreatedAt: time.Now().UTC()})
	var conflict *store.ErrConflict
	if errors.As(err, &conflict) {
		refuse("the provider account is linked to another user, or the user has one at this gate")
		return
	}
	if err != nil {
		refuse("store error")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "auth_identity_linked", AuthGate: cfg.Name, TargetTenant: row.LinkTenant,
		TargetSubject: row.LinkSubject, AuthAMR: a.AMR, SourceAddr: s.clientSource(r)})
	http.Redirect(w, r, "/ui/account?linked="+cfg.Name, http.StatusSeeOther)
}

// handleUnlinkUserIdentity serves DELETE /v1/_users/{subject}/identities/{gate}.
// The password gate's logins are its own routes' (DELETE
// /v1/_authgate/{gate}/credentials); this unlinks identity-gate accounts.
// Sessions made through the identity end on their next request.
func (s *Server) handleUnlinkUserIdentity(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	tenant, subject, self, ok := s.identitiesTarget(w, r)
	if !ok {
		return
	}
	gate := r.PathValue("gate")
	if _, pw := s.passwordGateNamed(gate); pw {
		writeJSONError(w, http.StatusConflict, "password_gate", "a password login is removed through its gate's credentials route")
		return
	}
	if self {
		// From a session, the user unlinks with a recent sign-in, like a link.
		if ua, ok := uiAuthFrom(r.Context()); ok && ua.session != nil {
			window := s.cfg().ResolvedAuth.ReauthWindow
			if window <= 0 {
				window = config.DefaultSessionReauthWindow
			}
			if ua.session.AuthTime.IsZero() || time.Since(ua.session.AuthTime) > window {
				writeJSONError(w, http.StatusForbidden, "reauth_required", "sign in again first")
				return
			}
		}
	}
	removed, err := s.store.UserIdentityDelete(r.Context(), tenant, subject, gate)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "identity_delete_failed", err.Error())
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, "identity_not_found", "no linked account of that gate")
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	s.recordAuthEvent(audit.Event{Action: "auth_identity_unlinked", AuthGate: gate, TargetTenant: tenant, TargetSubject: subject,
		ActorTenant: p.TenantID, ActorSubject: p.Subject, SourceAddr: s.clientSource(r)})
	w.WriteHeader(http.StatusNoContent)
}
