package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// delegatingProvider answers "answer" — except for an agent whose system prompt
// says DELEGATE, which first calls the Agent tool once, so a walk member has a
// sub-agent of its own. "DELEGATE:<agent>" names the agent it calls; a bare
// DELEGATE calls helper.
type delegatingProvider struct{}

func (p *delegatingProvider) ID() string                  { return "stub" }
func (p *delegatingProvider) Probe(context.Context) error { return nil }
func (p *delegatingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *delegatingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *delegatingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	delegates, answered, to := false, false, "helper"
	for _, b := range req.System {
		delegates = delegates || strings.Contains(b.Text, "DELEGATE")
		if _, name, ok := strings.Cut(b.Text, "DELEGATE:"); ok {
			to = strings.Fields(name)[0]
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			answered = answered || c.Type == "tool_result"
		}
	}
	ch := make(chan providers.Event, 2)
	if delegates && !answered {
		input := `{"name":"` + to + `","prompt":"help"}`
		if to == "POLL" {
			// Two background children the run does not wait on in the call.
			input = `{"op":"parallel_spawn","mode":"poll","spawns":[{"name":"helper","prompt":"a"},{"name":"helper","prompt":"b"}]}`
		}
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
			ID: "tu_delegate", Name: "Agent", Input: json.RawMessage(input),
		}}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: "answer"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}
	close(ch)
	return ch, nil
}

// newStampHarness is newWalkHarness with a member that delegates and a judge
// for the consolidating states.
func newStampHarness(t *testing.T) *walkHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"writer":    {Model: "stub-model", SystemPrompt: "write"},
			"judge":     {Model: "stub-model", SystemPrompt: "judge"},
			"delegator": {Model: "stub-model", SystemPrompt: "DELEGATE", Tools: []string{"Agent"}},
			"helper":    {Model: "stub-model", SystemPrompt: "help"},
			// chain → relay → helper: a member with a sub-agent that has one.
			"chain": {Model: "stub-model", SystemPrompt: "DELEGATE:relay", Tools: []string{"Agent"}},
			"relay": {Model: "stub-model", SystemPrompt: "DELEGATE:helper", Tools: []string{"Agent"}},
			// poller starts two helpers in the background (mode poll).
			"poller": {Model: "stub-model", SystemPrompt: "DELEGATE:POLL", Tools: []string{"Agent"}},
		},
		Channels: map[string]config.Channel{
			"in":  {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
			"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &delegatingProvider{}}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	cbus := channels.NewBus()
	pub := &channels.StorePublisher{Store: st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)}
	srv.SetSystemPublisher(pub)
	srv.SetChannelBus(cbus)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	return &walkHarness{t: t, srv: srv, st: st, bus: bus, pub: pub}
}

