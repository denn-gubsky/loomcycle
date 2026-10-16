package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

func newMetricsPromServer(t *testing.T, withPerUserCap bool) *Server {
	t.Helper()
	sem := concurrency.New(8, 16, 30*time.Second)
	if withPerUserCap {
		sem = sem.WithPerUserCap(4)
	}
	hookReg := hooks.NewSet()
	return &Server{
		cfgHolder:      config.NewHolder(&config.Config{}),
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            sem,
	}
}

// TestMetricsProm_EmitsExpectedSeries pins the metric set the
// observability-profiles RFC locks: 5 always-on series + build_info.
// Per-user series stays absent when the per-user cap is disabled
// (cardinality guard).
func TestMetricsProm_EmitsExpectedSeries(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	wantSeries := []string{
		"loomcycle_process_rss_bytes",
		"loomcycle_process_heap_alloc_bytes",
		"loomcycle_process_goroutines",
		"loomcycle_concurrency_active",
		"loomcycle_concurrency_queued",
		"loomcycle_concurrency_lent",
		"loomcycle_build_info",
	}
	for _, name := range wantSeries {
		if !strings.Contains(body, "# TYPE "+name+" gauge") {
			t.Errorf("missing TYPE line for %q in body:\n%s", name, body)
		}
		if !strings.Contains(body, name+" ") && !strings.Contains(body, name+"{") {
			t.Errorf("missing sample for %q", name)
		}
	}
	// per-user series should be absent when cap is 0
	if strings.Contains(body, "loomcycle_concurrency_per_user") {
		t.Errorf("loomcycle_concurrency_per_user should be omitted when per-user cap is disabled; got: %s", body)
	}
}

// TestMetricsProm_PerUserSeriesOnlyWhenCapEnabled pins the cardinality
// guard: per-user labels only appear when the operator opted into
// per-user fairness. Without this, anonymous workloads would explode
// cardinality with no operator-visible knob.
func TestMetricsProm_PerUserSeriesOnlyWhenCapEnabled(t *testing.T) {
	srv := newMetricsPromServer(t, true)
	// Acquire a slot for a specific user_id so PerUser is populated.
	rel, err := srv.sem.AcquireForUser(t.Context(), "", "alice")
	if err != nil {
		t.Fatalf("AcquireForUser failed: %v", err)
	}
	defer rel()

	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "loomcycle_concurrency_per_user{user_id=\"alice\"} 1") {
		t.Errorf("expected per-user series for alice, body:\n%s", body)
	}
}

// TestMetricsProm_NilSemReturnsZeros pins the safety path: a Server
// missing the semaphore (early init, certain test fixtures) emits
// concurrency=0 rather than 500-ing.
func TestMetricsProm_NilSemReturnsZeros(t *testing.T) {
	srv := &Server{cfgHolder: config.NewHolder(&config.Config{}), sem: nil}
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "loomcycle_concurrency_active 0") {
		t.Errorf("expected active=0 with nil sem; body:\n%s", body)
	}
	if !strings.Contains(body, "loomcycle_concurrency_queued 0") {
		t.Errorf("expected queued=0 with nil sem; body:\n%s", body)
	}
	if !strings.Contains(body, "loomcycle_concurrency_lent 0") {
		t.Errorf("expected lent=0 with nil sem; body:\n%s", body)
	}
}

// A run that lent its slot while its model call waits for a provider lane holds
// no slot and is not queued, yet it counts against the queue bound. Without its
// own series, a scrape showed spare queue room that admission would refuse.
func TestMetricsProm_LentSeriesCountsALentRun(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	slot := &concurrency.RunSlot{}
	rel, err := srv.sem.Admit(t.Context(), concurrency.AdmitRequest{Slot: slot})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer rel()
	if !slot.Lend() {
		t.Fatal("Lend refused with an empty queue")
	}

	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"loomcycle_concurrency_lent 1", "loomcycle_concurrency_active 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; body:\n%s", want, body)
		}
	}
}

