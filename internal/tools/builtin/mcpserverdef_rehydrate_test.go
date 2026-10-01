package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// seedMCPDef creates and (optionally) promotes one MCP server def.
func seedMCPDef(t *testing.T, s store.Store, defID, tenant, name, url string, active, retired bool) {
	t.Helper()
	ctx := context.Background()
	def, _ := json.Marshal(map[string]any{"transport": "streamable-http", "url": url,
		"headers": map[string]string{"X-Api-Key": "${LOOMCYCLE_K}"}})
	if _, err := s.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: defID, TenantID: tenant, Name: name, Definition: def}); err != nil {
		t.Fatal(err)
	}
	if active {
		if err := s.MCPServerDefSetActive(ctx, tenant, name, defID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if retired {
		if err := s.MCPServerDefSetRetired(ctx, defID, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRehydrateMCPRegistry_LoadsActiveDefsAndSkipsWhatBootSkips(t *testing.T) {
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "rehydrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	seedMCPDef(t, s, "mcp_live", "acme", "search", "https://acme.example.test/mcp", true, false)
	seedMCPDef(t, s, "mcp_inactive", "acme", "draft", "https://draft.example.test/mcp", false, false)
	seedMCPDef(t, s, "mcp_retired", "acme", "gone", "https://gone.example.test/mcp", true, true)
	// A shared-tenant name the static yaml also declares: yaml wins for "".
	seedMCPDef(t, s, "mcp_shadowed", "", "files", "https://shared.example.test/mcp", true, false)
	// The same name in a TENANT is a legitimate override and is loaded.
	seedMCPDef(t, s, "mcp_override", "beta", "files", "https://beta.example.test/mcp", true, false)

	static := map[string]config.MCPServer{"files": {Transport: "streamable-http", URL: "https://static.example.test/mcp"}}
	reg := loommcp.NewDynamicRegistry()
	n, err := RehydrateMCPRegistry(context.Background(), s, static, reg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || reg.Size() != 2 {
		t.Fatalf("activated %d, registry holds %d; want 2 and 2 (acme/search, beta/files)", n, reg.Size())
	}
	got, ok := reg.Get("acme", "search")
	if !ok || got.URL != "https://acme.example.test/mcp" || got.Headers["X-Api-Key"] != "${LOOMCYCLE_K}" {
		t.Errorf("acme/search = %+v, %v; want the def's url and its unexpanded reference header", got, ok)
	}
	if _, ok := reg.Get("beta", "files"); !ok {
		t.Error("a tenant's override of a static name was not loaded")
	}
	for _, k := range [][2]string{{"acme", "draft"}, {"acme", "gone"}, {"", "files"}} {
		if _, ok := reg.Get(k[0], k[1]); ok {
			t.Errorf("%s/%s was loaded; boot skips it", k[0], k[1])
		}
	}

	// A second pass over an unchanged store makes nothing newly live.
	again, err := RehydrateMCPRegistry(context.Background(), s, static, reg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 || reg.Size() != 2 {
		t.Errorf("second pass activated %d (registry %d); want 0 (2)", again, reg.Size())
	}
}

// gatedMCPDefStore pauses the first armed read of one kind: it does the real
// read, reports it on entered, then waits for release before returning that
// (by then stale) result. That is the window in which a rehydrate has read a
// def but not yet Set it.
type gatedMCPDefStore struct {
	store.Store
	gateList bool // true: gate MCPServerDefListNames; false: gate the per-def read

	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func newGatedMCPDefStore(s store.Store, gateList bool) *gatedMCPDefStore {
	return &gatedMCPDefStore{Store: s, gateList: gateList, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedMCPDefStore) arm() { g.mu.Lock(); g.armed = true; g.mu.Unlock() }

// take reports whether this call is the one to pause, disarming the gate.
func (g *gatedMCPDefStore) take() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.armed {
		return false
	}
	g.armed = false
	return true
}

func (g *gatedMCPDefStore) pause() {
	close(g.entered)
	<-g.release
}

func (g *gatedMCPDefStore) MCPServerDefListNames(ctx context.Context) ([]store.MCPServerDefNameSummary, error) {
	out, err := g.Store.MCPServerDefListNames(ctx)
	if g.gateList && g.take() {
		g.pause()
	}
	return out, err
}

func (g *gatedMCPDefStore) MCPServerDefGet(ctx context.Context, defID string) (store.MCPServerDefRow, error) {
	row, err := g.Store.MCPServerDefGet(ctx, defID)
	if !g.gateList && g.take() {
		g.pause()
	}
	return row, err
}

func (g *gatedMCPDefStore) MCPServerDefGetActive(ctx context.Context, tenantID, name string) (store.MCPServerDefRow, error) {
	row, err := g.Store.MCPServerDefGetActive(ctx, tenantID, name)
	if !g.gateList && g.take() {
		g.pause()
	}
	return row, err
}

// parkedOnRegistryLock reports whether a goroutine running fn is blocked
// acquiring the registry's Serialize lock, or false once done closes first.
// It reads goroutine stacks rather than waiting a fixed time, so the ordering
// it proves does not depend on scheduling.
func parkedOnRegistryLock(t *testing.T, fn string, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, fn) && strings.Contains(g, "(*DynamicRegistry).Serialize") && strings.Contains(g, "sync.(*Mutex)") {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s neither finished nor parked on the registry lock within 10s", fn)
	return false
}

func TestRehydrate_ConcurrentRetireIsNotUndone(t *testing.T) {
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "rehydrate-retire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seedMCPDef(t, s, "mcp_v1", "", "search", "https://search.example.test/mcp", true, false)

	g := newGatedMCPDefStore(s, false)
	reg := loommcp.NewDynamicRegistry()
	tool := &MCPServerDef{Store: g, Cfg: &config.Config{}, Registry: reg}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin"})

	g.arm()
	rehydrated := make(chan error, 1)
	go func() {
		_, err := RehydrateMCPRegistry(context.Background(), g, nil, reg, t.Logf)
		rehydrated <- err
	}()
	<-g.entered // the rehydrate has read mcp_v1 as live and has not Set it

	retired := make(chan tools.Result, 1)
	retireDone := make(chan struct{})
	go func() {
		defer close(retireDone)
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"mcp_v1","retired":true}`))
		retired <- res
	}()
	// Either the retire waits for the rehydrate's read-and-Set to finish, or
	// it has already run to completion inside the rehydrate's window.
	_ = parkedOnRegistryLock(t, "execRetire", retireDone)

	close(g.release)
	if err := <-rehydrated; err != nil {
		t.Fatal(err)
	}
	if res := <-retired; res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if spec, ok := reg.Get("", "search"); ok {
		t.Fatalf("retired server is back in the registry (%s): a rehydrate Set it after the retire removed it", spec.URL)
	}
}

func TestRehydrate_ConcurrentPromoteKeepsTheNewVersion(t *testing.T) {
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "rehydrate-promote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seedMCPDef(t, s, "mcp_v1", "", "search", "https://v1.example.test/mcp", true, false)
	seedMCPDef(t, s, "mcp_v2", "", "search", "https://v2.example.test/mcp", false, false)

	g := newGatedMCPDefStore(s, true)
	reg := loommcp.NewDynamicRegistry()
	tool := &MCPServerDef{Store: g, Cfg: &config.Config{}, Registry: reg}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin"})

	g.arm()
	rehydrated := make(chan error, 1)
	go func() {
		_, err := RehydrateMCPRegistry(context.Background(), g, nil, reg, t.Logf)
		rehydrated <- err
	}()
	<-g.entered // the rehydrate has listed mcp_v1 as the active version

	// The rehydrate holds no lock while listing, so the promote completes.
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"promote","def_id":"mcp_v2"}`)); res.IsError {
		t.Fatalf("promote: %s", res.Text)
	}
	close(g.release)
	if err := <-rehydrated; err != nil {
		t.Fatal(err)
	}
	spec, ok := reg.Get("", "search")
	if !ok || spec.URL != "https://v2.example.test/mcp" {
		t.Fatalf("registry serves %q (present=%v); want the promoted v2 url — the rehydrate Set the version it listed before the promote", spec.URL, ok)
	}
}