// everyKindTeam visits one state of every kind that starts runs, in order, so
// each visit's ordinal is known: wave=1, a=2, p=3, c=4, d=5.
const everyKindTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},` +
	`"fanout":{"agent":"writer","per":"message","max":1},"sink":{"channel":"out"}}},` +
	`{"state":"a","handler":{"kind":"agent","agent":"writer","consolidator":"judge"}},` +
	`{"state":"p","handler":{"kind":"parallel","agents":["writer","writer"],"consolidator":"judge"}},` +
	`{"state":"c","handler":{"kind":"consolidator","agent":"judge"}},` +
	`{"state":"d","handler":{"kind":"agent","agent":"delegator"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"a","on":"success"},{"from":"a","to":"p","on":"success"},` +
	`{"from":"p","to":"c","on":"success"},{"from":"c","to":"d","on":"success"},{"from":"d","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"],"subscribe":["in"]}}`

// TestTeamDefRun_EveryMemberCarriesItsWalkState: every run a walk state starts
// — an agent and its consolidator, each parallel member and its consolidator, a
// standalone consolidator, a Starter's wave member — records the walk, the state
// it ran in and which visit of that state. Only the Starter's used to carry even
// the walk id, so a walk-filtered view of any other state showed nothing.
//
// A member's OWN sub-agent is not a walk member, and belongs to the walk: it
// carries the walk, the state and the visit of the member that started it, and
// names that member, not the walk, as its parent run.
func TestTeamDefRun_EveryMemberCarriesItsWalkState(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "every", everyKindTeam)
	h.feed("acme")

	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"every","input":"go"}`)

	runs, err := h.st.ListActiveRunsByUser(context.Background(), "", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ visit, runs int }{
		"wave": {1, 1}, // the wave member
		"a":    {2, 2}, // writer + judge
		"p":    {3, 3}, // two writers + judge
		"c":    {4, 1}, // the standalone judge
		"d":    {5, 1}, // the delegator
	}
	// The delegator's own sub-agent is found by lineage, not by its stamp, so
	// a helper that wrongly carried the walk is still the one checked.
	byAgent := map[string]store.Run{}
	for _, r := range runs {
		byAgent[r.AgentID] = r
	}
	var helper *store.Run
	got := map[string]int{}
	for i, r := range runs {
		if parent, ok := byAgent[r.ParentAgentID]; ok && parent.ParentContext != nil && parent.ParentContext.State == "d" {
			helper = &runs[i]
			continue
		}
		pc := r.ParentContext
		if r.ID == walkID || pc == nil || pc.WalkID == "" {
			continue
		}
		if pc.WalkID != walkID {
			t.Errorf("run %s carries walk %q, want %q", r.ID, pc.WalkID, walkID)
		}
		w, ok := want[pc.State]
		if !ok {
			t.Errorf("run %s carries state %q, not one of the walk's", r.ID, pc.State)
			continue
		}
		if pc.StateVisit != w.visit {
			t.Errorf("run %s in state %q carries state_visit %d, want %d", r.ID, pc.State, pc.StateVisit, w.visit)
		}
		// Only the Starter's member is in a wave.
		if (pc.State == "wave") != (pc.WaveID != "") {
			t.Errorf("run %s in state %q: wave_id = %q", r.ID, pc.State, pc.WaveID)
		}
		got[pc.State]++
	}
	for state, w := range want {
		if got[state] != w.runs {
			t.Errorf("state %q: %d runs carry it, want %d", state, got[state], w.runs)
		}
	}

	if helper == nil {
		t.Fatal("the delegator's own sub-agent never ran")
	}
	pc := helper.ParentContext
	if pc == nil || pc.WalkID != walkID || pc.State != "d" || pc.StateVisit != 5 || pc.WaveID != "" {
		t.Fatalf("a member's own sub-agent carries %+v, want the walk, state d, visit 5 and no wave", pc)
	}
	if helper.IsWalkMember() || helper.ParentRunID == walkID || helper.ParentRunID != byAgent[helper.ParentAgentID].ID {
		t.Errorf("the sub-agent's parent run = %q, want the delegator's run, which tells it from a member", helper.ParentRunID)
	}
}

// chainTeam is one Starter state whose wave member starts a sub-agent that
// starts another.
const chainTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},` +
	`"fanout":{"agent":"chain","per":"message","max":1},"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"],"subscribe":["in"]}}`

// TestTeamDefRun_SubAgentsAtEveryDepthAreInTheWalksListing: the walk's listing
// returns the walk, its member and every run under the member, however deep.
// Each descendant carries the member's walk, state and visit. The wave stays
// the member's alone, so a wave's width is the runs its Starter dispatched.
func TestTeamDefRun_SubAgentsAtEveryDepthAreInTheWalksListing(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "chain", chainTeam)
	h.feed("acme")
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"chain","input":"go"}`)

	listed, err := h.srv.ListWalkRuns(alicePrincipal(context.Background()), walkID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	byAgent := map[string]connector.Run{}
	for _, a := range listed.Runs {
		byAgent[a.Agent] = a
	}
	member, relay, helper := byAgent["chain"], byAgent["relay"], byAgent["helper"]
	if len(listed.Runs) != 4 || member.RunID == "" || relay.RunID == "" || helper.RunID == "" {
		t.Fatalf("the walk's listing has %d runs (%v), want the walk, chain, relay and helper", len(listed.Runs), listed.Runs)
	}
	if member.ParentRunID != walkID || relay.ParentRunID != member.RunID || helper.ParentRunID != relay.RunID {
		t.Errorf("parent runs: chain←%q relay←%q helper←%q, want the walk, chain, relay",
			member.ParentRunID, relay.ParentRunID, helper.ParentRunID)
	}
	if pc := member.ParentContext; pc == nil || pc.WaveID == "" {
		t.Fatalf("the wave's member carries no wave: %+v", pc)
	}
	for name, a := range map[string]connector.Run{"relay": relay, "helper": helper} {
		pc := a.ParentContext
		if pc == nil || pc.WalkID != walkID || pc.State != "wave" || pc.StateVisit != 1 {
			t.Errorf("%s carries %+v, want the walk, state wave, visit 1", name, pc)
			continue
		}
		if pc.WaveID != "" || pc.WaveIndex != 0 || pc.BoardChunkID != "" {
			t.Errorf("%s carries the member's wave or board: %+v", name, *pc)
		}
	}
}