// TestMetricsProm_ReplicaIDLabelWhenSet pins the cluster-mode label:
// LOOMCYCLE_REPLICA_ID populates a replica_id label on every series.
// Single-replica deployments (env unset) omit the label entirely so
// the output stays clean.
func TestMetricsProm_ReplicaIDLabelWhenSet(t *testing.T) {
	t.Setenv("LOOMCYCLE_REPLICA_ID", "node-2")
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `loomcycle_process_goroutines{replica_id="node-2"}`) {
		t.Errorf("expected replica_id label on process series; body:\n%s", body)
	}
	if !strings.Contains(body, `replica_id="node-2"`) {
		t.Errorf("build_info should also carry replica_id; body:\n%s", body)
	}
}

// TestMetricsProm_ProviderGateSeries pins the per-provider gauges: slots-in-use
// + queue-depth appear (labelled by provider) only when a provider caps its
// chat calls, count CALLS in that lane, and stay absent otherwise (empty-series
// guard) — including for a provider that caps only another kind of call.
func TestMetricsProm_ProviderGateSeries(t *testing.T) {
	// No gates wired → the provider gauges must be absent.
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "loomcycle_provider_slots_in_use") {
		t.Errorf("provider gauges should be omitted when no provider is capped; body:\n%s", rec.Body.String())
	}

	// Cap provider "P" at 2 and hold one slot → slots_in_use{P}=1, queue_depth{P}=0.
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"P":        {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 2}}},
		"decision": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: 1}}},
	})
	srv.SetCallScheduler(sched)
	rel, err := sched.Acquire(t.Context(), "P", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("acquire P: %v", err)
	}
	defer rel()

	rec = httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "# TYPE loomcycle_provider_slots_in_use gauge") {
		t.Errorf("missing slots_in_use TYPE line; body:\n%s", body)
	}
	if !strings.Contains(body, `loomcycle_provider_slots_in_use{provider="P"} 1`) {
		t.Errorf("expected slots_in_use{P}=1; body:\n%s", body)
	}
	if !strings.Contains(body, `loomcycle_provider_queue_depth{provider="P"} 0`) {
		t.Errorf("expected queue_depth{P}=0; body:\n%s", body)
	}
	for _, chatOnly := range []string{"loomcycle_provider_slots_in_use", "loomcycle_provider_queue_depth"} {
		if strings.Contains(body, chatOnly+`{provider="decision"}`) {
			t.Errorf("a provider with no chat lane must not appear in the chat-call gauge %s; body:\n%s", chatOnly, body)
		}
	}
}

