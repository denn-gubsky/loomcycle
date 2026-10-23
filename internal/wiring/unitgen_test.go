package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// TestUnitGenerator_ResolvesTheBlockLikeTheReranker — the shared service construction:
// an alias resolves, and an overridden endpoint answers only to the generator's
// own credential name.
func TestUnitGenerator_ResolvesTheBlockLikeTheReranker(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
		Models:    map[string]config.ModelRef{"writer": {Provider: "openai", Model: "gpt-5.4-mini"}},
		Memory:    config.MemoryConfig{UnitGenerator: config.UnitGeneratorConfig{Model: "writer", BaseURL: "http://gpu.internal:8000/v1"}},
	}
	g, err := UnitGenerator(cfg, nil)
	if err != nil || g == nil {
		t.Fatalf("UnitGenerator = %v, %v", g, err)
	}
	if g.ModelID() != "gpt-5.4-mini" {
		t.Errorf("model = %q", g.ModelID())
	}
	// The driver is inside its provider's chat lane, so it is reached as every
	// registry provider is: through providers.As.
	p, _, err := unitGeneratorDriver(cfg, nil)
	if err != nil {
		t.Fatalf("unitGeneratorDriver: %v", err)
	}
	keyed, ok := providers.As[providers.KeyedProvider](p)
	if !ok {
		t.Fatal("the openai driver no longer reports its credential name")
	}
	if k := keyed.KeyEnvName(); k != unitGeneratorKeyEnvName {
		t.Errorf("credential name = %q, want %q", k, unitGeneratorKeyEnvName)
	}
	if none, err := UnitGenerator(&config.Config{}, nil); none != nil || err != nil {
		t.Errorf("no block: %v, %v", none, err)
	}
}

// TestUnitGenerator_AGenerationCallIsCountedInItsProvidersChatLane — the
// generator has a driver of its own, so wiring puts that driver in the chat
// lane of the provider the block resolved to: with the lane at 1, four
// generations asked for at once reach the endpoint one call at a time. With no
// scheduler they overlap.
func TestUnitGenerator_AGenerationCallIsCountedInItsProvidersChatLane(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	generateAll := func(sched *concurrency.CallScheduler) int {
		t.Helper()
		var mu sync.Mutex
		inFlight, peak, hits := 0, 0, 0
		// It refuses every request: what is measured is how many calls were AT
		// the endpoint together, not what came back.
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
			http.Error(w, "not today", http.StatusTeapot)
		}))
		defer srv.Close()
		g, err := UnitGenerator(&config.Config{
			Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
			Models:    map[string]config.ModelRef{"writer": {Provider: "openai", Model: "gpt-5.4-mini"}},
			Memory:    config.MemoryConfig{UnitGenerator: config.UnitGeneratorConfig{Model: "writer", BaseURL: srv.URL + "/v1"}},
		}, sched)
		if err != nil || g == nil {
			t.Fatalf("UnitGenerator = %v, %v", g, err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Each call is refused, so Generate tries it attempts times: what
				// is measured is how many reach the endpoint at once.
				_, _ = g.Generate(context.Background(), memory.UnitRequest{Text: "x", Kinds: []string{"description"}})
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
		"openai": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	if peak := generateAll(sched); peak != 1 {
		t.Errorf("gated: %d generation calls at the endpoint at once, want 1 (providers.openai.max_concurrent)", peak)
	}
	if st := sched.Stats()["openai"].Lanes[config.ModelKindChat]; st.Grants < 4 || st.Active != 0 || st.Queued != 0 {
		t.Errorf("chat lane = %+v, want the 4 calls granted and nothing left in it", st)
	}
	// Control: the same four with no lane overlap, so the 1 above is the lane.
	if peak := generateAll(nil); peak < 2 {
		t.Errorf("ungated: peak %d, want the calls to overlap — the check above proves nothing", peak)
	}
}
