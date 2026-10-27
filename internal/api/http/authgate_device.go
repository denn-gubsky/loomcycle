package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/device"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P5 — DeviceGate, the RFC 8628 device authorization grant.
//
//	CLI                                  server                         browser (signed in)
//	POST /v1/_auth/device ─────────────▶ pending row, two codes
//	   ◀── device_code, user_code, verification_uri, interval
//	show user_code + link                                               /ui/device: type the code
//	                                     POST /v1/_auth/device/info ◀── what is asking (client, source, the scopes)
//	                                     POST /v1/_auth/device/approve ◀── approve | deny (session + CSRF)
//	POST /v1/_auth/device/token (poll) ─▶ approved → redeemed once → issued token for the APPROVER
//
// Who the token is for is decided only by the approval: the person whose Web
// UI session approved the code, through whatever interactive gate that session
// came from. The poller holds the device code and nothing else; it never names
// a tenant, a subject or a scope: the token carries the approver's own
// member scopes (issueGateToken), and a client's `scope` is refused rather
// than half-honoured.
//
// What bounds a guesser:
//   - the device code is 256 random bits, stored as its SHA-256; an unknown
//     code costs the source a throttle failure;
//   - the user code is ~34 bits, lives ten minutes, is stored as a keyed hash,
//     and every wrong code typed on the approval page costs both the person
//     and their source address a failure (deviceGuessLimit per window, then
//     blocked) — a few dozen tries at a space of ten billion;
//   - each source may start only deviceStartLimit flows per window, so the
//     number of live codes an attacker could be guessing at stays small.

const (
	// deviceWindow is the throttle window of every device-flow counter.
	deviceWindow = 15 * time.Minute
	// deviceGuessLimit wrong user codes per person or source per window.
	deviceGuessLimit = 10
	// deviceStartLimit device flows a source may start per window.
	deviceStartLimit = 30
	// deviceBadPollLimit unknown device codes a source may poll per window.
	deviceBadPollLimit = 30
	// maxDeviceFormBytes bounds the two unauthenticated form posts.
	maxDeviceFormBytes = 4 << 10
)

// deviceThrottleBlock blocks a key for the rest of the window once it reaches
// limit.
func deviceThrottleBlock(limit int) func(int) time.Duration {
	return func(n int) time.Duration {
		if n >= limit {
			return deviceWindow
		}
		return 0
	}
}

// deviceKey is a throttle key whose identifying part is MAC'd, so the
// throttle table holds no address or subject.
func deviceKey(h auth.TokenHasher, scope, id string) string {
	return "device:" + scope + ":" + h.Hash("loomcycle/device-throttle/v1:"+scope+"\x00"+id)
}

// deviceThrottled reports whether key is blocked; a store error fails closed.
func (s *Server) deviceThrottled(ctx context.Context, key string) bool {
	_, until, err := s.store.AuthThrottleGet(ctx, key, time.Now())
	if err != nil {
		slog.Warn("auth: device throttle read failed (refusing)", "err", err)
		return true
	}
	return until.After(time.Now())
}

func (s *Server) deviceCount(ctx context.Context, key string, limit int) {
	if _, _, err := s.store.AuthThrottleRecordFailure(ctx, key, time.Now(), deviceWindow, deviceThrottleBlock(limit)); err != nil {
		slog.Warn("auth: recording a device throttle count failed", "err", err)
	}
}

// userCodeHash is a user code's stored form: keyed under the token pepper (a
// plain digest of a 34-bit code would be reversible by enumeration).
func userCodeHash(h auth.TokenHasher, canonical string) string {
	return h.Hash(device.UserCodeHashLabel + canonical)
}

// deviceGate is the configured device gate; ok false when there is none (the
// device routes then answer as if they did not exist).
func (s *Server) deviceGate() (config.AuthGate, bool) {
	g, ok := s.cfg().ResolvedAuth.DeviceGate()
	return g, ok && g.Device != nil
}

