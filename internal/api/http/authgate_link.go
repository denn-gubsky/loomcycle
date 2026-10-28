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
	"context"
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

// ownIdentityPatterns are the identity routes' mux patterns.
var ownIdentityPatterns = map[string]bool{
	"GET /v1/_users/{subject}/identities":           true,
	"POST /v1/_users/{subject}/identities":          true,
	"DELETE /v1/_users/{subject}/identities/{gate}": true,
}

// ownIdentityRequest reports whether the request is about the caller's OWN
// sign-in identities, decided AFTER routing from the pattern the mux matched
// and its {subject} value — never by re-reading the decoded path, where an
// encoded '/' in a segment can make "<self>/identities" out of a request the
// mux routes elsewhere. Only a user principal (not the legacy secret, not
// auth:manage) qualifies. It is the only self-service admission of these
// routes, at the scope gate and past the route's ACL question; the handlers
// then require, for a link or an unlink, the person's own Web UI session
// after a recent sign-in.
func ownIdentityRequest(p auth.Principal, r *http.Request) bool {
	if p.Subject == "" || p.Legacy || auth.IsAuthManager(p) || !ownIdentityPatterns[r.Pattern] {
		return false
	}
	return r.PathValue("subject") == p.Subject
}

// userCredentialSession reports whether a session's credential IS its user:
// a password-gate, identity-gate or passkey-gate session, whose principal is
// the user's own reach. A token-gate session stands for a pasted bearer, which may be
// narrower than its user (restricted, fewer scopes); linking from it would
// hand the user's full reach to every later sign-in through the linked
// identity — a restricted credential widening itself.
func userCredentialSession(row *store.UISessionRow) bool {
	return row != nil && (row.CredKind == credKindPassword || row.CredKind == credKindIdentity || row.CredKind == credKindPasskey)
}

// principalIsTheUser is the belt to userCredentialSession's braces: the
// session's principal carries no restriction and at least the scopes the
// user's access_mode grants.
func principalIsTheUser(p auth.Principal, u store.UserRow) bool {
	if !p.Restrictions.IsZero() {
		return false
	}
	want, err := auth.GrantableUserScopes(u.AccessMode)
	if err != nil {
		return false
	}
	for _, sc := range want {
		if !auth.HasScope(p.Scopes, sc) {
			return false
		}
	}
	return true
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
	if !s.linkingSessionOK(ctx, ua.session, p) {
		writeJSONError(w, http.StatusForbidden, "link_needs_user_session",
			"link an account from a session you signed in to with your password or an identity provider, not with a pasted token")
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

// linkingSessionOK holds a link or unlink to a session whose credential is
// the user (userCredentialSession), whose principal is the user's full reach
// (principalIsTheUser), and whose user is active.
func (s *Server) linkingSessionOK(ctx context.Context, row *store.UISessionRow, p auth.Principal) bool {
	if !userCredentialSession(row) {
		return false
	}
	u, err := s.store.UserGet(ctx, row.TenantID, row.Subject)
	return err == nil && u.Status == "active" && principalIsTheUser(p, u)
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
	// Re-checked here, not trusted from the start: the stored session must
	// still be one whose credential is the user.
	if !userCredentialSession(&sess) {
		refuse("the session that asked is not the user's own credential")
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
		// The user unlinks its own account only from its own session, with a
		// recent sign-in, like a link: the self path skips the route's ACL
		// question (ownIdentityRoute), so it must prove the person.
		ua, ok := uiAuthFrom(r.Context())
		if !ok || ua.session == nil || ua.pending {
			writeJSONError(w, http.StatusForbidden, "link_needs_own_session", "unlink an account from your own signed-in Web UI session")
			return
		}
		if p, _ := auth.PrincipalFromContext(r.Context()); !s.linkingSessionOK(r.Context(), ua.session, p) {
			writeJSONError(w, http.StatusForbidden, "link_needs_user_session",
				"unlink an account from a session you signed in to with your password or an identity provider")
			return
		}
		window := s.cfg().ResolvedAuth.ReauthWindow
		if window <= 0 {
			window = config.DefaultSessionReauthWindow
		}
		if ua.session.AuthTime.IsZero() || time.Since(ua.session.AuthTime) > window {
			writeJSONError(w, http.StatusForbidden, "reauth_required", "sign in again first")
			return
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
