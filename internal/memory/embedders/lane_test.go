package embedders

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestBuildGated_EmbeddingsAreCountedInTheProvidersEmbedderLane — the server's
// embedder takes the embedder lane of the provider its block RESOLVED to (here
// through a models: alias that carries the provider): with that lane at 1, four
// embeddings asked for at once reach the endpoint one at a time. Build, the
// CLI's construction, is the same embedder with no lane.
func TestBuildGated_EmbeddingsAreCountedInTheProvidersEmbedderLane(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak, hits := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(w, `{"embeddings":[[0.1,0.2,0.3]]}`)
	}))
	defer srv.Close()
	cfg := &config.Config{
		Models: map[string]config.ModelRef{"local-embedding": {Provider: "ollama", Model: "bge-m3:latest"}},
		Memory: config.MemoryConfig{Embedder: config.EmbedderConfig{Model: "local-embedding", BaseURL: srv.URL}},
	}
	embedAll := func(build func() (providers.Embedder, error)) int {
		t.Helper()
		mu.Lock()
		inFlight, peak, hits = 0, 0, 0
		mu.Unlock()
		e, err := build()
		if err != nil || e == nil {
			t.Fatalf("build = %v, %v", e, err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// The reply's shape is the driver's business; what is measured
				// is how many requests were at the endpoint at once.
				_, _ = e.Embed(context.Background(), []string{"text"})
			}()
		}
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if hits < 4 {
			t.Fatalf("the endpoint saw %d requests, want at least 4", hits)
		}
		return peak
	}

	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindEmbedder: {MaxConcurrent: 1}}},
	})
	if p := embedAll(func() (providers.Embedder, error) {
		return BuildGated(cfg, sched)
	}); p != 1 {
		t.Errorf("gated: %d embeddings at the endpoint at once, want 1 (providers.ollama.lanes.embedder.max_concurrent)", p)
	}
	if st := sched.Stats()["ollama"].Lanes[config.ModelKindEmbedder]; st.Grants != 4 || st.Active != 0 || st.Queued != 0 {
		t.Errorf("embedder lane = %+v, want 4 grants and nothing left in it", st)
	}
	// Control: the same four calls with no lane overlap, so the 1 above is the
	// lane and not an endpoint that happens to serialise.
	if p := embedAll(func() (providers.Embedder, error) {
		return Build(cfg)
	}); p < 2 {
		t.Errorf("ungated: peak %d, want the calls to overlap — the check above proves nothing", p)
	}
}

// TestBuildGated_NoEmbedderIsStillNil — an unconfigured embedder must stay a
// nil interface through the gated build: callers test `embedder == nil`.
func TestBuildGated_NoEmbedderIsStillNil(t *testing.T) {
	e, err := BuildGated(&config.Config{}, concurrency.NewCallScheduler(nil))
	if err != nil || e != nil {
		t.Fatalf("BuildGated(no block) = %#v, %v; want nil, nil", e, err)
	}
}
