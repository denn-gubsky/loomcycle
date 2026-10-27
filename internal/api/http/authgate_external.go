package http

// authgate_external.go — RFC FA §3.5, the external gate contract.
//
// An external gate is a separate process (a bridge, a website, a broker in
// front of an IdP loomcycle has never heard of). loomcycle never calls it and
// never runs it; it trusts exactly one thing, a JWT signed under the key the
// operator pinned for that gate in config. Every route here is reached WITHOUT
// a loomcycle bearer — the signature is the authentication — so each one:
//
//   - names the gate in its body, and verifies under that gate's key only
//     (iss must be the gate; one gate's key never verifies another's);
//   - checks aud (this deployment), exp (at most five minutes), lc_use (the
//     route's purpose) and a single-use jti in the store, so a replay to any
//     replica is refused (internal/authgate/external);
//   - throttles failures per client source, through the password gate's
//     throttle table, so a stream of junk is slowed before it costs much;
//   - answers every refusal with one opaque error, and audits it.
//
// What an assertion becomes — tenant, subject, provisioning, groups — is the
// shared identity core's (authgate_identity.go), never this file's: an
// external gate cannot assert a tenant outside its identity_map, and an
// untrusted one cannot raise access_mode or touch groups.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/external"
	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Body caps. A sync carries a whole user list; nothing else is more than a
// few kilobytes.
const (
	maxGateBody     = 64 << 10
	maxGateSyncBody = external.MaxCompactBytes + 1<<10
	// gateFailureLockout caps the per-source backoff on external-gate routes.
	gateFailureLockout = 15 * time.Minute
)

// storeReplayCache is the replay cache every replica shares: the store's
// auth_assertion_jtis table.
type storeReplayCache struct{ st store.AuthLifecycleStore }

func (c storeReplayCache) UseOnce(ctx context.Context, gate, jti string, until, now time.Time) (bool, error) {
	return c.st.AuthAssertionUseOnce(ctx, gate, jti, until, now)
}

// externalGate returns the configured external gate of that name.
func (s *Server) externalGate(name string) (config.AuthGate, bool) {
	g, ok := s.configuredGate(name)
	if !ok || g.Kind != config.AuthGateKindExternal || g.External == nil || g.Identity == nil {
		return config.AuthGate{}, false
	}
	return g, true
}

// externalVerifier builds the verifier of one gate: the key pinned in config
// (a public key, or an HMAC secret from the operator's env — never anything a
// tenant can write), this deployment's issuer URL as the audience, and the
// shared replay cache.
func (s *Server) externalVerifier(_ context.Context, g config.AuthGate) (*external.Verifier, error) {
	if g.External.Key.Algorithm() == "" {
		return nil, errors.New("the gate has no key")
	}
	return &external.Verifier{Gate: g.Name, Key: g.External.Key, Audience: external.Issuer(s.cfg().Env.PublicURL),
		Replay: storeReplayCache{s.store}}, nil
}

// assertionOf turns verified claims into the identity core's assertion. The
// external gate's identities are keyed (gate, "", sub): the gate is the only
// issuer it speaks for.
func assertionOf(c external.Claims) authgate.Assertion {
	claims := map[string]any{"sub": c.Subject}
	if c.Email != "" {
		claims["email"], claims["email_verified"] = c.Email, c.EmailVerified
	}
	if c.Tenant != "" {
		claims["tenant"] = c.Tenant
	}
	if c.DisplayName != "" {
		claims["name"] = c.DisplayName
	}
	if c.Groups != nil {
		gs := make([]any, len(c.Groups))
		for i, g := range c.Groups {
			gs[i] = g
		}
		claims["groups"] = gs
	}
	// The gate vouches for who the person is, not for how they proved it: an
	// amr it asserts ("otp", "hwk") is not taken, so a session or token it
	// produced can never pass for a second factor loomcycle did not see.
	return authgate.Assertion{Subject: c.Subject, TenantHint: c.Tenant, AMR: []string{"ext"}, AuthTime: c.AuthTime,
		Email: c.Email, EmailVerified: c.EmailVerified, DisplayName: c.DisplayName, Groups: c.Groups,
		AccessMode: c.AccessMode, Claims: claims, ExpiresAt: c.Expiry}
}

