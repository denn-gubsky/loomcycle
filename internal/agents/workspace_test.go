package agents

import (
	"reflect"
	"strings"
	"testing"
)

// RFC GW: a Markdown agent's frontmatter carries workspace to the agent.
func TestParseAgent_WorkspaceReachesTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name, lines string
		want        *AgentWorkspace
	}{
		{"absent", "", nil},
		{"full", "workspace:\n  ops: [read, exec]\n  open: true\n  engine: host\n",
			&AgentWorkspace{Ops: []string{"read", "exec"}, Open: true, Engine: "host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := parseAgent([]byte("---\nname: coder\ndescription: d\n" + tc.lines + "---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a.Workspace, tc.want) {
				t.Errorf("Workspace = %+v, want %+v", a.Workspace, tc.want)
			}
		})
	}
}

// A key a later phase adds is refused in the frontmatter too, rather than
// dropped on the way to config.
func TestParseAgent_WorkspaceRefusesAnUnknownKey(t *testing.T) {
	_, err := parseAgent([]byte("---\nname: coder\ndescription: d\nworkspace:\n  ops: [read]\n  share: true\n---\nbody\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "share"`) {
		t.Fatalf("err = %v, want an unknown key refusal", err)
	}
}

// workspace is authority, not authored behaviour: two agents differing only
// in it hash identically on the yaml path and the overlay path.
func TestSign_WorkspaceExcludedFromHash(t *testing.T) {
	bare := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p"})
	with := FromYAMLAgent(&Agent{Name: "x", SystemPrompt: "p", Workspace: &AgentWorkspace{Ops: []string{"exec"}, Open: true}})
	if Sign(bare) != Sign(with) {
		t.Error("workspace leaked into the yaml-path hash")
	}
	a, err := FromOverlay([]byte(`{"system_prompt":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromOverlay([]byte(`{"system_prompt":"p","workspace":{"ops":["exec"],"open":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if Sign(a) != Sign(b) {
		t.Error("workspace leaked into the overlay-path hash")
	}
}
