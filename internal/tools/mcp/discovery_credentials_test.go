package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// A server that puts its whole MCP route behind the run's named credential,
// tools/list included — the shape a web service with an auth middleware has.
var namedCredentialHeaders = map[string]string{"Authorization": "Bearer ${run.credentials.jobs}"}

// discoverJobs runs the run-start discovery of a tenant's dynamic server
// "jobs" with no stored tools, on a cold pool, from ctx.
func discoverJobs(t *testing.T, ctx context.Context, up *mcptest.Server) []tools.Tool {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		return mcphttp.New(mcphttp.Config{URL: up.URL, Headers: namedCredentialHeaders})
	}, nil, func(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID })
	t.Cleanup(pool.Close)
	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "jobs", Transport: "http", URL: up.URL, Headers: namedCredentialHeaders})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"acme/jobs": json.RawMessage(`{"transport":"http"}`), // no discovered_tools
	}}
	return mcp.DynamicToolsForRun(ctx, pool, reg, reader, "acme", map[string]bool{"jobs": true}, 5*time.Second, nil)
}

// A top-level run discovers before its identity is stamped. Its own
// credentials must authenticate the handshake; without them the server
// refused it and the run started with none of its tools, until a sub-agent
// (discovering on its parent's credentialed ctx) happened to warm the pool.
func TestDynamicToolsForRun_ACreatedRunsCredentialAuthenticatesDiscovery(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.WithRequiredAuthorization("Bearer t"))

	ctx := tools.WithMCPDiscoveryCredentials(context.Background(), "", map[string]string{"jobs": "t"})
	out := discoverJobs(t, ctx, up)

	if len(out) != 1 {
		t.Fatalf("advertised %d tool(s) to a run that carries the server's credential, want 1", len(out))
	}
	for _, m := range []string{"initialize", "tools/list"} {
		if got, ok := up.AuthorizationSeen(m); !ok || got != "Bearer t" {
			t.Errorf("%s carried Authorization %q (sent=%v), want %q", m, got, ok, "Bearer t")
		}
	}
}

// Without the credential nothing changes: the header is dropped and the
// handshake still goes out — not refused — so a server that lists its tools
// anonymously keeps working.
func TestDynamicToolsForRun_WithoutTheCredentialDiscoveryGoesWithoutTheHeader(t *testing.T) {
	up := mcptest.NewServer(t) // lists its tools to anyone

	out := discoverJobs(t, context.Background(), up)

	if len(out) != 1 {
		t.Fatalf("advertised %d tool(s), want 1 — an anonymous tools/list must still be fetched", len(out))
	}
	if got, ok := up.AuthorizationSeen("initialize"); !ok || got != "" {
		t.Errorf("initialize carried Authorization %q (sent=%v), want it sent with no header", got, ok)
	}
}

// A sub-agent discovers on its parent's ctx, whose identity already holds
// the credentials it inherits; those are the ones used, whatever else is on
// ctx.
func TestDynamicToolsForRun_AnIdentitysOwnCredentialsAreKept(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.WithRequiredAuthorization("Bearer parent"))

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		TenantID: "acme", UserCredentials: map[string]string{"jobs": "parent"},
	})
	ctx = tools.WithMCPDiscoveryCredentials(ctx, "", map[string]string{"jobs": "other"})
	if out := discoverJobs(t, ctx, up); len(out) != 1 {
		t.Fatalf("advertised %d tool(s), want 1 with the identity's own credential", len(out))
	}
	if got, _ := up.AuthorizationSeen("tools/list"); got != "Bearer parent" {
		t.Errorf("tools/list carried Authorization %q, want %q", got, "Bearer parent")
	}
}
