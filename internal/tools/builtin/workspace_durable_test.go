package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §4.2, P12 plan §3.2 (PR 3): a durable workspace is kept under the
// tenant, the user and a name past its run; a later run of the same user
// opens it again with its files; no two runs hold it at once; nobody else
// reaches it.

var wsKeeper = tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Share: true, Durable: true}

// durableFixture is a workspace fixture with workspace.durable on.
func durableFixture(t *testing.T, maxPerUser int) *wsFixture {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	f.tool.Manager.SetDurable(lifecycle.DurableLimits{Enabled: true, MaxPerUser: maxPerUser})
	return f
}

// runAsAgent is a run of user, in tenant, carrying agentID, granted pol.
func (f *wsFixture) runAsAgent(t *testing.T, tenant, user, agentID string, pol tools.WorkspacePolicyValue) context.Context {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), f.sess, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: user, AgentID: agentID, TenantID: tenant, RootRunID: r.ID})
	ctx = tools.WithRunID(ctx, r.ID)
	return tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: pol})
}

func (f *wsFixture) endRun(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := f.st.FinishRun(context.Background(), tools.RunID(ctx), store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.tool.Manager.ReleaseRun(context.Background(), tools.RunID(ctx)); err != nil {
		t.Fatal(err)
	}
}

const openDurable = `{"op":"open","name":"work","lifetime":"durable"}`

func readNotes(t *testing.T, f *wsFixture, ctx context.Context) string {
	t.Helper()
	return f.mustOK(t, ctx, `{"op":"read","name":"work","path":"notes.txt"}`).Text
}

// The operator's switch and the agent's grant are both needed; off by
// default.
func TestDurable_NeedsTheGrantAndTheSwitch(t *testing.T) {
	off := newWSFixture(t, lifecycle.Limits{})
	if c := refusalClass(off.call(t, off.runAsAgent(t, "acme", "alice", "a1", wsKeeper), openDurable)); c != "durable_off" {
		t.Errorf("with workspace.durable off = %q, want durable_off", c)
	}
	f := durableFixture(t, 8)
	if c := refusalClass(f.call(t, f.runAsAgent(t, "acme", "alice", "a2", wsFull), openDurable)); c != "not_granted" {
		t.Errorf("without the durable grant = %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, f.runAsAgent(t, "acme", "alice", "a3", wsKeeper), `{"op":"open","name":"work","lifetime":"forever"}`)); c != "invalid_argument" {
		t.Errorf("an unknown lifetime = %q, want invalid_argument", c)
	}
}

// What one run wrote, a later run of the same user reads; the first run's
// release parks it, and it reaches it no more.
func TestDurable_ALaterRunOfTheUserOpensItWithItsFiles(t *testing.T) {
	f := durableFixture(t, 8)
	first := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, first, openDurable)
	f.mustOK(t, first, `{"op":"write","name":"work","path":"notes.txt","content":"kept"}`)
	if res := f.mustOK(t, first, `{"op":"release","name":"work"}`); !strings.Contains(res.Text, `"released":true`) {
		t.Fatalf("release = %s", res.Text)
	}
	if c := refusalClass(f.call(t, first, `{"op":"read","name":"work","path":"notes.txt"}`)); c != "not_found" {
		t.Errorf("the first run after its release = %q, want not_found", c)
	}
	second := f.runAsAgent(t, "acme", "alice", "a2", wsKeeper)
	f.mustOK(t, second, openDurable)
	if got := readNotes(t, f, second); !strings.Contains(got, "kept") {
		t.Errorf("the second run read %s", got)
	}
	// An open without lifetime makes a workspace of its own, never the
	// durable one (D-P12-3).
	third := f.runAsAgent(t, "acme", "alice", "a3", wsKeeper)
	f.mustOK(t, third, `{"op":"open","name":"work"}`)
	if c := refusalClass(f.call(t, third, `{"op":"read","name":"work","path":"notes.txt"}`)); c == "" {
		t.Error("an ephemeral open of the name reached the durable workspace's file")
	}
}

// Another user of the tenant opens a workspace of their own under the same
// name, never this one, and lists nothing of it; another tenant reaches
// nothing.
func TestDurable_AnotherUserOrTenantCannotOpenOrSeeIt(t *testing.T) {
	f := durableFixture(t, 8)
	alice := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, alice, openDurable)
	f.mustOK(t, alice, `{"op":"write","name":"work","path":"notes.txt","content":"alice's"}`)
	f.endRun(t, alice)

	bob := f.runAsAgent(t, "acme", "bob", "b1", wsKeeper)
	if res := f.mustOK(t, bob, `{"op":"list"}`); strings.Contains(res.Text, `"name":"work"`) {
		t.Errorf("bob's list = %s", res.Text)
	}
	f.mustOK(t, bob, openDurable)
	if c := refusalClass(f.call(t, bob, `{"op":"read","name":"work","path":"notes.txt"}`)); c == "" {
		t.Error("bob read alice's durable workspace")
	}
	beta := f.runAsAgent(t, "beta", "alice", "x1", wsKeeper)
	if res := f.call(t, beta, openDurable); !res.IsError {
		if got := f.call(t, beta, `{"op":"read","name":"work","path":"notes.txt"}`); !got.IsError {
			t.Errorf("a run of another tenant read %s", got.Text)
		}
	}
	// Alice's is still hers.
	again := f.runAsAgent(t, "acme", "alice", "a2", wsKeeper)
	f.mustOK(t, again, openDurable)
	if got := readNotes(t, f, again); !strings.Contains(got, "alice's") {
		t.Errorf("alice's next run read %s", got)
	}
}