// TestMetricsProm_LaneSeriesCoverEveryLaneByKind — the two provider gauges above
// are the chat lane alone. Every lane, whatever its kind, has its own series
// labelled by provider and kind, and each provider reports how many distinct
// models are computing and how many calls wait for their model. Without them a
// saturated decision or embedder lane is invisible.
func TestMetricsProm_LaneSeriesCoverEveryLaneByKind(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "loomcycle_provider_lane_") || strings.Contains(rec.Body.String(), "loomcycle_provider_model") {
		t.Fatalf("lane series must be omitted when no provider caps anything; body:\n%s", rec.Body.String())
	}

	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"P": {
			Lanes: map[string]concurrency.LaneConfig{
				config.ModelKindChat:     {MaxConcurrent: 2, AffinityHold: time.Minute},
				config.ModelKindEmbedder: {MaxConcurrent: 1},
			},
			MaxModelsInFlight: 1,
		},
	})
	srv.SetCallScheduler(sched)
	ctx := t.Context()
	// A run's chat call that has ended keeps its slot reserved (the hold).
	relChat, err := sched.Acquire(ctx, "P", config.ModelKindChat, concurrency.CallInfo{Owner: "run-1", Model: "chat-m"})
	if err != nil {
		t.Fatal(err)
	}
	relChat()
	// One embedding in flight, one background embedding queued behind it. Both
	// are the same model, so the model cap admits them; the lane holds one.
	relEmb, err := sched.Acquire(ctx, "P", config.ModelKindEmbedder, concurrency.CallInfo{Model: "embed-m"})
	if err != nil {
		t.Fatal(err)
	}
	defer relEmb()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if rel, err := sched.Acquire(waitCtx, "P", config.ModelKindEmbedder, concurrency.CallInfo{Model: "embed-m", Background: true}); err == nil {
			rel()
		}
	}()
	// A call to a second model waits at the model cap.
	go func() {
		if rel, err := sched.Acquire(waitCtx, "P", config.ModelKindChat, concurrency.CallInfo{Model: "other-m"}); err == nil {
			rel()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := sched.Stats()["P"]
		if st.Lanes[config.ModelKindEmbedder].QueuedBackground == 1 && st.Models.Waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached one background embedding queued and one model waiter: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec = httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE loomcycle_provider_lane_slots_in_use gauge",
		"# TYPE loomcycle_provider_lane_grants_total counter",
		`loomcycle_provider_lane_max_concurrent{kind="chat",provider="P"} 2`,
		`loomcycle_provider_lane_slots_in_use{kind="chat",provider="P"} 0`,
		`loomcycle_provider_lane_slots_reserved{kind="chat",provider="P"} 1`,
		`loomcycle_provider_lane_grants_total{kind="chat",provider="P"} 1`,
		`loomcycle_provider_lane_max_concurrent{kind="embedder",provider="P"} 1`,
		`loomcycle_provider_lane_slots_in_use{kind="embedder",provider="P"} 1`,
		`loomcycle_provider_lane_queue_depth{kind="embedder",provider="P"} 1`,
		`loomcycle_provider_lane_queue_depth_background{kind="embedder",provider="P"} 1`,
		`loomcycle_provider_lane_preemptions_total{kind="embedder",provider="P"} 0`,
		`loomcycle_provider_models_in_flight{provider="P"} 1`,
		`loomcycle_provider_model_waiters{provider="P"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; body:\n%s", want, body)
		}
	}
}

// TestMetricsProm_TextFormatShape pins the Prometheus exposition
// format invariants: every metric line that isn't a comment must
// match `<name>[{labels}] <value>` and every named series must have
// a preceding HELP + TYPE line. Lightweight in-package check —
// avoids pulling in prometheus/common just for testing.
func TestMetricsProm_TextFormatShape(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	seen := map[string]struct {
		help bool
		typ  bool
	}{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)[0]
			s := seen[name]
			s.help = true
			seen[name] = s
		case strings.HasPrefix(line, "# TYPE "):
			name := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)[0]
			s := seen[name]
			s.typ = true
			seen[name] = s
		case strings.HasPrefix(line, "#"):
			// other comment — ignore
		default:
			// must be `name[{labels}] value`
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				t.Errorf("malformed sample line: %q", line)
				continue
			}
			name := parts[0]
			if i := strings.IndexByte(name, '{'); i >= 0 {
				name = name[:i]
			}
			if _, ok := seen[name]; !ok {
				t.Errorf("sample %q has no preceding HELP/TYPE", name)
			}
		}
	}

	for name, flags := range seen {
		if !flags.help {
			t.Errorf("metric %q missing HELP line", name)
		}
		if !flags.typ {
			t.Errorf("metric %q missing TYPE line", name)
		}
	}
}

// TestMetricsProm_BodyTrailingNewline pins a subtle but real
// Prometheus expectation — the response must end with a newline.
// Some scrapers reject responses that don't.
func TestMetricsProm_BodyTrailingNewline(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("body should end with a newline; ends with %q", body[len(body)-min(20, len(body)):])
	}
}

// TestMetricsProm_ContentType pins the standard Prometheus
// text-format content-type. Scrapers content-negotiate against this.
func TestMetricsProm_ContentType(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8", got)
	}
}

// _silenceUnused ensures io is referenced even if a future refactor
// drops uses; required because the package builds under -race with
// strict imports.
var _silenceUnused = io.Discard
