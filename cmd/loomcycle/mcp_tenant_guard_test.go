package main

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestMCPPoolBuild_TenantDefAlwaysDialsThroughTheGuard: LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS
// (on by default) lets the operator's own MCP servers sit on localhost or a
// private network. It must not extend to a server a tenant registered at
// runtime: that def always dials through the egress guard, so it reaches a
// private address only when the operator vouched for it in
// LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, and never a metadata address.
func TestMCPPoolBuild_TenantDefAlwaysDialsThroughTheGuard(t *testing.T) {
	peer := newEnvPeer(t) // on loopback
	cfg := &config.Config{
		Env:        config.Env{MCPAllowPrivateIPs: true}, // the default opt-out
		MCPServers: map[string]config.MCPServer{"static-peer": {Transport: "http", URL: peer.srv.URL}},
	}
	reg := mcp.NewDynamicRegistry()
	var credSubstitute func(ctx context.Context, s, targetURL string) (string, []string, error)
	build := mcpPoolBuild(cfg, mcpLookupView{reg}, &credSubstitute, (&warnLog{}).logf)
	call := func(name string) error {
		c, err := build("acme", name)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err = c.Call(ctx, "initialize", map[string]any{})
		return err
	}

	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "tenant-loopback", Transport: "http", URL: peer.srv.URL})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "tenant-metadata", Transport: "http", URL: "http://169.254.169.254/mcp"})
	for _, name := range []string{"tenant-loopback", "tenant-metadata"} {
		err := call(name)
		if err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: err = %v, want the egress guard's refusal", name, err)
			continue
		}
		// The refusal is actionable: it names the server, its tenant and both
		// ways to restore it.
		for _, want := range []string{`"` + name + `"`, `tenant "acme"`, "LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST", "re-save"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal lacks %q: %v", name, want, err)
			}
		}
	}
	if got := peer.seen(); len(got) != 0 {
		t.Fatalf("a tenant def reached a private address the operator did not vouch for (%d request(s))", len(got))
	}

	// The operator's servers keep the opt-out: static yaml, and a dynamic
	// version saved with operator authority.
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "operator-loopback", Transport: "http", URL: peer.srv.URL, OperatorAuthored: true})
	for _, name := range []string{"static-peer", "operator-loopback"} {
		if err := call(name); err != nil && strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: an operator's server was refused by the guard: %v", name, err)
		}
	}
	if got := peer.seen(); len(got) != 2 {
		t.Fatalf("the operator's servers made %d request(s), want 2", len(got))
	}

	// A tenant def whose host the operator vouched for dials.
	u, _ := url.Parse(peer.srv.URL)
	cfg.Env.HTTPPrivateHostAllowlist = []string{u.Hostname()}
	if err := call("tenant-loopback"); err != nil && strings.Contains(err.Error(), "blocked") {
		t.Errorf("a vouched tenant host was refused: %v", err)
	}
	if got := peer.seen(); len(got) != 3 {
		t.Fatalf("the vouched tenant def made %d total request(s), want 3", len(got))
	}
}
