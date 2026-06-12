package http

import (
	"context"
	"encoding/json"
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
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// latchProvider models a run that takes measurable time and records the
// high-water mark of concurrently in-flight Call()s — so a fan-out test can
// prove children ran CONCURRENTLY (max-in-flight > 1) rather than serialized.
type latchProvider struct {
	dwell   time.Duration
	inFlt   atomic.Int32
	maxSeen atomic.Int32
}

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
		over[i] = connector.SpawnRunRequest{Agent: "r"}
	}
	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{Spawns: over}); err == nil {
		t.Errorf("over-cap (%d) batch: want error, got nil", len(over))
	}

	if _, err := s.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Spawns: []connector.SpawnRunRequest{{Agent: "r"}},
		Mode:   "detach",
	}); err == nil {
		t.Error("mode=detach: want error (needs RFC P), got nil")
	}
}

// TestSpawnRunBatch_FanOutConcurrentAndInEnvelopeError proves the join: N
// children run CONCURRENTLY (max-in-flight > 1, wall-clock ≈ one dwell not N×),
// the envelope is index-aligned, and a bad-agent child surfaces as a failed
// result WITHOUT failing the batch.
func TestSpawnRunBatch_FanOutConcurrentAndInEnvelopeError(t *testing.T) {
	p := &latchProvider{dwell: 80 * time.Millisecond}
	s := newBatchTestServer(t, p, 8)

	req := connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
		{Agent: "r", Segments: oneUserSeg("a")},
		{Agent: "r", Segments: oneUserSeg("b")},
		{Agent: "nonexistent", Segments: oneUserSeg("c")}, // in-envelope failure
		{Agent: "r", Segments: oneUserSeg("d")},
		{Agent: "r", Segments: oneUserSeg("e")},
	}}

	start := time.Now()
	res, err := s.SpawnRunBatch(context.Background(), req)
	elapsed := time.Since(start)
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
	// Concurrency: the valid children ran at once (max-in-flight > 1), so the
	// wall-clock is ~one dwell, not the serial sum (4×80ms).
	if mx := p.maxSeen.Load(); mx < 2 {
		t.Errorf("max concurrent in-flight = %d, want > 1 (fan-out serialized?)", mx)
	}
	if elapsed > 4*60*time.Millisecond {
		t.Errorf("batch wall-clock %v ≈ serial sum — children did not run concurrently", elapsed)
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
