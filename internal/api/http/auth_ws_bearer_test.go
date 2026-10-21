package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/clienttools"
)

// The subprotocol bearer exists only because a browser cannot set an
// Authorization header on a WebSocket. Anywhere else it is a second, quieter
// way to send a credential, so it is read only on a WebSocket upgrade to the
// one WebSocket route (RFC ES #5).

// TestExtractBearer_SubprotocolIgnoredOutsideWebSocketUpgrade: a valid bearer
// carried only as a "bearer.<token>" subprotocol authenticates nothing unless the
// request is a WebSocket upgrade to /v1/client-tools.
func TestExtractBearer_SubprotocolIgnoredOutsideWebSocketUpgrade(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, "lct_ws", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})

	cases := []struct {
		name    string
		path    string
		upgrade bool
	}{
		{"plain GET of another route", "/v1/runs", false},
		{"upgrade to another route", "/v1/runs", true},
		{"plain GET of the WebSocket route", "/v1/client-tools", false},
	}
	for _, tc := range cases {
		for _, tok := range []string{"lct_ws", "legacy-secret"} {
			t.Run(tc.name+"/"+tok, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				req.Header.Set("Sec-WebSocket-Protocol", clientToolSubprotocol+", bearer."+tok)
				if tc.upgrade {
					req.Header.Set("Connection", "Upgrade")
					req.Header.Set("Upgrade", "websocket")
				}
				var reached bool
				rec := httptest.NewRecorder()
				s.authMiddleware(passthroughHandler(&reached)).ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized || reached {
					t.Errorf("status = %d reached = %v, want 401 and not reached", rec.Code, reached)
				}
			})
		}
	}
}

// TestExtractBearer_SubprotocolAcceptedOnClientToolsUpgrade: the real
// handshake a browser makes — a WebSocket dial to /v1/client-tools carrying the
// bearer as a subprotocol — authenticates, and the connection is filed under
// the token's own principal.
func TestExtractBearer_SubprotocolAcceptedOnClientToolsUpgrade(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-secret")
	seedToken(t, st, "lct_ws", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	s.cfg().Env.ClientToolMaxBytes = 1 << 20
	reg := clienttools.NewRegistry(0)
	s.clientTools = reg

	mux := http.NewServeMux()
	mux.Handle("GET /v1/client-tools", s.authMiddleware(http.HandlerFunc(s.handleClientTools)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts.URL)+"/v1/client-tools", &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol, "bearer.lct_ws"},
	})
	if err != nil {
		t.Fatalf("dial with a subprotocol bearer: %v", err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != clientToolSubprotocol {
		t.Errorf("negotiated subprotocol = %q, want %q (the bearer entry is never echoed)", c.Subprotocol(), clientToolSubprotocol)
	}

	b, _ := json.Marshal(clienttools.HelloFrame{
		Type:  clienttools.FrameHello,
		Tools: []clienttools.ToolSchema{{Name: "read_page", Description: "read"}},
	})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, data, err := c.Read(ctx); err != nil || clienttools.TypeOf(data) != clienttools.FrameHelloOK {
		t.Fatalf("hello_ok: err=%v frame=%s", err, data)
	}
	if got := len(reg.Provides(clienttools.PrincipalKey{Tenant: "acme", Subject: "alice"})); got != 1 {
		t.Errorf("tools filed under the token's principal = %d, want 1", got)
	}
}

// TestIsClientToolsUpgrade_HeaderTokenMatching: the upgrade test follows RFC
// 6455 header rules — Connection is a token list and both headers compare
// case-insensitively — so a browser behind a proxy that adds keep-alive still
// connects, and a near-miss does not count.
func TestIsClientToolsUpgrade_HeaderTokenMatching(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		connection string
		upgrade    string
		want       bool
	}{
		{"canonical", "/v1/client-tools", "Upgrade", "websocket", true},
		{"token list, mixed case", "/v1/client-tools", "keep-alive, UPGRADE", "WebSocket", true},
		{"no Connection upgrade token", "/v1/client-tools", "keep-alive", "websocket", false},
		{"Connection token is only a substring", "/v1/client-tools", "upgraded", "websocket", false},
		{"Upgrade is not websocket", "/v1/client-tools", "Upgrade", "h2c", false},
		{"other path", "/v1/client-tools/x", "Upgrade", "websocket", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Connection", tc.connection)
			req.Header.Set("Upgrade", tc.upgrade)
			if got := isClientToolsUpgrade(req); got != tc.want {
				t.Errorf("isClientToolsUpgrade = %v, want %v", got, tc.want)
			}
		})
	}
}