// gateForGroups is the gate as provisioning sees it for these claims: a gate
// that did not mention groups at all syncs none, so a bridge that could not
// look a membership up never strips the groups it gave before. An empty list
// is a statement ("in no group") and syncs.
func gateForGroups(g config.AuthGate, c external.Claims) config.AuthGate {
	if c.Groups != nil || g.Identity == nil || !g.Identity.SyncGroups {
		return g
	}
	m := *g.Identity
	m.SyncGroups = false
	g.Identity = &m
	return g
}

// gateSourceKey is the throttle key of a client source on the external-gate
// routes, MAC'd so the store holds no address.
func (s *Server) gateSourceKey(r *http.Request) string {
	src := throttleSourceKey(s.clientSource(r))
	if h, err := s.hasher(); err == nil {
		return "extsrc:" + h.Hash("external-gate-source\x00"+src)
	}
	return "extsrc:" + src
}

func gateSourceBlockFor(n int) time.Duration {
	return password.Backoff(n, password.SourceFree, gateFailureLockout)
}

// gateRequest is the body of every non-browser external-gate route: the gate
// it comes from, and the signed JWT (assertion for assert, request for revoke
// and sync).
type gateRequest struct {
	Gate      string `json:"gate"`
	Assertion string `json:"assertion,omitempty"`
	Request   string `json:"request,omitempty"`
}

// verifyGateRequest decodes and verifies a non-browser request for use. On
// any refusal it has written the (opaque) answer and returns false.
func (s *Server) verifyGateRequest(w http.ResponseWriter, r *http.Request, use external.Use, limit int64) (config.AuthGate, external.Claims, bool) {
	ctx := r.Context()
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "this server cannot verify gate assertions")
		return config.AuthGate{}, external.Claims{}, false
	}
	if !s.authConfigured(ctx) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server")
		return config.AuthGate{}, external.Claims{}, false
	}
	// A blocked source is still verified: a request the gate's key signed is
	// not junk, and a source shared with a stranger (a NAT, a proxy) must not
	// let the stranger's refusals shut the gate out. Only a refusal from a
	// blocked source answers 429; verifying a JWT is cheap next to what the
	// block guards (the body is capped, the replay cache is reached only by a
	// signed assertion).
	srcKey := s.gateSourceKey(r)
	blocked := s.sourceBlocked(ctx, srcKey)
	refuse := func(gate, reason string) {
		if blocked {
			writeJSONError(w, http.StatusTooManyRequests, "throttled", "too many refused requests; try again later")
			return
		}
		s.recordSourceFailure(ctx, srcKey, gateSourceBlockFor)
		s.recordAuthEvent(audit.Event{Action: "auth_assertion_refused", AuthGate: gate, AuthReason: reason, SourceAddr: s.clientSource(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_assertion", "the assertion was refused")
	}
	var req gateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		refuse("", "malformed body")
		return config.AuthGate{}, external.Claims{}, false
	}
	jwt := req.Assertion
	if use == external.UseRevoke || use == external.UseSync {
		jwt = req.Request
	}
	g, ok := s.externalGate(req.Gate)
	if !ok {
		refuse(req.Gate, "no such external gate")
		return config.AuthGate{}, external.Claims{}, false
	}
	v, err := s.externalVerifier(ctx, g)
	if err != nil {
		slog.Error("auth: external gate key unavailable", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "gate_unavailable", "the gate cannot be verified right now")
		return config.AuthGate{}, external.Claims{}, false
	}
	c, err := v.Verify(ctx, jwt, use)
	switch {
	case errors.Is(err, external.ErrUnavailable):
		slog.Error("auth: assertion replay cache unavailable (refusing)", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "gate_unavailable", "the assertion cannot be checked right now")
		return config.AuthGate{}, external.Claims{}, false
	case err != nil:
		refuse(g.Name, err.Error())
		return config.AuthGate{}, external.Claims{}, false
	}
	return g, c, true
}

// handleAuthRevoke is POST /v1/_auth/revoke {gate, request}: an external gate
// says one of its people is gone (`sub` of a lc_use=revoke JWT). The user is
// removed per the gate's removal policy: disabled at once (sessions end,
// issued tokens retire), unbound and maybe erased later. Only a user linked
// to this gate can be revoked by it; any other sub is answered the same 204,
// so a gate learns nothing about other gates' users.
func (s *Server) handleAuthRevoke(w http.ResponseWriter, r *http.Request) {
	g, c, ok := s.verifyGateRequest(w, r, external.UseRevoke, maxGateBody)
	if !ok {
		return
	}
	row, err := s.store.UserIdentityGet(r.Context(), g.Name, "", c.Subject)
	switch {
	case isNotFound(err):
		w.WriteHeader(http.StatusNoContent)
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "revoke_failed", "the revoke could not be applied; retry")
		return
	}
	if err := s.removeThroughGate(r.Context(), g, row.TenantID, row.Subject, store.RemovalReasonRevoked); err != nil && !isNotFound(err) {
		slog.Error("auth: external revoke failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "revoke_failed", "the revoke could not be applied; retry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeThroughGate starts a removal on the gate's own removal policy.
func (s *Server) removeThroughGate(ctx context.Context, g config.AuthGate, tenant, subject, reason string) error {
	rp := g.External.Removal
	return s.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: rp.UnbindAfter, EraseAfter: rp.EraseAfter},
		tenant, subject, g.Name, reason, "gate:"+g.Name)
}

// syncResult is POST /v1/_auth/sync's answer: counts only, never subjects.
type syncResult struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Refused   int `json:"refused"`
	Removed   int `json:"removed"`
	Unchanged int `json:"unchanged"`
}

