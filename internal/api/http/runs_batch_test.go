package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// latchProvider models a run that takes measurable time and records the
// high-water mark of concurrently in-flight Call()s — so a fan-out test can
// prove children ran CONCURRENTLY (max-in-flight > 1) rather than serialized.
//
// rendezvous, when set, makes each Call wait (up to rendezvousWait) until that
// many calls have been in flight together before it dwells. Without it the
// overlap depended on two children starting within one dwell of each other,
// which a slow race-detector runner does not guarantee: CI saw max-in-flight 1
// from a fan-out that was concurrent. A serialized fan-out still never reaches
// the rendezvous, so each of its calls waits out the timeout and the test
// fails as before.
type latchProvider struct {
	dwell      time.Duration
	rendezvous int32
	inFlt      atomic.Int32
	maxSeen    atomic.Int32
}

const rendezvousWait = 2 * time.Second

func (p *latchProvider) ID() string                    { return "stub" }
func (p *latchProvider) Probe(_ context.Context) error { return nil }
func (p *latchProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *latchProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *latchProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	n := p.inFlt.Add(1)
	for { // track the concurrent high-water mark
		old := p.maxSeen.Load()
		if n <= old || p.maxSeen.CompareAndSwap(old, n) {
			break
		}
	}
	if p.rendezvous > 0 {
		deadline := time.Now().Add(rendezvousWait)
		for p.maxSeen.Load() < p.rendezvous && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-time.After(p.dwell):
	case <-ctx.Done():
	}
	p.inFlt.Add(-1)
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

func newBatchTestServer(t *testing.T, p providers.Provider, maxRuns int) *Server {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"r": {Model: "stub-model", SystemPrompt: "be brief"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: maxRuns, MaxQueueDepth: maxRuns, QueueTimeoutMS: 2000},
	}
	cfg.Env.AuthToken = "" // open mode
	sem := concurrency.New(maxRuns, maxRuns, 2*time.Second)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(cfg, &stubResolver{p: p}, []tools.Tool{}, sem, st)
}

func oneUserSeg(text string) []loop.PromptSegment {
	return []loop.PromptSegment{{
		Role:    "user",
		Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}},
	}}
}

// TestSpawnRunBatch_RejectsMalformed pins the request-validation guards: an
// empty batch, an over-cap batch, and an unsupported mode all error BEFORE any
// child is spawned (so a bare server with no provider/store suffices).
func TestSpawnRunBatch_RejectsMalformed(t *testing.T) {
	s := &Server{}
	ctx := context.Background()

	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{}); err == nil {
		t.Error("empty batch: want error, got nil")
	}

	over := make([]connector.SpawnRunRequest, connector.MaxBatchSpawns+1)
	for i := range over {
		over[i] = connector.SpawnRunRequest{Agent: "r", Segments: oneSegment()}
	}
	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Spawns: over}); err == nil {
		t.Errorf("over-cap (%d) batch: want error, got nil", len(over))
	}

	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneSegment()}},
		Mode:   "fire-and-forget",
	}); err == nil {
		t.Error("unknown mode: want error, got nil")
	}

	// A detached batch has no join for timeout_ms to bound; accepting it
	// silently would let a caller believe its runs are time-limited.
	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Spawns:    []connector.SpawnRunRequest{{Agent: "r", Segments: oneSegment()}},
		Mode:      "detach",
		TimeoutMS: 1000,
	}); err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Errorf("mode=detach with timeout_ms: want an error naming timeout_ms, got %v", err)
	}
}