// TestTeamDefRun_SubAgentsShowOnTheWalkStream: the walk_id-filtered run-state
// stream carries a member's sub-agents, running and ended, with the member's
// state. Before they carried the walk, a member waiting on its children named
// runs the stream never mentioned.
func TestTeamDefRun_SubAgentsShowOnTheWalkStream(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "chain", chainTeam)
	h.feed("acme")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan connector.RunStateEvent, 256)
	go func() {
		_ = h.srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{
			UserID: "alice", TenantID: "acme", TenantScoped: true, WalkID: "",
		}, func(evt connector.RunStateEvent) error {
			events <- evt
			return nil
		})
	}()
	waitForSubscriber(t, h.bus)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"chain","input":"go"}`)

	// agent → the statuses the walk's filter passed for it.
	seen := map[string]map[string]bool{}
	deadline := time.After(3 * time.Second)
	for !(seen["relay"]["running"] && seen["relay"]["completed"] && seen["helper"]["running"] && seen["helper"]["completed"]) {
		select {
		case evt := <-events:
			if evt.RunID == walkID || !walkIDMatches(walkID, evt.RunID, evt.ParentContext) {
				continue
			}
			if evt.ParentContext.State != "wave" || evt.ParentContext.StateVisit != 1 {
				t.Errorf("%s event carries state %q visit %d, want wave / 1", evt.Agent, evt.ParentContext.State, evt.ParentContext.StateVisit)
			}
			if seen[evt.Agent] == nil {
				seen[evt.Agent] = map[string]bool{}
			}
			seen[evt.Agent][evt.Status] = true
		case <-deadline:
			t.Fatalf("the walk's stream carried %v, want relay and helper running and completed", seen)
		}
	}
}

// TestOpenTeamWalkRun_AWalkAMemberStartsCarriesTheOuterWalk: a walk started
// from inside a member of another walk is that member's descendant, so the
// outer walk's listing has its run. What the inner walk starts is its own.
func TestOpenTeamWalkRun_AWalkAMemberStartsCarriesTheOuterWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	member := tools.WithRunID(substrateAdminCtx(context.Background()), "r_member")
	member = store.WithWalkLineage(member, store.WalkTask{WalkID: "r_outer", State: "lead", Visit: 3})
	walkCtx, runID, finish, err := srv.openTeamWalkRun(member, builtin.WalkRunSpec{Name: "inner"})
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	defer finish(builtin.WalkEnd{})
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	pc := run.ParentContext
	if pc == nil || pc.WalkID != "r_outer" || pc.State != "lead" || pc.StateVisit != 3 || run.ParentRunID != "r_member" {
		t.Errorf("the inner walk's run carries %+v under parent %q, want the outer walk's state under the member", pc, run.ParentRunID)
	}
	if run.IsWalkMember() {
		t.Error("the inner walk's run reads as a member of the outer walk")
	}
	if ln, ok := store.WalkLineageFromContext(walkCtx); ok {
		t.Errorf("the inner walk's ctx still carries the outer walk %+v: its own runs would be filed under it", ln)
	}

	// A walk started outside any walk carries none.
	_, plainID, plainFinish, err := srv.openTeamWalkRun(tools.WithRunID(substrateAdminCtx(context.Background()), "r_caller"), builtin.WalkRunSpec{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	defer plainFinish(builtin.WalkEnd{})
	if plain, _ := srv.store.GetRun(context.Background(), plainID); plain.ParentContext != nil {
		t.Errorf("a walk started outside a walk carries %+v", *plain.ParentContext)
	}
}

// agentOnlyTeam has no starter: before every member carried the walk id, a
// walk-filtered stream of it showed nothing at all.
const agentOnlyTeam = `{"entry":"a","states":[` +
	`{"state":"a","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"a","to":"done","on":"success"}]}`

// TestTeamDefRun_WalkWithoutAStarterShowsOnTheWalkStream: the walk_id-filtered
// run-state stream carries the member of a walk that has no starter state.
func TestTeamDefRun_WalkWithoutAStarterShowsOnTheWalkStream(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan connector.RunStateEvent, 64)
	go func() {
		_ = h.srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{
			UserID: "alice", TenantID: "acme", TenantScoped: true,
		}, func(evt connector.RunStateEvent) error {
			events <- evt
			return nil
		})
	}()
	waitForSubscriber(t, h.bus)

	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.RunID != walkID && walkIDMatches(walkID, evt.RunID, evt.ParentContext) {
				if evt.ParentContext.State != "a" || evt.ParentContext.StateVisit != 1 {
					t.Errorf("member event carries state %q visit %d, want a / 1",
						evt.ParentContext.State, evt.ParentContext.StateVisit)
				}
				return
			}
		case <-deadline:
			t.Fatalf("the walk_id-filtered stream never saw a member of walk %s", walkID)
		}
	}
}

