package config

import (
	"github.com/denn-gubsky/loomcycle/internal/agents"

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
		{"everything", "    workspace:\n      ops: [read, write, exec]\n      open: true\n      share: true\n      templates: true\n      egress: [pypi, pypi-files]\n      engine: container\n",
			&AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Share: true, Templates: true, Egress: []string{"pypi", "pypi-files"}, Engine: "container"}},
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

// An unknown or repeated operation, an unknown engine, an egress entry that
// is no route name or is repeated, a non-flag expose, and any key a later
// phase adds (connect, templates) are refused at load, naming the agent and what is wrong.
func TestLoad_RefusesAnInvalidWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"unknown op", "    workspace: {ops: [read, run]}\n", []string{`"coder"`, "workspace.ops[1]", `"run"`}},
		{"duplicate op", "    workspace: {ops: [exec, exec]}\n", []string{`"coder"`, "workspace.ops[1]", "twice"}},
		{"unknown engine", "    workspace: {ops: [read], engine: vm}\n", []string{`"coder"`, "workspace.engine", `"vm"`}},
		{"unknown key", "    workspace: {ops: [read], listen: true}\n", []string{"workspace", `unknown key "listen"`}},
		{"connect not a flag", "    workspace: {ops: [read], connect: [8080]}\n", []string{"cannot unmarshal", "bool"}},
		{"expose not a flag", "    workspace: {ops: [read], expose: [8080]}\n", []string{"cannot unmarshal", "bool"}},
		{"egress not a route name", "    workspace: {ops: [read], egress: [pypi, PyPI]}\n", []string{`"coder"`, "workspace.egress[1]", `"PyPI" is not a route name`}},
		{"egress repeated", "    workspace: {ops: [read], egress: [npm, npm]}\n", []string{`"coder"`, "workspace.egress[1]", "twice"}},
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
		{&AgentWorkspace{Ops: []string{"read"}, Egress: []string{"pypi", "crm-2"}}, true},
		{&AgentWorkspace{Egress: []string{""}}, false},
		{&AgentWorkspace{Egress: []string{"py_pi"}}, false},
		{&AgentWorkspace{Egress: []string{"a", "a"}}, false},
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
	if err := json.Unmarshal([]byte(`{"ops":["read"],"open":true,"share":true,"connect":true,"durable":true,"templates":true,"egress":["npm"],"engine":"host"}`), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, AgentWorkspace{Ops: []string{"read"}, Open: true, Share: true, Connect: true, Durable: true, Templates: true, Egress: []string{"npm"}, Engine: "host"}) {
		t.Errorf("decoded %+v", w)
	}
	for _, in := range []string{`{"ops":["read"],"listen":true}`, `{"template":true}`} {
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
			got := strings.Join(workspaceWarnings("a", tc.def, WorkspaceModeRemote), "\n")
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

// A Markdown agent's workspace reaches config through a field-by-field copy
// (workspaceFromDiscovered): every key the mirror holds must arrive, or a
// key added to both (as share was, RFC GW P8) is silently dropped.
func TestWorkspaceFromDiscovered_CarriesEveryKey(t *testing.T) {
	mirror := reflect.TypeOf(agents.AgentWorkspace{})
	own := reflect.TypeOf(AgentWorkspace{})
	if mirror.NumField() != own.NumField() {
		t.Fatalf("agents.AgentWorkspace has %d fields, config.AgentWorkspace %d: add the new key to both and to workspaceFromDiscovered", mirror.NumField(), own.NumField())
	}
	in := &agents.AgentWorkspace{Ops: []string{"read", "write"}, Open: true, Share: true, Expose: true, Connect: true, Durable: true, Templates: true, Egress: []string{"pypi"}, Engine: "container"}
	got := workspaceFromDiscovered(in)
	want := &AgentWorkspace{Ops: []string{"read", "write"}, Open: true, Share: true, Expose: true, Connect: true, Durable: true, Templates: true, Egress: []string{"pypi"}, Engine: "container"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("workspaceFromDiscovered = %+v, want %+v", got, want)
	}
	// Every field of the full value is set, so a field the copy skips shows.
	v := reflect.ValueOf(*want)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("test value leaves %s unset; set it so the copy is checked", own.Field(i).Name)
		}
	}
}

// A route name no operator route has, while tenants may write none, can
// reach nothing: advised at load, never refused. With tenant routes on, a
// tenant may write it, so nothing is said.
func TestWorkspaceEgressWarnings_AdviseANameNoRouteCanHave(t *testing.T) {
	head := "workspace:\n  egress:\n    routes:\n      pypi: {url: \"https://pypi.org/\"}\n"
	agent := "agents:\n  data:\n    model: claude-sonnet-4-6\n    tools: [Workspace]\n    workspace: {ops: [read], open: true, egress: [pypi, crm]}\n"
	for _, tc := range []struct {
		name, block string
		want        bool
	}{
		{"tenant routes off", head, true},
		{"tenant routes on", head + "    tenant_routes: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConversion(t, tc.block+agent)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range cfg.Warnings {
				if strings.Contains(w, "workspace.egress names") {
					got = append(got, w)
				}
			}
			if tc.want && (len(got) != 1 || !strings.Contains(got[0], `"crm"`) || !strings.Contains(got[0], `agent "data"`)) {
				t.Errorf("advisories = %q, want one naming crm on agent data", got)
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("advisories = %q, want none", got)
			}
		})
	}
}
