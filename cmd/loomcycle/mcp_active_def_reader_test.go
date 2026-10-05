package main

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// activeMCPStore serves MCPServerDefGetActive from rows keyed "tenant/name";
// every other store method is the nil embedded interface (unused here).
type activeMCPStore struct {
	store.Store
	rows map[string]store.MCPServerDefRow
}

func (s activeMCPStore) MCPServerDefGetActive(_ context.Context, tenantID, name string) (store.MCPServerDefRow, error) {
	if row, ok := s.rows[tenantID+"/"+name]; ok {
		return row, nil
	}
	return store.MCPServerDefRow{}, &store.ErrNotFound{Kind: "mcp_server_def_active", ID: name}
}

// A tenant that retires its own MCP server, with a shared server of the same
// name, is dialed at the shared one (retire took the tenant's out of the
// registry). Run start must advertise the shared server's tools, not the
// retired version's cached ones. Un-retiring brings the tenant's back.
func TestMCPActiveDefReader_RetiredTenantVersionAdvertisesSharedTools(t *testing.T) {
	def := func(tool string, retired bool) store.MCPServerDefRow {
		return store.MCPServerDefRow{Retired: retired, Definition: json.RawMessage(
			`{"transport":"http","url":"https://mcp.example","discovered_tools":[{"name":"` + tool + `","input_schema":{"type":"object"}}]}`)}
	}
	st := activeMCPStore{rows: map[string]store.MCPServerDefRow{
		"acme/srv": def("retired_tool", true),
		"/srv":     def("shared_tool", false),
	}}
	reg := mcp.NewDynamicRegistry()
	reg.Set(mcp.DynamicMCPServerSpec{Name: "srv", Transport: "http", URL: "https://mcp.example"})
	pool := mcp.NewPool(func(string, string) (mcp.Caller, error) { panic("no dial expected: tools are cached") }, nil, nil)

	advertised := func() []string {
		var names []string
		for _, tl := range mcp.DynamicToolsForRun(context.Background(), pool, reg, mcpActiveDefReader{st}, "acme", nil, time.Second, nil) {
			names = append(names, tl.Name())
		}
		sort.Strings(names)
		return names
	}

	if got := advertised(); len(got) != 1 || got[0] != "mcp__srv__shared_tool" {
		t.Errorf("tenant run advertises %v, want only the shared server's [mcp__srv__shared_tool]", got)
	}

	live := st.rows["acme/srv"]
	live.Retired = false
	st.rows["acme/srv"] = live
	if got := advertised(); len(got) != 1 || got[0] != "mcp__srv__retired_tool" {
		t.Errorf("after un-retire the tenant run advertises %v, want its own [mcp__srv__retired_tool]", got)
	}
}
