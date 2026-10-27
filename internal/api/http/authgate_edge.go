package http

// authgate_edge.go — RFC FA P3: the edge gates. kind: header trusts a signed
// assertion a proxy in front of the server adds to every request (Cloudflare
// Access, an AWS ALB, a forward-auth proxy with a shared HMAC secret);
// kind: awsiam turns an AWS workload's signed sts:GetCallerIdentity into an
// issued token. Both verify in internal/authgate/{headergate,awsiam}; this
// file builds them from config and joins them to the identity core
// (authgate_identity.go), the session substrate (ui_session.go) and the
// issued-token path.
//
// How a header gate signs a browser in. The proxy already authenticated the
// person, so there is no password box and no redirect: the login page offers
// "Continue as signed in by <label>", which POSTs (same-origin) to
// /v1/_auth/complete/{gate}; that request carries the proxy's assertion, and
// a verified one becomes a session through provisionOrRefuse and
// establishSession like any other gate's. The session stays bound to the
// proxy: every later request it authenticates must carry a valid assertion
// that maps to the same tenant and subject, or the session ends. So signing
// out of Cloudflare Access (or the proxy dropping the person) ends the
// loomcycle session on the next request, and a stolen cookie is useless
// without the person's edge session too.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/awsiam"
	"github.com/denn-gubsky/loomcycle/internal/authgate/headergate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// EdgeGates are the built kind: header and kind: awsiam gates.
type EdgeGates struct {
	header map[string]*headergate.Gate
	awsiam map[string]*awsiam.Gate
	// headerNames is header's keys, sorted, so the filter runs in one order.
	headerNames []string
}

// edgeGatesHolder swaps the built gates atomically (set at boot).
type edgeGatesHolder struct{ p atomic.Pointer[EdgeGates] }

// EdgeGateDeps are what building the edge gates needs from the boot.
type EdgeGateDeps struct {
	// Replay records accepted one-time values (STS signatures, forward-auth
	// nonces) for every replica, in the external gates' assertion record.
	// Nil keeps replay protection to this replica.
	Replay store.AuthLifecycleStore
}

