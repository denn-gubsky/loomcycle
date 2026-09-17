package http

import (
	"context"
	"errors"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// routedServer wires a tiered agent over a request-recording provider, so what
// a routing override actually did is observable where it counts: the model the
// provider was asked for.
func routedServer(t *testing.T) (*Server, *httptest.Server, *recordingScriptedProvider, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"router": {Tier: "middle", Tools: []string{}, SystemPrompt: "you route"},
	}
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

func postRoutedRun(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const routedSegments = `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`

// THE CROSSING. The override is threaded from the wire, through validation,
// into the definition copy, into resolution, and finally into the provider
// Request. Every one of those is a hand-off that a unit test on either side
// leaves uncovered, and the only place the answer is observable is the model
// the provider was actually asked for.
func TestRoutingOverride_WireToProviderRequest(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	// Sanity: the tier's own answer is model-a, so "overridden" and
	// "re-derived" cannot produce the same string.
	if code, body := postRoutedRun(t, ts, `{"agent":"router",`+routedSegments+`}`); code != 200 {
		t.Fatalf("baseline run: %d %s", code, body)
	}
	if got := prov.waitForRequests(t, 1)[0].Model; got != "model-a" {
		t.Fatalf("fixture drifted: the tier resolves %q, expected model-a", got)
	}

	if code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-b",`+routedSegments+`}`); code != 200 {
		t.Fatalf("overridden run: %d %s", code, body)
	}
	if got := prov.waitForRequests(t, 2)[1].Model; got != "model-b" {
		t.Errorf("provider was asked for %q, want model-b — the override did not reach the request", got)
	}
}

// RFC DC V5 at the wire: a model outside what the deployment can reach is a
// 400, not a silent fallback to the tier's own pick.
func TestRoutingOverride_UnreachableModelIsRefusedAtTheWire(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-nowhere",`+routedSegments+`}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", code, strings.TrimSpace(body))
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a refused override", len(got))
	}
}

// The override survives a pause. This is the whole reason it lives in the run's
// configuration record rather than beside the request that carried it.
func TestRoutingOverride_SurvivesResume(t *testing.T) {
	srv, ts, prov, st := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	code, body := postRoutedRun(t, ts, `{"agent":"router","model":"model-b",`+routedSegments+`}`)
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	prov.waitForRequests(t, 1)

	run := onlyRun(t, st, extractSessionID(body))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Routing == nil || rec.Routing.Model != "model-b" {
		t.Fatalf("the run row records no routing override (%+v); nothing can be restored", rec.Routing)
	}

	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	if got := prov.waitForRequests(t, 2)[1].Model; got != "model-b" {
		t.Errorf("the resumed turn ran on %q, want model-b — the run silently reverted to "+
			"the tier's own pick mid-conversation", got)
	}
}

// --- RFC DC P2: the resource knobs ---

// THE CROSSING for a raisable knob: max_tokens is threaded from JSON, through
// validation, into a definition copy, into RunOptions, and onto the provider
// Request. The definition's own value is deliberately different, so "overridden"
// and "re-derived" cannot produce the same number.
func TestResourceOverride_MaxTokensReachesTheProviderRequest(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	code, body := postRoutedRun(t, ts, `{"agent":"router","max_tokens":4321,`+routedSegments+`}`)
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	if got := prov.waitForRequests(t, 1)[0].MaxTokens; got != 4321 {
		t.Errorf("provider was asked for max_tokens=%d, want 4321 — the override did not reach the request", got)
	}
}

