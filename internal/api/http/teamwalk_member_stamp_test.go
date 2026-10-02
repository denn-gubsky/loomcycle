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
// sub-agent of its own.
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
	delegates, answered := false, false
	for _, b := range req.System {
		delegates = delegates || strings.Contains(b.Text, "DELEGATE")
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			answered = answered || c.Type == "tool_result"
		}
	}
	ch := make(chan providers.Event, 2)
	if delegates && !answered {
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
			ID: "tu_delegate", Name: "Agent", Input: json.RawMessage(`{"name":"helper","prompt":"help"}`),
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
// A member's OWN sub-agent is not a walk member and carries none of it.
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
	if pc := helper.ParentContext; pc != nil && (pc.WalkID != "" || pc.State != "" || pc.StateVisit != 0 || pc.WaveID != "") {
		t.Errorf("a member's own sub-agent carries the walk: %+v", *pc)
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