// Names lists the gates, sorted.
func (e *EdgeGates) Names() []string {
	var out []string
	for n := range e.header {
		out = append(out, n)
	}
	for n := range e.awsiam {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// EdgeGatesFromConfig builds every kind: header and kind: awsiam gate. nil
// when there are none. A gate that cannot run (a forward-auth secret_env
// that is empty) is an error: a gate that silently accepts nobody — or,
// worse, verifies against an empty key — must fail the boot instead.
func EdgeGatesFromConfig(cfg *config.Config, deps EdgeGateDeps) (*EdgeGates, error) {
	e := &EdgeGates{header: map[string]*headergate.Gate{}, awsiam: map[string]*awsiam.Gate{}}
	for _, g := range cfg.ResolvedAuth.Gates {
		switch {
		case g.Header != nil:
			hg, err := buildHeaderGate(g, deps)
			if err != nil {
				return nil, fmt.Errorf("auth.gates.%s: %w", g.Name, err)
			}
			e.header[g.Name] = hg
			e.headerNames = append(e.headerNames, g.Name)
		case g.AWSIAM != nil:
			ag := &awsiam.Gate{
				ServerID: g.AWSIAM.ServerID, AllowedARNs: g.AWSIAM.AllowedARNs, Regions: g.AWSIAM.STSRegions,
				Replay: replay.New(0),
			}
			if deps.Replay != nil {
				ag.Shared, ag.SharedScope = storeReplayCache{deps.Replay}, "awsiam:"+g.Name
			}
			e.awsiam[g.Name] = ag
		}
	}
	if len(e.header) == 0 && len(e.awsiam) == 0 {
		return nil, nil
	}
	sort.Strings(e.headerNames)
	return e, nil
}

func buildHeaderGate(g config.AuthGate, deps EdgeGateDeps) (*headergate.Gate, error) {
	h := g.Header
	switch h.Preset {
	case config.HeaderPresetCloudflareAccess:
		cf, err := headergate.NewCloudflareAccess(h.TeamDomain, h.Audience, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		return headergate.NewCloudflareGate(cf, h.TrustedProxyCIDRs), nil
	case config.HeaderPresetAWSALB:
		alb := &headergate.ALB{
			SignerARN: h.ALBARN, Issuer: h.Issuer, ClientID: h.ClientID,
			Keys: &headergate.ALBKeys{Region: h.ALBRegion},
		}
		if headergate.ALBRegionFromSigner(h.ALBARN) != h.ALBRegion {
			return nil, errors.New("alb_arn is not an application load balancer ARN")
		}
		return headergate.NewALBGate(alb, h.TrustedProxyCIDRs), nil
	case config.HeaderPresetForwardAuthHMAC:
		secret, err := forwardAuthSecret(g)
		if err != nil {
			return nil, err
		}
		f := &headergate.ForwardAuthHMAC{Secret: secret, Replay: replay.New(0)}
		if deps.Replay != nil {
			f.Shared, f.SharedScope = storeReplayCache{deps.Replay}, "fwdhmac:"+g.Name
		}
		return headergate.NewForwardAuthGate(f, h.TrustedProxyCIDRs), nil
	}
	return nil, fmt.Errorf("preset %q has no implementation", h.Preset)
}

// forwardAuthSecret is the gate's secret, from the operator's environment
// only (read once at boot: the environment does not change under a running
// process). Never from a tenant's credential store: whoever can set a
// verification secret can forge every identity it verifies.
func forwardAuthSecret(g config.AuthGate) (func(context.Context) ([]byte, error), error) {
	h := g.Header
	v := os.Getenv(h.SecretEnv)
	if h.SecretEnv == "" || len(v) < 32 {
		return nil, fmt.Errorf("secret_env %s is unset or shorter than 32 bytes", h.SecretEnv)
	}
	b := []byte(v)
	return func(context.Context) ([]byte, error) { return b, nil }, nil
}

// SetEdgeGates installs the built edge gates (boot).
func (s *Server) SetEdgeGates(e *EdgeGates) { s.edgeGates.p.Store(e) }

func (s *Server) headerGateNamed(name string) (*headergate.Gate, bool) {
	e := s.edgeGates.p.Load()
	if e == nil {
		return nil, false
	}
	g, ok := e.header[name]
	return g, ok
}

func (s *Server) awsiamGateNamed(name string) (*awsiam.Gate, bool) {
	e := s.edgeGates.p.Load()
	if e == nil {
		return nil, false
	}
	g, ok := e.awsiam[name]
	return g, ok
}

// ---- the request filter ---------------------------------------------------

type ctxKeyEdgeRequest struct{}

// edgeRequest is one request's header-gate state: what the filter took for
// each gate, and each gate's verification, done at most once.
type edgeRequest struct {
	mu       sync.Mutex
	taken    map[string]edgeTaken
	verified map[string]edgeVerified
}

type edgeTaken struct {
	vals map[string]string
	err  error
}

type edgeVerified struct {
	a   authgate.Assertion
	err error
}

// edgeHeaderFilter runs every header gate's filter on every request, before
// any handler: each alias of a gate's header is stripped, and a gate's header
// from a peer outside its trusted_proxy_cidrs is removed (and remembered as
// refused, not absent). Verification is lazy (edgeAssertion), so a request
// nobody asks about costs nothing, and once per request, so a single-use
// forward-auth nonce is not spent twice by two readers of one request.
func (s *Server) edgeHeaderFilter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := s.edgeGates.p.Load()
		if e == nil || len(e.headerNames) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		er := &edgeRequest{taken: map[string]edgeTaken{}, verified: map[string]edgeVerified{}}
		for _, name := range e.headerNames {
			vals, err := e.header[name].Take(r)
			er.taken[name] = edgeTaken{vals: vals, err: err}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyEdgeRequest{}, er)))
	})
}

// edgeAssertion is gate name's verified assertion for this request:
// authgate.ErrNoCredential when the request carries none.
func (s *Server) edgeAssertion(r *http.Request, name string) (authgate.Assertion, error) {
	g, ok := s.headerGateNamed(name)
	if !ok {
		return authgate.Assertion{}, fmt.Errorf("auth gate %q is not a running header gate", name)
	}
	er, _ := r.Context().Value(ctxKeyEdgeRequest{}).(*edgeRequest)
	if er == nil {
		// Only reachable through a handler mounted outside Mux(); refuse
		// rather than read headers the filter never cleaned.
		return authgate.Assertion{}, errors.New("auth gate: request did not pass the header filter")
	}
	er.mu.Lock()
	defer er.mu.Unlock()
	if v, done := er.verified[name]; done {
		return v.a, v.err
	}
	t := er.taken[name]
	var v edgeVerified
	if t.err != nil {
		v.err = t.err
	} else {
		v.a, v.err = g.VerifyTaken(r.Context(), t.vals)
	}
	er.verified[name] = v
	return v.a, v.err
}

// ---- browser sign-in ------------------------------------------------------

