package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// childResults is the text of each tool result of the run of agent, in order.
func (f *wsRunFixture) childResults(t *testing.T, agent string) []string {
	t.Helper()
	runs, _, err := f.st.ListRunsByUser(context.Background(), "acme", "bob", nil, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	for _, r := range runs {
		if r.Agent == agent {
			runID = r.ID
		}
	}
	if runID == "" {
		t.Fatalf("no run of %s", agent)
	}
	evs, err := f.st.GetRunEventsSince(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type != "tool_result" {
			continue
		}
		// The stored event carries the result as its text field; read that,
		// so the JSON a tool answered is compared unescaped.
		var pe struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &pe); err != nil {
			t.Fatalf("tool_result payload %s: %v", ev.Payload, err)
		}
		out = append(out, pe.Text)
	}
	return out
}

// RFC GW §5.2 "At spawn": Agent op=spawn workspaces shares the parent's
// workspace with the child before its first turn. The child reads it by
// <name>@<parent agent id>, is refused a write through the read share and a
// command, and the parent's own copy is untouched.
func TestWorkspaceRun_SpawnTimeSharesReachTheChildBeforeItsFirstTurn(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Share: true}},
		"reviewer": {Model: "stub-model", SystemPrompt: "r", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}}},
	}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("p1", `{"op":"open","name":"work"}`),
		wsCall("p2", `{"op":"write","name":"work","path":"notes.txt","content":"from the builder"}`),
		agentCall("p3", `{"name":"reviewer","prompt":"review","workspaces":[{"name":"work","access":"read"}]}`),
		wsCall("c1", `{"op":"read","name":"work@ag_parent","path":"notes.txt"}`), // the child's first turn
		wsCall("c2", `{"op":"write","name":"work@ag_parent","path":"notes.txt","content":"changed"}`),
		wsCall("c3", `{"op":"exec","name":"work@ag_parent","command":"true"}`),
		wsCall("c4", `{"op":"list"}`),
		wsDone, // child
		wsCall("p4", `{"op":"read","name":"work","path":"notes.txt"}`),
		wsDone, // parent
	}})
	in := f.input("parent")
	in.AgentID = "ag_parent"
	if err := f.srv.RunOnce(context.Background(), in, f.callbacks()); err != nil {
		t.Fatal(err)
	}
	got := f.childResults(t, "reviewer")
	if len(got) != 4 {
		t.Fatalf("child results = %q", got)
	}
	if !strings.Contains(got[0], "from the builder") {
		t.Errorf("the child's first read = %s", got[0])
	}
	if !strings.Contains(got[1], "read_only_share") {
		t.Errorf("the child's write through a read share = %s", got[1])
	}
	if !strings.Contains(got[2], "not_shared_op") {
		t.Errorf("the child's command = %s", got[2])
	}
	if !strings.Contains(got[3], `"name":"work@ag_parent"`) || !strings.Contains(got[3], `"access":"read"`) {
		t.Errorf("the child's list = %s", got[3])
	}
	for _, r := range f.results() {
		if r.ToolUse != nil && r.ToolUse.ID == "p4" && !strings.Contains(r.Text, "from the builder") {
			t.Errorf("the parent's file = %s", r.Text)
		}
	}
}

// A spawn naming a workspace the parent does not hold fails the child
// before its first turn; one by a parent without share is refused before a
// child is made at all.
func TestWorkspaceRun_ASpawnShareThatCannotBeMadeStartsNoTurn(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true, Share: true}},
		"noshare": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read"}, Open: true}},
		"reviewer": {Model: "stub-model", SystemPrompt: "r", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read"}}},
	}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		agentCall("p1", `{"name":"reviewer","prompt":"review","workspaces":[{"name":"missing","access":"read"}]}`),
		wsDone, // parent; the child takes no turn, so no script is spent on it
	}})
	if err := f.srv.RunOnce(context.Background(), f.input("parent"), f.callbacks()); err != nil {
		t.Fatal(err)
	}
	res := f.results()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text, "missing") {
		t.Fatalf("parent's spawn result = %+v", res)
	}
	if got := f.childResults(t, "reviewer"); len(got) != 0 {
		t.Errorf("the child ran a turn: %q", got)
	}

	g := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("n1", `{"op":"open","name":"work"}`),
		agentCall("n2", `{"name":"reviewer","prompt":"review","workspaces":[{"name":"work","access":"read"}]}`),
		wsDone,
	}})
	if err := g.srv.RunOnce(context.Background(), g.input("noshare"), g.callbacks()); err != nil {
		t.Fatal(err)
	}
	res = g.results()
	if len(res) != 2 || !res[1].IsError || !strings.Contains(res[1].Text, "workspace.share") {
		t.Fatalf("spawn by a parent without share = %+v", res)
	}
	runs, _, _ := g.st.ListRunsByUser(context.Background(), "acme", "bob", nil, 10, "")
	for _, r := range runs {
		if r.Agent == "reviewer" {
			t.Errorf("a child run was made for a refused spawn: %s", r.ID)
		}
	}
}
