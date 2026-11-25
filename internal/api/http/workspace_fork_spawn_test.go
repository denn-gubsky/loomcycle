package http

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// resultsOf is the tool results of runID's transcript, in order.
func (f *wsRunFixture) resultsOf(t *testing.T, runID string) []string {
	t.Helper()
	evs, err := f.st.GetRunEventsSince(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type != "tool_result" {
			continue
		}
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

// runIDsOf is the ids of the runs of agent.
func (f *wsRunFixture) runIDsOf(t *testing.T, agent string) []string {
	t.Helper()
	runs, _, err := f.st.ListRunsByUser(context.Background(), "acme", "bob", nil, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range runs {
		if r.Agent == agent {
			out = append(out, r.ID)
		}
	}
	return out
}

// RFC GW §5.4 "For a fan-out": Agent op=spawn workspaces [{fork}] gives the
// child its own copy of the parent's workspace before its first turn. The
// child owns it (its own name, its own directory, its routes within its own
// grant), and what it changes leaves the parent's untouched. The fork is
// released when the child ends.
func TestWorkspaceRun_ASpawnForkIsTheChildsOwnCopy(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Egress: []string{"pypi", "npm"}}},
		"worker": {Model: "stub-model", SystemPrompt: "w", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Egress: []string{"pypi"}}},
	}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("p1", `{"op":"open","name":"work"}`),
		wsCall("p2", `{"op":"write","name":"work","path":"notes.txt","content":"from the builder"}`),
		wsCall("p3", `{"op":"exec","name":"work","command":"pwd -P"}`),
		agentCall("p4", `{"name":"worker","prompt":"try","workspaces":[{"fork":"work"}]}`),
		wsCall("c1", `{"op":"read","name":"work","path":"notes.txt"}`), // the child's first turn
		wsCall("c2", `{"op":"write","name":"work","path":"notes.txt","content":"the child's"}`),
		wsCall("c3", `{"op":"exec","name":"work","command":"pwd -P"}`),
		wsCall("c4", `{"op":"status","name":"work"}`),
		wsCall("c5", `{"op":"list"}`),
		wsDone, // child
		wsCall("p5", `{"op":"read","name":"work","path":"notes.txt"}`),
		wsDone, // parent
	}})
	if err := f.srv.RunOnce(context.Background(), f.input("parent"), f.callbacks()); err != nil {
		t.Fatal(err)
	}
	children := f.runIDsOf(t, "worker")
	if len(children) != 1 {
		t.Fatalf("child runs = %v", children)
	}
	got := f.resultsOf(t, children[0])
	if len(got) != 5 {
		t.Fatalf("child results = %q", got)
	}
	if !strings.Contains(got[0], "from the builder") {
		t.Errorf("the child's first read of its fork = %s", got[0])
	}
	if strings.Contains(got[1], `"error"`) {
		t.Errorf("the child's write into its own fork = %s", got[1])
	}
	if !strings.Contains(got[3], `"egress":["pypi"]`) {
		t.Errorf("the fork's routes = %s, want the parent's within the child's grant", got[3])
	}
	if !strings.Contains(got[4], `"name":"work"`) || strings.Contains(got[4], "@") {
		t.Errorf("the child's list = %s, want its own workspace", got[4])
	}
	var parentPwd string
	for _, r := range f.results() {
		switch {
		case r.ToolUse != nil && r.ToolUse.ID == "p3":
			parentPwd = pwdOf(t, r)
		case r.ToolUse != nil && r.ToolUse.ID == "p5" && !strings.Contains(r.Text, "from the builder"):
			t.Errorf("the parent's file after the child's write = %s", r.Text)
		}
	}
	var ea struct {
		Output string `json:"output"`
	}
	if json.Unmarshal([]byte(got[2]), &ea) != nil || strings.TrimSpace(ea.Output) == "" || strings.TrimSpace(ea.Output) == parentPwd {
		t.Errorf("the child's fork directory = %s, the parent's %q", got[2], parentPwd)
	}
	rows, err := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", children[0])
	if err != nil || len(rows) != 0 {
		t.Errorf("the child's workspaces after it ended = %+v, %v", rows, err)
	}
}

// A fork the spawn cannot make fails the child before its first turn: a
// name the parent does not hold, or a source below the child's engine
// floor. A parent without the grant to open workspaces is refused before
// any child run is made.
func TestWorkspaceRun_ASpawnForkThatCannotBeMadeStartsNoTurn(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Open: true, Share: true}},
		"noopen": {Model: "stub-model", SystemPrompt: "p", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Share: true}},
		"worker": {Model: "stub-model", SystemPrompt: "w", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}}},
		"strict": {Model: "stub-model", SystemPrompt: "s", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write", "exec"}, Engine: "container"}},
	}
	for _, tc := range []struct {
		why, spawn, child, want string
	}{
		{"a name the parent does not hold", `{"name":"worker","prompt":"x","workspaces":[{"fork":"missing"}]}`, "worker", "missing"},
		{"below the child's engine floor", `{"name":"strict","prompt":"x","workspaces":[{"fork":"work"}]}`, "strict", "engine"},
	} {
		f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
			wsCall("p1", `{"op":"open","name":"work"}`),
			agentCall("p2", tc.spawn),
			wsDone, // parent; the child takes no turn
		}})
		if err := f.srv.RunOnce(context.Background(), f.input("parent"), f.callbacks()); err != nil {
			t.Fatal(err)
		}
		res := f.results()
		if len(res) != 2 || !res[1].IsError || !strings.Contains(res[1].Text, tc.want) {
			t.Errorf("%s: the spawn's result = %+v", tc.why, res)
		}
		for _, id := range f.runIDsOf(t, tc.child) {
			if got := f.resultsOf(t, id); len(got) != 0 {
				t.Errorf("%s: the child ran a turn: %q", tc.why, got)
			}
		}
	}

	g := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("n1", `{"op":"open","name":"work"}`),
		agentCall("n2", `{"name":"worker","prompt":"x","workspaces":[{"fork":"work"}]}`),
		wsDone,
	}})
	if err := g.srv.RunOnce(context.Background(), g.input("noopen"), g.callbacks()); err != nil {
		t.Fatal(err)
	}
	res := g.results()
	if len(res) != 2 || !res[1].IsError || !strings.Contains(res[1].Text, "workspace.open") {
		t.Fatalf("a fork by a parent without open = %+v", res)
	}
	if ids := g.runIDsOf(t, "worker"); len(ids) != 0 {
		t.Errorf("a child run was made for a refused spawn: %v", ids)
	}
}