// handleHeaderLogin is POST /v1/_auth/complete/{gate} for a header gate.
func (s *Server) handleHeaderLogin(w http.ResponseWriter, r *http.Request, cfgGate config.AuthGate) {
	// Login CSRF: only this server's own page may sign a browser in.
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_login_refused", "session login must be same-origin")
		return
	}
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	a, err := s.edgeAssertion(r, cfgGate.Name)
	switch {
	case errors.Is(err, authgate.ErrNoCredential):
		writeJSONError(w, http.StatusUnauthorized, "no_assertion", "this request did not come through "+cfgGate.Label)
		return
	case err != nil:
		if !errors.Is(err, authgate.ErrInvalidCredential) {
			slog.Error("auth: header gate unavailable", "gate", cfgGate.Name, "err", err)
		}
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: cfgGate.Name, SourceAddr: s.clientSource(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	}
	s.completeIdentityLogin(w, r, cfgGate, a)
}

// completeIdentityLogin turns a verified identity-gate assertion into a
// session: provision (or refuse), then establishSession. Refusals answer one
// opaque 401; provisionOrRefuse has audited the reason.
func (s *Server) completeIdentityLogin(w http.ResponseWriter, r *http.Request, cfgGate config.AuthGate, a authgate.Assertion) {
	ctx := r.Context()
	login, err := s.provisionOrRefuse(ctx, cfgGate, a, s.clientSource(r))
	switch {
	case errors.Is(err, errIdentityRefused), errors.Is(err, errUserDisabled):
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	case errors.Is(err, errSessionsUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "sessions_unavailable", "this server cannot keep Web UI sessions")
		return
	case err != nil:
		slog.Error("auth: provisioning a gate login failed", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	ident := sessionIdentity{TenantID: login.TenantID, Subject: login.Subject, Gate: cfgGate.Name, AMR: a.AMR, AuthTime: a.AuthTime}
	if err := s.establishSession(w, r, ident); err != nil {
		if errors.Is(err, errUserDisabled) {
			writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
			return
		}
		slog.Error("auth: creating a Web UI session failed", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// edgeSessionHolds re-checks a header-gate session against this request: it
// must carry a valid assertion of the same gate that maps to the session's
// own tenant and subject. holds=false with definitive=true ends the session
// (the proxy no longer vouches, or vouches for someone else); definitive=false
// refuses this request only (the store, or the verifier, could not say). A
// session from any other gate holds.
func (s *Server) edgeSessionHolds(r *http.Request, row store.UISessionRow) (holds, definitive bool) {
	cfgGate, configured := s.configuredGate(row.Gate)
	if !configured || cfgGate.Header == nil {
		return true, true
	}
	if _, ok := s.headerGateNamed(row.Gate); !ok {
		// A header gate in config whose verifier is not running: no request
		// can be checked, so none is let through.
		return false, false
	}
	a, err := s.edgeAssertion(r, row.Gate)
	if err != nil {
		// No assertion, or a refused one, ends the session. A verifier
		// fault (an unreadable stored secret) keeps it for when it recovers.
		return false, errors.Is(err, authgate.ErrNoCredential) || errors.Is(err, authgate.ErrInvalidCredential)
	}
	tgt, _, err := s.resolveAssertion(r.Context(), cfgGate, a)
	if err != nil {
		return false, errors.Is(err, errIdentityRefused)
	}
	return tgt.TenantID == row.TenantID && tgt.Subject == row.Subject, true
}

// ---- machine login: POST /v1/_auth/assert ---------------------------------

// maxAssertBody bounds POST /v1/_auth/assert before it is parsed.
const maxAssertBody = 64 << 10

// awsiamTokenTTL is the issued token's lifetime for an AWS IAM login, before
// the gate's and the deployment's caps. GetCallerIdentity does not say when
// the caller's credentials expire, so the token takes the default life of an
// assumed-role session; a workload logs in again with fresh credentials.
const awsiamTokenTTL = time.Hour

// handleAuthAssert is POST /v1/_auth/assert: a non-browser client presents a
// gate's credential and receives an issued token. The body names the gate:
// a kind: awsiam gate is answered here, every other name goes to the
// external gates' path (handleExternalAssert), which also refuses a name no
// gate has, so an unknown gate is refused the same way whatever its kind.
func (s *Server) handleAuthAssert(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAssertBody+1))
	if err != nil || len(raw) > maxAssertBody {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "body_too_large", "assertion body too large")
		return
	}
	var head struct {
		Gate string `json:"gate"`
	}
	// A body that does not parse names no awsiam gate; the external path
	// refuses it as malformed and counts the refusal against the source.
	_ = json.Unmarshal(raw, &head)
	cfgGate, cok := s.identityGate(head.Gate)
	g, gok := s.awsiamGateNamed(head.Gate)
	if !cok || !gok {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		s.handleExternalAssert(w, r)
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "this server cannot verify gate assertions")
		return
	}
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server")
		return
	}
	s.handleAWSIAMAssert(w, r, cfgGate, g, raw)
}

