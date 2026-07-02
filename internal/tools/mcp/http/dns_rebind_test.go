package http

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestNew_BlockPrivateIPs is the DNS-rebinding guard: the MCP-HTTP client
// blocks private-IP dials ONLY when BlockPrivateIPs is set (opt-in via
// LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS=0). The default must still reach a
// localhost/private MCP server (the common deployment — incl. jobs-search-agent's
// /api/mcp), so blocking by default would be a breaking change.
func TestNew_BlockPrivateIPs(t *testing.T) {
	srv := newFakeServer(t, "ok") // loopback httptest server
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Default: reaches the loopback MCP server (unchanged behaviour).
	c, err := New(Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "initialize", map[string]any{}); err != nil {
		t.Fatalf("default client should reach the loopback MCP server, got: %v", err)
	}

	// Opt-in block: the loopback dial is refused.
	blocked, err := New(Config{URL: srv.URL, BlockPrivateIPs: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Call(ctx, "initialize", map[string]any{}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("BlockPrivateIPs client must refuse the loopback dial, got: %v", err)
	}

	// The operator's private-host allowlist exempts the internal MCP host.
	host, _, _ := net.SplitHostPort(srv.Listener.Addr().String())
	allowed, err := New(Config{URL: srv.URL, BlockPrivateIPs: true, PrivateHostAllowlist: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allowed.Call(ctx, "initialize", map[string]any{}); err != nil {
		t.Fatalf("allowlisted internal MCP host should dial through: %v", err)
	}
}
