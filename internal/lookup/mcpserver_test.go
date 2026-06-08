package lookup_test

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// fakeDynReg is a lookup.MCPDynamicRegistry stub for the resolver test.
// RFC N: keyed by (tenant, name); the "" tenant is the shared registry.
type fakeDynReg map[[2]string]lookup.MCPServerSpec

func (f fakeDynReg) Get(tenantID, name string) (lookup.MCPServerSpec, bool) {
	s, ok := f[[2]string{tenantID, name}]
	return s, ok
}

// TestMCPServer_StaticDynamicPrecedence pins the lookup chain MCP now shares
// with every other primitive: static yaml → dynamic substrate, yaml-wins on
// collision, AllowedTools carried only for the static source (dynamic =
// allow-all). This is the resolver-level guard the MCP lazy resolver lacked
// before #341 — it had duplicated the membership check static-only. The lazy
// resolver now routes through this function, so this test covers both.
func TestMCPServer_StaticDynamicPrecedence(t *testing.T) {
	cfg := &config.Config{MCPServers: map[string]config.MCPServer{
		"static-srv": {Transport: "http", URL: "http://s/mcp", AllowedTools: []string{"a", "b"}},
		"both":       {Transport: "http", URL: "http://yaml/mcp"},
	}}
	dyn := fakeDynReg{
		{"", "dyn-srv"}: {Transport: "http", URL: "http://d/mcp"},
		{"", "both"}:    {Transport: "http", URL: "http://substrate/mcp"},
	}

	// Default "" tenant: order collapses to static → shared-dynamic, exactly
	// as pre-RFC-N.
	if spec, ok := lookup.MCPServer(cfg, dyn, "", "static-srv"); !ok || spec.Source != "static" || !reflect.DeepEqual(spec.AllowedTools, []string{"a", "b"}) {
		t.Errorf("static: got (%+v, %v), want source=static + allowed [a b]", spec, ok)
	}
	if spec, ok := lookup.MCPServer(cfg, dyn, "", "dyn-srv"); !ok || spec.Source != "dynamic" || len(spec.AllowedTools) != 0 {
		t.Errorf("dynamic: got (%+v, %v), want source=dynamic + no allowed_tools (allow-all)", spec, ok)
	}
	if spec, ok := lookup.MCPServer(cfg, dyn, "", "both"); !ok || spec.Source != "static" || spec.URL != "http://yaml/mcp" {
		t.Errorf("collision: got (%+v, %v), want the static (yaml) entry to win", spec, ok)
	}
	if _, ok := lookup.MCPServer(cfg, dyn, "", "nope"); ok {
		t.Error("unknown name must return ok=false")
	}
	if _, ok := lookup.MCPServer(nil, nil, "", "x"); ok {
		t.Error("nil cfg + nil dyn must return ok=false")
	}
}

// TestMCPServer_TenantResolutionPrecedence pins the RFC N tenant axis:
//  1. a per-tenant dynamic registration shadows the shared static base;
//  2. otherwise the static shared base resolves;
//  3. otherwise a shared ("") dynamic registration resolves;
//  4. the "" tenant preserves static-first (no tenant shadow pass).
func TestMCPServer_TenantResolutionPrecedence(t *testing.T) {
	cfg := &config.Config{MCPServers: map[string]config.MCPServer{
		"shared-srv": {Transport: "http", URL: "http://static/mcp"},
	}}
	dyn := fakeDynReg{
		// tenant-a overrides the shared static "shared-srv" by name.
		{"tenant-a", "shared-srv"}: {Transport: "http", URL: "http://a-override/mcp"},
		// a shared-dynamic-only name (no static entry).
		{"", "shared-dyn"}: {Transport: "http", URL: "http://shared-dyn/mcp"},
	}

	// 1. tenant-a's override shadows the static base.
	if spec, ok := lookup.MCPServer(cfg, dyn, "tenant-a", "shared-srv"); !ok || spec.URL != "http://a-override/mcp" {
		t.Errorf("tenant override: got (%+v, %v), want a-override URL", spec, ok)
	}
	// A different tenant with no override falls through to the static base.
	if spec, ok := lookup.MCPServer(cfg, dyn, "tenant-b", "shared-srv"); !ok || spec.Source != "static" || spec.URL != "http://static/mcp" {
		t.Errorf("tenant-b base: got (%+v, %v), want the static shared base", spec, ok)
	}
	// 3. shared-dynamic-only name resolves for any tenant.
	if spec, ok := lookup.MCPServer(cfg, dyn, "tenant-a", "shared-dyn"); !ok || spec.URL != "http://shared-dyn/mcp" {
		t.Errorf("shared dynamic: got (%+v, %v), want shared-dyn URL", spec, ok)
	}
	// 4. "" tenant never sees tenant-a's override — resolves the static base.
	if spec, ok := lookup.MCPServer(cfg, dyn, "", "shared-srv"); !ok || spec.Source != "static" || spec.URL != "http://static/mcp" {
		t.Errorf("default tenant: got (%+v, %v), want the static base (no tenant shadow)", spec, ok)
	}
	// tenant-a's override is invisible to the "" tenant entirely (the name
	// only exists in static for "").
	if _, ok := lookup.MCPServer(cfg, dyn, "tenant-c", "a-only-nonexistent"); ok {
		t.Error("nonexistent name must return ok=false")
	}
}

// TestMCPServer_DriftDetection pins the substrate-shape invariant for
// MCPServerDef. SubstrateMCPServer and the persistence shape in
// internal/tools/builtin/mcpserverdef.go (`mcpServerOverlay`) must
// stay in lockstep — same field set, same json tags.
//
// Symmetric today (marshal + unmarshal both use json-tagged structs),
// so the AgentDef-style "yaml-only consumer" bug PR #184 fixed CAN'T
// fire here. Forward-looking test for future refactors that might
// introduce an asymmetric intermediate.
func TestMCPServer_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"transport":        true,
		"url":              true,
		"headers":          true,
		"command":          true, // stdio (F31)
		"args":             true, // stdio (F31)
		"env":              true, // stdio (F31)
		"description":      true,
		"discovered_tools": true,
	}
	have := jsonTagsOf(reflect.TypeOf(lookup.SubstrateMCPServer{}))
	for tag := range want {
		if !have[tag] {
			t.Errorf("SubstrateMCPServer missing json tag %q (must mirror mcpServerOverlay in internal/tools/builtin/mcpserverdef.go)", tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("SubstrateMCPServer has json tag %q not in expected set — if added deliberately, update the `want` map", tag)
		}
	}
}

// TestSubstrateMCPServerTool_DriftDetection covers the nested
// discovered-tools shape. Same posture as the parent struct.
func TestSubstrateMCPServerTool_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"name":         true,
		"description":  true,
		"input_schema": true,
	}
	have := jsonTagsOf(reflect.TypeOf(lookup.SubstrateMCPServerTool{}))
	for tag := range want {
		if !have[tag] {
			t.Errorf("SubstrateMCPServerTool missing json tag %q", tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("SubstrateMCPServerTool has json tag %q not in expected set", tag)
		}
	}
}
