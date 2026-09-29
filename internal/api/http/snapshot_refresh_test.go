package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/mcptest"
)

// The post-restore refresh (RFC DP §4.11, V10): an MCP server def a restore
// writes is dialable at once — no restart — because the refresh reloads the
// in-process registry with boot's own function, and it does so before any
// paused run is resumed.

// registryView adapts the dynamic registry to the lookup chain, as main.go's
// mcpLookupView does, so the test pool resolves names exactly the way the
// production pool's build callback does.
type registryView struct{ reg *loommcp.DynamicRegistry }

func (v registryView) Get(tenantID, name string) (lookup.MCPServerSpec, bool) {
	s, ok := v.reg.Get(tenantID, name)
	if !ok {
		return lookup.MCPServerSpec{}, false
	}
	return lookup.MCPServerSpec{Transport: s.Transport, URL: s.URL, Headers: s.Headers, Source: "dynamic"}, true
}

// mcpRefreshHarness is a server whose restore refresh is wired the way main.go
// wires it, plus a pool that dials through the same registry.
type mcpRefreshHarness struct {
	srv  *Server
	st   store.Store
	reg  *loommcp.DynamicRegistry
	pool *loommcp.Pool
}

func newMCPRefreshHarness(t *testing.T) *mcpRefreshHarness {
	t.Helper()
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	t.Cleanup(cleanup)
	h := &mcpRefreshHarness{srv: srv, st: st, reg: loommcp.NewDynamicRegistry()}
	srv.SetMCPRegistryRefresh(func(ctx context.Context) (int, error) {
		return builtin.RehydrateMCPRegistry(ctx, st, srv.cfg().MCPServers, h.reg, t.Logf)
	})
	h.pool = loommcp.NewPool(
		func(tenant, name string) (loommcp.Caller, error) {
			spec, ok := lookup.MCPServer(srv.cfg(), registryView{h.reg}, tenant, name)
			if !ok {
				return nil, fmt.Errorf("mcp_servers.%s: not in static yaml or dynamic registry (tenant=%q)", name, tenant)
			}
			return mcphttp.New(mcphttp.Config{URL: spec.URL, Headers: spec.Headers})
		},
		func(c loommcp.Caller) {
			if cl, ok := c.(interface{ Close() error }); ok {
				_ = cl.Close()
			}
		},
		func(ctx context.Context) string { return tools.RunIdentity(ctx).TenantID },
	)
	t.Cleanup(h.pool.Close)
	return h
}

// dial handshakes name as tenant's run would.
func (h *mcpRefreshHarness) dial(t *testing.T, tenant, name string) ([]loommcp.ToolDescriptor, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_dial", TenantID: tenant})
	_, descs, err := h.pool.Get(ctx, name)
	return descs, err
}

func mcpDefJSON(url string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"transport": "streamable-http", "url": url})
	return b
}