// oauthError writes an RFC 6749 §5.2 error.
func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// readDeviceForm parses a bounded urlencoded (or JSON object of strings) body.
func readDeviceForm(r *http.Request) (url.Values, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxDeviceFormBytes+1))
	if err != nil || len(raw) > maxDeviceFormBytes {
		return nil, errors.New("body too large")
	}
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("Content-Type")), "application/json") {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		v := url.Values{}
		for k, val := range m {
			v.Set(k, val)
		}
		return v, nil
	}
	return url.ParseQuery(string(raw))
}

// handleDeviceAuthorize is POST /v1/_auth/device (RFC 8628 §3.1).
func (s *Server) handleDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.deviceGate(); !ok || !s.authConfigured(ctx) || s.store == nil {
		writeJSONError(w, http.StatusNotFound, "device_login_disabled", "device login is not enabled on this server")
		return
	}
	h, err := s.hasher()
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
		return
	}
	src := s.clientSource(r)
	startKey := deviceKey(h, "start", throttleSourceKey(src))
	if s.deviceThrottled(ctx, startKey) {
		oauthError(w, http.StatusTooManyRequests, device.ErrSlowDown, "too many device logins from this address; try again later")
		return
	}
	form, err := readDeviceForm(r)
	if err != nil {
		oauthError(w, http.StatusBadRequest, device.ErrInvalidRequest, "the request is not a valid form")
		return
	}
	clientID := form.Get("client_id")
	if !device.ValidClientID(clientID) {
		oauthError(w, http.StatusBadRequest, device.ErrInvalidClient, "client_id must be 1-64 of letters, digits, - _ . :")
		return
	}
	if form.Get("scope") != "" {
		oauthError(w, http.StatusBadRequest, device.ErrInvalidScope,
			"this server issues the approving person's own scopes; omit scope")
		return
	}
	s.deviceCount(ctx, startKey, deviceStartLimit)
	now := time.Now()
	var userCode, deviceCode string
	for attempt := 0; ; attempt++ {
		dc, dch, err := device.NewDeviceCode()
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
			return
		}
		uc, err := device.NewUserCode()
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
			return
		}
		err = s.store.DeviceAuthCreate(ctx, store.DeviceAuthRow{
			DeviceCodeHash: dch, UserCodeHash: userCodeHash(h, uc), ClientID: clientID,
			SourceAddr: src, CreatedAt: now,
			ExpiresAt: now.Add(device.CodeTTL), Interval: device.DefaultInterval,
		})
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) && attempt < 3 {
			continue // a live user code collided; draw again
		}
		if err != nil {
			slog.Error("auth: creating a device authorization failed", "err", err)
			oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
			return
		}
		userCode, deviceCode = uc, dc
		break
	}
	verify := strings.TrimRight(s.cfg().Env.PublicURL, "/") + "/ui/device"
	formatted := device.FormatUserCode(userCode)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        formatted,
		"verification_uri": verify,
		// The fragment never reaches a server log or a Referer.
		"verification_uri_complete": verify + "#" + formatted,
		"expires_in":                int(device.CodeTTL.Seconds()),
		"interval":                  int(device.DefaultInterval.Seconds()),
	})
}

