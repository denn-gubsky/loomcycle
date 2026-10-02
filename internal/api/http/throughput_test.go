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

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// slowUsage is a decode-dominated call 4× slower than the reference machine:
// 100 output tokens, T_ref = 1 s + 1 s, wall 8 s. queueMs is extra wall time the
// server did not account for.
func slowUsage(provider, model string, queueMs int64) *providers.Usage {
	return &providers.Usage{OutputTokens: 100, Provider: provider, Model: model,
		Timing: &providers.CallTiming{DurationMs: 8000 + queueMs, TTFTMs: 1000, QueueMs: queueMs}}
}

// TestRecordCallUsage_PersistsTheTimingAndASeedRestoresTheEstimate — the ledger
// row carries the call's timing, the live estimate learns from it, and a fresh
// server on the same store (a restart) seeds the same estimate back.
func TestRecordCallUsage_PersistsTheTimingAndASeedRestoresTheEstimate(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(&config.Config{}, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		run := "run-" + string(rune('a'+i))
		srv.recordCallUsage(ctx, run, tools.RunIdentityValue{TenantID: "t"}, "s", 0, slowUsage("ollama-local", "qwen3.6", 250))
	}

	rows, err := st.TokenUsageForRun(ctx, "run-a")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger = %+v, %v", rows, err)
	}
	if r := rows[0]; r.DurationMs != 8250 || r.TTFTMs != 1000 || r.QueueMs != 250 {
		t.Errorf("ledger timing = duration %d ttft %d queue %d, want 8250 / 1000 / 250", r.DurationMs, r.TTFTMs, r.QueueMs)
	}
	live := srv.Throughput().Stat("ollama-local", "qwen3.6")
	if live.Samples != 6 || live.Source != "measured" || live.Slowdown < 3.99 || live.Slowdown > 4.01 {
		t.Fatalf("live estimate = %+v, want 6 measured samples at 4×", live)
	}

	restarted := New(&config.Config{}, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	if got := restarted.Throughput().Stat("ollama-local", "qwen3.6"); got.Samples != 0 {
		t.Fatalf("a new server knew %d samples before seeding", got.Samples)
	}
	if err := restarted.SeedThroughput(ctx); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Throughput().Stat("ollama-local", "qwen3.6"); got != live {
		t.Fatalf("seeded estimate = %+v, want the live one %+v", got, live)
	}
}

// TestObserveCallTiming_OffModeMeasuresNothing — with timeout_scaling off there
// is no estimate, every hook is a no-op, and the routing view omits the block.
func TestObserveCallTiming_OffModeMeasuresNothing(t *testing.T) {
	cfg := &config.Config{TimeoutScaling: config.TimeoutScaling{Mode: "off"}}
	srv := New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), nil)
	if srv.Throughput() != nil || srv.callObserver("r") != nil {
		t.Fatal("mode off built an estimate")
	}
	srv.ObserveCallTiming(slowUsage("p", "m", 0)) // must not panic
	if srv.candidateThroughput("p", "m", true) != nil {
		t.Error("mode off still reports a throughput block")
	}
}

func routingRaw(t *testing.T, srv *Server, scopes []string) (routingResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_routing", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Scopes: scopes}))
	rr := httptest.NewRecorder()
	srv.handleRouting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp routingResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp, rr.Body.String()
}

// TestRouting_ShowsEachCandidatesThroughput — every candidate carries its
// measured slowdown, sample count and the multiplier it WOULD apply; the mode
// says it is measure-only. An admin also sees the queue time; a tenant gets the
// same block with the queue time stripped, as the view already strips raw
// provider errors.
func TestRouting_ShowsEachCandidatesThroughput(t *testing.T) {
	srv := routingTestServer(t)
	for i := 0; i < 6; i++ {
		srv.observeCall("r"+string(rune('a'+i)), slowUsage("deepseek", "deepseek-v4-pro", 400))
	}

	admin, _ := routingRaw(t, srv, []string{auth.ScopeAdmin})
	if admin.ThroughputMode != "measure" {
		t.Errorf("throughput_mode = %q, want measure", admin.ThroughputMode)
	}
	mid := middleTier(t, admin)
	ds, an := mid.Cascade[0].Throughput, mid.Cascade[1].Throughput
	if ds == nil || ds.Samples != 6 || ds.Source != "measured" || ds.Slowdown < 3.99 || ds.Slowdown > 4.01 || ds.Multiplier < 4 {
		t.Fatalf("deepseek throughput = %+v, want 6 measured samples at 4×", ds)
	}
	if ds.QueueMsP50 != 400 {
		t.Errorf("admin queue_ms_p50 = %d, want 400", ds.QueueMsP50)
	}
	// Never measured, not local: the prior, exactly 1.
	if an == nil || an.Samples != 0 || an.Multiplier != 1 || an.Source != "prior" {
		t.Errorf("anthropic throughput = %+v, want the 1× prior", an)
	}

	tenant, raw := routingRaw(t, srv, []string{auth.ScopeTenant})
	tds := middleTier(t, tenant).Cascade[0].Throughput
	if tds == nil || tds.Samples != 6 || tds.Multiplier != ds.Multiplier {
		t.Fatalf("tenant throughput = %+v, want the same estimate as the admin's", tds)
	}
	if strings.Contains(raw, "queue_ms_p50") {
		t.Error("the tenant view carries queue_ms_p50 (operator box contention); it must be admin-only")
	}
}

// TestMetricsProm_ExportsEachMeasuredModelsThroughput — one series per measured
// (provider, model); a model never measured has no series.
func TestMetricsProm_ExportsEachMeasuredModelsThroughput(t *testing.T) {
	t.Setenv("LOOMCYCLE_REPLICA_ID", "")
	srv := New(&config.Config{}, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), nil)
	for i := 0; i < 6; i++ {
		srv.observeCall("r"+string(rune('a'+i)), slowUsage("ollama-local", "qwen3.6:latest", 0))
	}
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`loomcycle_model_slowdown{model="qwen3.6:latest",provider="ollama-local"} 4`,
		`loomcycle_model_throughput_samples{model="qwen3.6:latest",provider="ollama-local"} 6`,
		`loomcycle_model_timeout_multiplier{model="qwen3.6:latest",provider="ollama-local"} `,
		`loomcycle_model_decode_tps{model="qwen3.6:latest",provider="ollama-local"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