// A sub-agent that carries its member's walk still takes only verdicts, as a
// sub-agent always did; a member, whose parent run is the walk, takes a
// retune. The walk id alone no longer tells the two apart.
func TestTakesOnlyVerdicts_ASubAgentCarryingTheWalkIsStillASubAgent(t *testing.T) {
	walk := &store.ParentContext{WalkID: "r_walk", State: "lead", StateVisit: 1}
	for name, tc := range map[string]struct {
		run  store.Run
		want bool
	}{
		"a top-level run":                  {store.Run{}, false},
		"a walk's member":                  {store.Run{ParentRunID: "r_walk", ParentContext: walk}, false},
		"a member's sub-agent":             {store.Run{ParentRunID: "r_member", ParentContext: walk}, true},
		"a sub-agent outside any walk":     {store.Run{ParentRunID: "r_parent"}, true},
		"a sub-agent with caller tracking": {store.Run{ParentRunID: "r_parent", ParentContext: &store.ParentContext{FunctionKey: "k"}}, true},
	} {
		if got := takesOnlyVerdicts(tc.run); got != tc.want {
			t.Errorf("%s: takesOnlyVerdicts = %v, want %v", name, got, tc.want)
		}
	}
}

// pollTeam is one agent state whose member starts two background children.
const pollTeam = `{"entry":"lead","states":[` +
	`{"state":"lead","handler":{"kind":"agent","agent":"poller"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"lead","to":"done","on":"success"}]}`

// TestTeamDefRun_BackgroundChildrenAreInTheWalksListing: a member that starts
// two children with mode poll, and is then reported as waiting on its
// children, has both in the walk's listing with its state.
func TestTeamDefRun_BackgroundChildrenAreInTheWalksListing(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "poll", pollTeam)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"poll","input":"go"}`)

	listed, err := h.srv.ListWalkRuns(alicePrincipal(context.Background()), walkID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var member connector.Run
	var children []connector.Run
	for _, r := range listed.Runs {
		switch r.Agent {
		case "poller":
			member = r
		case "helper":
			children = append(children, r)
		}
	}
	if len(listed.Runs) != 4 || member.RunID == "" || len(children) != 2 {
		t.Fatalf("the walk's listing has %d runs, want the walk, the member and its two children: %+v", len(listed.Runs), listed.Runs)
	}
	for _, c := range children {
		pc := c.ParentContext
		if c.ParentRunID != member.RunID || pc == nil || pc.WalkID != walkID || pc.State != "lead" || pc.StateVisit != 1 {
			t.Errorf("child %s: parent %q, context %+v; want the member as parent and the walk, state lead, visit 1", c.RunID, c.ParentRunID, pc)
		}
	}
}
