package mcp_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

// staticServers builds a cfg.MCPServers-shaped map of stdio servers (Tools nil =
// allow all), the shape StaticToolsForRun reads for the operator tools filter.
func staticServers(names ...string) map[string]config.MCPServer {
	m := map[string]config.MCPServer{}
	for _, n := range names {
		m[n] = config.MCPServer{Transport: "stdio"}
	}
	return m
}

// TestStaticToolsForRun_RecoversSkippedReferenced is the core regression: a static
// server SKIPPED at boot but referenced by the run is handshaked at run start and
// its tools advertised — the exact gap that left an agent's mcp__<server>__* tools
// invisible until a loomcycle restart. On the pre-fix code (no run-start static
// re-probe) this server contributes zero tools.
func TestStaticToolsForRun_RecoversSkippedReferenced(t *testing.T) {
	pool := newPoolWithFake(t, "ok") // exposes one tool: web_search

	out := mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("search"), map[string]bool{"search": true}, // skipped at boot
		"", map[string]bool{"search": true}, // referenced by the run
		5*time.Second, nil)
	if len(out) != 1 {
		t.Fatalf("want 1 recovered tool, got %d", len(out))
	}
	if got, want := out[0].Name(), "mcp__search__web_search"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
}

// TestStaticToolsForRun_IgnoresBootRegistered — a server NOT in the skipped set is
// already in the boot catalog (candidateTools' floor); StaticToolsForRun must not
// re-advertise or re-dial it. This is the discriminator vs a blanket re-probe.
func TestStaticToolsForRun_IgnoresBootRegistered(t *testing.T) {
	dialed := false
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		dialed = true
		return nil, errors.New("must not dial a boot-registered server")
	}, nil, nil)
	t.Cleanup(pool.Close)

	out := mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("search"), map[string]bool{}, // empty skipped set
		"", map[string]bool{"search": true}, time.Second, nil)
	if len(out) != 0 || dialed {
		t.Errorf("boot-registered server must be ignored (tools=%d dialed=%v)", len(out), dialed)
	}
}

// TestStaticToolsForRun_SkipsUnreferenced — a skipped server the run does NOT
// reference is never handshaked, so one down peer can't slow every run.
func TestStaticToolsForRun_SkipsUnreferenced(t *testing.T) {
	dialed := false
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		dialed = true
		return nil, errors.New("must not dial an unreferenced server")
	}, nil, nil)
	t.Cleanup(pool.Close)

	out := mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("search"), map[string]bool{"search": true},
		"", map[string]bool{}, time.Second, nil) // not referenced by the run
	if len(out) != 0 || dialed {
		t.Errorf("unreferenced skipped server must not be dialed (tools=%d dialed=%v)", len(out), dialed)
	}
}

// TestStaticToolsForRun_CacheFastPath — once the first run re-probes + warms the
// pool cache, a second run advertises from cache with NO fresh handshake.
func TestStaticToolsForRun_CacheFastPath(t *testing.T) {
	var builds int32
	build := func(_, _ string) (mcp.Caller, error) {
		atomic.AddInt32(&builds, 1)
		c, err := stdio.Spawn(stdio.Config{Command: os.Args[0], Env: []string{"BE_MCP_SERVER=ok"}})
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	teardown := func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	}
	pool := mcp.NewPool(build, teardown, nil)
	t.Cleanup(pool.Close)

	probe := func() []tools.Tool {
		return mcp.StaticToolsForRun(context.Background(), pool,
			staticServers("search"), map[string]bool{"search": true},
			"", map[string]bool{"search": true}, 5*time.Second, nil)
	}
	if out := probe(); len(out) != 1 {
		t.Fatalf("first run: want 1 tool, got %d", len(out))
	}
	if out := probe(); len(out) != 1 {
		t.Fatalf("second run: want 1 tool from cache, got %d", len(out))
	}
	if n := atomic.LoadInt32(&builds); n != 1 {
		t.Errorf("want exactly 1 handshake across two runs (cache fast path), got %d", n)
	}
}

// TestStaticToolsForRun_DialsRunTenant — the enumerator runs before RunIdentity is
// on ctx, so the handshake must stamp the EXPLICIT run tenant onto the pool ctx
// (else pool.Get dials the shared "" slot). Mirrors the dynamic-path guard.
func TestStaticToolsForRun_DialsRunTenant(t *testing.T) {
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

	// ctx has NO RunIdentity (run-creation time); the explicit tenant must win.
	mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("search"), map[string]bool{"search": true},
		"acme", map[string]bool{"search": true}, time.Second, nil)
	if gotTenant != "acme" {
		t.Errorf("handshake dialed tenant %q, want %q (tenant stamp missing)", gotTenant, "acme")
	}
}

// TestStaticToolsForRun_HandshakeFailsGracefully — a referenced skipped peer that's
// still down advertises nothing and never panics (the lazy resolver backstops the
// actual call).
func TestStaticToolsForRun_HandshakeFailsGracefully(t *testing.T) {
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		return nil, errors.New("peer still down")
	}, nil, nil)
	t.Cleanup(pool.Close)

	out := mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("down"), map[string]bool{"down": true},
		"", map[string]bool{"down": true}, time.Second, nil)
	if len(out) != 0 {
		t.Fatalf("want 0 tools when the peer is down, got %d", len(out))
	}
}

// TestStaticToolsForRun_AppliesOperatorFilter — the per-server tools allowlist is
// honored exactly as the boot loop does, so a re-probe can't widen past the
// operator's narrowing.
func TestStaticToolsForRun_AppliesOperatorFilter(t *testing.T) {
	pool := newPoolWithFake(t, "ok") // exposes web_search
	servers := map[string]config.MCPServer{
		"search": {Transport: "stdio", Tools: []string{"not_web_search"}}, // excludes web_search
	}
	out := mcp.StaticToolsForRun(context.Background(), pool,
		servers, map[string]bool{"search": true},
		"", map[string]bool{"search": true}, 5*time.Second, nil)
	if len(out) != 0 {
		t.Errorf("operator tools filter must drop web_search on re-probe; got %d tools", len(out))
	}
}

// TestStaticToolsForRun_NoSkipsIsNoop — the common case (nothing skipped at boot)
// short-circuits: no work, no allocation, nil result.
func TestStaticToolsForRun_NoSkipsIsNoop(t *testing.T) {
	if out := mcp.StaticToolsForRun(context.Background(), nil, nil, nil, "", nil, time.Second, nil); out != nil {
		t.Errorf("empty skipped set + nil pool must be a no-op; got %v", out)
	}
}