// handleAuthSync is POST /v1/_auth/sync {gate, request}: an external gate
// reconciles its users and their groups in bulk (lc_use=sync, users[]).
// Each listed user is provisioned (per the identity map's provision rule) and
// has its gate groups synced, exactly as at a login; a repeat changes
// nothing. With complete: true on a gate whose identity_map says
// authoritative_for_users, every user linked to the gate that the list omits
// is removed per the gate's removal policy. A complete sync of no users is
// refused: it would remove everyone, and a gate meaning that revokes them.
func (s *Server) handleAuthSync(w http.ResponseWriter, r *http.Request) {
	g, c, ok := s.verifyGateRequest(w, r, external.UseSync, maxGateSyncBody)
	if !ok {
		return
	}
	ctx := r.Context()
	if c.Complete && len(c.Users) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty_complete_sync", "a complete sync must list at least one user; revoke users one by one instead")
		return
	}
	var res syncResult
	listed := make([]string, 0, len(c.Users))
	for _, u := range c.Users {
		listed = append(listed, u.Subject)
	}
	// Who a complete sync removes is decided, and held to the gate's ceiling,
	// before anyone is provisioned, so a refused sync changes nothing.
	var remove []lifecycle.Identity
	if c.Complete && g.Identity.AuthoritativeForUsers {
		ids, err := s.store.UserIdentityListGate(ctx, g.Name)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "sync_failed", "the sync stopped part-way; it is safe to repeat")
			return
		}
		linked := make([]lifecycle.Identity, len(ids))
		for i, id := range ids {
			linked[i] = lifecycle.Identity{TenantID: id.TenantID, Subject: id.Subject, ExternalSubject: id.ExternalSubject}
		}
		for _, id := range lifecycle.Missing(linked, listed) {
			if _, err := s.store.AuthRemovalGet(ctx, id.TenantID, id.Subject); err == nil {
				res.Unchanged++
				continue
			}
			remove = append(remove, id)
		}
		if ceiling := g.External.SyncRemovalCeiling(len(ids)); len(remove) > ceiling {
			s.recordAuthEvent(audit.Event{Action: "auth_gate_sync_refused", AuthGate: g.Name, SourceAddr: s.clientSource(r),
				AuthReason: fmt.Sprintf("would remove %d of %d users, over the ceiling of %d", len(remove), len(ids), ceiling)})
			writeJSONError(w, http.StatusConflict, "sync_removal_limit", fmt.Sprintf(
				"this sync would remove %d users, over the gate's ceiling of %d (max_sync_removals); nothing was changed", len(remove), ceiling))
			return
		}
	}
	for _, u := range c.Users {
		uc := external.Claims{Gate: c.Gate, Subject: u.Subject, Email: u.Email, EmailVerified: u.EmailVerified,
			DisplayName: u.DisplayName, Groups: u.Groups, Tenant: u.Tenant, AccessMode: u.AccessMode,
			AuthTime: c.IssuedAt, Expiry: c.Expiry}
		out, err := s.provisionOrRefuse(ctx, gateForGroups(g, uc), assertionOf(uc), s.clientSource(r))
		switch {
		case err == nil && out.Created:
			res.Created++
		case err == nil:
			res.Updated++
		case errors.Is(err, errIdentityRefused), errors.Is(err, errUserDisabled):
			res.Refused++
		default:
			slog.Error("auth: sync provisioning failed", "gate", g.Name, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "sync_failed", "the sync stopped part-way; it is safe to repeat")
			return
		}
	}
	for _, id := range remove {
		if err := s.removeThroughGate(ctx, g, id.TenantID, id.Subject, store.RemovalReasonSync); err != nil {
			if isNotFound(err) {
				continue
			}
			slog.Error("auth: sync removal failed", "gate", g.Name, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "sync_failed", "the sync stopped part-way; it is safe to repeat")
			return
		}
		res.Removed++
	}
	s.recordAuthEvent(audit.Event{Action: "auth_gate_synced", AuthGate: g.Name, SourceAddr: s.clientSource(r),
		AuthReason: fmt.Sprintf("created %d, updated %d, refused %d, removed %d", res.Created, res.Updated, res.Refused, res.Removed)})
	writeJSON(w, http.StatusOK, res)
}