// Two live runs never hold it at once: the second is refused, naming the
// holder's agent id. Once the holder ends, the next run takes it.
func TestDurable_TwoRunsNeverHoldItAtOnce(t *testing.T) {
	f := durableFixture(t, 8)
	holder := f.runAsAgent(t, "acme", "alice", "a_holder", wsKeeper)
	f.mustOK(t, holder, openDurable)
	other := f.runAsAgent(t, "acme", "alice", "a_other", wsKeeper)
	res := f.call(t, other, openDurable)
	if c := refusalClass(res); c != "held" || !strings.Contains(res.Text, "a_holder") {
		t.Fatalf("a second run = %q %s, want held naming a_holder", c, res.Text)
	}
	if c := refusalClass(f.call(t, other, `{"op":"read","name":"work","path":"x"}`)); c != "not_found" {
		t.Errorf("the refused run reaches it: %q", c)
	}
	// The holder ended without a release (the sweeper has not run): the
	// next open takes it.
	if err := f.st.FinishRun(context.Background(), tools.RunID(holder), store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	f.mustOK(t, other, openDurable)
	if c := refusalClass(f.call(t, holder, `{"op":"read","name":"work","path":"x"}`)); c != "not_found" {
		t.Errorf("the ended holder still reaches it: %q", c)
	}
}

// The run's end parks it, never removes it; a paused owner keeps its pin.
func TestDurable_RunEndParksNeverRemovesAndPausedKeepsThePin(t *testing.T) {
	f := durableFixture(t, 8)
	paused := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, paused, openDurable)
	if err := f.st.SetRunPauseState(context.Background(), tools.RunID(paused), store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if err := f.tool.Manager.ReleaseRun(context.Background(), tools.RunID(paused)); err != nil {
		t.Fatal(err)
	}
	if res := f.mustOK(t, paused, `{"op":"list"}`); !strings.Contains(res.Text, `"work"`) {
		t.Errorf("a paused owner lost its pin: %s", res.Text)
	}
	if err := f.st.SetRunPauseState(context.Background(), tools.RunID(paused), store.PauseStateRunning); err != nil {
		t.Fatal(err)
	}
	f.mustOK(t, paused, `{"op":"write","name":"work","path":"notes.txt","content":"still"}`)
	f.endRun(t, paused)
	r, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work")
	if err != nil || r.OwnerKind != store.WorkspaceOwnerParked {
		t.Fatalf("after the run's end: %+v, %v; want parked", r, err)
	}
	next := f.runAsAgent(t, "acme", "alice", "a2", wsKeeper)
	f.mustOK(t, next, openDurable)
	if got := readNotes(t, f, next); !strings.Contains(got, "still") {
		t.Errorf("after the run's end the files are %s", got)
	}
}

// Release ends what the workspace shared: a peer's share is gone.
func TestDurable_ReleaseEndsItsShares(t *testing.T) {
	f := durableFixture(t, 8)
	owner := f.runAsAgent(t, "acme", "alice", "a_owner", wsKeeper)
	peer := f.runAsAgent(t, "acme", "alice", "a_peer", wsKeeper)
	f.mustOK(t, owner, openDurable)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"notes.txt","content":"x"}`)
	f.mustOK(t, owner, `{"op":"share","name":"work","with":"a_peer","access":"read"}`)
	f.mustOK(t, peer, `{"op":"read","name":"work@a_owner","path":"notes.txt"}`)
	f.mustOK(t, owner, `{"op":"release","name":"work"}`)
	if c := refusalClass(f.call(t, peer, `{"op":"read","name":"work@a_owner","path":"notes.txt"}`)); c != "not_found" {
		t.Errorf("a peer after the owner's release = %q, want not_found", c)
	}
}

// discard removes it, files and row; a later open makes a new, empty one.
// A parked workspace no run holds can be discarded by a run of its user.
func TestDurable_DiscardRemovesIt(t *testing.T) {
	f := durableFixture(t, 8)
	first := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, first, openDurable)
	f.mustOK(t, first, `{"op":"write","name":"work","path":"notes.txt","content":"gone"}`)
	f.endRun(t, first)
	second := f.runAsAgent(t, "acme", "alice", "a2", wsKeeper)
	if res := f.mustOK(t, second, `{"op":"release","name":"work","discard":true}`); !strings.Contains(res.Text, `"released":true`) {
		t.Fatalf("discard of a parked workspace = %s", res.Text)
	}
	if _, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err == nil {
		t.Error("the row survived a discard")
	}
	f.mustOK(t, second, openDurable)
	if c := refusalClass(f.call(t, second, `{"op":"read","name":"work","path":"notes.txt"}`)); c == "" {
		t.Error("a discarded workspace's file is back")
	}
	// Another user cannot discard alice's.
	bob := f.runAsAgent(t, "acme", "bob", "b1", wsKeeper)
	f.endRun(t, second)
	if res := f.call(t, bob, `{"op":"release","name":"work","discard":true}`); !res.IsError {
		t.Errorf("bob discarded alice's durable workspace: %s", res.Text)
	}
	if _, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err != nil {
		t.Errorf("alice's after bob's discard: %v", err)
	}
}

// workspace.durable.max_per_user bounds a user's durable workspaces, pinned
// and parked together.
func TestDurable_BoundedPerUser(t *testing.T) {
	f := durableFixture(t, 2)
	run := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	for _, n := range []string{"one", "two"} {
		f.mustOK(t, run, fmt.Sprintf(`{"op":"open","name":%q,"lifetime":"durable"}`, n))
		f.mustOK(t, run, fmt.Sprintf(`{"op":"release","name":%q}`, n))
	}
	if c := refusalClass(f.call(t, run, `{"op":"open","name":"three","lifetime":"durable"}`)); c != "limit" {
		t.Errorf("a third durable workspace = %q, want limit", c)
	}
	if res := f.mustOK(t, f.runAsAgent(t, "acme", "bob", "b1", wsKeeper), `{"op":"open","name":"one","lifetime":"durable"}`); res.IsError {
		t.Error("another user's bound was spent by alice's")
	}
}

// Over loomcycle's MCP server no workspace is durable (ruling Q3).
func TestDurable_AnMCPOwnerCannotAskForIt(t *testing.T) {
	f := durableFixture(t, 8)
	ctx := lifecycle.WithPrincipalOwner(context.Background(), lifecycle.PrincipalOwner("acme", "alice", "tok-1"))
	ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: wsKeeper})
	if c := refusalClass(f.call(t, ctx, openDurable)); c != "not_over_mcp" {
		t.Errorf("a durable open over MCP = %q, want not_over_mcp", c)
	}
}

// list says which of the run's workspaces are durable.
func TestDurable_ListSaysDurable(t *testing.T) {
	f := durableFixture(t, 8)
	run := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, run, openDurable)
	var out struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, run, `{"op":"list"}`).Text), &out); err != nil || len(out.Workspaces) != 1 || out.Workspaces[0]["lifetime"] != "durable" {
		t.Errorf("list = %+v, %v", out, err)
	}
}

// Discarding destroys a workspace another run made: it needs the durable
// grant, and a grant that covers what the workspace was made with (its
// engine and routes), as pinning it does. Refused, nothing is touched.
// (Security review of #2973.)
func TestDurable_DiscardNeedsTheDurableGrant(t *testing.T) {
	f := durableFixture(t, 8)
	maker := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, maker, openDurable)
	f.mustOK(t, maker, `{"op":"write","name":"work","path":"notes.txt","content":"keep me"}`)
	f.endRun(t, maker)

	noDurable := f.runAsAgent(t, "acme", "alice", "a2", wsFull)
	if c := refusalClass(f.call(t, noDurable, `{"op":"release","name":"work","discard":true}`)); c != "not_granted" {
		t.Errorf("discard without the durable grant = %q, want not_granted", c)
	}
	strict := tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Durable: true, Engine: "container"}
	if c := refusalClass(f.call(t, f.runAsAgent(t, "acme", "alice", "a3", strict), `{"op":"release","name":"work","discard":true}`)); c != "not_granted" {
		t.Errorf("discard by a run whose grant does not cover the host engine it was made on = %q, want not_granted", c)
	}
	if _, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err != nil {
		t.Fatalf("the row after refused discards: %v", err)
	}
	again := f.runAsAgent(t, "acme", "alice", "a4", wsKeeper)
	f.mustOK(t, again, openDurable)
	if got := readNotes(t, f, again); !strings.Contains(got, "keep me") {
		t.Errorf("the files after refused discards: %s", got)
	}
	// Its own holder, with the grant, may.
	f.mustOK(t, again, `{"op":"release","name":"work","discard":true}`)
	if _, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err == nil {
		t.Error("the holder's discard left the row")
	}
}

// A grant that names fewer egress routes than the workspace was made with
// cannot discard it either.
func TestDurable_DiscardNeedsTheGrantToCoverItsRoutes(t *testing.T) {
	f := durableFixture(t, 8)
	routed := wsKeeper
	routed.Egress = []string{"pypi"}
	maker := f.runAsAgent(t, "acme", "alice", "a1", routed)
	if res := f.call(t, maker, openDurable); res.IsError {
		t.Skipf("this fixture cannot open a workspace with a route: %s", res.Text)
	}
	f.endRun(t, maker)
	if c := refusalClass(f.call(t, f.runAsAgent(t, "acme", "alice", "a2", wsKeeper), `{"op":"release","name":"work","discard":true}`)); c != "not_granted" {
		t.Errorf("discard by a run without the workspace's route = %q, want not_granted", c)
	}
	if _, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err != nil {
		t.Errorf("the row after a refused discard: %v", err)
	}
}

// A durable git workspace (RFC GW P11) is neither re-opened nor discarded
// by another run until P11 settles the grant that covers its repository and
// credential: refused not_granted, nothing touched.
func TestDurable_AGitWorkspaceIsNotTakenByAnotherRun(t *testing.T) {
	f := durableFixture(t, 8)
	maker := f.runAsAgent(t, "acme", "alice", "a1", wsKeeper)
	f.mustOK(t, maker, openDurable)
	r, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.WorkspaceSetGit(context.Background(), "acme", r.ID, store.WorkspaceGit{URL: "https://forge.example/acme/repo.git", Branch: "loom/work", Credential: "$cred:forge", Mirror: "m1"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	f.endRun(t, maker)
	next := f.runAsAgent(t, "acme", "alice", "a2", wsKeeper)
	for _, in := range []string{openDurable, `{"op":"release","name":"work","discard":true}`} {
		if c := refusalClass(f.call(t, next, in)); c != "not_granted" {
			t.Errorf("%s on a parked git workspace = %q, want not_granted", in, c)
		}
	}
	if got, err := f.st.WorkspaceGetDurable(context.Background(), "acme", "alice", "work"); err != nil || got.OwnerKind != store.WorkspaceOwnerParked || got.Git == nil {
		t.Errorf("after the refusals: %+v, %v; want it parked with its git record", got, err)
	}
}
