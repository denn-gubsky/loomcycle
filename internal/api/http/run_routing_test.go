package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
)

// A run's own `routing:` block reaches run-start resolution. The router agent
// has NO routing block; its tier's first candidate (primary/model-a) is busy
// — its one chat slot taken, its calls measured at ~40 s — so only a
// queue-aware start passes it over for secondary/model-b. The model the
// provider is asked for is the only place that is observable.
func TestRunRouting_PerRunBlockReachesRunStartResolution(t *testing.T) {
	srv, ts, prov, st := routedServer(t)

	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"primary": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	srv.SetCallScheduler(sched)
	release, err := sched.Acquire(context.Background(), "primary", config.ModelKindChat, concurrency.CallInfo{Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	srv.throughput = throughput.New(&config.Config{}, nil, nil)
	for i := 0; i < 5; i++ {
		srv.throughput.Observe(throughput.Sample{RunID: fmt.Sprintf("r%d", i), At: time.Now(),
			Key: throughput.Key{Provider: "primary", Model: "model-a"}, Output: 100,
			CallTiming: providers.CallTiming{DurationMs: 40000}})
	}
	if def := srv.cfg().Agents["router"]; def.Routing != nil {
		t.Fatalf("fixture drifted: router carries its own routing block %+v; the run's must be the only one", def.Routing)
	}

	// Bounded: were the block dropped, the run would land on the busy provider,
	// and a fixture whose calls go through its lanes would wait there forever.
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"router","agent_id":"a_run_routing","routing":{"queue_aware":true,"switch_after_ms":20000},`+routedSegments+`}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run: status %d: %s", resp.StatusCode, b)
	}
	if got := prov.waitForRequests(t, 1)[0].Model; got != "model-b" {
		t.Errorf("provider was asked for %q, want model-b — the run's routing block did not reach run-start resolution", got)
	}

	// The run's record says it chose the block, for its spec and its report.
	run, err := st.GetRunByAgentID(context.Background(), "a_run_routing")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := decodeRunConfig(run.RunConfig)
	if rec.QueueRouting == nil || !rec.QueueRouting.QueueAware || rec.QueueRouting.SwitchAfterMS != 20000 {
		t.Errorf("run_config queue_routing = %+v, want the run's own block", rec.QueueRouting)
	}

	// A continuation starts a run too, through its own intake.
	resp, err = client.Post(ts.URL+"/v1/sessions/"+run.SessionID+"/messages", "application/json", strings.NewReader(
		`{"routing":{"queue_aware":true},`+routedSegments+`}`))
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continuation: status %d: %s", resp.StatusCode, b)
	}
	if got := prov.waitForRequests(t, 2)[1].Model; got != "model-b" {
		t.Errorf("continuation: provider was asked for %q, want model-b — the continuation's routing block was dropped", got)
	}
}

// The block replaces the definition's WHOLE: an empty one turns the
// definition's queue-aware routing off for the run, and a set one is taken as
// given rather than merged field by field.
func TestRunRouting_ReplacesTheDefinitionsBlockWhole(t *testing.T) {
	srv, _, _, _ := routedServer(t)
	def := config.AgentDef{Tier: "middle", Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 5000}}

	off, err := srv.effectiveDef(context.Background(), def, runOverrides{QueueRouting: &config.AgentRouting{}})
	if err != nil {
		t.Fatal(err)
	}
	if off.Routing == nil || off.Routing.QueueAware || off.Routing.SwitchAfterMS != 0 {
		t.Errorf("empty per-run block: effective routing = %+v, want an empty block", off.Routing)
	}

	own, err := srv.effectiveDef(context.Background(), def, runOverrides{QueueRouting: &config.AgentRouting{QueueAware: true}})
	if err != nil {
		t.Fatal(err)
	}
	if own.Routing.SwitchAfterMS != 0 {
		t.Errorf("switch_after_ms = %d, want 0 (the default) — the definition's 5000 was merged in", own.Routing.SwitchAfterMS)
	}
	if def.Routing.SwitchAfterMS != 5000 || !def.Routing.QueueAware {
		t.Errorf("the stored definition's block was changed: %+v", def.Routing)
	}

	kept, err := srv.effectiveDef(context.Background(), def, runOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if kept.Routing == nil || kept.Routing.SwitchAfterMS != 5000 {
		t.Errorf("no per-run block: effective routing = %+v, want the definition's", kept.Routing)
	}
}