// handleAuthComplete is POST /v1/_auth/complete/{gate}: the password gate's
// form post, or a same-site external gate posting its assertion as a form
// field. Each kind has its own checks.
func (s *Server) handleAuthComplete(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.externalGate(r.PathValue("gate")); ok {
		if !s.authConfigured(r.Context()) {
			writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
			return
		}
		s.completeExternalLogin(w, r, g)
		return
	}
	s.handlePasswordLogin(w, r)
}

// defaultGateMaxActive is how many issued tokens of one user a gate keeps
// alive when its issued_token.max_active is unset: two, so a restarted bridge
// does not cut off a stream its predecessor is still draining.
const defaultGateMaxActive = 2

// assertResponse is POST /v1/_auth/assert's answer. The token is shown once.
type assertResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Subject   string    `json:"subject"`
	Tenant    string    `json:"tenant"`
}

// handleAuthAssert is POST /v1/_auth/assert {gate, assertion}: a non-browser
// client of an external gate (a bridge, per platform user) presents the
// gate's assertion and receives an issued token for the person (RFC FA §7):
// short-lived, the user's own member reach, held to the gate's issued_token
// ceiling and to anything narrower the assertion's `token` claim asks for.
// The user is provisioned per the identity map first, exactly as at a login;
// a pending user gets no token.
func (s *Server) handleAuthAssert(w http.ResponseWriter, r *http.Request) {
	g, c, ok := s.verifyGateRequest(w, r, external.UseAssert, maxGateBody)
	if !ok {
		return
	}
	ctx := r.Context()
	out, err := s.provisionOrRefuse(ctx, gateForGroups(g, c), assertionOf(c), s.clientSource(r))
	switch {
	case errors.Is(err, errIdentityRefused), errors.Is(err, errUserDisabled):
		writeJSONError(w, http.StatusUnauthorized, "invalid_assertion", "the assertion was refused")
		return
	case err != nil:
		slog.Error("auth: provisioning an asserted identity failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "assert_failed", "the assertion could not be applied; retry")
		return
	case out.Pending:
		writeJSONError(w, http.StatusForbidden, "awaiting_approval", "the user awaits an operator's approval")
		return
	}
	restrictions, ttl, err := gateTokenRequest(g.External, c.Token)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_token_request", err.Error())
		return
	}
	tok, row, err := s.issueGateToken(ctx, out.TenantID, out.Subject, g.Name, ttl, restrictions)
	if err != nil {
		if errors.Is(err, errGateTokenUser) {
			writeJSONError(w, http.StatusUnauthorized, "invalid_assertion", "the assertion was refused")
			return
		}
		slog.Error("auth: issuing a gate token failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "assert_failed", "the assertion could not be applied; retry")
		return
	}
	keep := g.External.MaxActive
	if keep <= 0 {
		keep = defaultGateMaxActive
	}
	if err := s.retireGateTokensBeyond(ctx, out.TenantID, out.Subject, g.Name, keep); err != nil {
		slog.Warn("auth: retiring a user's older gate tokens failed", "gate", g.Name, "err", err)
	}
	writeJSON(w, http.StatusOK, assertResponse{Token: tok, ExpiresAt: row.ExpiresAt.UTC(), Subject: out.Subject, Tenant: out.TenantID})
}

// gateTokenRequest is the issued token's restrictions and lifetime: the
// gate's ceiling, narrowed by what the assertion asks for. Asking for an
// agent outside the ceiling is refused, not silently dropped.
func gateTokenRequest(e *config.AuthExternalGate, req *external.TokenRequest) (auth.Restrictions, time.Duration, error) {
	ttl := e.TokenTTL
	if req == nil {
		return e.TokenRestrictions, ttl, nil
	}
	var want auth.Restrictions
	if req.Agents != nil {
		want.Agents = append([]string(nil), req.Agents...)
	}
	if req.Overrides != nil {
		want.Overrides = &auth.OverridePolicy{Allow: append([]string(nil), req.Overrides...)}
	}
	if req.RatePerMin > 0 {
		want.Rate = &auth.RateLimit{PerMinute: req.RatePerMin, Burst: req.RateBurst}
	}
	if err := want.Validate(req.Agents != nil); err != nil {
		return auth.Restrictions{}, 0, err
	}
	out, err := want.Within(e.TokenRestrictions)
	if err != nil {
		return auth.Restrictions{}, 0, err
	}
	if req.TTLSeconds > 0 {
		if d := time.Duration(req.TTLSeconds) * time.Second; d < ttl {
			ttl = d
		}
	}
	return out, ttl, nil
}

// retireGateTokensBeyond keeps the newest keep live tokens this gate issued to
// the user and retires the rest, so a gate that asks often (a restarted
// bridge) cannot pile up live bearers. Tokens from other gates, and the
// user's hand-minted tokens, are left alone.
func (s *Server) retireGateTokensBeyond(ctx context.Context, tenant, subject, gate string, keep int) error {
	rows, err := s.store.OperatorTokenDefListBySubject(ctx, tenant, subject)
	if err != nil {
		return err
	}
	var live []store.OperatorTokenDefRow
	for _, row := range rows {
		if row.IssuedByGate == gate && isDelegatedUserToken(row, tenant, subject) && tokenActive(row) {
			live = append(live, row)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
			return live[i].CreatedAt.After(live[j].CreatedAt)
		}
		return live[i].DefID > live[j].DefID
	})
	now := time.Now().UTC()
	retired := false
	for i := keep; i < len(live); i++ {
		if err := s.store.OperatorTokenDefSetRetiredAt(ctx, live[i].DefID, now); err != nil {
			return err
		}
		retired = true
		s.recordTokenAudit(ctx, "retire", live[i])
	}
	if retired {
		s.invalidateTokenCache(ctx)
	}
	return nil
}