func (s *Server) handleAWSIAMAssert(w http.ResponseWriter, r *http.Request, cfgGate config.AuthGate, g *awsiam.Gate, raw []byte) {
	ctx := r.Context()
	ip := s.clientSource(r)
	key, err := s.assertSourceKey(cfgGate.Name, ip)
	if err != nil {
		slog.Error("auth: assert throttle key", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "assert_failed", "the assertion could not be applied; retry")
		return
	}
	// Every refused attempt counts against the source, so a stream of junk —
	// each of which can cost this server an STS call — slows itself down.
	if s.sourceBlocked(ctx, key) {
		writeJSONError(w, http.StatusTooManyRequests, "throttled", "too many refused requests; try again later")
		return
	}
	// The answers are the external gates' (handleExternalAssert), word for
	// word: the route is shared, and a different refusal here would tell an
	// unauthenticated caller which names are AWS IAM gates.
	refuse := func(reason string) {
		s.recordSourceFailure(ctx, key, assertSourceBlockFor)
		if reason != "" {
			s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: cfgGate.Name, AuthReason: reason, SourceAddr: ip})
		}
		writeJSONError(w, http.StatusUnauthorized, "invalid_assertion", "the assertion was refused")
	}
	d, err := awsiam.ParseLoginData(raw)
	if err != nil {
		refuse("malformed login")
		return
	}
	id, err := g.Verify(ctx, d)
	switch {
	case awsiam.IsUnavailable(err):
		slog.Warn("auth: STS unavailable for an AWS IAM login", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "sts_unavailable", "AWS STS could not be reached; try again")
		return
	case err != nil:
		refuse(err.Error())
		return
	}
	a := id.Assertion()
	login, err := s.provisionOrRefuse(ctx, cfgGate, a, ip)
	switch {
	case errors.Is(err, errIdentityRefused), errors.Is(err, errUserDisabled):
		refuse("") // provisionOrRefuse audited the reason
		return
	case err != nil:
		slog.Error("auth: provisioning an AWS IAM login failed", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "assert_failed", "the assertion could not be applied; retry")
		return
	}
	if login.Pending {
		writeJSONError(w, http.StatusForbidden, "awaiting_approval", "the user awaits an operator's approval")
		return
	}
	tok, row, err := s.issueGateToken(ctx, login.TenantID, login.Subject, cfgGate.Name, awsiamTokenTTL, auth.Restrictions{})
	switch {
	case errors.Is(err, errGateTokenUser):
		writeJSONError(w, http.StatusUnauthorized, "invalid_assertion", "the assertion was refused")
		return
	case err != nil:
		slog.Error("auth: issuing an AWS IAM token failed", "gate", cfgGate.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "assert_failed", "the assertion could not be applied; retry")
		return
	}
	s.recordAuthEvent(audit.Event{Action: "auth_login", ActorTenant: login.TenantID, ActorSubject: login.Subject,
		AuthGate: cfgGate.Name, AuthAMR: a.AMR, SourceAddr: ip})
	// One endpoint, one answer shape: the external gates' assertResponse.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, assertResponse{Token: tok, ExpiresAt: row.ExpiresAt.UTC(), Subject: login.Subject, Tenant: login.TenantID})
}

// assertSourceKey is the throttle key for a client address at an assertion
// gate, hashed under the token pepper so the store holds no address.
func (s *Server) assertSourceKey(gate, ip string) (string, error) {
	h, err := s.hasher()
	if err != nil {
		return "", err
	}
	return "src:assert:" + gate + ":" + h.Hash("assert-source\x00"+throttleSourceKey(ip)), nil
}

// assertSourceFree: machines share addresses less than people behind an
// office NAT do, so fewer free failures than the login form allows.
const assertSourceFree = 5

// assertSourceBlockFor is the per-source backoff for assertion failures: a
// few free (a workload retrying with stale credentials), then exponential up
// to the password gate's default lockout.
func assertSourceBlockFor(n int) time.Duration {
	return password.Backoff(n, assertSourceFree, password.DefaultLockout)
}
