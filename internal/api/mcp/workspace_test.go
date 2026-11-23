package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The description names every op the tool takes over MCP, and the schema
// offers exactly those ops (RFC GW D35).
func TestWorkspaceMCP_DescriptionListsEveryOp(t *testing.T) {
	d := workspaceToolDescriptor()
	for _, op := range builtin.WorkspaceMCPOps {
		if !strings.Contains(d.Description, op) {
			t.Errorf("the description does not name %q", op)
		}
	}
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.InputSchema, &s); err != nil {
		t.Fatal(err)
	}
	got := s.Properties["op"].Enum
	want := slices.Clone(builtin.WorkspaceMCPOps)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("op enum = %v, want %v", got, want)
	}
	// What a run alone does is not offered.
	for _, prop := range []string{"with", "access", "code", "kernel"} {
		if _, ok := s.Properties[prop]; ok {
			t.Errorf("the schema offers %q, an argument of an op only a run takes", prop)
		}
	}
}

// The tool is a tenant-confinable one that needs runs:create, as starting a
// run does: a member token with runs:read alone neither lists nor calls it.
func TestWorkspaceMCP_NeedsTheScopeThatCreatesARun(t *testing.T) {
	if !tenantConfinableTools["workspace"] {
		t.Error("workspace is not tenant-confinable")
	}
	if scope, gated := toolScope("workspace"); !gated || scope != "runs:create" {
		t.Errorf("toolScope = %q %v, want runs:create", scope, gated)
	}
}