// TestSpawnRunBatch_FanOutConcurrentAndInEnvelopeError proves the join: N
// children run CONCURRENTLY (max-in-flight > 1, wall-clock ≈ one dwell not N×),
// the envelope is index-aligned, and a bad-agent child surfaces as a failed
// result WITHOUT failing the batch.
func TestSpawnRunBatch_FanOutConcurrentAndInEnvelopeError(t *testing.T) {
	p := &latchProvider{dwell: 80 * time.Millisecond, rendezvous: 2}
	s := newBatchTestServer(t, p, 8)

	req := connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
		{Agent: "r", Segments: oneUserSeg("a")},
		{Agent: "r", Segments: oneUserSeg("b")},
		{Agent: "nonexistent", Segments: oneUserSeg("c")}, // in-envelope failure
		{Agent: "r", Segments: oneUserSeg("d")},
		{Agent: "r", Segments: oneUserSeg("e")},
	}}

	res, err := s.SpawnRunBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("SpawnRunBatch: %v", err)
	}
	if res.Spawned != 5 || len(res.Results) != 5 {
		t.Fatalf("Spawned=%d len(Results)=%d, want 5/5", res.Spawned, len(res.Results))
	}
	// The bad-agent child (index 2) failed in-envelope; the others completed.
	if res.Results[2].Status != "failed" || res.Results[2].Error == "" {
		t.Errorf("results[2] = %+v, want a failed status with an error (unknown agent)", res.Results[2])
	}
	for _, i := range []int{0, 1, 3, 4} {
		if res.Results[i].Status != "completed" {
			t.Errorf("results[%d].Status = %q, want completed", i, res.Results[i].Status)
		}
	}
	// Concurrency: the valid children overlapped in flight (max-in-flight >= 2).
	// This is the deterministic proof; an earlier wall-clock ceiling was dropped
	// because the race detector's goroutine overhead inflates the concurrent
	// wall-clock toward the serial sum, making any timing bound flaky.
	if mx := p.maxSeen.Load(); mx < 2 {
		t.Errorf("max concurrent in-flight = %d, want >= 2 (fan-out serialized?)", mx)
	}
}

// TestRunsBatch_HTTPEndpoint exercises POST /v1/runs:batch end-to-end (also
// proves the colon path routes through ServeMux) and returns the envelope; an
// over-cap body is a 400.
func TestRunsBatch_HTTPEndpoint(t *testing.T) {
	p := &latchProvider{dwell: 5 * time.Millisecond}
	s := newBatchTestServer(t, p, 8)

	body := `{"spawns":[
		{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"a"}]}]},
		{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"b"}]}]}
	]}`
	rec := doJSON(t, s, "POST", "/v1/runs:batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got connector.BatchSpawnResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if got.Spawned != 2 || len(got.Results) != 2 {
		t.Fatalf("Spawned=%d len(Results)=%d, want 2/2", got.Spawned, len(got.Results))
	}
	for i, r := range got.Results {
		if r.RunID == "" || r.Status != "completed" {
			t.Errorf("results[%d] = %+v, want a completed run with a run_id", i, r)
		}
	}

	// Over-cap → 400.
	var big strings.Builder
	big.WriteString(`{"spawns":[`)
	for i := 0; i <= connector.MaxBatchSpawns; i++ {
		if i > 0 {
			big.WriteString(",")
		}
		big.WriteString(`{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`)
	}
	big.WriteString(`]}`)
	if rec := doJSON(t, s, "POST", "/v1/runs:batch", big.String()); rec.Code != http.StatusBadRequest {
		t.Errorf("over-cap status = %d, want 400", rec.Code)
	}
}

func oneSegment() []loop.PromptSegment {
	return []loop.PromptSegment{{
		Role:    "user",
		Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}},
	}}
}

