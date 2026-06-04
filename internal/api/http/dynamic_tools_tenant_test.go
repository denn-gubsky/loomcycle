package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// tenantAdvertisingEnumerator replicates the RFC N §3 advertising filter
// wired in cmd/loomcycle/main.go's SetDynamicToolEnumerator: enumerate ONLY
// the names visible to the run's tenant (NamesForTenant = own + shared) and
// read each name's active def with the tenant→shared precedence. Replicated
// here (not imported) because the production closure lives in package main;
// the logic under test — NamesForTenant + tenantFromCtx + the per-name
// tenant-scoped GetActive — is the package-level surface this exercises.
func tenantAdvertisingEnumerator(reg *loommcp.DynamicRegistry, st store.Store) func(context.Context) []tools.Tool {
	return func(ctx context.Context) []tools.Tool {
		var out []tools.Tool
		tenant := tenantFromCtx(ctx)
		for _, name := range reg.NamesForTenant(tenant) {
			row, gerr := st.MCPServerDefGetActive(ctx, tenant, name)
			if gerr != nil && tenant != "" {
				row, gerr = st.MCPServerDefGetActive(ctx, "", name)
			}
			if gerr != nil {
				continue
			}
			var def lookup.SubstrateMCPServer
			if json.Unmarshal(row.Definition, &def) != nil {
				continue
			}
			for _, dt := range def.DiscoveredTools {
				out = append(out, namedTool{"mcp__" + name + "__" + dt.Name})
			}
		}
		return out
	}
}

// registerActiveMCPServer creates + promotes an active mcp_server_def for
// (tenant, name) with one discovered tool, then mirrors it into the
// in-memory registry — exactly what the substrate tool's create/promote
// does, so the advertising enumerator can resolve it.
func registerActiveMCPServer(t *testing.T, st store.Store, reg *loommcp.DynamicRegistry, tenant, name, toolName string) {
	t.Helper()
	ctx := context.Background()
	def := map[string]any{
		"transport": "http",
		"url":       "https://" + name + ".example/mcp",
		"discovered_tools": []map[string]any{
			{"name": toolName, "description": "d"},
		},
	}
	body, _ := json.Marshal(def)
	row, err := st.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID:      "mdef_" + tenant + "_" + name,
		Name:       name,
		Definition: body,
		CreatedAt:  time.Now(),
		TenantID:   tenant,
	})
	if err != nil {
		t.Fatalf("create %s/%s: %v", tenant, name, err)
	}
	if err := st.MCPServerDefSetActive(ctx, tenant, name, row.DefID, ""); err != nil {
		t.Fatalf("promote %s/%s: %v", tenant, name, err)
	}
	reg.Set(loommcp.DynamicMCPServerSpec{TenantID: tenant, Name: name, Transport: "http", URL: "https://" + name + ".example/mcp"})
}

// TestDynamicTools_TenantScopedAdvertising is the RFC N §3 regression guard:
// an MCP server registered under tenant B must NOT appear in a run's
// candidate tool set when the run's authoritative principal is tenant A, and
// MUST appear for tenant B. A shared ("") server is visible to both. This is
// the boundary that keeps tenant A from ever seeing — let alone dialing —
// tenant B's MCP tools.
func TestDynamicTools_TenantScopedAdvertising(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := loommcp.NewDynamicRegistry()

	// Same server NAME under two tenants with distinct tools, plus a shared
	// one, plus a tenant-B-ONLY name (no A counterpart) — the latter is the
	// case the NamesForTenant filter must exclude from A's set. A naive
	// all-pairs Names() enumeration would leak "secret" into A's candidates.
	registerActiveMCPServer(t, st, reg, "tenant-a", "crm", "a_tool")
	registerActiveMCPServer(t, st, reg, "tenant-b", "crm", "b_tool")
	registerActiveMCPServer(t, st, reg, "tenant-b", "secret", "b_secret_tool")
	registerActiveMCPServer(t, st, reg, "", "billing", "shared_tool")

	// Registry-level §3 boundary: tenant A's candidate NAME set must not
	// contain B's exclusive "secret" server. This guards against a
	// regression that swaps NamesForTenant back to the all-pairs Names()
	// (the leak the per-tenant enumeration closes at the name layer, before
	// the per-name tenant-scoped GetActive provides defence-in-depth).
	for _, n := range reg.NamesForTenant("tenant-a") {
		if n == "secret" {
			t.Fatal("NamesForTenant(tenant-a) leaked tenant-b's exclusive 'secret' server")
		}
	}

	srv := New(&config.Config{}, &stubResolver{}, nil, concurrency.New(4, 4, time.Second), nil)
	srv.SetDynamicToolEnumerator(tenantAdvertisingEnumerator(reg, st))

	ctxA := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "tenant-a", Subject: "alice"})
	ctxB := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "tenant-b", Subject: "bob"})

	namesA := toolNames(srv.candidateTools(ctxA))
	namesB := toolNames(srv.candidateTools(ctxB))

	// Tenant A sees its OWN crm tool + the shared billing tool, never B's
	// same-name override and never B's exclusive "secret" server.
	assertHas(t, "tenant-a", namesA, "mcp__crm__a_tool", true)
	assertHas(t, "tenant-a", namesA, "mcp__billing__shared_tool", true)
	assertHas(t, "tenant-a", namesA, "mcp__crm__b_tool", false)
	assertHas(t, "tenant-a", namesA, "mcp__secret__b_secret_tool", false)

	// Tenant B sees its OWN crm tool + the shared billing tool, never A's.
	assertHas(t, "tenant-b", namesB, "mcp__crm__b_tool", true)
	assertHas(t, "tenant-b", namesB, "mcp__billing__shared_tool", true)
	assertHas(t, "tenant-b", namesB, "mcp__crm__a_tool", false)
}

func assertHas(t *testing.T, who string, names []string, want string, shouldHave bool) {
	t.Helper()
	has := false
	for _, n := range names {
		if n == want {
			has = true
			break
		}
	}
	if has != shouldHave {
		t.Errorf("%s candidate set: has(%q)=%v, want %v (set=%v)", who, want, has, shouldHave, names)
	}
}
