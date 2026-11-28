package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// RFC HH §4: workspace.toolchains reaches the agent from yaml and JSON, and
// a malformed, repeated or over-long list is refused.
func TestAgentWorkspace_ToolchainsAreReadAndChecked(t *testing.T) {
	cfg, err := loadYAMLString(t, workspaceYAMLHead+"    workspace:\n      open: true\n      toolchains: [cpp, cmake]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["coder"].Workspace; !reflect.DeepEqual(got, &AgentWorkspace{Open: true, Toolchains: []string{"cpp", "cmake"}}) {
		t.Errorf("yaml: Workspace = %+v", got)
	}

	var w AgentWorkspace
	if err := json.Unmarshal([]byte(`{"open":true,"toolchains":["go"]}`), &w); err != nil || !reflect.DeepEqual(w.Toolchains, []string{"go"}) {
		t.Fatalf("json: %+v, %v", w, err)
	}

	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	for _, tc := range []struct {
		list []string
		want string
	}{
		{[]string{"cpp", "Bad Name"}, `workspace.toolchains[1]: "Bad Name"`},
		{[]string{"go", "go"}, `workspace.toolchains[1]: "go" is listed twice`},
		{many, "workspace.toolchains: at most 32"},
	} {
		err := ValidateAgentWorkspace(&AgentWorkspace{Open: true, Toolchains: tc.list})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.list, err, tc.want)
		}
	}
	if err := ValidateAgentWorkspace(&AgentWorkspace{Open: true, Toolchains: []string{"cpp", "c++", "go1.26"}}); err != nil {
		t.Errorf("well-formed names refused: %v", err)
	}
}

// Clone never aliases the list, so narrowing a copy cannot reach the
// definition it came from.
func TestAgentWorkspace_CloneCopiesToolchains(t *testing.T) {
	w := &AgentWorkspace{Open: true, Toolchains: []string{"cpp"}}
	c := w.Clone()
	c.Toolchains[0] = "go"
	if w.Toolchains[0] != "cpp" {
		t.Error("Clone aliased the toolchains list")
	}
}
