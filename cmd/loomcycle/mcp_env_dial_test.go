package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// envPeer is an httptest MCP peer that records every Authorization header it
// receives. It answers nothing useful: the tests only ask what reached it.
type envPeer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newEnvPeer(t *testing.T) *envPeer {
	p := &envPeer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *envPeer) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auth...)
}

// TestMCPPoolBuild_NonOperatorDefWithEnvRefSendsNothing pins the dial-time half
// of the env rule: a dynamic def stored without the operator_authored bit (a
// row written before authoring refused it) is not dialed when it holds a
// ${NAME}, so the operator's env value never reaches its host. The operator's
// own def, and a non-operator def with no ${NAME}, still dial.
func TestMCPPoolBuild_NonOperatorDefWithEnvRefSendsNothing(t *testing.T) {
	t.Setenv("LOOMCYCLE_PEER_KEY_GLOBEX", "globex-secret-value")
	peer := newEnvPeer(t)
	cfg := &config.Config{Env: config.Env{MCPAllowPrivateIPs: true}} // the httptest peer is on loopback
	reg := mcp.NewDynamicRegistry()
	var credSubstitute func(ctx context.Context, s string) (string, []string, error)
	build := mcpPoolBuild(cfg, mcpLookupView{reg}, &credSubstitute)
	keyHeader := map[string]string{"Authorization": "Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}

	for name, hdr := range map[string]map[string]string{
		"leaky-header": keyHeader,
		"leaky-nested": {"Authorization": "Bearer ${run.credentials.x:-${LOOMCYCLE_PEER_KEY_GLOBEX}}"},
	} {
		reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: name, Transport: "http", URL: peer.srv.URL, Headers: hdr})
		c, err := build("acme", name)
		if err == nil {
			_, _ = c.Call(context.Background(), "initialize", map[string]any{})
			t.Fatalf("%s: a non-operator def holding a ${NAME} was dialed", name)
		}
		if strings.Contains(err.Error(), "globex-secret-value") {
			t.Fatalf("%s: refusal carries the env value: %v", name, err)
		}
	}
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "leaky-stdio", Transport: "stdio",
		Command: "true", Env: map[string]string{"TOKEN": "${LOOMCYCLE_PEER_KEY_GLOBEX}"}})
	if c, err := build("acme", "leaky-stdio"); err == nil {
		if cl, ok := c.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
		t.Fatal("a non-operator stdio def holding a ${NAME} was spawned")
	}
	if got := peer.seen(); len(got) != 0 {
		t.Fatalf("the peer received %d request(s) from refused defs: %q", len(got), got)
	}

	// Controls: the same peer is reachable, and the operator's def sends its key.
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "plain", Transport: "http", URL: peer.srv.URL,
		Headers: map[string]string{"Authorization": "Bearer literal"}})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "blessed", Transport: "http", URL: peer.srv.URL,
		Headers: keyHeader, OperatorAuthored: true})
	for _, name := range []string{"plain", "blessed"} {
		c, err := build("acme", name)
		if err != nil {
			t.Fatalf("%s: not dialed: %v", name, err)
		}
		_, _ = c.Call(context.Background(), "initialize", map[string]any{})
	}
	got := peer.seen()
	if len(got) != 2 || got[0] != "Bearer literal" || got[1] != "Bearer globex-secret-value" {
		t.Fatalf("controls: want the literal then the operator's expanded key, got %d header(s)", len(got))
	}
}
