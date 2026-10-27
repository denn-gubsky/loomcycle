package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestConcurrencyStats_ReportsCallsHeldBackByTheirModelCap — a call waiting
// for its model's own cap (model_caps) beside a free slot is counted as queued
// AND as blocked_on_model, on the JSON the Lanes page reads and on
// the Prometheus series; without the second count a queued call next to a free
// slot reads as a scheduler stall.
func TestConcurrencyStats_ReportsCallsHeldBackByTheirModelCap(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "modelcap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(makeBaseConfig(), &stubResolver{p: nil}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"P": {Lanes: map[string]concurrency.LaneConfig{
			config.ModelKindChat: {MaxConcurrent: 4, ModelCaps: map[string]int{"one-slot": 1}},
		}},
	})
	srv.SetCallScheduler(sched)
	rel, err := sched.Acquire(t.Context(), "P", config.ModelKindChat, concurrency.CallInfo{Model: "one-slot@65536"})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	waitCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		if r, err := sched.Acquire(waitCtx, "P", config.ModelKindChat, concurrency.CallInfo{Model: "one-slot@65536"}); err == nil {
			r()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for sched.Stats()["P"].Lanes[config.ModelKindChat].Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second call to the one-slot model never queued")
		}
		time.Sleep(time.Millisecond)
	}

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_concurrency/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"blocked_on_model":1`) {
		t.Errorf("body has no blocked_on_model 1: %s", rec.Body.String())
	}
	var got concurrencyStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if l := got.Providers["P"].Lanes[config.ModelKindChat]; l.Active != 1 || l.Queued != 1 || l.BlockedOnModel != 1 {
		t.Errorf("lanes.chat = %+v, want one in flight, one queued and held back by its model", l)
	}

	rec = httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	if want := `loomcycle_provider_lane_queue_depth_blocked_on_model{kind="chat",provider="P"} 1`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics missing %q; body:\n%s", want, rec.Body.String())
	}
}
