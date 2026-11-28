package http

// workspace_exposure_lease.go — a client's lease on a workspace page (RFC HI
// §2, D4).
//
// POST /v1/workspaces/exposure-leases/{id} {ttl_s} → {lease_expires_at}
// leases page id until ttl_s from now, or keeps a later lease another client
// took; DELETE on the same address ends it for every client. While a lease is
// live the page does not close idle, and the parked interactive run that owns
// the page's workspace does not end at its idle deadline (idleExtendFor).
//
// The address is not the RFC's /v1/workspaces/exposures/{id}/lease: the page
// itself is served at /v1/workspaces/exposures/{id}/{path...}, every method,
// and the mux would give POST …/{id}/lease to the more specific pattern, so a
// page's own /lease path could not be reached. Beside exposure-tickets/{id}
// it shadows nothing.
//
//   - The bearer in the Authorization header only, as the ticket mint: a Web
//     UI cookie never takes a lease, so a page of another site cannot keep
//     pages (and runs) alive in the user's name.
//   - The page's own user only (tenant and subject, as the route admits). The
//     legacy token and open mode name no user. Every other case is the
//     unknown id's 404.
//   - ttl_s from 1 to workspace.expose.max_lease_s, else 400 naming the
//     bound; checked before the page is looked up. Leases off (max_lease_s
//     0) is 403 leases_off, answered before the page is looked up too, so it
//     says nothing of any page.

import (
	"context"
	"encoding/json"
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
	// exposureLeaseBody bounds a lease request's body: {"ttl_s": n}.
	exposureLeaseBody = 4 << 10
	// maxLeaseTTLS caps the ttl_s read before it becomes a duration, so a
	// huge number cannot wrap into the allowed range; anything this large is
	// past every max_lease_s and refused as such.
	maxLeaseTTLS = 1 << 31
	// idleExtendTimeout bounds a parked run's lease question at its idle
	// deadline: a store that does not answer ends the run as without a lease.
	idleExtendTimeout = 10 * time.Second
)

// exposureLeaseHolder is the caller a lease may be taken or ended for: one
// presenting its bearer in the Authorization header, naming a user. A
// cookie or a missing header is 401; a caller naming no user, the unknown
// id's 404.
func exposureLeaseHolder(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if _, viaCookie := uiAuthFrom(r.Context()); viaCookie || !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "bearer_required",
			"a page's lease is taken only with the bearer in the Authorization header")
		return auth.Principal{}, false
	}
	p, ok := exposureStatePrincipal(r)
	if !ok {
		exposureStateNotFound(w)
		return auth.Principal{}, false
	}
	return p, true
}

// writeExposureLeaseError answers a lifecycle refusal.
func writeExposureLeaseError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lifecycle.ErrExposureNotFound):
		exposureStateNotFound(w)
	case errors.Is(err, lifecycle.ErrLeasesOff):
		writeJSONError(w, http.StatusForbidden, "leases_off", err.Error())
	case errors.Is(err, lifecycle.ErrLeaseTTL):
		writeJSONError(w, http.StatusBadRequest, "invalid_argument", err.Error())
	default:
		slog.WarnContext(r.Context(), "workspace exposure: cannot write the page's lease", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "the page's lease cannot be written now")
	}
}

// handleExposureLease serves POST /v1/workspaces/exposure-leases/{id}.
func (s *Server) handleExposureLease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.exposures == nil {
		exposureStateNotFound(w)
		return
	}
	p, ok := exposureLeaseHolder(w, r)
	if !ok {
		return
	}
	var body struct {
		TTLS *int64 `json:"ttl_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, exposureLeaseBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_argument", `the body must be {"ttl_s": <seconds>}`)
		return
	}
	var ttlS int64
	if body.TTLS != nil {
		ttlS = *body.TTLS
	}
	if ttlS > maxLeaseTTLS {
		ttlS = maxLeaseTTLS
	}
	id := r.PathValue("id")
	end, err := s.exposures.x.LeaseExposure(r.Context(), p.TenantID, p.Subject, id, time.Duration(ttlS)*time.Second)
	if err != nil {
		writeExposureLeaseError(w, r, err)
		return
	}
	s.recordAuthEvent(audit.Event{Action: "exposure_lease_taken", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetName: "exposure:" + id, AuthClient: p.OAuthClient, AuthGate: p.IssuedByGate,
		SourceAddr: s.clientSource(r)})
	writeJSON(w, http.StatusOK, map[string]string{"lease_expires_at": end.UTC().Format(time.RFC3339Nano)})
}

// handleExposureLeaseRelease serves DELETE /v1/workspaces/exposure-leases/{id}.
func (s *Server) handleExposureLeaseRelease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.exposures == nil {
		exposureStateNotFound(w)
		return
	}
	p, ok := exposureLeaseHolder(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.exposures.x.ReleaseExposureLease(r.Context(), p.TenantID, p.Subject, id); err != nil {
		writeExposureLeaseError(w, r, err)
		return
	}
	s.recordAuthEvent(audit.Event{Action: "exposure_lease_released", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetName: "exposure:" + id, AuthClient: p.OAuthClient, AuthGate: p.IssuedByGate,
		SourceAddr: s.clientSource(r)})
	w.WriteHeader(http.StatusNoContent)
}

// idleExtendFor is a top-level interactive run's loop.RunOptions.IdleExtend
// (RFC HI D4): at its idle deadline the run asks for the latest live lease on
// a page of its workspaces. The answer is read from the store, since the
// lease may have been taken through another replica. A failure to answer
// ends the run as it would without a lease. nil while leases are off, or
// workspace.expose is.
func (s *Server) idleExtendFor(runID string) func(context.Context) time.Time {
	if s.exposures == nil || s.exposures.x.ExposeMaxLease() <= 0 || runID == "" {
		return nil
	}
	x := s.exposures.x
	return func(ctx context.Context) time.Time {
		ctx, cancel := context.WithTimeout(ctx, idleExtendTimeout)
		defer cancel()
		end, err := x.RunLeaseEnd(ctx, runID)
		if err != nil {
			slog.WarnContext(ctx, "workspace exposure: cannot read a parked run's page leases; it ends idle", "run_id", runID, "err", err)
			return time.Time{}
		}
		return end
	}
}