// handleDeviceToken is POST /v1/_auth/device/token (RFC 8628 §3.4–3.5).
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dg, ok := s.deviceGate()
	if !ok || !s.authConfigured(ctx) || s.store == nil {
		writeJSONError(w, http.StatusNotFound, "device_login_disabled", "device login is not enabled on this server")
		return
	}
	h, err := s.hasher()
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
		return
	}
	badKey := deviceKey(h, "poll", throttleSourceKey(s.clientSource(r)))
	if s.deviceThrottled(ctx, badKey) {
		oauthError(w, http.StatusTooManyRequests, device.ErrSlowDown, "too many unknown device codes from this address")
		return
	}
	form, err := readDeviceForm(r)
	if err != nil {
		oauthError(w, http.StatusBadRequest, device.ErrInvalidRequest, "the request is not a valid form")
		return
	}
	if form.Get("grant_type") != device.GrantType {
		oauthError(w, http.StatusBadRequest, device.ErrUnsupportedGrantType, "grant_type must be "+device.GrantType)
		return
	}
	code := form.Get("device_code")
	if !device.WellFormedDeviceCode(code) {
		s.deviceCount(ctx, badKey, deviceBadPollLimit)
		oauthError(w, http.StatusBadRequest, device.ErrInvalidGrant, "unknown device_code")
		return
	}
	now := time.Now()
	row, early, err := s.store.DeviceAuthPoll(ctx, device.HashDeviceCode(code), now, device.SlowDownStep, device.MaxInterval)
	if err != nil {
		if isNotFound(err) {
			s.deviceCount(ctx, badKey, deviceBadPollLimit)
			oauthError(w, http.StatusBadRequest, device.ErrInvalidGrant, "unknown device_code")
			return
		}
		slog.Error("auth: device poll failed", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
		return
	}
	// A device code is bound to the client that asked for it (RFC 8628 §3.4).
	if form.Get("client_id") != row.ClientID {
		oauthError(w, http.StatusUnauthorized, device.ErrInvalidClient, "client_id does not match the device code")
		return
	}
	switch {
	case !row.Live(now):
		oauthError(w, http.StatusBadRequest, device.ErrExpiredToken, "the device code expired; start a new login")
		return
	case row.Status == store.DeviceAuthRedeemed:
		oauthError(w, http.StatusBadRequest, device.ErrInvalidGrant, "the device code was already used")
		return
	case row.Status == store.DeviceAuthDenied:
		oauthError(w, http.StatusBadRequest, device.ErrAccessDenied, "the login was denied")
		return
	case early:
		oauthError(w, http.StatusBadRequest, device.ErrSlowDown, "polling too fast; wait the interval")
		return
	case row.Status == store.DeviceAuthPending:
		oauthError(w, http.StatusBadRequest, device.ErrAuthorizationPending, "waiting for the person to approve")
		return
	}
	// Approved. Spend the code before issuing, so a racing second poll can
	// never receive a second token; a failure after this point costs the
	// person a new login, never a duplicate token.
	red, ok, err := s.store.DeviceAuthRedeem(ctx, row.DeviceCodeHash, now)
	if err != nil {
		slog.Error("auth: device redeem failed", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "device login is unavailable")
		return
	}
	if !ok {
		oauthError(w, http.StatusBadRequest, device.ErrInvalidGrant, "the device code was already used")
		return
	}
	token, issued, err := s.issueDeviceToken(ctx, dg, red)
	if err != nil {
		s.recordAuthEvent(audit.Event{
			Action: "auth_login_failed", ActorTenant: red.DecidedTenant, ActorSubject: red.DecidedSubject,
			AuthGate: dg.Name, AuthReason: deviceRefusalReason(err), SourceAddr: s.clientSource(r),
		})
		if errors.Is(err, errDeviceRefused) {
			oauthError(w, http.StatusBadRequest, device.ErrAccessDenied, "the login is no longer allowed")
			return
		}
		slog.Error("auth: issuing a device token failed", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "the token could not be issued")
		return
	}
	s.recordAuthEvent(audit.Event{
		Action: "auth_login", ActorTenant: red.DecidedTenant, ActorSubject: red.DecidedSubject,
		AuthGate: dg.Name, AuthAMR: []string{"device"}, SourceAddr: s.clientSource(r),
	})
	resp := map[string]any{"access_token": token, "token_type": "Bearer"}
	if !issued.ExpiresAt.IsZero() {
		resp["expires_in"] = int64(time.Until(issued.ExpiresAt).Seconds())
	}
	if len(issued.AllowedScopes) > 0 {
		resp["scope"] = strings.Join(issued.AllowedScopes, " ")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// errDeviceRefused: the approval no longer holds (the user was disabled, the
// tenant's gate policy changed, the session's gate is not allowed).
var errDeviceRefused = errors.New("device login refused")

func deviceRefusalReason(err error) string {
	if errors.Is(err, errDeviceRefused) {
		return strings.TrimPrefix(err.Error(), errDeviceRefused.Error()+": ")
	}
	return "issue_failed"
}

// deviceSessionCaller is the signed-in person on the approval page: only a
// Web UI session may approve, whatever gate it came through, never a bearer
// (a CLI holding a token could otherwise approve its own device codes, or a
// leaked token approve an attacker's).
func deviceSessionCaller(ctx context.Context) (*store.UISessionRow, bool) {
	a, ok := uiAuthFrom(ctx)
	if !ok || a.session == nil {
		return nil, false
	}
	return a.session, true
}

type deviceDecisionRequest struct {
	UserCode string `json:"user_code"`
	// Approve: true approves, false denies. Absent on /info.
	Approve *bool `json:"approve,omitempty"`
}

// lookupForApproval finds the live, pending authorization a typed user code
// names, counting a miss against the person and their source.
func (s *Server) lookupForApproval(w http.ResponseWriter, r *http.Request, sess *store.UISessionRow) (store.DeviceAuthRow, string, *deviceDecisionRequest, bool) {
	ctx := r.Context()
	h, err := s.hasher()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "device_unavailable", "device login is unavailable")
		return store.DeviceAuthRow{}, "", nil, false
	}
	personKey := deviceKey(h, "guess-person", sess.TenantID+"\x00"+sess.Subject)
	srcKey := deviceKey(h, "guess-src", throttleSourceKey(s.clientSource(r)))
	if s.deviceThrottled(ctx, personKey) || s.deviceThrottled(ctx, srcKey) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many wrong codes; wait a few minutes")
		return store.DeviceAuthRow{}, "", nil, false
	}
	var req deviceDecisionRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxDeviceFormBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "body must be {user_code[, approve]}")
		return store.DeviceAuthRow{}, "", nil, false
	}
	miss := func() {
		s.deviceCount(ctx, personKey, deviceGuessLimit)
		s.deviceCount(ctx, srcKey, deviceGuessLimit)
		// One answer for unknown, expired and already-decided codes.
		writeJSONError(w, http.StatusNotFound, "code_invalid", "that code is not valid or has expired")
	}
	canon, ok := device.NormalizeUserCode(req.UserCode)
	if !ok {
		miss()
		return store.DeviceAuthRow{}, "", nil, false
	}
	uh := userCodeHash(h, canon)
	row, err := s.store.DeviceAuthGetByUserCode(ctx, uh)
	if err != nil {
		if !isNotFound(err) {
			slog.Error("auth: device lookup failed", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "device_unavailable", "device login is unavailable")
			return store.DeviceAuthRow{}, "", nil, false
		}
		miss()
		return store.DeviceAuthRow{}, "", nil, false
	}
	if row.Status != store.DeviceAuthPending || !row.Live(time.Now()) {
		miss()
		return store.DeviceAuthRow{}, "", nil, false
	}
	return row, uh, &req, true
}

