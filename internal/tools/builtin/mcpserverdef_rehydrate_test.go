package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
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
