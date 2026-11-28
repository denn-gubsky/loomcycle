package http

// workspace_exposure_frame.go — a page's frame capability (RFC HI §1, D1–D3
// as ruled by the owner 2026-11-28).
//
// A browser frame cannot send the Authorization header, so a page could not
// be shown in an <iframe>. The page's own user mints a capability over an
// ordinary call its bearer header authenticated, and gets a path,
// /v1/workspaces/exposures/{id}/~f/<cap>/, that serves that one page with no
// header: relative addresses inside the page keep the capability with no
// help from the page.
//
//   - The capability is the socket ticket's shape under its own format and
//     MAC domain (lxf1), signed under the key derived from
//     LOOMCYCLE_SECRET_KEY, naming the minting credential by reference and
//     bound to it by a peppered MAC, and to one page by its Exposure claim.
//     It sits in an address, so it names the tenant, subject and app only by
//     a peppered digest keyed by its jti (Who).
//   - It is stateless and not spent: a frame makes many requests. Every
//     request re-checks the minting credential, through a cache of at most
//     30 seconds, so a retired or revoked token stops the frame within it;
//     an open socket re-checks without the cache at its own recheck.
//   - Its life (D2): ten minutes on its own; after that only while a client
//     lease on the page is live; never past its hard end, which is mint +
//     max_lease_s (ten minutes while leases are off).
//   - It is read on the exposure route's frame path alone, and there it is
//     the only credential: no bearer, ticket or cookie is read beside it. It
//     is never a bearer anywhere (the bearer path never reads an address, and
//     lxf1 verifies as nothing else), it never reaches the application (the
//     route cuts its prefix, and Referer, before the application sees the
//     request), and it is never logged: refusals log a reason, the mint and
//     the first use its jti, and the panic log redacts it.

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

const (
	// exposureFrameFormat is a frame capability's format and MAC domain.
	exposureFrameFormat = "lxf1"
	// exposureFrameSegment marks the frame form of the route's path:
	// {id}/~f/<cap>/…. An application path that starts with it is reachable
	// only through a frame.
	exposureFrameSegment = "~f"
	// exposureFrameOwnLife is how long a capability is valid with no lease
	// (D2), and its whole life while leases are off.
	exposureFrameOwnLife = 10 * time.Minute
	// exposureFrameRecheck is the longest the minting credential's answer is
	// reused before it is resolved again (RFC HI §1: at most 30 s).
	exposureFrameRecheck = 30 * time.Second
	// exposureFrameFirstUseGate namespaces a capability's jti in the shared
	// one-time store, so its first use is audited once across replicas.
	exposureFrameFirstUseGate = "exposure-frame-first-use"
	// exposureFrameSkew is how far a capability's mint time may lie ahead
	// of this replica's clock.
	exposureFrameSkew = time.Minute
	// exposureFrameMaxLife bounds the hard life a capability may claim: the
	// longest max_lease_s config takes (86400 s), and a minute of skew.
	exposureFrameMaxLife = 24*time.Hour + exposureFrameSkew
	// redactedSegment stands for a capability in a logged path.
	redactedSegment = "[redacted]"
)

// exposureFrameLife is a capability's hard life from its mint: max_lease_s,
// or the ten minutes of its own life while leases are off. D2's "never past
// max_lease_s" holds even when max_lease_s is under ten minutes.
func exposureFrameLife(maxLease time.Duration) time.Duration {
	if maxLease <= 0 {
		return exposureFrameOwnLife
	}
	return maxLease
}

// exposureFrameHardEnd is the end no lease carries capability c past: its
// own hard end, and mint + the current max_lease_s (an operator who lowered
// the bound, or turned leases off, is not held to an older, longer one).
// The zero time: the claims are not a capability this build mints.
func exposureFrameHardEnd(c ticketClaims, maxLease time.Duration, now time.Time) time.Time {
	iat, exp := time.UnixMilli(c.Iat), time.UnixMilli(c.Exp)
	if c.Iat == 0 || iat.After(now.Add(exposureFrameSkew)) || !exp.After(iat) || exp.Sub(iat) > exposureFrameMaxLife {
		return time.Time{}
	}
	if bound := iat.Add(exposureFrameLife(maxLease)); bound.Before(exp) {
		return bound
	}
	return exp
}

