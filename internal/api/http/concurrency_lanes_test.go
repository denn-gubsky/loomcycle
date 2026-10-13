package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestConcurrencyStats_ReportsEveryLaneAndTheModelCap — the providers block
// keeps its two chat-lane counts (the contract it has always had) and adds, per
// provider, every lane by kind and the calls its cap on models in flight is
// counting. A provider that caps only a non-chat kind is listed too: before,
// its saturated embedder or decision lane was invisible here.
func TestConcurrencyStats_ReportsEveryLaneAndTheModelCap(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "lanestats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(makeBaseConfig(), &stubResolver{p: nil}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"P": {
			Lanes: map[string]concurrency.LaneConfig{
				config.ModelKindChat:     {MaxConcurrent: 2},
				config.ModelKindEmbedder: {MaxConcurrent: 1},
			},
			MaxModelsInFlight: 2,
		},
		"E": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindEmbedder: {MaxConcurrent: 3}}},
	})
	srv.SetCallScheduler(sched)
	relChat, err := sched.Acquire(t.Context(), "P", config.ModelKindChat, concurrency.CallInfo{Model: "chat-m@65536"})
	if err != nil {
		t.Fatal(err)
	}
	defer relChat()
	relEmb, err := sched.Acquire(t.Context(), "P", config.ModelKindEmbedder, concurrency.CallInfo{Model: "embed-m"})
	if err != nil {
		t.Fatal(err)
	}
	defer relEmb()

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_concurrency/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got concurrencyStatsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := got.Providers["P"]
	if p.Active != 1 || p.Queued != 0 {
		t.Errorf("providers.P = active %d queued %d, want the chat lane's 1/0", p.Active, p.Queued)
	}
	if l := p.Lanes[config.ModelKindChat]; l.MaxConcurrent != 2 || l.Active != 1 || l.Grants != 1 {
		t.Errorf("providers.P.lanes.chat = %+v, want cap 2, one in flight, one grant", l)
	}
	if l := p.Lanes[config.ModelKindEmbedder]; l.MaxConcurrent != 1 || l.Active != 1 {
		t.Errorf("providers.P.lanes.embedder = %+v, want cap 1 and one in flight", l)
	}
	if p.ModelsInFlight["chat-m@65536"] != 1 || p.ModelsInFlight["embed-m"] != 1 || p.ModelWaiters != 0 {
		t.Errorf("providers.P models = %+v waiters %d, want both models in flight once and nobody waiting", p.ModelsInFlight, p.ModelWaiters)
	}
	e, ok := got.Providers["E"]
	if !ok || e.Active != 0 || e.Queued != 0 || e.Lanes[config.ModelKindEmbedder].MaxConcurrent != 3 || len(e.Lanes) != 1 {
		t.Errorf("providers.E = %+v (listed: %v), want a provider that caps only embeddings listed with that lane", e, ok)
	}
}