// A fan-out child with no prompt reaches the model as a NULL user turn: it gets
// the system prompt, answers whatever that implies, and COMPLETES — so the
// caller reads a green envelope and the emptiness is visible only in the
// thinking trace. handleSpawnRun has refused this since F47 and handleRuns for
// longer; both batch surfaces accepted it, so the identical caller mistake was a
// 422 on one path and a silently-empty run on another.
//
// The guard is in SpawnRunBatch rather than in each handler because there are
// two of them and POST /v1/runs:batch performs no per-child validation at all.
func TestSpawnRunBatch_RefusesAChildWithNoPrompt(t *testing.T) {
	s := &Server{}
	ctx := context.Background()

	_, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Spawns: []connector.SpawnRunRequest{
			{Agent: "r", Segments: oneSegment()},
			{Agent: "r"}, // no prompt
		},
	})
	if err == nil {
		t.Fatal("a child with no segments was accepted — it would have run against an empty " +
			"prompt and reported success")
	}
	// The message has to name WHICH child and show the shape, because the field
	// is easy to get wrong and the caller has up to 32 of them.
	if !strings.Contains(err.Error(), "spawns[1]") {
		t.Errorf("error does not name the offending child: %v", err)
	}
	if !strings.Contains(err.Error(), "trusted-text") {
		t.Errorf("error does not show the shape that would work: %v", err)
	}
}

