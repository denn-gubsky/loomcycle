package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// A server whose header sends the per-run bearer, the shape a per-user web
// service declares. The run-start re-probe is discovery — initialize and
// tools/list, as the boot handshake does — and holds no run's credential.
// It ran under the run's tenant, which stamps a run identity; for any tenant
// but "" the client then refused the handshake for the missing bearer, before
// sending anything. A server skipped at boot (it started after loomcycle) was
// never advertised again, until a restart.
var perRunBearerHeaders = map[string]string{"Authorization": "Bearer ${run.user_bearer}"}

// tenantPool dials url for every name, keyed by the run's tenant as the
// production pool is.
func tenantPool(t *testing.T, url string) *mcp.Pool {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		return mcphttp.New(mcphttp.Config{URL: url, Headers: perRunBearerHeaders})
	}, nil, func(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID })
	t.Cleanup(pool.Close)
	return pool
}

func TestStaticToolsForRun_APerRunBearerServerRecoversUnderANamedTenant(t *testing.T) {
	up := mcptest.NewServer(t)
	servers := map[string]config.MCPServer{"ccq": {Transport: "http", URL: up.URL, Headers: perRunBearerHeaders}}

	out := mcp.StaticToolsForRun(context.Background(), tenantPool(t, up.URL), servers,
		map[string]bool{"ccq": true}, // skipped at boot
		"default", map[string]bool{"ccq": true}, 5*time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("a server that is up now advertised %d tool(s) under tenant default, want 1", len(out))
	}
	if got, want := out[0].Name(), "mcp__ccq__check_user"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
}

func TestDynamicToolsForRun_APerRunBearerServerRecoversUnderANamedTenant(t *testing.T) {
	up := mcptest.NewServer(t)
	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "default", Name: "ccq", Transport: "http", URL: up.URL, Headers: perRunBearerHeaders})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"default/ccq": json.RawMessage(`{"transport":"http"}`), // no discovered_tools
	}}

	out := mcp.DynamicToolsForRun(context.Background(), tenantPool(t, up.URL), reg, reader,
		"default", map[string]bool{"ccq": true}, 5*time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("a referenced server with no stored tools advertised %d tool(s) under tenant default, want 1", len(out))
	}
}

// The re-probe sends discovery without the bearer; a tool CALL is a run's
// request and still refuses to go without it.
func TestStaticToolsForRun_ARecoveredToolsCallStillNeedsTheRunsBearer(t *testing.T) {
	up := mcptest.NewServer(t)
	servers := map[string]config.MCPServer{"ccq": {Transport: "http", URL: up.URL, Headers: perRunBearerHeaders}}
	out := mcp.StaticToolsForRun(context.Background(), tenantPool(t, up.URL), servers,
		map[string]bool{"ccq": true}, "default", map[string]bool{"ccq": true}, 5*time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("advertised %d tool(s), want 1", len(out))
	}
	input := json.RawMessage(`{"user_id":"alice"}`)

	noBearer := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "default", UserID: "alice"})
	res, err := out[0].Execute(noBearer, input)
	if err != nil {
		t.Fatalf("a call from a run without its bearer: %v", err)
	}
	// The refusal reaches the model as a not-retryable business error.
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryBusiness || res.Error.Retryable {
		t.Fatalf("a call from a run without its bearer = %+v, want the refusal (business, not retryable)", res)
	}
	if n := up.ToolCalls(); n != 0 {
		t.Fatalf("the server received %d tool call(s) without the bearer, want 0", n)
	}

	withBearer := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "default", UserID: "alice", UserBearer: "alice"})
	if _, err := out[0].Execute(withBearer, input); err != nil {
		t.Fatalf("a call from a run with its bearer: %v", err)
	}
	if up.MatchedBearers() != 1 {
		t.Errorf("matched bearers = %d, want 1 (the call carries the run's bearer)", up.MatchedBearers())
	}
}
