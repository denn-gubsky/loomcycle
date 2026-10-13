package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The calls a run makes that are not its own chat calls — an embedding for a
// Memory search, a decision — are made from inside a TOOL, on the tool's ctx.
// A lane can queue them as that run (its turn among users, its lease, its
// class) only if that ctx carries what the loop stamped on the run's. These
// tests drive a real run through the server and look at the ctx its tool was
// handed, and at the lane the tool's embedding queued in.

// countingEmbedder is an embedder that answers at once and counts its calls.
type countingEmbedder struct{ calls atomic.Int32 }

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	return make([][]float32, len(texts)), nil
}
func (*countingEmbedder) Model() string    { return "embed-m" }
func (*countingEmbedder) Provider() string { return "stub" }
func (*countingEmbedder) Dimension() int   { return 3 }

// laneProbeTool embeds one text through the embedder it holds, as the Memory
// tool does, and records what its ctx says about the run it is called in.
type laneProbeTool struct {
	embedder providers.Embedder

	mu         sync.Mutex
	meta       providers.RunMeta
	hasMeta    bool
	background bool
}

func (*laneProbeTool) Name() string                 { return "LaneProbe" }
func (*laneProbeTool) Description() string          { return "embeds one text" }
func (*laneProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *laneProbeTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	meta, ok := providers.RunMetaFromContext(ctx)
	p.mu.Lock()
	p.meta, p.hasMeta, p.background = meta, ok, lanegate.IsBackground(ctx)
	p.mu.Unlock()
	if _, err := p.embedder.Embed(ctx, []string{"x"}); err != nil {
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{Text: "probed"}, nil
}

func (p *laneProbeTool) seen() (providers.RunMeta, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meta, p.hasMeta, p.background
}

// backgroundRecorder is a provider that notes whether each chat call arrived
// marked as background work.
type backgroundRecorder struct {
	*meteredProvider
	mu         sync.Mutex
	background []bool
}

func (b *backgroundRecorder) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	b.mu.Lock()
	b.background = append(b.background, lanegate.IsBackground(ctx))
	b.mu.Unlock()
	return b.meteredProvider.Call(ctx, req)
}

// probeServer is a server with one agent whose run calls LaneProbe once and
// then answers. Provider P caps chat and embedder calls at one each; the
// returned release frees the embedder slot the test holds from the start, so
// the tool's embedding has to queue.
func probeServer(t *testing.T) (srv *Server, sched *concurrency.CallScheduler, probe *laneProbeTool, emb *countingEmbedder, host *backgroundRecorder, release func()) {
	t.Helper()
	host = &backgroundRecorder{meteredProvider: &meteredProvider{
		meter: &flightMeter{},
		scripts: map[string][][]providers.Event{
			"tooler-m": {toolThenDone("tu_probe", "LaneProbe", `{}`), textThenDone("tooler done")},
		},
	}}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"tooler": {Provider: "P", Model: "tooler-m", Tools: []string{"LaneProbe"}}}
	probe = &laneProbeTool{}
	srv, sched = lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": host}, probe)
	sched.Reload(map[string]concurrency.ProviderLanesConfig{"P": {Lanes: map[string]concurrency.LaneConfig{
		config.ModelKindChat:     {MaxConcurrent: 1},
		config.ModelKindEmbedder: {MaxConcurrent: 1},
	}}})
	emb = &countingEmbedder{}
	probe.embedder = lanegate.WrapEmbedder("P", emb, sched)
	release, err := sched.Acquire(context.Background(), "P", config.ModelKindEmbedder, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the embedder slot: %v", err)
	}
	return srv, sched, probe, emb, host, release
}

func waitEmbedderLane(t *testing.T, sched *concurrency.CallScheduler, what string, ok func(concurrency.LaneStats) bool) concurrency.LaneStats {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if st := sched.Stats()["P"].Lanes[config.ModelKindEmbedder]; ok(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the embedder lane never reached %s: %+v", what, sched.Stats()["P"].Lanes[config.ModelKindEmbedder])
	return concurrency.LaneStats{}
}

// TestServiceLane_AToolsEmbeddingQueuesAsItsRun — a live run's tool call
// carries the run's identity to the lane, queues as foreground work, and holds
// no chat slot while it waits: the run is in a tool, not in a model call.
func TestServiceLane_AToolsEmbeddingQueuesAsItsRun(t *testing.T) {
	srv, sched, probe, emb, host, release := probeServer(t)
	ts := httptest.NewServer(srv.Mux())
	var once sync.Once
	free := func() { once.Do(release) }
	t.Cleanup(ts.Close)
	t.Cleanup(free) // LIFO: the slot is freed before the server closes

	body := make(chan string, 1)
	go func() { body <- postLaneRun(t, ts, laneRunBody("tooler", "tooler-1")) }()
	st := waitEmbedderLane(t, sched, "the tool's embedding queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if st.QueuedBackground != 0 {
		t.Errorf("a live run's embedding queued as background: %+v", st)
	}
	if n := emb.calls.Load(); n != 0 {
		t.Errorf("the embedder was called %d times while its call was queued", n)
	}
	if chat := chatLaneOf(sched, "P"); chat.Active != 0 {
		t.Errorf("chat lane active = %d while the run waits inside a tool, want 0", chat.Active)
	}

	free()
	if got := <-body; !strings.Contains(got, "tooler done") {
		t.Fatalf("the run did not finish after its embedding was served; body=\n%s", got)
	}
	meta, hasMeta, background := probe.seen()
	if !hasMeta || meta.RunID != "tooler-1" {
		t.Errorf("the tool's ctx carries run identity %+v (present=%v), want the run tooler-1 — its calls would queue as nobody's", meta, hasMeta)
	}
	if background {
		t.Error("a live run's tool ctx is marked background")
	}
	if n := emb.calls.Load(); n != 1 {
		t.Errorf("the embedder was called %d times, want 1", n)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for i, bg := range host.background {
		if bg {
			t.Errorf("chat call %d of a live run arrived marked background", i)
		}
	}
}

// TestServiceLane_ARunStartedAsMaintenanceQueuesAsBackground — a run started
// under a ctx marked lanegate.WithBackground (a consolidation pass) carries the
// mark to everything it does: its own chat calls and the calls its tools make.
// The mark is set once, on the root ctx, and nothing below has to repeat it.
func TestServiceLane_ARunStartedAsMaintenanceQueuesAsBackground(t *testing.T) {
	srv, sched, probe, _, host, release := probeServer(t)
	var once sync.Once
	free := func() { once.Do(release) }
	t.Cleanup(free)

	done := make(chan error, 1)
	go func() {
		done <- srv.RunOnce(lanegate.WithBackground(context.Background()), runner.RunInput{
			Agent: "tooler", AgentID: "sweep-1",
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		}, runner.RunCallbacks{})
	}()
	st := waitEmbedderLane(t, sched, "the tool's embedding queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if st.QueuedBackground != 1 {
		t.Errorf("a maintenance run's embedding queued as foreground: %+v", st)
	}
	free()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run never finished")
	}
	if _, _, background := probe.seen(); !background {
		t.Error("the tool's ctx lost the background mark")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.background) != 2 {
		t.Fatalf("the run made %d chat calls, want 2", len(host.background))
	}
	for i, bg := range host.background {
		if !bg {
			t.Errorf("chat call %d of a maintenance run arrived unmarked: it would queue ahead of live runs", i)
		}
	}
}
