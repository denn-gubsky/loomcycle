package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func postEmbeddings(ts *httptest.Server) <-chan gatewayResult {
	out := make(chan gatewayResult, 1)
	go func() {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(ts.URL+"/v1/embeddings", "application/json",
			strings.NewReader(`{"model":"embed-m","input":"hello"}`))
		if err != nil {
			out <- gatewayResult{body: err.Error()}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out <- gatewayResult{resp.StatusCode, string(raw)}
	}()
	return out
}

// TestEmbeddings_RequestWaitingForTheEmbedderHoldsNoGlobalSlot — POST
// /v1/embeddings is sent directly, outside any run, like the LLM gateway, and
// takes its slots the same way. With the embedder lane at one slot and
// provider_queue_depth 1: a request that finds the embedder busy waits for it
// holding NO global slot, the next one is refused with 429
// provider_concurrency_exhausted, and the waiting one completes — its Embed
// taking no second slot — once the embedder frees up.
//
// Waiting on a global slot instead, a handful of requests queued on a busy
// embedder would use the global cap up for every tenant.
func TestEmbeddings_RequestWaitingForTheEmbedderHoldsNoGlobalSlot(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Concurrency.ProviderQueueDepth = 1
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": &heldProvider{gate: make(chan struct{})}})
	sched.Reload(map[string]concurrency.ProviderLanesConfig{"P": {Lanes: map[string]concurrency.LaneConfig{
		config.ModelKindEmbedder: {MaxConcurrent: 1},
	}}})
	emb := &countingEmbedder{}
	// The health observer goes around the lane wrapper, as main wires it.
	srv.SetEmbedder(providers.ObserveEmbedder(lanegate.WrapEmbedder("P", emb, sched)))
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	busy, err := sched.Acquire(context.Background(), "P", config.ModelKindEmbedder, concurrency.CallInfo{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := postEmbeddings(ts)
	waitEmbedderLane(t, sched, "one call in flight and one queued", func(st concurrency.LaneStats) bool {
		return st.Active == 1 && st.Queued == 1
	})
	if st := srv.sem.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Fatalf("global slots while a request waits for the embedder: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}

	refused := <-postEmbeddings(ts)
	if refused.status != http.StatusTooManyRequests || !strings.Contains(refused.body, `"provider_concurrency_exhausted"`) {
		t.Fatalf("request past the bound: status=%d body=%s, want 429 provider_concurrency_exhausted", refused.status, refused.body)
	}
	if emb.calls.Load() != 0 {
		t.Fatalf("the embedder was called %d times while its slot was taken", emb.calls.Load())
	}

	busy()
	if r := <-waiting; r.status != http.StatusOK {
		t.Fatalf("the waiting request: status=%d body=%s, want 200", r.status, r.body)
	}
	if emb.calls.Load() != 1 {
		t.Fatalf("embedder calls = %d, want 1", emb.calls.Load())
	}
	waitEmbedderLane(t, sched, "idle", func(st concurrency.LaneStats) bool { return st.Active == 0 && st.Queued == 0 })
	if st := srv.sem.Stats(); st.Active != 0 {
		t.Errorf("global active at the end = %d, want 0", st.Active)
	}
}
