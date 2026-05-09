package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// authedTestServer constructs a minimal Server with an auth token
// set. Wire surface is the authMiddleware-wrapped handler under
// test; the server has no providers / store / agents because the
// middleware doesn't need them.
func authedTestServer(t *testing.T, token string) *Server {
	t.Helper()
	hookReg := hooks.NewRegistry()
	cfg := &config.Config{}
	cfg.Env.AuthToken = token
	return &Server{
		cfg:            cfg,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
}

// passthroughHandler is a dummy that the auth middleware wraps for
// the test — confirms whether the request made it past the auth
// gate.
func passthroughHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestAuthMiddleware_BearerHeaderAccepted is the original v0.4
// contract — every existing adapter / SDK keeps working.
func TestAuthMiddleware_BearerHeaderAccepted(t *testing.T) {
	s := authedTestServer(t, "secret")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d, reached = %v; want 200 + reached", rec.Code, reached)
	}
}

// TestAuthMiddleware_CookieAcceptedWhenNoBearer pins the v0.7.3
// addition: when the bearer header is absent, the middleware checks
// the loomcycle_session cookie and authenticates against the same
// stored token. This is the path the SPA uses after /ui?token=...
// has set the cookie.
func TestAuthMiddleware_CookieAcceptedWhenNoBearer(t *testing.T) {
	s := authedTestServer(t, "secret")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	req.AddCookie(&http.Cookie{Name: webui.SessionCookie, Value: "secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d, reached = %v; want 200 + reached (cookie auth path)", rec.Code, reached)
	}
}

// TestAuthMiddleware_BearerWinsOverCookie pins precedence: an
// explicit bearer header is checked first. A wrong bearer must
// fail even if the cookie is correct — defends against a stale
// cookie masking a deliberate bearer-with-bad-token attempt
// (e.g. a misconfigured CI run).
func TestAuthMiddleware_BearerWinsOverCookie(t *testing.T) {
	s := authedTestServer(t, "secret")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.AddCookie(&http.Cookie{Name: webui.SessionCookie, Value: "secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (bearer should win + fail)", rec.Code)
	}
	if reached {
		t.Error("handler reached despite invalid bearer; cookie must not silently rescue")
	}
}

func TestAuthMiddleware_BadCookieRejected(t *testing.T) {
	s := authedTestServer(t, "secret")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	req.AddCookie(&http.Cookie{Name: webui.SessionCookie, Value: "wrong"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if reached {
		t.Error("handler reached despite invalid cookie")
	}
}

func TestAuthMiddleware_NoCredentialsRejected(t *testing.T) {
	s := authedTestServer(t, "secret")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no creds at all)", rec.Code)
	}
	if reached {
		t.Error("handler reached without any creds")
	}
}

func TestAuthMiddleware_OpenModeWhenTokenEmpty(t *testing.T) {
	// AuthToken == "" is the dev-mode "open" path — middleware lets
	// every request through. Production deployments set the token;
	// startup logs a warning when it's empty.
	s := authedTestServer(t, "")
	var reached bool
	h := s.authMiddleware(passthroughHandler(&reached))

	req := httptest.NewRequest("GET", "/v1/agents/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d, reached = %v; want 200 + reached (open-mode)", rec.Code, reached)
	}
}