// queue_aware chooses among a tier's candidates, so where the EFFECTIVE
// definition is a pin — the agent's own, or one the run made by naming a
// model — it is refused, naming the field, rather than silently doing nothing.
// A negative switch_after_ms is refused too.
func TestRunRouting_QueueAwareOnAPinIsRefused(t *testing.T) {
	srv, ts, prov, _ := routedServer(t)

	pinned := config.AgentDef{Provider: "primary", Model: "model-a"}
	_, err := srv.effectiveDef(context.Background(), pinned, runOverrides{QueueRouting: &config.AgentRouting{QueueAware: true}})
	if !errors.Is(err, runner.ErrInvalidArgument) || !strings.Contains(fmt.Sprint(err), "routing.queue_aware") {
		t.Errorf("pinned agent + per-run queue_aware: err = %v, want ErrInvalidArgument naming routing.queue_aware", err)
	}
	if _, err := srv.effectiveDef(context.Background(), pinned, runOverrides{QueueRouting: &config.AgentRouting{}}); err != nil {
		t.Errorf("pinned agent + an empty block (nothing to choose): err = %v, want accepted", err)
	}

	for _, tc := range []struct{ what, body, want string }{
		{"the run's own model pin", `{"agent":"router","model":"model-b","routing":{"queue_aware":true},` + routedSegments + `}`, "routing.queue_aware"},
		{"a negative switch_after_ms", `{"agent":"router","routing":{"switch_after_ms":-1},` + routedSegments + `}`, "routing.switch_after_ms"},
	} {
		code, body := postRoutedRun(t, ts, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: status %d (%s), want 400 naming %s", tc.what, code, strings.TrimSpace(body), tc.want)
		}
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for refused runs", len(got))
	}

	// RunOnce — gRPC, MCP, a batch child, a configured run — is held to the same check.
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	err = srv.RunOnce(context.Background(), runner.RunInput{Agent: "router", Model: "model-b",
		Routing: &config.AgentRouting{QueueAware: true}, Segments: seg}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrInvalidArgument) || !strings.Contains(fmt.Sprint(err), "routing.queue_aware") {
		t.Errorf("RunOnce with a model pin + queue_aware: err = %v, want ErrInvalidArgument naming routing.queue_aware", err)
	}
}

// An agent that runs on its definition only refuses a run's routing block, on
// the HTTP intake and on RunOnce alike, from a caller no token lock would stop.
func TestRunRouting_AllowOverridesFalseRefusesIt(t *testing.T) {
	s, ts, _ := restrictedFixtureServer(t)
	code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runBodyWith("fixed", "routing", `{"queue_aware":false}`))
	if code != http.StatusForbidden || !strings.Contains(body, "override_not_allowed") || !strings.Contains(body, "routing") {
		t.Errorf("POST /v1/runs: status %d, want 403 override_not_allowed naming routing: %s", code, body)
	}

	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	err := s.RunOnce(t.Context(), runner.RunInput{Agent: "fixed", Routing: &config.AgentRouting{}, Segments: seg}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrOverrideNotAllowed) {
		t.Errorf("RunOnce: err = %v, want ErrOverrideNotAllowed", err)
	}
}

// A queued run stores its RunInput and a restart starts it from that, so the
// block must survive both request conversions and the JSON round trip — a
// restarted run that lost it would start somewhere its caller passed over.
func TestRunRouting_SurvivesTheQueuedRestartRoundTrip(t *testing.T) {
	want := config.AgentRouting{QueueAware: true, SwitchAfterMS: 7000}
	in, err := requestRunInput(runRequest{Agent: "r", Routing: &want, Segments: oneUserSeg("go")})
	if err != nil || in.Routing == nil || *in.Routing != want {
		t.Fatalf("requestRunInput: Routing = %+v (%v), want %+v", in.Routing, err, want)
	}
	cont, err := messagesRunInput("s_1", store.Session{Agent: "r"}, messagesRequest{Routing: &want, Segments: oneUserSeg("go")})
	if err != nil || cont.Routing == nil || *cont.Routing != want {
		t.Fatalf("messagesRunInput: Routing = %+v (%v), want %+v", cont.Routing, err, want)
	}

	s, _ := newGatedBatchServer(t, 1)
	run := queuedFixture(t, s, "a_qrouting", "", in, "")
	q := store.QueuedRun{Input: queuedRowInput(t, s, run.ID)}
	if q.Run, err = s.store.GetRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	got, why := s.restartInput(q)
	if why != "" {
		t.Fatalf("restartInput: %s", why)
	}
	if got.Routing == nil || *got.Routing != want {
		t.Errorf("restarted Routing = %+v, want %+v", got.Routing, want)
	}
}