// handleDeviceInfo is POST /v1/_auth/device/info: what a code is asking for,
// so the person sees the client, the scopes and where the request came from
// before they approve. The code rides the body, never the URL.
func (s *Server) handleDeviceInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.deviceGate(); !ok || s.store == nil {
		writeJSONError(w, http.StatusNotFound, "device_login_disabled", "device login is not enabled on this server")
		return
	}
	sess, ok := deviceSessionCaller(r.Context())
	if !ok {
		writeJSONError(w, http.StatusForbidden, "session_required", "approve a device login from a signed-in Web UI session")
		return
	}
	row, _, _, ok := s.lookupForApproval(w, r, sess)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.deviceApprovalView(r.Context(), row, sess))
}

// deviceApprovalView is what the approval page shows: who is asking and what
// the token would carry — the approver's own member scopes. scopes is empty
// when the person is not a user a token can be issued to (approving is then
// refused).
func (s *Server) deviceApprovalView(ctx context.Context, row store.DeviceAuthRow, sess *store.UISessionRow) map[string]any {
	scopes := []string{}
	if u, err := s.store.UserGet(ctx, sess.TenantID, sess.Subject); err == nil {
		if g, gerr := auth.GrantableUserScopes(u.AccessMode); gerr == nil {
			scopes = g
		}
	}
	return map[string]any{
		"client_id":   row.ClientID,
		"scopes":      scopes,
		"source_addr": row.SourceAddr,
		"created_at":  row.CreatedAt.UTC(),
		"expires_at":  row.ExpiresAt.UTC(),
		"tenant_id":   sess.TenantID,
		"subject":     sess.Subject,
	}
}

