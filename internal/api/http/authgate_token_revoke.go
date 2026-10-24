package http

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// handleSelfRevokeToken is POST /v1/_auth/token/revoke (RFC FA P5): the bearer
// on the request retires itself. It is how `loomcycle logout` ends the token
// `loomcycle login` was issued, so a person can sign a machine out without an
// operator. Retiring the credential you present is never an escalation, so it
// needs no scope, and an agent-restricted token reaches it (runPlaneRoute).
//
// It only ever touches the presented credential, and only a delegated user
// token: a config principal or the legacy secret has no row to retire, and an
// operator token (admin or tenant scope) is retired by an operator, so a
// misdirected logout cannot take an operator's automation down. A Web UI
// session uses POST /v1/_auth/logout instead; a cookie never retires a bearer.
func (s *Server) handleSelfRevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := uiAuthFrom(ctx); ok {
		writeJSONError(w, http.StatusBadRequest, "use_logout",
			"a Web UI session signs out with POST /v1/_auth/logout; this route retires the bearer it is called with")
		return
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; there is no token to retire")
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "token revocation requires a persistent store")
		return
	}
	if p.Legacy || p.TokenDefID == "" || strings.HasPrefix(p.TokenDefID, "cfg:") {
		writeJSONError(w, http.StatusForbidden, "not_revocable",
			"this credential is not a minted token and cannot retire itself; change it where it is configured")
		return
	}
	row, err := s.store.OperatorTokenDefGet(ctx, p.TokenDefID)
	if err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusUnauthorized, "token_not_found", "the token no longer exists")
			return
		}
		slog.Error("auth: self-revoke lookup failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "token_lookup_failed", "the token could not be looked up")
		return
	}
	if !isDelegatedUserToken(row, p.TenantID, p.Subject) {
		writeJSONError(w, http.StatusForbidden, "not_revocable",
			"an operator token is retired by an operator, not by itself")
		return
	}
	now := time.Now().UTC()
	if err := s.store.OperatorTokenDefSetRetiredAt(ctx, row.DefID, now); err != nil {
		if isNotFound(err) {
			writeJSONError(w, http.StatusUnauthorized, "token_not_found", "the token no longer exists")
			return
		}
		slog.Error("auth: self-revoke failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "token_revoke_failed", "the token could not be retired")
		return
	}
	s.invalidateTokenCache(ctx)
	s.recordTokenAudit(ctx, "retire", row)
	slog.Info("auth: a token retired itself", "tenant", row.TenantID, "subject", row.Subject, "def_id", row.DefID)
	writeJSON(w, http.StatusOK, map[string]any{"def_id": row.DefID, "retired_at": now.Format(time.RFC3339Nano)})
}
