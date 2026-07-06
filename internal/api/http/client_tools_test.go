package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/clienttools"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// clientToolsTestConfig is a minimal config with the client-tool knobs set (a
// zero ClientToolMaxBytes would make SetReadLimit reject every frame).
func clientToolsTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Env.ClientToolMaxBytes = 1 << 20
	cfg.Env.SSEKeepaliveInterval = 20 * time.Second
	return cfg
}

// clientToolsTestServer stands up just the /v1/client-tools handler over an
// httptest server, with a principal injected on ctx (open-mode auth) so the
// handler files the connection under a known key.
func clientToolsTestServer(t *testing.T, reg *clienttools.Registry, p auth.Principal) *httptest.Server {
	t.Helper()
	srv := &Server{}
	srv.cfg = clientToolsTestConfig()
	srv.clientTools = reg
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		srv.handleClientTools(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func TestHandleClientTools_HelloRoundTrip(t *testing.T) {
	reg := clienttools.NewRegistry(0)
	ts := clientToolsTestServer(t, reg, auth.Principal{TenantID: "t1", Subject: "u1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Send hello registering one tool.
	hello := clienttools.HelloFrame{
		Type:   clienttools.FrameHello,
		Client: "test/1",
		Tools:  []clienttools.ToolSchema{{Name: "browser.read_page", Description: "read the page"}},
	}
	b, _ := json.Marshal(hello)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// Expect hello_ok naming the accepted tool.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello_ok: %v", err)
	}
	if clienttools.TypeOf(data) != clienttools.FrameHelloOK {
		t.Fatalf("first frame type = %q, want hello_ok; raw=%s", clienttools.TypeOf(data), data)
	}
	var ok clienttools.HelloOKFrame
	if err := json.Unmarshal(data, &ok); err != nil {
		t.Fatal(err)
	}
	if len(ok.Accepted) != 1 || ok.Accepted[0] != "browser.read_page" || ok.ConnectionID == "" {
		t.Errorf("hello_ok = %+v, want accepted [browser.read_page] + a connection_id", ok)
	}

	// The connection is now in the registry under the principal key.
	if reg.Count() != 1 {
		t.Errorf("registry Count = %d, want 1", reg.Count())
	}
	if len(reg.Provides(clienttools.PrincipalKey{Tenant: "t1", Subject: "u1"})) != 1 {
		t.Errorf("registry should advertise the registered tool for the principal")
	}
}

func TestHandleClientTools_DisabledWhenNoRegistry(t *testing.T) {
	srv := &Server{}
	srv.cfg = clientToolsTestConfig()
	// clientTools nil → endpoint refuses.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/client-tools", nil)
	srv.handleClientTools(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when client-tools disabled", rr.Code)
	}
}
