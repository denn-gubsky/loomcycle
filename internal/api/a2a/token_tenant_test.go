package a2a

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// startA2AServerWithAuth is startA2AServer with the production frontier
// authenticator over the given principals.
func startA2AServerWithAuth(t *testing.T, backend *scriptedBackend, cfg *config.Config, tokens map[string]auth.Principal) *httptest.Server {
	t.Helper()
	var real atomic.Pointer[http.Handler]
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := real.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "server not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(httpSrv.Close)
	cfg.Env.A2APublicBaseURL = httpSrv.URL
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		p, ok := tokens[bearer]
		return p, ok
	}
	srv, err := New(context.Background(), Deps{
		Cfg:   cfg,
		Store: backend,
		Conn:  integrationConnector{},
		Run:   backend,
		Auth:  FrontierAuthenticator(func(context.Context) bool { return true }, resolve, false),
	})
	if err != nil || srv == nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	srv.Mount(mux, nil)
	h := srv.PathTenantWrapper(mux)
	real.Store(&h)
	return httpSrv
}

// A token belongs to one tenant. Host routing chooses the tenant a run is
// made in, and the caller chooses the host, so a tenant's token must reach
// only its own tenant's host: otherwise it starts runs, and reads and cancels
// tasks, in any tenant it names. An admin token reaches every tenant.
func TestA2A_ATenantTokenReachesOnlyItsOwnTenant(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{"loomcycle-fleet": fixtureCard()},
	}
	cfg.Env.A2AServerEnabled = true
	cfg.Env.A2AServerCardName = "loomcycle-fleet"
	cfg.Env.A2ATenancyRouting = "host"
	httpSrv := startA2AServerWithAuth(t, backend, cfg, map[string]auth.Principal{
		"acme-tok":  {TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}},
		"admin-tok": {TenantID: "", Subject: "root", Scopes: []string{auth.ScopeAdmin, auth.ScopeRunsCreate}},
	})
	send := func(host, token string) {
		body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"hi"}],"metadata":{"skillId":"research"}}}}`
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a/jsonrpc", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send (host=%s): %v", host, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	send("tenant-acme.agents.example", "acme-tok")   // its own tenant: runs
	send("tenant-globex.agents.example", "acme-tok") // another tenant: refused
	send("agents.example", "acme-tok")               // the shared root: refused
	send("tenant-globex.agents.example", "admin-tok")

	backend.mu.Lock()
	defer backend.mu.Unlock()
	want := []string{"acme", "globex"}
	if len(backend.ranTenants) != len(want) {
		t.Fatalf("ran tenants = %q, want %q (acme's token ran in another tenant)", backend.ranTenants, want)
	}
	for i, w := range want {
		if backend.ranTenants[i] != w {
			t.Errorf("run %d ran in %q, want %q", i, backend.ranTenants[i], w)
		}
	}
}

// With no routing the body may name a tenant ("attribution-only"), but a
// tenant's token runs in its own tenant whatever the body says.
func TestA2A_WithNoRoutingATenantTokenRunsInItsOwnTenant(t *testing.T) {
	backend := newScriptedBackend(completeWithText("done"))
	cfg := twoAgentCardCfg()
	httpSrv := startA2AServerWithAuth(t, backend, cfg, map[string]auth.Principal{
		"acme-tok": {TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}},
	})
	body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"tenant":"globex","message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"hi"}],"metadata":{"skillId":"research"}}}}`
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a/jsonrpc", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer acme-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.ranTenants) != 1 || backend.ranTenants[0] != "acme" {
		t.Fatalf("ran tenants = %q, want [acme]", backend.ranTenants)
	}
}