// handleDeviceApprove is POST /v1/_auth/device/approve {user_code, approve}.
// authMiddleware has already applied the session CSRF check (the custom
// header and a same-origin request) to this cookie-authenticated write.
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dg, ok := s.deviceGate()
	if !ok || s.store == nil {
		writeJSONError(w, http.StatusNotFound, "device_login_disabled", "device login is not enabled on this server")
		return
	}
	sess, ok := deviceSessionCaller(ctx)
	if !ok {
		writeJSONError(w, http.StatusForbidden, "session_required", "approve a device login from a signed-in Web UI session")
		return
	}
	row, uh, req, ok := s.lookupForApproval(w, r, sess)
	if !ok {
		return
	}
	if req.Approve == nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "approve must be true or false")
		return
	}
	var pin devicePin
	if *req.Approve {
		err := s.deviceApprovalAllowed(ctx, sess)
		if err == nil {
			pin, err = s.pinDeviceApproval(r, sess, row.DeviceCodeHash)
		}
		if err != nil {
			s.recordAuthEvent(audit.Event{
				Action: "auth_login_failed", ActorTenant: sess.TenantID, ActorSubject: sess.Subject,
				AuthGate: dg.Name, AuthReason: deviceRefusalReason(err), SourceAddr: s.clientSource(r),
			})
			if !errors.Is(err, errDeviceRefused) {
				slog.Error("auth: device approval check failed", "err", err)
				writeJSONError(w, http.StatusInternalServerError, "device_unavailable", "device login is unavailable")
				return
			}
			writeJSONError(w, http.StatusForbidden, "device_not_allowed", "this account cannot approve a device login")
			return
		}
	}
	decided, err := s.store.DeviceAuthDecide(ctx, uh, store.DeviceAuthDecision{
		Approve: *req.Approve, TenantID: sess.TenantID, Subject: sess.Subject, Gate: sess.Gate, At: time.Now(),
		Session: pin.session, CredMAC: pin.credMAC, NotAfter: pin.notAfter,
	})
	if err != nil {
		slog.Error("auth: device decision failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "device_unavailable", "device login is unavailable")
		return
	}
	if !decided {
		writeJSONError(w, http.StatusNotFound, "code_invalid", "that code is not valid or has expired")
		return
	}
	action := "auth_device_denied"
	if *req.Approve {
		action = "auth_device_approved"
	}
	s.recordAuthEvent(audit.Event{
		Action: action, ActorTenant: sess.TenantID, ActorSubject: sess.Subject, AuthGate: sess.Gate,
		AuthReason: "client:" + row.ClientID, SourceAddr: s.clientSource(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"approved": *req.Approve})
}

// deviceApprovalAllowed decides, at approval, whether this session may approve
// a device login: its subject must be an active user (an issued token is a
// user's token) and the tenant's gate policy must admit the session's gate.
// issueDeviceToken asks again at redemption: either may change in between.
func (s *Server) deviceApprovalAllowed(ctx context.Context, sess *store.UISessionRow) error {
	u, err := s.store.UserGet(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: not_a_user", errDeviceRefused)
		}
		return err
	}
	if u.Status != "active" {
		return fmt.Errorf("%w: user_not_active", errDeviceRefused)
	}
	// The token must not out-reach the session that approves it. A session
	// made from a restricted or narrower credential (a pasted token limited
	// to some agents, say) would otherwise trade itself for the user's full
	// member token.
	granted, err := auth.GrantableUserScopes(u.AccessMode)
	if err != nil {
		return fmt.Errorf("%w: access_mode", errDeviceRefused)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.TenantID != sess.TenantID || p.Subject != sess.Subject || !p.Restrictions.IsZero() {
		return fmt.Errorf("%w: session_narrower_than_token", errDeviceRefused)
	}
	for _, sc := range granted {
		if !auth.HasScope(p.Scopes, sc) {
			return fmt.Errorf("%w: session_narrower_than_token", errDeviceRefused)
		}
	}
	if err := s.tenantGateAllowed(ctx, sess.TenantID, sess.Gate); err != nil {
		if errors.Is(err, errTenantGatePolicy) {
			return fmt.Errorf("%w: tenant_policy", errDeviceRefused)
		}
		return err
	}
	return nil
}

// devicePin is what an approval records of the session that approved it (see
// store.DeviceAuthRow.DecidedSession).
type devicePin struct {
	session  string
	credMAC  string
	notAfter time.Time
}

// deviceCredMAC binds a device approval to the credential hash its session was
// made from, keyed by the pepper and the device code, so the redemption can
// re-check the credential without the session id (which the server never
// stores) and a row copied to another code binds nothing.
func deviceCredMAC(h auth.TokenHasher, deviceCodeHash, credHash string) string {
	return h.Hash("loomcycle/device-cred/v1:" + deviceCodeHash + "\x00" + credHash)
}

// sessionAbsoluteEnd is the latest a session can live to however active it
// stays: its absolute expiry, held to the configured lifetime.
func sessionAbsoluteEnd(row store.UISessionRow, policy config.AuthPolicy) time.Time {
	end := row.AbsoluteExpiresAt
	if t := row.CreatedAt.Add(policy.SessionAbsolute()); t.Before(end) {
		end = t
	}
	return end
}

// pinDeviceApproval records, at approval, what the approving session stands
// on: the session itself, its credential (re-resolved through the session's
// own binding, as on every request) and the latest the issued token may live
// to — the session's absolute end, or sooner the expiry of the token the
// session was made from.
//
// The cap is the ABSOLUTE end, not the idle one: idle expiry slides with use,
// so the session could legitimately reach its absolute end, and a CLI token
// held to an eight-hour idle window would be no login at all. What must not
// happen is a token outliving everything its approver could have reached — a
// session about to end, or a pasted token about to expire, laundered into a
// fresh full-length token. That would also let a chain of device logins
// extend one credential forever.
func (s *Server) pinDeviceApproval(r *http.Request, sess *store.UISessionRow, deviceCodeHash string) (devicePin, error) {
	ctx := r.Context()
	h, err := s.hasher()
	if err != nil {
		return devicePin{}, err
	}
	id, ok := s.sessionIDFromCookie(r)
	if !ok || !macEqual(sessionHash(id), sess.SessionHash) {
		return devicePin{}, fmt.Errorf("%w: session_unbound", errDeviceRefused)
	}
	var (
		credHash string
		pending  bool
	)
	_, ok, definitive := s.boundCredentialPrincipal(ctx, *sess, func(c string) bool {
		if macEqual(credentialMAC(id, h, c), sess.CredMAC) {
			credHash = c
			return true
		}
		return false
	}, &pending)
	if !ok || pending || credHash == "" {
		if !definitive {
			return devicePin{}, errors.New("re-reading the approving session's credential failed")
		}
		return devicePin{}, fmt.Errorf("%w: session_credential_gone", errDeviceRefused)
	}
	notAfter := sessionAbsoluteEnd(*sess, s.cfg().ResolvedAuth)
	// Only a token row carries an expiry; a password, an identity, a config
	// principal and the legacy secret do not.
	if sess.CredKind == credKindToken {
		tr, err := s.store.OperatorTokenDefGet(ctx, sess.CredRef)
		if err != nil {
			return devicePin{}, err
		}
		if !tr.ExpiresAt.IsZero() && tr.ExpiresAt.Before(notAfter) {
			notAfter = tr.ExpiresAt
		}
	}
	if !time.Now().Before(notAfter) {
		return devicePin{}, fmt.Errorf("%w: session_ending", errDeviceRefused)
	}
	return devicePin{session: sess.SessionHash, credMAC: deviceCredMAC(h, deviceCodeHash, credHash), notAfter: notAfter}, nil
}

// deviceApprovalLifetime re-checks at redemption what the approval pinned:
// the approving session must still be live and not ended, and the credential
// it was made from must still resolve as the one bound at approval (a retired
// or expired token, a changed password or an unlinked identity all fail). It
// returns how long the token may live: up to the pinned not-after, truncated
// to the second so the stamp a moment later stays inside it.
func (s *Server) deviceApprovalLifetime(ctx context.Context, row store.DeviceAuthRow, now time.Time) (time.Duration, error) {
	if row.DecidedSession == "" || row.DecidedCredMAC == "" || row.DecidedNotAfter.IsZero() {
		return 0, fmt.Errorf("%w: approval_unpinned", errDeviceRefused)
	}
	sess, err := s.store.UISessionGet(ctx, row.DecidedSession)
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: approving_session_ended", errDeviceRefused)
		}
		return 0, err
	}
	if !sess.RevokedAt.IsZero() || !now.Before(sessionExpiresAt(sess, s.cfg().ResolvedAuth)) ||
		sess.TenantID != row.DecidedTenant || sess.Subject != row.DecidedSubject {
		return 0, fmt.Errorf("%w: approving_session_ended", errDeviceRefused)
	}
	h, err := s.hasher()
	if err != nil {
		return 0, err
	}
	var pending bool
	_, ok, definitive := s.boundCredentialPrincipal(ctx, sess, func(c string) bool {
		return macEqual(deviceCredMAC(h, row.DeviceCodeHash, c), row.DecidedCredMAC)
	}, &pending)
	if !ok || pending {
		// Fail closed either way: the code is already spent, and a token the
		// server could not tie to a live credential is not issued.
		if !definitive {
			return 0, fmt.Errorf("%w: approving_credential_unverified", errDeviceRefused)
		}
		return 0, fmt.Errorf("%w: approving_credential_gone", errDeviceRefused)
	}
	left := row.DecidedNotAfter.Sub(now).Truncate(time.Second)
	if left <= 0 {
		return 0, fmt.Errorf("%w: approving_session_ended", errDeviceRefused)
	}
	return left, nil
}

