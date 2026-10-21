package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A child of the same definition inherits its parent's per-run routing block,
// as it inherits a per-run model, and records it so a resume keeps it.
func TestOverrideInheritance_TheRunsRoutingBlockIsInherited(t *testing.T) {
	srv := routingServer(t)
	parentRec := runConfigRecord{QueueRouting: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 15000}}
	ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{Overrides: tools.RunOverridesValue{
		Record: parentRec.marshal(), AgentName: "router",
	}})

	got, cfg := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "router")
	if got.Routing == nil || !got.Routing.QueueAware || got.Routing.SwitchAfterMS != 15000 {
		t.Errorf("child routing = %+v, want the parent's per-run block", got.Routing)
	}
	if cfg.QueueRouting == nil || cfg.QueueRouting.SwitchAfterMS != 15000 {
		t.Errorf("child record queue_routing = %+v, want it persisted", cfg.QueueRouting)
	}
	// Recorded as a copy: the child's record and the parent's do not share it.
	cfg.QueueRouting.SwitchAfterMS = 1
	if got.Routing.SwitchAfterMS != 15000 {
		t.Error("the child's record shares its routing block with the definition it runs")
	}

	// Across definitions nothing is inherited, the routing block included.
	if other, _ := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "summariser"); other.Routing != nil {
		t.Errorf("a different agent inherited the routing block: %+v", other.Routing)
	}
}

// What a child inherits is what it RUNS on: its provider and model are
// resolved after inheritance. Here the parent runs with a per-run model
// (model-b, the tier's second candidate) and spawns its own agent; the child's
// call goes to model-b. It used to resolve before inheriting, so the child ran
// on the tier's first candidate while its record claimed model-b.
func TestSubAgent_RunsOnTheModelItInherits(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"router": {Tier: "middle", Tools: []string{"Agent"}, SystemPrompt: "you route"},
	}
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	}
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"router","prompt":"go"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	prov := &recordingScriptedProvider{scripts: [][]providers.Event{spawn, endTurn(), endTurn()}, defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "inherit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	if code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-b",`+routedSegments+`}`); code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	reqs := prov.waitForRequests(t, 3)
	for i, who := range []string{"the parent", "the child", "the parent's last turn"} {
		if reqs[i].Model != "model-b" {
			t.Errorf("%s asked for %q, want model-b", who, reqs[i].Model)
		}
	}
}

// A retune can set, replace or switch off the run's routing block: whole, as
// at start ({} kept as a block that turns it off), kept by a retune of
// another field, and refused where the run is pinned to a model.
func TestRetune_TheRoutingBlockIsReplacedWholeAndValidated(t *testing.T) {
	srv, ts, _, st := routedServer(t)
	if code, body := postRoutedRun(t, ts, `{"agent":"router","agent_id":"a_retune_routing",`+routedSegments+`}`); code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	ctx := context.Background()
	run, err := st.GetRunByAgentID(ctx, "a_retune_routing")
	if err != nil {
		t.Fatal(err)
	}
	stored := func() *config.AgentRouting {
		t.Helper()
		r, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := decodeRunConfig(r.RunConfig)
		return rec.QueueRouting
	}

	if _, err := srv.retuneRun(ctx, run, &runOverridesWire{Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 15000}}); err != nil {
		t.Fatalf("retune: %v", err)
	}
	if got := stored(); got == nil || !got.QueueAware || got.SwitchAfterMS != 15000 {
		t.Fatalf("stored routing = %+v, want the retuned block", got)
	}
	// A retune of another field keeps it.
	if _, err := srv.retuneRun(ctx, run, &runOverridesWire{MaxTokens: 512}); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got == nil || !got.QueueAware {
		t.Errorf("a retune of max_tokens dropped the routing block: %+v", got)
	}
	// {} replaces it whole and is kept as a block, so it still overrides the
	// definition's.
	if _, err := srv.retuneRun(ctx, run, &runOverridesWire{Routing: &config.AgentRouting{}}); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got == nil || got.QueueAware {
		t.Errorf("after {}: stored routing = %+v, want an empty block", got)
	}
	// queue_aware on a run pinned to a model is refused, and nothing changes.
	if _, err := srv.retuneRun(ctx, run, &runOverridesWire{Model: "model-b", Routing: &config.AgentRouting{QueueAware: true}}); err == nil {
		t.Error("queue_aware with a model pin was accepted")
	}
	if got := stored(); got == nil || got.QueueAware {
		t.Errorf("a refused retune changed the stored block: %+v", got)
	}
}
