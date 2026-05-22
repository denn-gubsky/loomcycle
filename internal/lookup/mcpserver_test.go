package lookup_test

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

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