// captureMCPDefs builds an envelope on a store of its own holding a live def
// ("restored"), a def whose active row is retired ("retired"), and a def that
// collides with one the target already serves ("taken").
func captureMCPDefs(t *testing.T, restoredURL string) []byte {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	for _, d := range []struct {
		id, name, url string
		retired       bool
	}{
		{"mcp_src_restored", "restored", restoredURL, false},
		{"mcp_src_retired", "retired", "https://retired.example.test/mcp", true},
		{"mcp_src_taken", "taken", "https://source-side.example.test/mcp", false},
	} {
		if _, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: d.id, TenantID: "acme", Name: d.name, Definition: mcpDefJSON(d.url)}); err != nil {
			t.Fatal(err)
		}
		if err := src.MCPServerDefSetActive(ctx, "acme", d.name, d.id, ""); err != nil {
			t.Fatal(err)
		}
		if d.retired {
			if err := src.MCPServerDefSetRetired(ctx, d.id, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seedLiveTakenDef gives the target its own live "taken" def, loaded into the
// registry the way boot loads it. The restore must leave it serving.
func (h *mcpRefreshHarness) seedLiveTakenDef(t *testing.T, liveURL string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "mcp_dst_taken", TenantID: "acme", Name: "taken", Definition: mcpDefJSON(liveURL)}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.MCPServerDefSetActive(ctx, "acme", "taken", "mcp_dst_taken", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := builtin.RehydrateMCPRegistry(ctx, h.st, nil, h.reg, t.Logf); err != nil {
		t.Fatal(err)
	}
}

// assertRestoredRegistry checks what the refresh must have left live.
func (h *mcpRefreshHarness) assertRestoredRegistry(t *testing.T, liveURL string, counts map[string]int) {
	t.Helper()
	descs, err := h.dial(t, "acme", "restored")
	if err != nil {
		t.Fatalf("the restored MCP server def is not dialable without a restart: %v", err)
	}
	if len(descs) != 1 || descs[0].Name != "check_user" {
		t.Errorf("restored server's tools = %+v, want its check_user", descs)
	}
	if _, ok := h.reg.Get("acme", "retired"); ok {
		t.Error("a def whose active row is retired was activated")
	}
	if got, _ := h.reg.Get("acme", "taken"); got.URL != liveURL {
		t.Errorf("the target's live def now points at %q; the live row must stand (%q)", got.URL, liveURL)
	}
	if counts["mcp_server_defs_activated"] != 1 {
		t.Errorf("restored[mcp_server_defs_activated] = %d, want 1 (only the restored def)", counts["mcp_server_defs_activated"])
	}
	// "taken" v1 collides with the target's own row and is not written.
	if counts["mcp_server_defs"] != 2 {
		t.Errorf("restored[mcp_server_defs] = %d, want 2", counts["mcp_server_defs"])
	}
}

func TestRestoreSnapshot_RestoredMCPServerDefIsDialableWithoutRestart(t *testing.T) {
	upstream := mcptest.NewServer(t)
	h := newMCPRefreshHarness(t)
	const liveURL = "https://target-live.example.test/mcp"
	h.seedLiveTakenDef(t, liveURL)
	raw := captureMCPDefs(t, upstream.URL)

	if _, err := h.dial(t, "acme", "restored"); err == nil {
		t.Fatal("setup: the def is dialable before the restore")
	}

	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	h.srv.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp snapshotRestoreResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	h.assertRestoredRegistry(t, liveURL, resp.Restored)
}

func TestConnectorRestoreSnapshot_RestoredMCPServerDefIsDialableWithoutRestart(t *testing.T) {
	upstream := mcptest.NewServer(t)
	h := newMCPRefreshHarness(t)
	const liveURL = "https://target-live.example.test/mcp"
	h.seedLiveTakenDef(t, liveURL)
	raw := captureMCPDefs(t, upstream.URL)

	res, err := h.srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	h.assertRestoredRegistry(t, liveURL, res.Restored)
}

// The refresh runs before the restored paused runs are resumed, so a resumed
// run finds the caches current. The refresh records whether the restored run
// was still waiting to resume when it ran.
func TestRestoreSnapshot_RefreshRunsBeforePausedRunsResume(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	runID, raw := capturePausedRun(t, store.RunIdentity{AgentID: "a_order", UserID: "alice", TenantID: "acme"}, "qa", nil)

	var statusAtRefresh store.RunStatus
	var refreshed int
	srv.SetMCPRegistryRefresh(func(ctx context.Context) (int, error) {
		refreshed++
		run, err := st.GetRun(ctx, runID)
		if err != nil {
			t.Errorf("refresh: read restored run: %v", err)
			return 0, nil
		}
		statusAtRefresh = run.Status
		return 0, nil
	})

	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	srv.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if refreshed != 1 {
		t.Fatalf("refresh ran %d times, want 1", refreshed)
	}
	// Resume settled the run ("qa" does not resolve here, so it is flagged
	// failed); the refresh saw it before that, still as restored.
	after, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == store.RunRunning {
		t.Fatalf("setup: resume did not settle the restored run (status %q); the ordering is unobservable", after.Status)
	}
	if statusAtRefresh != store.RunRunning {
		t.Errorf("the refresh saw the restored run as %q; it ran after the resume (want %q)", statusAtRefresh, store.RunRunning)
	}
}
