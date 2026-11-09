package http

// app_signins.go — which apps a person is signed in to through loomcycle's
// OAuth authorization server, and ending one. Before these routes an app
// sign-in (a refresh-token family) could be ended only by disabling the
// whole user: nobody could answer "which apps is Alice signed in to", and a
// person could not sign one lost device out. The operator routes sit with the
// rest of /v1/_users/{subject}; the self-service ones under /v1/_me answer
// for the caller's own sign-ins only.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The revoke reasons an ended app sign-in records.
const (
	oauthRevokeByUser     = "signed_out_by_user"
	oauthRevokeByOperator = "ended_by_operator"
)

// appSignInsResponse is the body of both list routes.
type appSignInsResponse struct {
	Subject    string                `json:"subject"`
	AppSignIns []connector.AppSignIn `json:"app_sign_ins"`
}

// isSelf reports whether the principal on ctx is (tenant, subject) itself.
func isSelf(ctx context.Context, tenant, subject string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return ok && p.TenantID == tenant && p.Subject == subject
}

// appSignInAccess asks the caller's access control the operator route's
// question about subject — read or manage principal:user:<subject> — unless
// the caller is that subject (the /v1/_me routes ask nothing, as /v1/_me
// does not) or the HTTP route gate already asked it.
func (s *Server) appSignInAccess(ctx context.Context, tenant, subject, method string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if ok && p.OAuthClient != "" {
		return errAppTokenSignIns
	}
	if !ok || isSelf(ctx, tenant, subject) {
		return nil
	}
	surface := appSignInSurface(method, subject)
	if routeGateAnswered(ctx, surface) {
		return nil
	}
	return s.aclAdmit(ctx, p, surface)
}

// appSignInSurface is the question the operator routes ask about subject:
// the one authz.HTTPSurface answers for GET (read) and DELETE (manage) of
// /v1/_users/{subject}/app-sign-ins, built from the subject itself rather
// than from a path, which a subject holding a "/" would split.
// TestAppSignIns_TheTwinsAskTheRoutesQuestion holds the two equal.
func appSignInSurface(method, subject string) authz.Surface {
	action := "read"
	if method != http.MethodGet {
		action = "manage"
	}
	return authz.Surface{Action: action, Type: "principal", Name: "user:" + subject}
}

