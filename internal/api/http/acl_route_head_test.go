package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// RFC EZ P6: the mux serves HEAD with the GET handler, so the gates ask HEAD
// what they ask GET. Before the route table they keyed on the method string:
// HEAD /v1/runs/{run_id} needed no scope, so a token without runs:read ran the
// run-read handler and learned from the status whether a run exists.
func TestACLRoutes_HEADIsGatedAsGET(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, "lct_channels", "acme", "chan", []string{auth.ScopeChannelPublish, auth.ScopeChannelRead}, time.Time{})
	seedToken(t, st, "lct_reader", "acme", "reader", []string{auth.ScopeRunsRead}, time.Time{})
	mux := http.NewServeMux()
	mux.Handle("GET /v1/runs/{run_id}", s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	for _, c := range []struct {
		method, bearer string
		want           int
	}{
		{http.MethodGet, "lct_channels", http.StatusForbidden},
		{http.MethodHead, "lct_channels", http.StatusForbidden},
		{http.MethodGet, "lct_reader", http.StatusOK},
		{http.MethodHead, "lct_reader", http.StatusOK},
	} {
		req := httptest.NewRequest(c.method, "/v1/runs/run_1", nil)
		req.Header.Set("Authorization", "Bearer "+c.bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s with %s: %d, want %d", c.method, c.bearer, rec.Code, c.want)
		}
	}
}
