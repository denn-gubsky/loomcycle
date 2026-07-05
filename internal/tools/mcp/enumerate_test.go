package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// fakeDefReader is an in-memory mcp.ActiveDefReader keyed by tenant+"/"+name.
type fakeDefReader struct{ defs map[string]json.RawMessage }

func (f fakeDefReader) ActiveMCPServerDef(_ context.Context, tenant, name string) (json.RawMessage, bool) {
	d, ok := f.defs[tenant+"/"+name]
	return d, ok
}

func regWith(specs ...mcp.DynamicMCPServerSpec) *mcp.DynamicRegistry {
	r := mcp.NewDynamicRegistry()
	for _, s := range specs {
		r.Set(s)
	}
	return r
}

// TestDynamicToolsForRun_CacheAdvertisedWithoutHandshake — a server whose def
// carries discovered_tools is advertised straight from cache; the pool is never
// dialed (the build callback here would fail if it were).
func TestDynamicToolsForRun_CacheAdvertisedWithoutHandshake(t *testing.T) {
	built := false
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		built = true
		return nil, errors.New("handshake must not happen on the cache path")
	}, nil, nil)
	t.Cleanup(pool.Close)

	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "", Name: "telegram", Transport: "stdio"})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"/telegram": json.RawMessage(`{"transport":"stdio","discovered_tools":[{"name":"send_message","description":"send","input_schema":{"type":"object"}}]}`),
	}}

	// wantServers nil: cache path does not depend on the reference set.
	out := mcp.DynamicToolsForRun(context.Background(), pool, reg, reader, "", nil, time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("want 1 advertised tool, got %d", len(out))
	}
	if got, want := out[0].Name(), "mcp__telegram__send_message"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
	if built {
		t.Error("pool was dialed on the cache path — should advertise from discovered_tools only")
	}
}

// TestDynamicToolsForRun_HandshakesReferencedEmptyCache is the F33 regression:
// a referenced server with an EMPTY discovered_tools cache is handshaked at run
// start and its tools advertised. On the pre-F33 enumerator this server
// contributed zero tools, so an agent referencing only it saw nothing.
func TestDynamicToolsForRun_HandshakesReferencedEmptyCache(t *testing.T) {
	pool := newPoolWithFake(t, "ok") // fake exposes one tool: web_search

	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "", Name: "search", Transport: "stdio"})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"/search": json.RawMessage(`{"transport":"stdio"}`), // no discovered_tools → empty cache
	}}

	want := map[string]bool{"search": true} // agent's tools referenced mcp__search__*
	out := mcp.DynamicToolsForRun(context.Background(), pool, reg, reader, "", want, 5*time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("want 1 handshaked+advertised tool, got %d", len(out))
	}
	if got, want := out[0].Name(), "mcp__search__web_search"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
}

// TestDynamicToolsForRun_SkipsUnreferencedEmptyCache — an empty-cache server the
// run does NOT reference is never handshaked (so one down peer can't slow every
// run). Proven by a build callback that records whether it was dialed.
func TestDynamicToolsForRun_SkipsUnreferencedEmptyCache(t *testing.T) {
	dialed := false
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		dialed = true
		return nil, errors.New("should not be dialed for an unreferenced server")
	}, nil, nil)
	t.Cleanup(pool.Close)

	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "", Name: "search", Transport: "stdio"})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"/search": json.RawMessage(`{"transport":"stdio"}`), // empty cache
	}}

	// wantServers empty → server is not referenced by the run.
	out := mcp.DynamicToolsForRun(context.Background(), pool, reg, reader, "", nil, time.Second, nil)
	if len(out) != 0 {
		t.Fatalf("want 0 tools for an unreferenced empty-cache server, got %d", len(out))
	}
	if dialed {
		t.Error("pool was dialed for an unreferenced server — F33 handshake must be scoped to referenced servers")
	}
}

// TestDynamicToolsForRun_HandshakeDialsRunTenant guards the tenant stamp: the
// enumerator runs before RunIdentity is on ctx, so the handshake must stamp the
// EXPLICIT run tenant onto the pool ctx — otherwise pool.Get would dial the
// shared ("") server instead of the tenant's own. We assert the pool's build
// callback is invoked with the run's tenant.
func TestDynamicToolsForRun_HandshakeDialsRunTenant(t *testing.T) {
	var gotTenant string
	pool := mcp.NewPool(
		func(tenant, _ string) (mcp.Caller, error) {
			gotTenant = tenant
			return nil, errors.New("stop after recording tenant")
		},
		nil,
		func(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID },
	)
	t.Cleanup(pool.Close)

	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "search", Transport: "stdio"})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"acme/search": json.RawMessage(`{"transport":"stdio"}`), // empty cache
	}}

	// ctx has NO RunIdentity (run-creation time); the explicit tenant must win.
	mcp.DynamicToolsForRun(context.Background(), pool, reg, reader, "acme", map[string]bool{"search": true}, time.Second, nil)
	if gotTenant != "acme" {
		t.Errorf("handshake dialed tenant %q, want %q (tenant stamp missing)", gotTenant, "acme")
	}
}

// TestDynamicToolsForRun_ReferencedHandshakeFailsGracefully — a referenced
// server whose handshake fails (peer down) advertises nothing and does not
// error the enumeration; other servers still resolve.
func TestDynamicToolsForRun_ReferencedHandshakeFailsGracefully(t *testing.T) {
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		return nil, errors.New("peer down")
	}, nil, nil)
	t.Cleanup(pool.Close)

	reg := regWith(mcp.DynamicMCPServerSpec{TenantID: "", Name: "down", Transport: "stdio"})
	reader := fakeDefReader{defs: map[string]json.RawMessage{
		"/down": json.RawMessage(`{"transport":"stdio"}`),
	}}

	out := mcp.DynamicToolsForRun(context.Background(), pool, reg, reader, "", map[string]bool{"down": true}, time.Second, nil)
	if len(out) != 0 {
		t.Fatalf("want 0 tools when the referenced peer is down, got %d", len(out))
	}
}