// AppSignIns implements connector.Connector.
func (s *Server) AppSignIns(ctx context.Context, tenant, subject string) ([]connector.AppSignIn, error) {
	if s.store == nil {
		return nil, errSessionsUnavailable
	}
	if err := s.appSignInAccess(ctx, tenant, subject, http.MethodGet); err != nil {
		return nil, err
	}
	rows, err := s.store.OAuthFamilyList(ctx, tenant, subject, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := make([]connector.AppSignIn, 0, len(rows))
	for _, f := range rows {
		name := f.ClientID
		if c, ok, err := s.appClient(ctx, f.ClientID); err == nil && ok {
			name = c.Label
		}
		expires := f.AbsoluteExpiresAt
		if f.IdleExpiresAt.Before(expires) {
			expires = f.IdleExpiresAt
		}
		out = append(out, connector.AppSignIn{FamilyID: f.FamilyID, ClientID: f.ClientID, ClientName: name, Gate: f.Gate,
			CreatedAt: f.CreatedAt.UTC(), LastUsedAt: f.LastUsedAt.UTC(), ExpiresAt: expires.UTC()})
	}
	return out, nil
}

// EndAppSignIn implements connector.Connector: the family is revoked and the
// access tokens issued through it are retired on every replica. A family that
// is not a live sign-in of (tenant, subject) — another user's, another
// tenant's, already ended — is the same not-found, so the answer names no
// sign-in the caller may not see.
func (s *Server) EndAppSignIn(ctx context.Context, tenant, subject, familyID string) error {
	if s.store == nil {
		return errSessionsUnavailable
	}
	if err := s.appSignInAccess(ctx, tenant, subject, http.MethodDelete); err != nil {
		return err
	}
	notFound := &store.ErrNotFound{Kind: "app_sign_in", ID: familyID}
	fam, err := s.store.OAuthFamilyGet(ctx, familyID)
	switch {
	case isNotFound(err):
		return notFound
	case err != nil:
		return err
	case fam.TenantID != tenant || fam.Subject != subject:
		return notFound
	case !fam.RevokedAt.IsZero():
		// Already ended — perhaps by this call's earlier try, whose token
		// retirement then faulted. Its access tokens are retired again, so a
		// retry finishes the job; the answer stays not-found, as for any
		// sign-in that is no longer live.
		_, err := s.store.OAuthTokenRetire(ctx, fam.ClientID, familyID, time.Now().UTC())
		s.invalidateTokenCache(ctx)
		if err != nil {
			return err
		}
		return notFound
	}
	reason := oauthRevokeByOperator
	if isSelf(ctx, tenant, subject) {
		reason = oauthRevokeByUser
	}
	now := time.Now().UTC()
	_, revokeErr := s.store.OAuthFamilyRevoke(ctx, familyID, reason, now)
	// The tokens go whatever the revoke answered: a retry after a partial
	// failure finishes the job, as endSubjectAppSignIns does.
	_, retireErr := s.store.OAuthTokenRetire(ctx, fam.ClientID, familyID, now)
	s.invalidateTokenCache(ctx)
	if err := errors.Join(revokeErr, retireErr); err != nil {
		return err
	}
	p, _ := auth.PrincipalFromContext(ctx)
	s.recordAuthEvent(audit.Event{Action: "oauth_revoked", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetTenant: tenant, TargetSubject: subject, AuthGate: fam.Gate,
		AuthClient: fam.ClientID, AuthRecordID: familyID, AuthReason: reason})
	return nil
}

// writeAppSignInError answers a connector error of the two routes.
func writeAppSignInError(w http.ResponseWriter, err error) {
	var denied *errACLDenied
	switch {
	case errors.As(err, &denied):
		writeACLDenied(w, denied.surface, denied.decision)
	case errors.Is(err, errAppTokenSignIns):
		writeJSONError(w, http.StatusForbidden, "app_token_refused", err.Error())
	case isNotFound(err):
		writeJSONError(w, http.StatusNotFound, "app_sign_in_not_found", "no such app sign-in for this user")
	case errors.Is(err, errSessionsUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "store not configured; app sign-ins require a persistent store")
	default:
		slog.Error("users: app sign-ins failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "app_sign_ins_failed", "the app sign-ins could not be read or ended")
	}
}

// operatorAppSignInTarget resolves the tenant and the named user of an
// operator route: the caller's tenant (an admin focuses one with ?tenant=),
// and a user that exists in it.
func (s *Server) operatorAppSignInTarget(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if !s.requireStore(w) {
		return "", "", false
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return "", "", false
	}
	tenant, _, ok := s.userCrudTenant(w, r)
	if !ok {
		return "", "", false
	}
	if _, err := s.store.UserGet(r.Context(), tenant, subject); err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			return "", "", false
		}
		writeJSONError(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return "", "", false
	}
	return tenant, subject, true
}

// errAppTokenSignIns is the refusal of an app's own token by the app
// sign-in operations, on every route and transport: an app signed in on the
// person's behalf, capped to what it was granted, must not read or end the
// person's sign-ins. The person does it with its own session or token.
var errAppTokenSignIns error = appTokenRefusal{}

type appTokenRefusal struct{}

func (appTokenRefusal) Error() string {
	return "an app's token cannot list or end app sign-ins; use your own session or token"
}

// HTTPStatus lets a transport map the refusal without knowing its type.
func (appTokenRefusal) HTTPStatus() int { return http.StatusForbidden }

// selfAppSignInTarget is the caller's own (tenant, subject). A principal
// with no subject (open mode, the legacy token) has no sign-ins of its own.
func selfAppSignInTarget(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || p.Subject == "" {
		writeJSONError(w, http.StatusForbidden, "subject_required", "app sign-ins belong to a signed-in person; this credential names none")
		return "", "", false
	}
	return p.TenantID, p.Subject, true
}

// handleListUserAppSignIns serves GET /v1/_users/{subject}/app-sign-ins.
func (s *Server) handleListUserAppSignIns(w http.ResponseWriter, r *http.Request) {
	tenant, subject, ok := s.operatorAppSignInTarget(w, r)
	if !ok {
		return
	}
	s.writeAppSignIns(w, r, tenant, subject)
}

// handleEndUserAppSignIn serves DELETE /v1/_users/{subject}/app-sign-ins/{family_id}.
func (s *Server) handleEndUserAppSignIn(w http.ResponseWriter, r *http.Request) {
	tenant, subject, ok := s.operatorAppSignInTarget(w, r)
	if !ok {
		return
	}
	s.endAppSignIn(w, r, tenant, subject)
}

// handleListMyAppSignIns serves GET /v1/_me/app-sign-ins.
func (s *Server) handleListMyAppSignIns(w http.ResponseWriter, r *http.Request) {
	tenant, subject, ok := selfAppSignInTarget(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	s.writeAppSignIns(w, r, tenant, subject)
}

// handleEndMyAppSignIn serves DELETE /v1/_me/app-sign-ins/{family_id}.
func (s *Server) handleEndMyAppSignIn(w http.ResponseWriter, r *http.Request) {
	tenant, subject, ok := selfAppSignInTarget(w, r)
	if !ok || !s.requireStore(w) {
		return
	}
	s.endAppSignIn(w, r, tenant, subject)
}

func (s *Server) writeAppSignIns(w http.ResponseWriter, r *http.Request, tenant, subject string) {
	list, err := s.AppSignIns(r.Context(), tenant, subject)
	if err != nil {
		writeAppSignInError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, appSignInsResponse{Subject: subject, AppSignIns: list})
}

func (s *Server) endAppSignIn(w http.ResponseWriter, r *http.Request, tenant, subject string) {
	familyID := strings.TrimSpace(r.PathValue("family_id"))
	if familyID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_family_id", "family_id is required")
		return
	}
	if err := s.EndAppSignIn(r.Context(), tenant, subject, familyID); err != nil {
		writeAppSignInError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