// exposureFrameEnd is when capability c stops being valid, as of now: its
// own ten minutes, then the page's lease while one is live, never past its
// hard end. The zero time: the claims are not a capability this build
// mints.
func exposureFrameEnd(c ticketClaims, lease *time.Time, maxLease time.Duration, now time.Time) time.Time {
	hard := exposureFrameHardEnd(c, maxLease, now)
	if hard.IsZero() {
		return hard
	}
	end := time.UnixMilli(c.Iat).Add(exposureFrameOwnLife)
	if maxLease > 0 && lease != nil && lease.After(end) {
		end = *lease
	}
	if end.After(hard) {
		end = hard
	}
	return end
}

// exposureFrameOf returns the capability and page id of a request on the
// route's frame path, and whether the path is in the frame form at all. It
// reads the exposure route alone (the mux's pattern, so no other route's
// path is ever read for a capability). frameCap is empty when the frame
// segment carries none, which is refused like a bad one.
func exposureFrameOf(r *http.Request) (frameCap, id string, framed bool) {
	if r.Pattern != exposureRoutePattern || !strings.HasPrefix(r.URL.Path, exposurePrefix) {
		return "", "", false
	}
	segs := strings.SplitN(strings.TrimPrefix(r.URL.Path, exposurePrefix), "/", 4)
	if len(segs) < 2 || segs[1] != exposureFrameSegment {
		return "", "", false
	}
	if len(segs) > 2 {
		frameCap = segs[2]
	}
	return frameCap, segs[0], true
}

// exposureFrameRest is the application's own path of a frame request (the
// escaped path past {id}/~f/<cap>), and the prefix a Location is rewritten
// under. ok is false when the escaped path does not spell the same
// capability the decoded one does.
func exposureFrameRest(r *http.Request, id, frameCap string) (rest, prefix string, ok bool) {
	esc, found := strings.CutPrefix(r.URL.EscapedPath(), exposurePrefix+id+"/")
	if !found {
		return "", "", false
	}
	segs := strings.SplitN(esc, "/", 3)
	if len(segs) < 2 {
		return "", "", false
	}
	if seg, err := url.PathUnescape(segs[0]); err != nil || seg != exposureFrameSegment {
		return "", "", false
	}
	if seg, err := url.PathUnescape(segs[1]); err != nil || seg != frameCap {
		return "", "", false
	}
	rest = "/"
	if len(segs) == 3 {
		rest += segs[2]
	}
	return rest, exposurePrefix + id + "/" + exposureFrameSegment + "/" + frameCap, true
}

// redactExposureFrame is path as it may be logged: a frame capability's
// segment replaced, the rest kept.
func redactExposureFrame(path string) string {
	rest, ok := strings.CutPrefix(path, exposurePrefix)
	if !ok {
		return path
	}
	segs := strings.SplitN(rest, "/", 4)
	if len(segs) < 3 || segs[1] != exposureFrameSegment {
		return path
	}
	segs[2] = redactedSegment
	return exposurePrefix + strings.Join(segs, "/")
}

// exposureFrameWho binds a capability to who minted it without naming them.
func exposureFrameWho(h auth.TokenHasher, jti string, p auth.Principal) string {
	return h.Hash("loomcycle/exposure-frame-who/v1:" + jti + "\x00" + p.TenantID + "\x00" + p.Subject + "\x00" + p.OAuthClient)
}

// verifiedExposureFrame checks a capability's signature, shape and page;
// not its life, nor its credential.
func (s *Server) verifiedExposureFrame(frameCap, id string) (ticketClaims, string) {
	key, err := s.clientToolsTicketKey()
	if err != nil {
		return ticketClaims{}, "no_frame_key"
	}
	c, reason := verifyTicket(key, exposureFrameFormat, frameCap)
	if reason != "" {
		return ticketClaims{}, reason
	}
	if c.Iat == 0 || c.Who == "" || c.Tenant != "" || c.Subject != "" {
		return ticketClaims{}, "malformed"
	}
	if c.Exposure == "" || c.Exposure != id {
		return ticketClaims{}, "another_page"
	}
	return c, ""
}

// exposureFrameHolder re-resolves the credential a verified capability names
// and checks it still is who and what minted it (ticketHolder's check, with
// who compared by digest). It neither checks the capability's life nor
// caches.
func (s *Server) exposureFrameHolder(r *http.Request, c ticketClaims) (auth.Principal, string) {
	h, err := s.hasher()
	if err != nil {
		return auth.Principal{}, "no_hasher"
	}
	bound := func(credHash string) bool { return macEqual(ticketCredMAC(h, c.JTI, credHash), c.CredMAC) }
	p, reason := s.ticketCredentialPrincipal(r, c, bound)
	if reason != "" {
		return auth.Principal{}, reason
	}
	authority, err := ticketAuthority(p)
	if err != nil {
		return auth.Principal{}, "authority_unreadable"
	}
	if !macEqual(exposureFrameWho(h, c.JTI, p), c.Who) || !macEqual(authority, c.Authority) {
		return auth.Principal{}, "authority_changed"
	}
	return p, ""
}

