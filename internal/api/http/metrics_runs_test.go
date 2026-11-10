package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// installMetrics gives t a fresh meter state and returns a renderer of the
// instrument series as /metrics renders them for opts.
func installMetrics(t *testing.T) func(lcotel.PromOptions) string {
	t.Helper()
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatalf("SetMetricsForTest: %v", err)
	}
	t.Cleanup(restore)
	return func(opts lcotel.PromOptions) string {
		t.Helper()
		var b bytes.Buffer
		if err := lcotel.WritePrometheus(context.Background(), &b, opts); err != nil {
			t.Fatalf("WritePrometheus: %v", err)
		}
		return b.String()
	}
}

// sampleValue is the value of the one sample line that starts with series
// (name and full label set), or false.
func sampleValue(text, series string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			return f, err == nil
		}
	}
	return 0, false
}

// waitForSample waits for series to read want: a run's span, which ends its
// count, ends after the response the client reads.
func waitForSample(t *testing.T, render func(lcotel.PromOptions) string, series string, want float64) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := render(lcotel.PromOptions{})
		if v, ok := sampleValue(out, series); ok && v == want {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never read %v:\n%s", series, want, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// subAgentFixture is a parent that calls the Agent tool once, a child that
// answers, over a scripted provider and a real sqlite store.
func subAgentFixture(t *testing.T) (*httptest.Server, *storesqlite.Store) {
	t.Helper()
	t.Setenv("LOOMCYCLE_REPLICA_ID", "")
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent"},
			"child":  {Model: "stub-model", Tools: []string{}, SystemPrompt: "child"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"hello"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 4}},
		},
		{
			{Type: providers.EventText, Text: "child says hi"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 4, CacheCreationTokens: 3}},
		},
		{
			{Type: providers.EventText, Text: "parent done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
		},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, st
}

func postParentRun(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "parent done") {
		t.Fatalf("run = %d:\n%s", resp.StatusCode, body)
	}
	return string(body)
}

// A run started over HTTP and the sub-agent it spawned are each counted once
// as completed, under their own trigger, and leave nothing active.
func TestRunMetrics_HTTPRunAndItsSubAgentCountedByTrigger(t *testing.T) {
	render := installMetrics(t)
	ts, _ := subAgentFixture(t)
	postParentRun(t, ts)

	out := waitForSample(t, render, `loomcycle_runs_active{tenant=""}`, 0)
	for series, want := range map[string]float64{
		`loomcycle_runs_total{status="completed",tenant="",trigger="http"}`:     1,
		`loomcycle_runs_total{status="completed",tenant="",trigger="subagent"}`: 1,
		`loomcycle_run_duration_seconds_count{status="completed",tenant=""}`:    2,
	} {
		if got, ok := sampleValue(out, series); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v\n%s", series, got, ok, want, out)
		}
	}
}

// A run whose answer a reviewer rejected is counted rejected, as the row
// stores it, not completed: no error ended it.
func TestFinishRun_CountsARejectedRunAsRejected(t *testing.T) {
	render := installMetrics(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{Tenant: "rej"})
	(&Server{}).finishRun(context.Background(), "", loop.RunResult{StopReason: loop.StopReasonRejected}, nil, runStateMeta{otelSpan: span})
	span.End()
	if got, ok := sampleValue(render(lcotel.PromOptions{}), `loomcycle_runs_total{status="rejected",tenant="rej",trigger="http"}`); !ok || got != 1 {
		t.Errorf("rejected run counted = %v (present %v), want 1:\n%s", got, ok, render(lcotel.PromOptions{}))
	}
}

// The token counters are the usage ledger: over a set of mocked runs (a
// parent and its sub-agent, twice), each token type's counter equals the sum
// of that type over the ledger rows the runs wrote.
func TestTokenMetrics_MatchTheLedgerTotals(t *testing.T) {
	render := installMetrics(t)
	ts, st := subAgentFixture(t)
	postParentRun(t, ts)
	// A second deployment, so the set holds two independent ledgers.
	ts2, st2 := subAgentFixture(t)
	postParentRun(t, ts2)
	out := waitForSample(t, render, `loomcycle_runs_active{tenant=""}`, 0)

	var in, outTok, cr, cw int64
	for _, s := range []*storesqlite.Store{st, st2} {
		rows, err := s.UsageReport(context.Background(), store.UsageQuery{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("ledger total = %+v, %v; want one row", rows, err)
		}
		in += rows[0].InputTokens
		outTok += rows[0].OutputTokens
		cr += rows[0].CacheReadTokens
		cw += rows[0].CacheCreationTokens
	}
	if in == 0 || outTok == 0 || cr == 0 || cw == 0 {
		t.Fatalf("the fixture is wrong: ledger totals in=%d out=%d cache_read=%d cache_write=%d; every type must be non-zero", in, outTok, cr, cw)
	}
	for typ, want := range map[string]int64{"input": in, "output": outTok, "cache_read": cr, "cache_write": cw} {
		var got float64
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "loomcycle_tokens_total{") && strings.Contains(line, `type="`+typ+`"`) {
				v, _ := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
				got += v
			}
		}
		if got != float64(want) {
			t.Errorf("loomcycle_tokens_total{type=%q} = %v, ledger = %d", typ, got, want)
		}
	}
	if !strings.Contains(out, `key="operator"`) {
		t.Errorf("tokens not attributed to the key that paid:\n%s", out)
	}
}

// A gateway call is a model call like a run's turn: each route, streamed or
// not, times it under the gateway purpose, with tracing off.
func TestGatewayRoutes_TimeEachModelCallUnderTheGatewayPurpose(t *testing.T) {
	render := installMetrics(t)
	srv, _ := makeServer(t, gatewayProvider(), makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	for _, tc := range []struct{ path, body string }{
		{"/v1/_llm/chat", `{"messages":[{"role":"user","content":"hi"}],"provider":"scripted","model":"scripted-model"}`},
		{"/v1/_llm/chat", `{"messages":[{"role":"user","content":"hi"}],"provider":"scripted","model":"scripted-model","stream":true}`},
		{"/v1/chat/completions", `{"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`},
		{"/v1/chat/completions", `{"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted","stream":true}`},
	} {
		resp, err := http.Post(ts.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, resp.StatusCode)
		}
	}
	out := render(lcotel.PromOptions{})
	if got, ok := sampleValue(out, `loomcycle_model_call_duration_seconds_count{model="scripted-model",provider="scripted",purpose="gateway"}`); !ok || got != 4 {
		t.Errorf("gateway model calls timed = %v (present %v), want 4:\n%s", got, ok, out)
	}
}
