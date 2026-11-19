package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const workspaceYAMLHead = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  coder:
    model: claude-sonnet-4-6
    tools: [Read]
`

// RFC GW: workspace reaches the agent as declared; absent is nil.
func TestLoad_WorkspaceReachesTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        *AgentWorkspace
	}{
		{"absent", "", nil},
		{"read only", "    workspace: {ops: [read]}\n", &AgentWorkspace{Ops: []string{"read"}}},
		{"everything", "    workspace:\n      ops: [read, write, exec]\n      open: true\n      engine: container\n",
			&AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Engine: "container"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAMLString(t, workspaceYAMLHead+tc.field)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Agents["coder"].Workspace; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Workspace = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An unknown or repeated operation, an unknown engine, and any key a later
// phase adds (share, expose, egress, …) are refused at load, naming the
// agent and what is wrong.
func TestLoad_RefusesAnInvalidWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"unknown op", "    workspace: {ops: [read, run]}\n", []string{`"coder"`, "workspace.ops[1]", `"run"`}},
		{"duplicate op", "    workspace: {ops: [exec, exec]}\n", []string{`"coder"`, "workspace.ops[1]", "twice"}},
		{"unknown engine", "    workspace: {ops: [read], engine: vm}\n", []string{`"coder"`, "workspace.engine", `"vm"`}},
		{"share", "    workspace: {ops: [read], share: true}\n", []string{"workspace", `unknown key "share"`}},
		{"expose", "    workspace: {ops: [read], expose: [8080]}\n", []string{"workspace", `unknown key "expose"`}},
		{"egress", "    workspace: {ops: [read], egress: [pypi]}\n", []string{"workspace", `unknown key "egress"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAMLString(t, workspaceYAMLHead+tc.field)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q: %v", w, err)
				}
			}
		})
	}
}

func TestValidateAgentWorkspace_RefusesUnknownAndRepeatedValues(t *testing.T) {
	for _, tc := range []struct {
		in *AgentWorkspace
		ok bool
	}{
		{nil, true},
		{&AgentWorkspace{}, true},
		{&AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Engine: "host"}, true},
		{&AgentWorkspace{Engine: "container"}, true},
		{&AgentWorkspace{Ops: []string{"Read"}}, false},
		{&AgentWorkspace{Ops: []string{""}}, false},
		{&AgentWorkspace{Ops: []string{"write", "read", "write"}}, false},
		{&AgentWorkspace{Engine: "Container"}, false},
	} {
		if err := ValidateAgentWorkspace(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidateAgentWorkspace(%+v) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

// The JSON shape (the AgentDef overlay, register_agent) is as strict as the
// yaml one: a key a later phase adds is refused, not dropped.
func TestAgentWorkspace_JSONRefusesUnknownKeys(t *testing.T) {
	var w AgentWorkspace
	if err := json.Unmarshal([]byte(`{"ops":["read"],"open":true,"engine":"host"}`), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, AgentWorkspace{Ops: []string{"read"}, Open: true, Engine: "host"}) {
		t.Errorf("decoded %+v", w)
	}
	for _, in := range []string{`{"ops":["read"],"share":true}`, `{"connect":["x"]}`, `{"templates":[]}`} {
		var w AgentWorkspace
		if err := json.Unmarshal([]byte(in), &w); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: err = %v, want an unknown field refusal", in, err)
		}
	}
}

// The grant on an agent without the Workspace tool allows nothing: advised
// at boot, never refused. A block granting nothing is no grant.
func TestWorkspaceWarnings_AdviseAGrantWithoutTheTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  AgentDef
		want string // "" = no advisory
	}{
		{"absent", AgentDef{Tools: []string{"Read"}}, ""},
		{"grants nothing", AgentDef{Tools: []string{"Read"}, Workspace: &AgentWorkspace{Engine: "host"}}, ""},
		{"with the tool", AgentDef{Tools: []string{"Workspace"}, Workspace: &AgentWorkspace{Ops: []string{"read"}}}, ""},
		{"no tool", AgentDef{Tools: []string{"Read"}, Workspace: &AgentWorkspace{Open: true}}, "Workspace is not in tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(workspaceWarnings("a", tc.def), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("advisory = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Workspace tool held with a grant of nothing is reported inert.
func TestInertToolGrants_WorkspaceWithoutAGrantIsInert(t *testing.T) {
	inert := func(a AgentDef) bool {
		for _, g := range InertToolGrants(a) {
			if g.Tool == "Workspace" {
				return true
			}
		}
		return false
	}
	if !inert(AgentDef{Tools: []string{"Workspace"}}) {
		t.Error("Workspace with no grant was not reported inert")
	}
	if !inert(AgentDef{Tools: []string{"Workspace"}, Workspace: &AgentWorkspace{Engine: "container"}}) {
		t.Error("Workspace with a grant of nothing was not reported inert")
	}
	if inert(AgentDef{Tools: []string{"Workspace"}, Workspace: &AgentWorkspace{Ops: []string{"exec"}}}) {
		t.Error("Workspace with ops was reported inert")
	}
}

// A yaml entry that only grants workspace is an overlay on an existing agent,
// and a yaml overlay without the field keeps the discovered agent's.
func TestMergeAgentDef_WorkspaceOverlayReplacesOrKeeps(t *testing.T) {
	if !looksLikeAnOverrideOnly(AgentDef{Workspace: &AgentWorkspace{Ops: []string{"read"}}}) {
		t.Error("a workspace-only declaration was not recognised as an override")
	}
	base := AgentDef{Workspace: &AgentWorkspace{Ops: []string{"read", "exec"}, Open: true}}
	got := mergeAgentDef(base, AgentDef{Workspace: &AgentWorkspace{Ops: []string{"read"}}}).Workspace
	if !reflect.DeepEqual(got, &AgentWorkspace{Ops: []string{"read"}}) {
		t.Errorf("an overlay of {ops:[read]} gave %+v", got)
	}
	if got := mergeAgentDef(base, AgentDef{}).Workspace; !reflect.DeepEqual(got, base.Workspace) {
		t.Errorf("an overlay without the field gave %+v, want the base's", got)
	}
}