// Both surfaces inherit it — that is the point of putting it in the shared
// method rather than in the handler someone happened to be looking at.
func TestRunsBatch_HTTPEndpointRefusesAChildWithNoPrompt(t *testing.T) {
	_, ts, _, _ := routedServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs:batch", "application/json",
		strings.NewReader(`{"spawns":[{"agent":"router"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(string(body), "segments is required") {
		t.Errorf("body does not explain the refusal: %s", strings.TrimSpace(string(body)))
	}
}

// newGatedBatchServer is newBatchTestServer driven by a gatedProvider, so a
// child stays running until the test releases it.
func newGatedBatchServer(t *testing.T, maxRuns int) (*Server, chan struct{}) {
	t.Helper()
	gate := make(chan struct{}, 8)
	return newBatchTestServer(t, &gatedProvider{text: "partial", gate: gate}, maxRuns), gate
}

// A detached batch returns while its children are still running — each with
// the handle a caller reads it by — and a child that cannot start is reported
// in its own slot, as a join reports it. The children then run to completion
// after the caller has gone: the call's ctx ending does not cancel them.
func TestSpawnRunBatch_DetachReturnsHandlesAndRunsOutliveTheCaller(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)
	ctx, cancelCaller := context.WithCancel(context.Background())

	res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Mode: "detach",
		Spawns: []connector.SpawnRunRequest{
			{Agent: "r", Segments: oneUserSeg("a")},
			{Agent: "nonexistent", Segments: oneUserSeg("b")},
			{Agent: "r", Segments: oneUserSeg("c")},
		},
	})
	if err != nil {
		t.Fatalf("SpawnRunBatch(detach): %v", err)
	}
	// The gate is still shut, so had the call joined it could not have returned.
	if res.Spawned != 3 || len(res.Results) != 3 {
		t.Fatalf("Spawned=%d len(Results)=%d, want 3/3", res.Spawned, len(res.Results))
	}
	for _, i := range []int{0, 2} {
		r := res.Results[i]
		if r.Status != "running" || r.RunID == "" || r.AgentID == "" || r.SessionID == "" {
			t.Errorf("results[%d] = %+v, want a running child with run, agent and session ids", i, r)
		}
	}
	if r := res.Results[1]; r.Status != "failed" || r.Error == "" || r.RunID != "" {
		t.Errorf("results[1] = %+v, want an unstarted child failed in its slot", r)
	}

	// The caller leaves; the runs do not.
	cancelCaller()
	time.Sleep(50 * time.Millisecond)
	for _, i := range []int{0, 2} {
		waitRunStatus(t, s.store, res.Results[i].RunID, store.RunRunning)
	}
	gate <- struct{}{}
	gate <- struct{}{}
	for _, i := range []int{0, 2} {
		waitRunStatus(t, s.store, res.Results[i].RunID, store.RunCompleted)
	}
}

// A detached child holds its admission slot until it ends, as a join child
// does — otherwise a caller could keep any number of runs going past the
// concurrency gate by issuing detached batches. It stops through the cancel
// registry, like any run.
func TestSpawnRunBatch_DetachedChildHoldsItsSlotUntilCancelled(t *testing.T) {
	s, _ := newGatedBatchServer(t, 2)

	res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{
		Mode:   "detach",
		Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneUserSeg("a")}},
	})
	if err != nil {
		t.Fatalf("SpawnRunBatch(detach): %v", err)
	}
	child := res.Results[0]
	if child.Status != "running" {
		t.Fatalf("child = %+v, want running", child)
	}
	if got := s.sem.Stats().Active; got != 1 {
		t.Errorf("active admission slots after the call returned = %d, want 1 (the running child's)", got)
	}

	cr, err := s.CancelRun(context.Background(), child.AgentID, "test")
	if err != nil || !cr.Cancelled {
		t.Fatalf("CancelRun(%s) = %+v, %v; want cancelled", child.AgentID, cr, err)
	}
	waitRunStatus(t, s.store, child.RunID, store.RunCancelled)
	deadline := time.Now().Add(5 * time.Second)
	for s.sem.Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("active admission slots = %d after the child ended, want 0", s.sem.Stats().Active)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// POST /v1/runs:batch takes mode "detach" and answers with the handles; the
// run reads back through GET /v1/runs/{run_id}. timeout_ms with it is a 400.
func TestRunsBatch_HTTPEndpointDetach(t *testing.T) {
	s, gate := newGatedBatchServer(t, 8)

	body := `{"mode":"detach","spawns":[
		{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"a"}]}]}
	]}`
	rec := doJSON(t, s, "POST", "/v1/runs:batch", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got connector.BatchSpawnResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(got.Results) != 1 || got.Results[0].Status != "running" || got.Results[0].RunID == "" {
		t.Fatalf("results = %+v, want one running child with a run_id", got.Results)
	}
	runID := got.Results[0].RunID
	rec = doJSON(t, s, "GET", "/v1/runs/"+runID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"running"`) {
		t.Errorf("GET /v1/runs/%s = %d %s, want the running run", runID, rec.Code, rec.Body.String())
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, runID, store.RunCompleted)

	timed := `{"mode":"detach","timeout_ms":500,"spawns":[
		{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"a"}]}]}
	]}`
	if rec := doJSON(t, s, "POST", "/v1/runs:batch", timed); rec.Code != http.StatusBadRequest {
		t.Errorf("detach + timeout_ms status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// A caller that leaves while a detached child still waits for admission
// withdraws that child: it never starts, so no run exists that nobody holds a
// handle to. (A child that has started is not affected — see above.)
func TestSpawnRunBatch_DetachCallerLeavingBeforeAdmissionStartsNothing(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	// A queue wait far longer than the test: had the caller's leaving not
	// withdrawn the waiter, the call could only end when that wait did.
	s.sem = concurrency.New(1, 1, time.Minute)
	first, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{
		Mode:   "detach",
		Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneUserSeg("holds the only slot")}},
	})
	if err != nil || first.Results[0].Status != "running" {
		t.Fatalf("first batch = %+v, %v; want a running child", first, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	type outcome struct {
		res connector.BatchSpawnResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
			Mode:   "detach",
			Spawns: []connector.SpawnRunRequest{{Agent: "r", Segments: oneUserSeg("queued")}},
		})
		done <- outcome{res, err}
	}()
	var res connector.BatchSpawnResult
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("SpawnRunBatch(detach): %v", o.err)
		}
		res = o.res
	case <-time.After(10 * time.Second):
		gate <- struct{}{} // let the held run end so the leaked call can too
		t.Fatal("the call did not return after its caller left: the queued child was not withdrawn")
	}
	if r := res.Results[0]; r.Status != "cancelled" || r.RunID != "" {
		t.Errorf("queued child = %+v, want cancelled with no run", r)
	}
	if q := s.sem.Stats().Queued; q != 0 {
		t.Errorf("admission queue = %d after the caller left, want 0 (the waiter withdrawn)", q)
	}
	gate <- struct{}{}
	waitRunStatus(t, s.store, first.Results[0].RunID, store.RunCompleted)
}