// fanOutProvider answers by what each request is, not by call order, so
// three children running at once each get their own script: a child writes
// its prompt into its fork, reads it back and lists its workspaces.
type fanOutProvider struct{}

func (p *fanOutProvider) ID() string                  { return "scripted" }
func (p *fanOutProvider) Probe(context.Context) error { return nil }
func (p *fanOutProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *fanOutProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *fanOutProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	var sys strings.Builder
	for _, b := range req.System {
		sys.WriteString(b.Text)
	}
	turn := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			turn++
		}
	}
	var evs []providers.Event
	if strings.Contains(sys.String(), "FANOUT-PARENT") {
		evs = map[int][]providers.Event{
			0: wsCall("p1", `{"op":"open","name":"work"}`),
			1: wsCall("p2", `{"op":"write","name":"work","path":"who.txt","content":"parent"}`),
			2: agentCall("p3", `{"op":"parallel_spawn","spawns":[`+
				`{"name":"worker","prompt":"child-0","workspaces":[{"fork":"work"}]},`+
				`{"name":"worker","prompt":"child-1","workspaces":[{"fork":"work"}]},`+
				`{"name":"worker","prompt":"child-2","workspaces":[{"fork":"work","as":"mine"}]}]}`),
			3: wsCall("p4", `{"op":"read","name":"work","path":"who.txt"}`),
		}[turn]
	} else {
		label, name := "", "work"
		for _, b := range req.Messages[0].Content {
			if i := strings.Index(b.Text, "child-"); i >= 0 && len(b.Text) >= i+7 {
				label = b.Text[i : i+7]
			}
		}
		if label == "child-2" {
			name = "mine"
		}
		evs = map[int][]providers.Event{
			0: wsCall("c1", fmt.Sprintf(`{"op":"write","name":%q,"path":"who.txt","content":%q}`, name, label)),
			1: wsCall("c2", fmt.Sprintf(`{"op":"read","name":%q,"path":"who.txt"}`, name)),
			2: wsCall("c3", `{"op":"list"}`),
		}[turn]
	}
	if evs == nil {
		evs = wsDone
	}
	ch := make(chan providers.Event, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// RFC GW §11 "Checkpoint and fork": a fan-out of three children gives three
// forks, each pinned to its child. Each child writes into its own fork and
// reads back only its own write, sees one workspace of its own and none of
// its siblings', and the parent's file is untouched.
func TestWorkspaceRun_AFanOutOfThreeGivesThreeForksEachPinnedToItsChild(t *testing.T) {
	agents := map[string]config.AgentDef{
		"parent": {Model: "stub-model", SystemPrompt: "FANOUT-PARENT", Tools: []string{"Agent", "Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write"}, Open: true}},
		"worker": {Model: "stub-model", SystemPrompt: "FANOUT-CHILD", Tools: []string{"Workspace"},
			Workspace: &config.AgentWorkspace{Ops: []string{"read", "write"}}},
	}
	f := newWSRunFixture(t, agents, &fanOutProvider{})
	if err := f.srv.RunOnce(context.Background(), f.input("parent"), f.callbacks()); err != nil {
		t.Fatal(err)
	}
	children := f.runIDsOf(t, "worker")
	if len(children) != 3 {
		t.Fatalf("child runs = %v", children)
	}
	seen := map[string]bool{}
	for _, id := range children {
		got := f.resultsOf(t, id)
		if len(got) != 3 {
			t.Fatalf("child %s results = %q", id, got)
		}
		label := got[1]
		if !strings.HasPrefix(label, "child-") || seen[label] {
			t.Errorf("child %s read back %q: want its own write, once per child", id, label)
		}
		seen[label] = true
		var list struct {
			Workspaces []struct {
				Name string `json:"name"`
			} `json:"workspaces"`
			SharedWithMe []json.RawMessage `json:"shared_with_me"`
		}
		if err := json.Unmarshal([]byte(got[2]), &list); err != nil || len(list.Workspaces) != 1 || len(list.SharedWithMe) != 0 {
			t.Errorf("child %s list = %s, want exactly its own fork", id, got[2])
		}
	}
	for _, r := range f.results() {
		if r.ToolUse != nil && r.ToolUse.ID == "p4" && r.Text != "parent" {
			t.Errorf("the parent's file after three forks = %q", r.Text)
		}
	}
}
