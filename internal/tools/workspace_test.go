package tools

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC GW: a child holds the operations both it and its parent run hold, may
// open only when both may, and is held to the stricter engine requirement. A
// parent with no workspace access passes none on, whatever the child lists.
func TestRunPolicy_WorkspaceNarrowsForAChild(t *testing.T) {
	none := WorkspacePolicyValue{}
	all := WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true}
	read := WorkspacePolicyValue{Read: true}
	readHost := WorkspacePolicyValue{Read: true, Engine: "host"}
	allContainer := WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Engine: "container"}
	for _, c := range []struct {
		name        string
		parent, own WorkspacePolicyValue
		want        WorkspacePolicyValue
	}{
		{"neither grants", none, none, none},
		{"a parent with none passes none on", none, all, none},
		{"a child that lists none takes none", all, none, none},
		{"a read-only parent withholds exec and open", read, all, read},
		{"a read-only child gains nothing", all, read, read},
		{"the parent's engine requirement holds", allContainer, readHost, WorkspacePolicyValue{Read: true, Engine: "container"}},
		{"the child's engine requirement holds", readHost, allContainer, WorkspacePolicyValue{Read: true, Engine: "container"}},
		{"no shared op leaves nothing, engine and all", WorkspacePolicyValue{Exec: true, Engine: "host"}, read, none},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RunPolicy{Workspace: c.parent}.Inherit(ChildGrant{Own: RunPolicy{Workspace: c.own}}).Workspace
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// WorkspaceFor reads a definition's block: nil and a block granting nothing
// both grant nothing (engine included), unknown ops grant nothing, and Ops
// and Grant report the result.
func TestWorkspaceFor_ReadsTheBlockAndGrantRoundTrips(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      *config.AgentWorkspace
		want    WorkspacePolicyValue
		wantOps []string
	}{
		{"absent", nil, WorkspacePolicyValue{}, []string{}},
		{"engine only", &config.AgentWorkspace{Engine: "container"}, WorkspacePolicyValue{}, []string{}},
		{"unknown op", &config.AgentWorkspace{Ops: []string{"run"}}, WorkspacePolicyValue{}, []string{}},
		{"exec and read", &config.AgentWorkspace{Ops: []string{"exec", "read"}, Engine: "host"},
			WorkspacePolicyValue{Read: true, Exec: true, Engine: "host"}, []string{"read", "exec"}},
		{"open only", &config.AgentWorkspace{Open: true}, WorkspacePolicyValue{Open: true}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := WorkspaceFor(c.in)
			if got != c.want {
				t.Fatalf("WorkspaceFor(%+v) = %+v, want %+v", c.in, got, c.want)
			}
			if ops := got.Ops(); ops == nil || !reflect.DeepEqual(ops, c.wantOps) {
				t.Errorf("Ops() = %#v, want %#v", ops, c.wantOps)
			}
			if back := WorkspaceFor(got.Grant()); back != got {
				t.Errorf("WorkspaceFor(Grant()) = %+v, want %+v", back, got)
			}
			if got.Grant() == nil && got.Grants() {
				t.Error("Grant() is nil for a policy that grants something")
			}
			if got.Allows("read") != got.Read || got.Allows("exec") != got.Exec || got.Allows("open") {
				t.Errorf("Allows disagrees with %+v", got)
			}
		})
	}
}

// AcceptsEngine: a workspace's engine meets the requirement when it is at
// least as isolating; an unknown engine never does.
func TestWorkspacePolicyValue_AcceptsEngineAtLeastTheRequirement(t *testing.T) {
	for _, c := range []struct {
		req, engine string
		want        bool
	}{
		{"", "host", true}, {"", "container", true},
		{"host", "host", true}, {"host", "container", true},
		{"container", "host", false}, {"container", "container", true},
		{"", "vm", false}, {"", "", false},
	} {
		if got := (WorkspacePolicyValue{Exec: true, Engine: c.req}).AcceptsEngine(c.engine); got != c.want {
			t.Errorf("requirement %q, engine %q: got %v, want %v", c.req, c.engine, got, c.want)
		}
	}
}