// The fan-out ceiling is the one knob that may only be LOWERED, and an attempt
// to raise it is refused at the wire rather than clamped: a silently-adjusted
// value and an honoured one look identical from outside.
func TestResourceOverride_RaisingFanoutIsRefusedAtTheWire(t *testing.T) {
	_, ts, prov, _ := routedServer(t)

	code, body := postRoutedRun(t, ts, `{"agent":"router","max_concurrent_children":64,`+routedSegments+`}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", code, strings.TrimSpace(body))
	}
	if !strings.Contains(body, "may only be lowered") {
		t.Errorf("the refusal does not say why: %q", strings.TrimSpace(body))
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a refused override", len(got))
	}
}

// Lowering is accepted, persisted, and restored — so a resumed run's children
// are as narrow as the original's were.
func TestResourceOverride_LoweredFanoutSurvivesResume(t *testing.T) {
	srv, ts, prov, st := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	code, body := postRoutedRun(t, ts, `{"agent":"router","max_concurrent_children":1,`+routedSegments+`}`)
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	prov.waitForRequests(t, 1)

	run := onlyRun(t, st, extractSessionID(body))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Resources == nil || rec.Resources.MaxConcurrentChildren != 1 {
		t.Fatalf("the run row records no resource override (%+v)", rec.Resources)
	}

	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	prov.waitForRequests(t, 2)
	// The cap is not on the provider Request — it gates the Agent tool at spawn
	// time — so the round trip asserted here is the record's, and the ctx hop it
	// feeds is pinned structurally by
	// TestResourceOverride_CapLookupPrefersTheRunsOwnCap.
	restored, ok := decodeRunConfig(mustGetRun(t, st, run.ID).RunConfig)
	if !ok || restored.Resources == nil || restored.Resources.MaxConcurrentChildren != 1 {
		t.Errorf("the resumed run's record lost its fan-out cap: %+v", restored.Resources)
	}
}

// mustGetRun re-reads a run row.
func mustGetRun(t *testing.T, st store.Store, runID string) store.Run {
	t.Helper()
	r, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r
}

// THE CROSSING for the fan-out cap. It is the one override that never reaches
// RunOptions: it is resolved from the calling agent's NAME through CapLookup at
// spawn time, so the run's own value has to arrive by ctx and CapLookup has to
// prefer it. Delete either half and the record still round-trips perfectly
// while every spawned fan-out runs at the definition's width.
//
// Asserted structurally because the behavioural path needs a real
// parallel_spawn, and the failure this guards is someone simplifying the
// closure back to a bare def lookup.
func TestResourceOverride_CapLookupPrefersTheRunsOwnCap(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), "CapLookup: func(ctx context.Context, callingAgent string) int {")
	if i < 0 {
		t.Fatal("CapLookup closure not found; this guard has stopped guarding")
	}
	body := string(src)[i:]
	if end := strings.Index(body, "\n\t\t},"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "tools.FanoutCap(ctx)") {
		t.Error("CapLookup no longer consults the RUN's own fan-out cap, so a per-run " +
			"max_concurrent_children is recorded and then ignored: the name lookup it falls " +
			"back to cannot see a per-run value")
	}
	if strings.Index(body, "tools.FanoutCap(ctx)") > strings.Index(body, "lookup.Agent(") {
		t.Error("CapLookup consults the definition BEFORE the run's own cap; the run's value " +
			"must win, and it can only ever be narrower")
	}
}

// RunOnce is the UNIVERSAL run path — every trigger surface routes through it,
// and it is the one the gRPC and MCP twins will use in P6. It is also the path
// no HTTP test reaches, which is how its budget wiring was left reading the
// un-overridden definition while handleRuns worked perfectly.
//
// Found by a fail-before probe, not by review: the probe aimed at handleRuns
// could not even find the line it meant to break in RunOnce.
func TestResourceOverride_RunOnceAppliesTheBudget(t *testing.T) {
	srv, _, prov, _ := routedServer(t)

	err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent:     "router",
		MaxTokens: 777,
		Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
		},
	}, runner.RunCallbacks{OnEvent: func(providers.Event) {}})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := prov.waitForRequests(t, 1)[0].MaxTokens; got != 777 {
		t.Errorf("provider was asked for max_tokens=%d, want 777 — RunOnce recorded the "+
			"override and then resolved the loop's budget from the definition", got)
	}
}

// The same path refuses a raise, so every trigger surface inherits the ceiling
// rather than only the two HTTP handlers.
func TestResourceOverride_RunOnceRefusesARaisedCeiling(t *testing.T) {
	srv, _, prov, _ := routedServer(t)

	err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent:                 "router",
		MaxConcurrentChildren: 99,
		Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
		},
	}, runner.RunCallbacks{OnEvent: func(providers.Event) {}})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the provider was called %d time(s) for a refused override", len(got))
	}
}