// redeemExposureFrame authenticates a request on the frame path by its
// capability: signature, page, hard end, then the minting credential
// through the recheck cache. The lease half of its life needs the page's
// row, which the route reads (D2). A refusal is opaque to the client; the
// log names the reason, never the capability.
func (s *Server) redeemExposureFrame(r *http.Request, frameCap, id string) (auth.Principal, bool) {
	p, reason := s.exposureFramePrincipal(r, frameCap, id)
	if reason != "" {
		slog.Info("auth: page frame refused", "reason", reason)
		return auth.Principal{}, false
	}
	return p, true
}

func (s *Server) exposureFramePrincipal(r *http.Request, frameCap, id string) (auth.Principal, string) {
	// The frame path takes the capability alone: a bearer header, or a
	// bearer or ticket subprotocol, beside it is refused rather than one
	// of the two chosen.
	if r.Header.Get("Authorization") != "" || (isWebSocketUpgrade(r) && socketBearerOffered(r)) {
		return auth.Principal{}, "second_credential"
	}
	if frameCap == "" {
		return auth.Principal{}, "malformed"
	}
	c, reason := s.verifiedExposureFrame(frameCap, id)
	if reason != "" {
		return auth.Principal{}, reason
	}
	now := time.Now()
	hard := exposureFrameHardEnd(c, s.exposeMaxLease(), now)
	if hard.IsZero() {
		return auth.Principal{}, "malformed"
	}
	if !now.Before(hard) {
		return auth.Principal{}, "expired"
	}
	if p, ok := s.frameCreds.get(c.JTI, now); ok {
		return p, ""
	}
	p, reason := s.exposureFrameHolder(r, c)
	if reason != "" {
		s.frameCreds.drop(c.JTI)
		return auth.Principal{}, reason
	}
	s.frameCreds.put(c.JTI, p, now)
	s.auditExposureFrameFirstUse(r, c, p)
	return p, ""
}

// auditExposureFrameFirstUse records a capability's first use, by its jti,
// once across replicas. A store that cannot say is logged and the request
// served: the audit trail is not the capability's check.
func (s *Server) auditExposureFrameFirstUse(r *http.Request, c ticketClaims, p auth.Principal) {
	if s.store == nil {
		return
	}
	ctx := r.Context()
	fresh, err := s.store.AuthAssertionUseOnce(ctx, exposureFrameFirstUseGate, c.JTI, time.UnixMilli(c.Exp), time.Now())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("auth: cannot record a page frame's first use", "jti", c.JTI, "err", err)
		}
		return
	}
	if !fresh {
		return
	}
	s.recordAuthEvent(audit.Event{Action: "exposure_frame_first_used", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetDefID: tokenDefOf(c), TargetName: "exposure:" + c.Exposure,
		AuthClient: p.OAuthClient, AuthGate: p.IssuedByGate, AuthReason: c.Kind, AuthRecordID: c.JTI, SourceAddr: s.clientSource(r)})
}

// exposeMaxLease is workspace.expose.max_lease_s as the route sees it; 0
// while the route is off.
func (s *Server) exposeMaxLease() time.Duration {
	if s.exposures == nil {
		return 0
	}
	return s.exposures.x.ExposeMaxLease()
}