// externalRedirect is an external gate as a redirect gate (the login start
// that authgate_oidc.go serves for every redirect gate): the login state and
// the browser binding are the shared machinery's; this adds where the browser
// goes. The return is completeExternalLogin's, not the provider callback's.
type externalRedirect struct {
	g        config.AuthGate
	audience string
}

func (e externalRedirect) Kind() string { return config.AuthGateKindExternal }

func (e externalRedirect) Begin(context.Context, http.ResponseWriter, *http.Request, authgate.LoginState) error {
	return authgate.ErrNotInteractive
}

func (e externalRedirect) Complete(context.Context, *http.Request, authgate.LoginState) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNotInteractive
}

func (e externalRedirect) Verify(context.Context, *http.Request) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNoCredential
}

func (e externalRedirect) EndSessionURL(context.Context, string) string { return "" }

// AuthURL is the gate's callback_url with the one-time login_state, the
// audience the assertion must name, and the URL to send the browser back to
// (which carries the state again, so the shared callback half finds the
// login's row before anything else is read).
func (e externalRedirect) AuthURL(_ context.Context, ls authgate.LoginState) (string, error) {
	target, err := url.Parse(e.g.External.CallbackURL)
	if err != nil {
		return "", err
	}
	complete := e.audience + "/v1/_auth/complete/" + url.PathEscape(e.g.Name) + "?state=" + url.QueryEscape(ls.State)
	q := target.Query()
	q.Set("login_state", ls.State)
	q.Set("aud", e.audience)
	q.Set("complete", complete)
	target.RawQuery = q.Encode()
	return target.String(), nil
}

// externalRedirectGate is the redirect gate of an external gate a browser can
// sign in through (it has a callback_url).
func (s *Server) externalRedirectGate(name string) (redirectGate, config.AuthGate, bool) {
	g, ok := s.externalGate(name)
	if !ok || !g.External.Interactive() {
		return nil, config.AuthGate{}, false
	}
	return externalRedirect{g: g, audience: external.Issuer(s.cfg().Env.PublicURL)}, g, true
}

