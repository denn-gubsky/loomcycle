package lookup_test

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// fakeDynReg is a lookup.MCPDynamicRegistry stub for the resolver test.
type fakeDynReg map[string]lookup.MCPServerSpec

func (f fakeDynReg) Get(name string) (lookup.MCPServerSpec, bool) {
	s, ok := f[name]
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
		"dyn-srv": {Transport: "http", URL: "http://d/mcp"},
		"both":    {Transport: "http", URL: "http://substrate/mcp"},
	}

	if spec, ok := lookup.MCPServer(cfg, dyn, "static-srv"); !ok || spec.Source != "static" || !reflect.DeepEqual(spec.AllowedTools, []string{"a", "b"}) {
		t.Errorf("static: got (%+v, %v), want source=static + allowed [a b]", spec, ok)
	}
	if spec, ok := lookup.MCPServer(cfg, dyn, "dyn-srv"); !ok || spec.Source != "dynamic" || len(spec.AllowedTools) != 0 {
		t.Errorf("dynamic: got (%+v, %v), want source=dynamic + no allowed_tools (allow-all)", spec, ok)
	}
	if spec, ok := lookup.MCPServer(cfg, dyn, "both"); !ok || spec.Source != "static" || spec.URL != "http://yaml/mcp" {
		t.Errorf("collision: got (%+v, %v), want the static (yaml) entry to win", spec, ok)
	}
	if _, ok := lookup.MCPServer(cfg, dyn, "nope"); ok {
		t.Error("unknown name must return ok=false")
	}
	if _, ok := lookup.MCPServer(nil, nil, "x"); ok {
		t.Error("nil cfg + nil dyn must return ok=false")
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