// handleExposureFrame serves POST /v1/workspaces/exposure-frames/{id}: a
// frame capability for page id, to the page's own user, bound to the
// credential of a bearer header. The middleware admitted the caller as it
// admits the page.
func (s *Server) handleExposureFrame(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.exposures == nil {
		exposureNotFound(w)
		return
	}
	if _, viaCookie := uiAuthFrom(r.Context()); viaCookie || !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		writeJSONError(w, http.StatusUnauthorized, "bearer_required",
			"a page's frame is minted only with the bearer in the Authorization header")
		return
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || p.Legacy || p.TenantID == "" || p.Subject == "" {
		exposureNotFound(w)
		return
	}
	id := r.PathValue("id")
	e, _, err := s.exposures.x.OpenExposure(r.Context(), p.TenantID, p.Subject, id)
	if err != nil {
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
			slog.Error("auth: page frame key unavailable", "err", err)
		}
		writeJSONError(w, http.StatusServiceUnavailable, "frame_unavailable",
			"this server issues no page frames: LOOMCYCLE_SECRET_KEY is not set")
		return
	}
	h, err := s.hasher()
	if err != nil {
		slog.Error("auth: page frame not issued", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "frame_unavailable", "a frame could not be issued; try again")
		return
	}
	c, err := s.ticketBinding(r, p)
	if err != nil {
		slog.Warn("auth: page frame not issued", "tenant", p.TenantID, "subject", p.Subject, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "frame_unavailable", "a frame could not be issued for this credential; try again")
		return
	}
	now := time.Now()
	maxLease := s.exposeMaxLease()
	c.Tenant, c.Subject, c.Client = "", "", ""
	c.Who = exposureFrameWho(h, c.JTI, p)
	c.Exposure = id
	c.Iat = now.UnixMilli()
	c.Exp = now.Add(exposureFrameLife(maxLease)).UnixMilli()
	frameCap, err := signTicket(key, exposureFrameFormat, c)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "the frame could not be issued")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "exposure_frame_issued", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetDefID: tokenDefOf(c), TargetName: "exposure:" + id, AuthClient: p.OAuthClient,
		AuthGate: p.IssuedByGate, AuthReason: c.Kind, AuthRecordID: c.JTI, SourceAddr: s.clientSource(r)})
	// expires_at is the capability's end as of now: its ten minutes, or the
	// page's live lease's end. A renewed lease carries it further.
	writeJSON(w, http.StatusOK, map[string]string{
		"frame_path": exposurePrefix + id + "/" + exposureFrameSegment + "/" + frameCap + "/",
		"expires_at": exposureFrameEnd(c, e.LeaseExpiresAt, maxLease, now).UTC().Format(time.RFC3339Nano),
	})
}

// frameAdmission is the route's half of a frame request's check, the part
// that needs the page's row: the capability's life (D2), as of now.
type frameAdmission struct {
	claims ticketClaims
	// maxLease reads workspace.expose.max_lease_s now.
	maxLease func() time.Duration
}

// validFor reports whether the capability is valid now on page e.
func (f frameAdmission) validFor(e store.WorkspaceExposure, now time.Time) bool {
	return now.Before(exposureFrameEnd(f.claims, e.LeaseExpiresAt, f.maxLease(), now))
}

// needsLease reports whether the capability is past its own ten minutes, so
// only a live lease keeps it valid.
func (f frameAdmission) needsLease(now time.Time) bool {
	return !now.Before(time.UnixMilli(f.claims.Iat).Add(exposureFrameOwnLife))
}

// exposureFrameCache keeps each capability's resolved credential for at most
// its window, so a frame's many requests do not each read the credential.
// The zero value is ready. Bounded: when full, expired entries go, and if
// none have, all.
type exposureFrameCache struct {
	mu sync.Mutex
	// ttl overrides the window (tests); negative caches nothing.
	ttl time.Duration
	by  map[string]exposureFrameEntry
}

type exposureFrameEntry struct {
	p  auth.Principal
	at time.Time
}

// exposureFrameCacheMax bounds the entries one replica keeps.
const exposureFrameCacheMax = 4096

func (c *exposureFrameCache) window() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.windowLocked()
}

func (c *exposureFrameCache) windowLocked() time.Duration {
	if c.ttl != 0 {
		return c.ttl
	}
	return exposureFrameRecheck
}

// setWindow overrides the window (tests).
func (c *exposureFrameCache) setWindow(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = d
}

func (c *exposureFrameCache) get(jti string, now time.Time) (auth.Principal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.by[jti]
	if !ok || now.Sub(e.at) >= c.windowLocked() || now.Before(e.at) {
		return auth.Principal{}, false
	}
	return e.p, true
}

func (c *exposureFrameCache) put(jti string, p auth.Principal, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.windowLocked()
	if w <= 0 {
		return
	}
	if c.by == nil {
		c.by = map[string]exposureFrameEntry{}
	}
	if len(c.by) >= exposureFrameCacheMax {
		for k, e := range c.by {
			if now.Sub(e.at) >= w {
				delete(c.by, k)
			}
		}
		if len(c.by) >= exposureFrameCacheMax {
			c.by = map[string]exposureFrameEntry{}
		}
	}
	c.by[jti] = exposureFrameEntry{p: p, at: now}
}

func (c *exposureFrameCache) drop(jti string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.by, jti)
}
