package http

// workspace_exposure_ticket.go — a one-time ticket for a page's socket (RFC GW
// P9, D-P9-42 as ruled by the owner 2026-11-25: a route the RFC does not
// name, authorised by that ruling).
//
// A browser cannot set Authorization on a WebSocket, and strict sign-in mode
// reads no bearer from a subprotocol (RFC FA §4), so a browser could not open
// a page's socket there. As RFC GJ does for the client-tools socket, the
// client mints a ticket over an ordinary call that its bearer header
// authenticated, and presents it as `ticket.<t>` on the upgrade.
//
//   - The ticket is the client-tools ticket's shape under its own format and
//     MAC domain (lxp1): HMAC-signed under the key derived from
//     LOOMCYCLE_SECRET_KEY, 30 seconds of life, naming the credential that
//     minted it by reference and bound to it by a peppered MAC, and to one
//     page by its Exposure claim. Neither kind verifies as the other.
//   - It is spent once, in the shared one-time store (its own namespace), so
//     it is refused after one use on every replica, and any replica sharing
//     the secret key can redeem it: behind a load balancer the mint and the
//     socket need not reach the same replica.
//   - It works in every mode, beside the bearer subprotocol (which strict
//     mode does not read). The handshake re-resolves the minting credential,
//     and the page's own decision (the workspace's own user) runs again.
//   - Only a request authenticated by its Authorization header mints one: a
//     Web UI cookie never does, so a page of another site cannot have one
//     minted in the user's name. The ticket itself is never logged; its jti
//     is, at the mint (audit) and at the handshake.

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

const (
	// exposureTicketFormat is a page socket ticket's format and MAC domain.
	exposureTicketFormat = "lxp1"
	// exposureTicketReplayGate namespaces its jti in the one-time store.
	exposureTicketReplayGate = "exposure-socket-ticket"
)

// exposureTicketOf returns the ticket a page socket's upgrade carries as a
// `ticket.<t>` entry, and the page id of the upgrade's path. Read on that
// upgrade only.
func exposureTicketOf(r *http.Request) (ticket, exposureID string, ok bool) {
	if !isExposureSocketUpgrade(r) {
		return "", "", false
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, exposurePrefix), "/")
	if id == "" {
		return "", "", false
	}
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if t, found := strings.CutPrefix(strings.TrimSpace(p), ticketSubprotocolPrefix); found && t != "" {
				return t, id, true
			}
		}
	}
	return "", "", false
}

// redeemExposureTicket authenticates a page socket's upgrade by its ticket.
// A refusal is opaque to the client; the log names the reason, never the
// ticket.
func (s *Server) redeemExposureTicket(r *http.Request, t, exposureID string) (auth.Principal, bool) {
	p, reason := s.spendTicket(r, t, exposureTicketFormat, exposureTicketReplayGate, exposureID)
	if reason != "" {
		slog.Info("auth: page socket ticket refused", "reason", reason)
		return auth.Principal{}, false
	}
	return p, true
}

// handleExposureTicket serves POST /v1/workspaces/exposure-tickets/{id}: a
// one-time ticket for page id's socket, to the page's own user, bound to the
// credential of a bearer header. The middleware admitted the caller as it
// admits the page.
func (s *Server) handleExposureTicket(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.exposures == nil {
		exposureNotFound(w)
		return
	}
	if _, viaCookie := uiAuthFrom(r.Context()); viaCookie || !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "bearer_required",
			"a page socket's ticket is minted only with the bearer in the Authorization header")
		return
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || p.Legacy || p.TenantID == "" || p.Subject == "" {
		exposureNotFound(w)
		return
	}
	id := r.PathValue("id")
	if _, _, err := s.exposures.x.OpenExposure(r.Context(), p.TenantID, p.Subject, id); err != nil {
		if errors.Is(err, lifecycle.ErrExposureNotFound) {
			exposureNotFound(w)
			return
		}
		slog.WarnContext(r.Context(), "workspace exposure: cannot read the exposure", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "the page cannot be served now")
		return
	}
	key, err := s.clientToolsTicketKey()
	if err != nil {
		if !errors.Is(err, errTicketUnavailable) {
			slog.Error("auth: page socket ticket key unavailable", "err", err)
		}
		writeJSONError(w, http.StatusServiceUnavailable, "ticket_unavailable",
			"this server issues no tickets: LOOMCYCLE_SECRET_KEY is not set")
		return
	}
	c, err := s.ticketBinding(r, p)
	if err != nil {
		slog.Warn("auth: page socket ticket not issued", "tenant", p.TenantID, "subject", p.Subject, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "ticket_unavailable", "a ticket could not be issued for this credential; try again")
		return
	}
	c.Exposure = id
	exp := time.Now().Add(clientToolsTicketTTL)
	c.Exp = exp.UnixMilli()
	t, err := signTicket(key, exposureTicketFormat, c)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "the ticket could not be issued")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "exposure_socket_ticket_issued", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetDefID: tokenDefOf(c), AuthClient: p.OAuthClient,
		AuthGate: p.IssuedByGate, AuthReason: c.Kind, AuthRecordID: c.JTI, SourceAddr: s.clientSource(r)})
	writeJSON(w, http.StatusOK, map[string]string{"ticket": t, "expires_at": exp.UTC().Format(time.RFC3339Nano)})
}

// reauthenticateExposureSocket re-checks an open page socket's credential at
// each recheck. A socket a ticket opened re-resolves the credential the
// ticket names, without spending it again or minding its 30 seconds; any
// other, the request's own credential as the document feed does. Either
// way the route must still admit it.
func (s *Server) reauthenticateExposureSocket(r *http.Request) (auth.Principal, error) {
	t, id, ok := exposureTicketOf(r)
	if !ok || r.Header.Get("Authorization") != "" {
		return s.reauthenticateFeed(r)
	}
	c, reason := s.verifiedExposureTicket(t, id)
	if reason != "" {
		return auth.Principal{}, errFeedCredentialRevoked
	}
	p, reason := s.ticketHolder(r, c)
	if reason != "" {
		return auth.Principal{}, errFeedCredentialRevoked
	}
	return s.readmit(r, p)
}

// exposureSocketExpiry is when an open page socket's credential stops being
// valid: for a ticket's socket, the end of the token the ticket names; for
// any other, the request's credential's (credentialExpiry).
func (s *Server) exposureSocketExpiry(r *http.Request) time.Time {
	t, id, ok := exposureTicketOf(r)
	if !ok || r.Header.Get("Authorization") != "" {
		return s.credentialExpiry(r)
	}
	c, reason := s.verifiedExposureTicket(t, id)
	if reason != "" || c.Kind != ticketKindToken || s.store == nil {
		return time.Time{}
	}
	row, err := s.store.OperatorTokenDefGet(r.Context(), c.Ref)
	if err != nil || (row.ExpiresAt.IsZero() && row.RetiredAt.IsZero()) {
		return time.Time{}
	}
	return row.ValidUntil()
}

// verifiedExposureTicket checks a page ticket's signature and page; not its
// expiry or its spend.
func (s *Server) verifiedExposureTicket(t, id string) (ticketClaims, string) {
	key, err := s.clientToolsTicketKey()
	if err != nil {
		return ticketClaims{}, "no_ticket_key"
	}
	c, reason := verifyTicket(key, exposureTicketFormat, t)
	if reason != "" {
		return ticketClaims{}, reason
	}
	if c.Exposure != id {
		return ticketClaims{}, "another_page"
	}
	return c, ""
}