// completeExternalLogin is GET/POST /v1/_auth/complete/{gate}?state=… for an
// external gate: the browser returns from the gate with an assertion
// (lc_use=login) carrying the same login state. The state must be live, made
// for this gate, and bound to THIS browser's login cookie (takeLoginState) —
// an assertion carried to another browser signs nobody in — and the signed
// login_state must be that state. Then the identity core provisions, the
// session is established, and the browser goes to the return_to the login
// began with (a pending user to /ui/pending).
func (s *Server) completeExternalLogin(w http.ResponseWriter, r *http.Request, g config.AuthGate) {
	ctx := r.Context()
	name := g.Name
	w.Header().Set("Cache-Control", "no-store")
	// The assertion rides the URL; the next page must not send it on.
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.requireStore(w) {
		return
	}
	srcKey := s.gateSourceKey(r)
	if s.sourceBlocked(ctx, srcKey) {
		loginRedirect(w, r, name, "too_many_attempts")
		return
	}
	source := s.clientSource(r)
	refuse := func(reason string) {
		s.recordSourceFailure(ctx, srcKey, gateSourceBlockFor)
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: name, AuthReason: reason, SourceAddr: source})
		loginRedirect(w, r, name, "login_failed")
	}
	row, ls, code := s.takeLoginState(w, r, g)
	if code != "" {
		s.recordSourceFailure(ctx, srcKey, gateSourceBlockFor)
		loginRedirect(w, r, name, code)
		return
	}
	if row.Purpose != store.LoginPurposeLogin {
		refuse("not a sign-in state")
		return
	}
	assertion := r.URL.Query().Get("assertion")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxGateBody)
		if err := r.ParseForm(); err == nil && r.PostForm.Get("assertion") != "" {
			assertion = r.PostForm.Get("assertion")
		}
	}
	v, err := s.externalVerifier(ctx, g)
	if err != nil {
		slog.Error("auth: external gate key unavailable", "gate", name, "err", err)
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	// The login state is compared inside the verification, before the jti is
	// recorded, so an assertion carried to another browser's login is not
	// used up by the attempt.
	c, err := v.VerifyFor(ctx, assertion, external.UseLogin, func(c external.Claims) error {
		if subtle.ConstantTimeCompare([]byte(c.LoginState), []byte(ls.State)) != 1 {
			return errors.New("the assertion was made for another login")
		}
		return nil
	})
	if errors.Is(err, external.ErrUnavailable) {
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	if s.sessionRefusal(r) != "" {
		loginRedirect(w, r, name, "insecure_transport")
		return
	}
	a := assertionOf(c)
	out, err := s.provisionOrRefuse(ctx, gateForGroups(g, c), a, source)
	if err != nil {
		if errors.Is(err, errIdentityRefused) || errors.Is(err, errUserDisabled) {
			loginRedirect(w, r, name, "login_failed")
			return
		}
		slog.Error("auth: provisioning a gate login failed", "gate", name, "err", err)
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	if err := s.establishSession(w, r, sessionIdentity{TenantID: out.TenantID, Subject: out.Subject, Gate: name,
		AMR: a.AMR, AuthTime: a.AuthTime}); err != nil {
		slog.Error("auth: establishing a gate session failed", "gate", name, "err", err)
		loginRedirect(w, r, name, "gate_unavailable")
		return
	}
	dest := row.ReturnTo
	if dest == "" {
		dest = "/ui/"
	}
	if out.Pending {
		dest = "/ui/pending"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// gateProbeTimeout bounds one discovery probe; gateProbeMax its body.
const (
	gateProbeTimeout = 5 * time.Second
	gateProbeMax     = 64 << 10
)

// authgateEntry is one gate of GET /v1/_authgate.
type authgateEntry struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Label       string          `json:"label"`
	Interactive bool            `json:"interactive"`
	Trusted     bool            `json:"trusted,omitempty"`
	Tenant      string          `json:"tenant,omitempty"`
	TenantClaim string          `json:"tenant_claim,omitempty"`
	Provision   string          `json:"provision,omitempty"`
	Key         *authgateKey    `json:"key,omitempty"`
	CallbackURL string          `json:"callback_url,omitempty"`
	Discovery   string          `json:"discovery_url,omitempty"`
	Probe       *authgateProbe  `json:"probe,omitempty"`
	Removal     *authgateRemove `json:"on_removed,omitempty"`
}

type authgateKey struct {
	Algorithm   string `json:"alg"`
	KeyID       string `json:"kid,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	SecretEnv   string `json:"secret_env,omitempty"`
}

type authgateRemove struct {
	UnbindAfter string `json:"unbind_after"`
	EraseAfter  string `json:"erase_after"`
}

// authgateProbe is what the gate's /.well-known/loomcycle-authgate said.
type authgateProbe struct {
	Reachable  bool     `json:"reachable"`
	Status     int      `json:"status,omitempty"`
	Error      string   `json:"error,omitempty"`
	NameOK     bool     `json:"name_matches"`
	KeyOK      bool     `json:"key_matches"`
	Capability []string `json:"capabilities,omitempty"`
}

// handleAuthgateList is GET /v1/_authgate (admin): every configured gate —
// kind, label, whether a browser can sign in through it, its identity map's
// tenant and provisioning, an external gate's key (algorithm, kid,
// fingerprint; never key material) and removal timing. With ?probe=1 each
// external gate's discovery document is fetched (bounded, no redirects) and
// compared with the registration.
func (s *Server) handleAuthgateList(w http.ResponseWriter, r *http.Request) {
	probe := r.URL.Query().Get("probe") == "1"
	out := []authgateEntry{}
	for _, g := range s.cfg().ResolvedAuth.Gates {
		e := authgateEntry{Name: g.Name, Kind: g.Kind, Label: g.Label, Interactive: true}
		if m := g.Identity; m != nil {
			e.Trusted, e.Tenant, e.TenantClaim, e.Provision = m.Trusted, m.Tenant, m.TenantClaim, m.Provision
		}
		if x := g.External; x != nil {
			e.Interactive = x.Interactive()
			e.CallbackURL, e.Discovery = x.CallbackURL, x.DiscoveryURL
			if x.HMACSecretEnv != "" {
				e.Key = &authgateKey{Algorithm: "HS256", SecretEnv: x.HMACSecretEnv}
			} else {
				e.Key = &authgateKey{Algorithm: x.Key.Algorithm(), KeyID: x.Key.KeyID(), Fingerprint: x.Key.Fingerprint()}
			}
			erase := "never"
			if x.Removal.EraseAfter > 0 {
				erase = x.Removal.EraseAfter.String()
			}
			e.Removal = &authgateRemove{UnbindAfter: x.Removal.UnbindAfter.String(), EraseAfter: erase}
			if probe && x.DiscoveryURL != "" {
				p := s.probeGate(r.Context(), g)
				e.Probe = &p
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"gates": out})
}

// gateDiscovery is the gate's /.well-known/loomcycle-authgate document.
type gateDiscovery struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	KeyID        string   `json:"key_id"`
	Fingerprint  string   `json:"fingerprint"`
	Capabilities []string `json:"capabilities"`
}

// probeGate fetches an external gate's discovery document. The URL is the
// operator's own config, read by an admin; the request still follows no
// redirect and reads a bounded body, and its error is a class, not the
// response.
func (s *Server) probeGate(ctx context.Context, g config.AuthGate) authgateProbe {
	ctx, cancel := context.WithTimeout(ctx, gateProbeTimeout)
	defer cancel()
	u := strings.TrimRight(g.External.DiscoveryURL, "/") + "/.well-known/loomcycle-authgate"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return authgateProbe{Error: "bad_url"}
	}
	hc := &http.Client{Timeout: gateProbeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return authgateProbe{Error: "unreachable"}
	}
	defer resp.Body.Close()
	p := authgateProbe{Reachable: true, Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		p.Error = "status"
		return p
	}
	var d gateDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, gateProbeMax)).Decode(&d); err != nil {
		p.Error = "malformed"
		return p
	}
	p.NameOK = d.Name == g.Name
	k := g.External.Key
	switch {
	case g.External.HMACSecretEnv != "":
		p.KeyOK = d.Fingerprint == "" && d.KeyID == ""
	case d.Fingerprint != "":
		p.KeyOK = d.Fingerprint == k.Fingerprint()
	case d.KeyID != "":
		p.KeyOK = k.KeyID() != "" && d.KeyID == k.KeyID()
	}
	p.Capability = d.Capabilities
	return p
}