// issueDeviceToken mints the token a redeemed device code yields: for the
// person who approved it, through the gate their session came from (its
// token_ttl and the tenant's gate policy apply, and the token is tagged with
// it), at most the device gate's own lifetime and never past what the
// approving session could reach (deviceApprovalLifetime), with its rate
// restriction.
func (s *Server) issueDeviceToken(ctx context.Context, dg config.AuthGate, row store.DeviceAuthRow) (string, store.OperatorTokenDefRow, error) {
	left, err := s.deviceApprovalLifetime(ctx, row, time.Now())
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	ttl := dg.Device.TokenTTL
	if ttl <= 0 || left < ttl {
		ttl = left
	}
	var r auth.Restrictions
	if d := dg.Device; d.RatePerMinute > 0 {
		r.Rate = &auth.RateLimit{PerMinute: d.RatePerMinute, Burst: d.RateBurst}
	}
	tok, issued, err := s.issueGateToken(ctx, row.DecidedTenant, row.DecidedSubject, row.DecidedGate, ttl, r)
	switch {
	case errors.Is(err, errGateTokenUser):
		return "", issued, fmt.Errorf("%w: user_not_active", errDeviceRefused)
	case errors.Is(err, errGateTokenGate):
		return "", issued, fmt.Errorf("%w: approving_gate_gone", errDeviceRefused)
	case errors.Is(err, errTenantGatePolicy):
		return "", issued, fmt.Errorf("%w: tenant_policy", errDeviceRefused)
	}
	return tok, issued, err
}
