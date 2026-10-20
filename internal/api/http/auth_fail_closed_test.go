package http

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A deployment authenticated only by a minted admin token decides "is auth on?"
// by counting admin tokens in the store. If that count cannot be read, the
// answer must be "yes": reading it as "no" is open mode, which serves the
// request with no principal at all.

// countErrStore is a store whose admin-token count always fails, as it does
// during a database outage.
type countErrStore struct{ store.Store }

func (countErrStore) OperatorTokenDefCountActiveAdmin(context.Context) (int, error) {
	return 0, errors.New("store unavailable")
}

func TestAuthMiddleware_StoreErrorFailsClosed(t *testing.T) {
	s, st := tokenAuthServer(t, "") // no legacy secret: auth rests on the admin token
	seedToken(t, st, "lct_admin", "default", "root", []string{auth.ScopeAdmin}, time.Time{})
	s.store = countErrStore{Store: st}

	for _, path := range []string{"/v1/_me", "/v1/runs"} {
		var reached bool
		rec := httptest.NewRecorder()
		s.authMiddleware(passthroughHandler(&reached)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s with no bearer during a store error: status = %d reached = %v, want 401 and not reached", path, rec.Code, reached)
		}
	}
}

// TestAuthConfigured_StoreErrorReadsAsConfigured covers the exported predicate
// the gRPC interceptor and the A2A frontier use for the same decision.
func TestAuthConfigured_StoreErrorReadsAsConfigured(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	seedToken(t, st, "lct_admin", "default", "root", []string{auth.ScopeAdmin}, time.Time{})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !s.AuthConfigured(cancelled) {
		t.Error("AuthConfigured on a cancelled context = false (open mode), want true")
	}
	s.store = countErrStore{Store: st}
	if !s.AuthConfigured(context.Background()) {
		t.Error("AuthConfigured during a store error = false (open mode), want true")
	}
}

// TestAuthMiddleware_HalfClosedRequestFailsClosed: a client that sends a
// request and then half-closes its socket gets its request context cancelled
// by net/http, which makes the admin-token count fail. Over a real connection,
// that request must still be refused.
func TestAuthMiddleware_HalfClosedRequestFailsClosed(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	seedToken(t, st, "lct_admin", "default", "root", []string{auth.ScopeAdmin}, time.Time{})

	var reached atomic.Int64
	mux := http.NewServeMux()
	mux.Handle("GET /v1/_me", s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	})))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Repeated: whether the cancellation lands before the count is a race, and
	// one lucky ordering must not hide the bug.
	const attempts = 30
	for i := 0; i < attempts; i++ {
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("GET /v1/_me HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("attempt %d: half-closed no-bearer request = %d, want 401", i, resp.StatusCode)
			}
		}
		_ = conn.Close()
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d of %d half-closed no-bearer requests reached the handler", n, attempts)
	}
}
