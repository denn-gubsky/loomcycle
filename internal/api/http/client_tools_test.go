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
		Tools:  []clienttools.ToolSchema{{Name: "browser_read_page", Description: "read the page"}},
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
	if len(ok.Accepted) != 1 || ok.Accepted[0] != "browser_read_page" || ok.ConnectionID == "" {
		t.Errorf("hello_ok = %+v, want accepted [browser_read_page] + a connection_id", ok)
	}

	// The connection is now in the registry under the principal key.
	if reg.Count() != 1 {
		t.Errorf("registry Count = %d, want 1", reg.Count())
	}
	if len(reg.Provides(clienttools.PrincipalKey{Tenant: "t1", Subject: "u1"})) != 1 {
		t.Errorf("registry should advertise the registered tool for the principal")
	}
}

func TestCandidateTools_AdvertisesAndGatesClientTools(t *testing.T) {
	reg := clienttools.NewRegistry(0)
	key := clienttools.PrincipalKey{Tenant: "t1", Subject: "u1"}
	silent := func(context.Context, any) error { return nil }
	_, dereg, _ := reg.Register(key, []clienttools.ToolSchema{
		{Name: "browser_read_page"}, {Name: "browser_click"},
	}, silent)
	defer dereg()

	srv := &Server{}
	srv.cfg = clientToolsTestConfig()
	srv.clientTools = reg
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t1", Subject: "u1"})

	// candidateTools advertises the principal's client-tools (client__ prefixed).
	cands := srv.candidateTools(ctx, "t1", []string{"client__browser_*"})
	names := map[string]bool{}
	for _, tl := range cands {
		names[tl.Name()] = true
	}
	if !names["client__browser_read_page"] || !names["client__browser_click"] {
		t.Fatalf("candidateTools should advertise both client-tools; got %v", names)
	}

	// filterTools narrows to exactly what the agent grants.
	filtered := filterTools(cands, []string{"client__browser_read_page"}, nil)
	fnames := map[string]bool{}
	for _, tl := range filtered {
		fnames[tl.Name()] = true
	}
	if !fnames["client__browser_read_page"] {
		t.Errorf("granted client__browser_read_page should survive filtering; got %v", fnames)
	}
	if fnames["client__browser_click"] {
		t.Errorf("ungranted client__browser_click should be filtered out; got %v", fnames)
	}

	// A different principal sees no client-tools.
	other := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t1", Subject: "someone-else"})
	for _, tl := range srv.candidateTools(other, "t1", []string{"client__browser_*"}) {
		if len(tl.Name()) >= len(clienttools.ToolPrefix) && tl.Name()[:len(clienttools.ToolPrefix)] == clienttools.ToolPrefix {
			t.Errorf("a different principal must not see client-tools; saw %q", tl.Name())
		}
	}
}

// TestHandleClientTools_CrossOriginAccepted is the regression guard for the
// Origin bug: a browser client (extension / web app) ALWAYS sends an Origin
// header (host != loomcycle's Host), which coder/websocket's default same-origin
// check would 403 — so every real browser was rejected while curl (no Origin)
// sailed through. The handshake must SUCCEED with a cross-origin Origin, because
// auth is a bearer in the subprotocol (unforgeable cross-origin) + a
// SameSite=Strict cookie (never sent cross-site). Fail-before: drop
// InsecureSkipVerify and this dial gets a 403.
func TestHandleClientTools_CrossOriginAccepted(t *testing.T) {
	reg := clienttools.NewRegistry(0)
	ts := clientToolsTestServer(t, reg, auth.Principal{TenantID: "t1", Subject: "u1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Simulate a browser: an Origin whose host is NOT the server's Host.
	c, _, err := websocket.Dial(ctx, wsURL(ts.URL), &websocket.DialOptions{
		Subprotocols: []string{clientToolSubprotocol},
		HTTPHeader:   http.Header{"Origin": []string{"chrome-extension://abcdefghijklmnop"}},
	})
	if err != nil {
		t.Fatalf("cross-origin handshake must be accepted (bearer auth, not cookie CSRF), got: %v", err)
	}
	defer c.CloseNow()

	hello := clienttools.HelloFrame{Type: clienttools.FrameHello, Tools: []clienttools.ToolSchema{{Name: "browser_read_page"}}}
	b, _ := json.Marshal(hello)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if clienttools.TypeOf(data) != clienttools.FrameHelloOK {
		t.Errorf("expected hello_ok on a cross-origin connection; got %s", data)
	}
}

func TestHandleClientTools_RejectsWireUnsafeNames(t *testing.T) {
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

	// One valid, one dotted (illegal) bare name. The dotted one is skipped at
	// the hello boundary; only the valid one is accepted + registered.
	hello := clienttools.HelloFrame{
		Type: clienttools.FrameHello,
		Tools: []clienttools.ToolSchema{
			{Name: "browser_read_page"},
			{Name: "browser.click"}, // illegal (dot) → skipped
		},
	}
	b, _ := json.Marshal(hello)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ok clienttools.HelloOKFrame
	if err := json.Unmarshal(data, &ok); err != nil {
		t.Fatal(err)
	}
	if len(ok.Accepted) != 1 || ok.Accepted[0] != "browser_read_page" {
		t.Errorf("only the wire-safe name should be accepted; got %v", ok.Accepted)
	}
	for _, s := range reg.Provides(clienttools.PrincipalKey{Tenant: "t1", Subject: "u1"}) {
		if s.Name == "browser.click" {
			t.Error("a dotted bare name must not be registered")
		}
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
