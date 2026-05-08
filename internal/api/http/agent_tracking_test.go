package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// pausableProvider blocks inside Call() until release is signalled OR
// ctx is cancelled. Used by cancel tests to give an external cancel
// request time to fire before the loop's response stream completes.
type pausableProvider struct {
	release    chan struct{}
	ctxFired   atomic.Bool
	finalText  string
}

func (p *pausableProvider) ID() string                            { return "pausable" }
func (p *pausableProvider) Probe(_ context.Context) error          { return nil }
func (p *pausableProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"pausable-model"}, nil
}
func (p *pausableProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *pausableProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 4)
	go func() {
		defer close(ch)
		select {
		case <-p.release:
			// Released — emit a normal completion.
			ch <- providers.Event{Type: providers.EventText, Text: p.finalText}
			ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
		case <-ctx.Done():
			// Cancelled — record so tests can assert and emit nothing
			// further. The driver's normal contract is to close the
			// channel; the loop sees zero events and treats it as the
			// iteration ending without text or done.
			p.ctxFired.Store(true)
		}
	}()
	return ch, nil
}

// makeServer builds a Server with a fresh sqlite store, semaphore, and
// stub provider — the common boilerplate every test below shares.
func makeServer(t *testing.T, prov providers.Provider, cfg *config.Config) (*Server, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tracking.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	return srv, dbPath
}

func makeBaseConfig() *config.Config {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", AllowedTools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	return cfg
}

// extractAgentID parses the SSE body for the v0.4 `event: agent` frame
// and returns the agent_id field. Tests use this to discover the
// generated id when the request didn't supply one.
func extractAgentID(body string) string {
	const marker = "event: agent\ndata: "
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	tail := body[i+len(marker):]
	end := strings.Index(tail, "\n")
	if end < 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(tail[:end]), &payload); err != nil {
		return ""
	}
	if v, ok := payload["agent_id"].(string); ok {
		return v
	}
	return ""
}

// POST /v1/runs with a caller-supplied agent_id announces it in the
// event: agent SSE frame so the caller can confirm it round-tripped
// (and so adapter UIs can wire UI tracking from a single source).
func TestRuns_AcceptsAgentID_AnnouncesInAgentEvent(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","user_id":"alice","agent_id":"a_caller","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	got := extractAgentID(string(body))
	if got != "a_caller" {
		t.Errorf("event:agent did not announce caller's agent_id: got %q, want a_caller\nbody:\n%s", got, body)
	}
}

// When agent_id is omitted, loomcycle generates one and announces it.
func TestRuns_GeneratesAgentIDWhenOmitted(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := extractAgentID(string(body))
	if !strings.HasPrefix(got, "a_") {
		t.Errorf("generated agent_id should be a_-prefixed, got %q", got)
	}
	if len(got) != 18 {
		t.Errorf("generated agent_id length = %d, want 18 (a_ + 16 hex)", len(got))
	}
}

// Invalid charset on user_id or agent_id is a 400 — guards against
// malformed input reaching SQL queries or registry keys.
func TestRuns_RejectsInvalidIdent(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	for _, badField := range []string{
		`"user_id":"has space"`,
		`"agent_id":"has/slash"`,
		`"user_id":"` + strings.Repeat("a", 200) + `"`,
	} {
		body := `{"agent":"default",` + badField + `,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
		if resp.StatusCode != 400 {
			t.Errorf("body %s → status %d, want 400", badField, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// Two concurrent runs with the same agent_id: the second 409s. Locks
// in the registry's ErrInUse semantics at the HTTP layer.
//
// EMPIRICAL: removing the `if errors.Is(regErr, cancel.ErrInUse)`
// branch in handleRuns sends both runs through; the second would either
// crash on the registry's silent overwrite or run normally — either
// way this test fails.
func TestRuns_RejectsDuplicateActiveAgentID(t *testing.T) {
	cfg := makeBaseConfig()
	// Pausable provider so the first run holds the registry slot for
	// the duration of the test.
	prov := &pausableProvider{
		release:   make(chan struct{}),
		finalText: "ok",
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// First run: kick off but DON'T release. We poll for the run to
	// appear in the registry to avoid racing the request.
	go func() {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","agent_id":"a_dup","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	// Wait until the registry has the entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.cancelReg.Get("a_dup"); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := srv.cancelReg.Get("a_dup"); !ok {
		t.Fatal("first run did not register within the deadline")
	}

	// Second run with the same agent_id should 409.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","agent_id":"a_dup","segments":[{"role":"user","content":[{"type":"trusted-text","text":"y"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 409\nbody: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agent_id_in_use") {
		t.Errorf("expected agent_id_in_use code, got: %s", body)
	}

	// Release the first run so the test exits cleanly.
	close(prov.release)
}

// REGRESSION: a 409 from the duplicate-active path MUST NOT leave the
// just-created run row at status=running. The handler creates a
// session+run BEFORE registering with the cancel registry; a registry
// collision was previously orphaning that row at status=running for
// the heartbeat sweeper to clean up later — polluting
// ListActiveRunsByUser in the meantime.
//
// EMPIRICAL: removing the s.finishRunFailedReason call on the ErrInUse
// branch of handleRuns regresses this test (the row stays at running).
func TestRuns_DuplicateActiveAgentID_DoesNotLeakRunningRow(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// First run: kick off and wait for registry presence.
	go func() {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","user_id":"alice","agent_id":"a_dup_leak","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitForRegistry(t, srv, "a_dup_leak", 2*time.Second)

	// Second run with same agent_id → 409.
	resp2, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","user_id":"alice","agent_id":"a_dup_leak","segments":[{"role":"user","content":[{"type":"trusted-text","text":"y"}]}]}`,
	))
	if err != nil {
		t.Fatalf("POST /v1/runs (duplicate agent_id): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", resp2.StatusCode)
	}

	// At this point Alice should have exactly ONE running row (the
	// first one) — NOT two. The second run's row, created before the
	// registry collision was detected, must have been transitioned
	// to status=failed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := srv.store.ListActiveRunsByUser(context.Background(), "alice", "")
		failedCount := 0
		runningCount := 0
		for _, r := range runs {
			switch r.Status {
			case "running":
				runningCount++
			case "failed":
				failedCount++
			}
		}
		// We expect: 1 running (the live first run) + 1 failed (the
		// 409-rejected second). If we see 2 running, the leak is back.
		if runningCount == 1 && failedCount == 1 {
			break
		}
		if runningCount > 1 {
			t.Fatalf("BLOCKING: %d running rows for alice, expected 1 (the 409 leaked a row)", runningCount)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Sanity: there should be at least one failed row with the
	// "agent_id collision" reason.
	allRuns, _ := srv.store.ListActiveRunsByUser(context.Background(), "alice", "failed")
	foundCollision := false
	for _, r := range allRuns {
		if strings.Contains(r.ErrorMsg, "agent_id collision") {
			foundCollision = true
		}
	}
	if !foundCollision {
		t.Errorf("expected a failed row with 'agent_id collision' error, got: %+v", allRuns)
	}

	close(prov.release)
}

// Cancelling a running run via POST /v1/agents/{id}/cancel writes
// status=cancelled (NOT failed) in the store. The cause-aware path
// in finishRunWithCancel discriminates API-cancel from client-disconnect.
//
// EMPIRICAL: reverting finishRunWithCancel to call finishRun directly
// (skipping the cause check) makes this test fail — the run would land
// at status=failed (because runErr is ctx.Canceled) instead of cancelled.
func TestCancel_RunningRun_WritesCancelledStatus(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &pausableProvider{release: make(chan struct{})}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Kick off a slow run.
	go func() {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"default","user_id":"u","agent_id":"a_x","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitForRegistry(t, srv, "a_x", 2*time.Second)

	// Cancel.
	cancelResp, err := http.Post(ts.URL+"/v1/agents/a_x/cancel", "application/json",
		strings.NewReader(`{"reason":"user_clicked_stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		body, _ := io.ReadAll(cancelResp.Body)
		t.Fatalf("cancel status = %d\nbody: %s", cancelResp.StatusCode, body)
	}

	// Wait for the run to terminate. Polling the registry isn't a
	// good synchronization point — Cancel deletes the entry IMMEDIATELY
	// (before the run goroutine has finalized the DB row). Instead we
	// poll the DB directly until status leaves "running", then assert
	// the terminal status. The deadline is generous because on the
	// loaded test runner the goroutine schedule can stretch.
	deadline := time.Now().Add(2 * time.Second)
	var run = struct {
		Status     string
		StopReason string
	}{}
	for time.Now().Before(deadline) {
		got, err := srv.store.GetRunByAgentID(context.Background(), "a_x")
		if err == nil && got.Status != "running" {
			run.Status = string(got.Status)
			run.StopReason = got.StopReason
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled (cause-aware path didn't fire)", run.Status)
	}
	if !strings.Contains(run.StopReason, "user_clicked_stop") {
		t.Errorf("stop_reason = %q, want it to contain user_clicked_stop", run.StopReason)
	}
}

// Cancel of an unknown agent_id (never seen) returns 404.
func TestCancel_UnknownAgentID_404(t *testing.T) {
	cfg := makeBaseConfig()
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/agents/a_nope/cancel", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /v1/agents/a_nope/cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404\nbody: %s", resp.StatusCode, body)
	}
}

// Cancel of a finished run is idempotent: returns 200 with cancelled=false
// and the existing terminal status. Lets retrying clients converge.
func TestCancel_AlreadyFinished_Idempotent(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Run completes immediately.
	resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","agent_id":"a_done","user_id":"u","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Cancel after termination.
	cancelResp, err := http.Post(ts.URL+"/v1/agents/a_done/cancel", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /v1/agents/a_done/cancel: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		body, _ := io.ReadAll(cancelResp.Body)
		t.Errorf("status = %d, want 200 idempotent\nbody: %s", cancelResp.StatusCode, body)
	}
	body, _ := io.ReadAll(cancelResp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["cancelled"] != false {
		t.Errorf("cancelled = %v, want false (already terminated)", got["cancelled"])
	}
	if got["status"] != "completed" {
		t.Errorf("status echo = %v, want completed", got["status"])
	}
}

// GET /v1/agents/{id} returns the run's current status and surfaces
// the live flag indicating registry presence.
func TestGetAgent_ReturnsCurrentStatus(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","agent_id":"a_get","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// After completion, GET returns the terminal record.
	getResp, err := http.Get(ts.URL + "/v1/agents/a_get")
	if err != nil {
		t.Fatalf("GET /v1/agents/a_get: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("status = %d\nbody: %s", getResp.StatusCode, body)
	}
	var doc agentResponse
	if err := json.NewDecoder(getResp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.AgentID != "a_get" {
		t.Errorf("agent_id = %q", doc.AgentID)
	}
	if doc.UserID != "alice" {
		t.Errorf("user_id = %q (denormalisation not working)", doc.UserID)
	}
	if doc.Status != "completed" {
		t.Errorf("status = %q, want completed", doc.Status)
	}
	if doc.Live {
		t.Errorf("live = true after termination, should be false")
	}
}

// GET /v1/users/{user_id}/agents lists active runs for the user only,
// filters out other users.
func TestListUserAgents_FiltersByUserAndStatus(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// alice's first run completes
			{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
			// bob's run completes
			{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
			// alice's second run completes
			{
				{Type: providers.EventText, Text: "ok"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	for _, body := range []string{
		`{"agent":"default","user_id":"alice","agent_id":"a_a1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		`{"agent":"default","user_id":"bob","agent_id":"a_b1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		`{"agent":"default","user_id":"alice","agent_id":"a_a2","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	} {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// All-statuses for alice — should see exactly her two completed runs.
	getResp, err := http.Get(ts.URL + "/v1/users/alice/agents?status=all")
	if err != nil {
		t.Fatalf("GET /v1/users/alice/agents: %v", err)
	}
	defer getResp.Body.Close()
	var listDoc struct {
		Agents []agentResponse `json:"agents"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&listDoc); err != nil {
		t.Fatal(err)
	}
	if len(listDoc.Agents) != 2 {
		t.Fatalf("got %d agents for alice, want 2", len(listDoc.Agents))
	}
	for _, a := range listDoc.Agents {
		if a.UserID != "alice" {
			t.Errorf("non-alice agent leaked: %+v", a)
		}
	}
}

// Cancel cascades to a sub-agent at the HTTP level: parent's runCtx
// propagates to the sub-loop AND the registry walk explicitly cancels
// the sub.
//
// The cascade MECHANISM is fully proven at the registry unit level by
// TestRegistry_Cancel_CascadesTree (parent + 2 children + grandchild,
// every cancelFn fires, full cascaded list). The HTTP-level integration
// version of the test below is skipped because reliably orchestrating
// "parent up + child up + neither completes" with the current
// scriptedProvider requires a more sophisticated multi-channel stub
// than the codebase has today, and the cascade behaviour is not
// expected to differ at the HTTP layer (it's a thin wrapper over
// Registry.Cancel).
//
// Re-enable once the test infrastructure has a per-script-entry pause
// channel; until then, rely on the unit-level cascade test plus
// TestSubAgent_InheritsUserID_GeneratesFreshAgentID (which proves the
// sub registers correctly, the prerequisite for cascade).
func TestCancel_CascadesToSubAgent(t *testing.T) {
	t.Skip("see comment above — cascade is unit-tested by TestRegistry_Cancel_CascadesTree")
	cfg := makeBaseConfig()
	cfg.Agents["parent"] = config.AgentDef{
		Model: "stub-model", AllowedTools: []string{"Agent"},
	}
	cfg.Agents["child"] = config.AgentDef{
		Model: "stub-model", AllowedTools: []string{},
	}
	// Provider script: parent emits one tool_call to spawn child;
	// child blocks indefinitely on a pausable provider released by
	// the test (or by ctx cancel). Parent uses scriptedProvider, but
	// since sub-runs go through providers.Get(...) for the SAME
	// ProviderResolver, we use a routing provider that switches
	// behaviour by request.
	scripted := &scriptedProvider{
		scripts: [][]providers.Event{
			// parent iter 1: tool_call to Agent
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_p_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
			},
			// child iter 1: blocks until ctx cancel — emit nothing,
			// done with stop_reason from ctx cancel
			{}, // empty events causes the loop to see ctx cancel via done channel close
		},
	}
	srv, _ := makeServer(t, scripted, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Kick off parent — pausable behaviour comes from the empty
	// child script forcing the loop to wait for events that never
	// arrive (until ctx fires).
	go func() {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"parent","user_id":"u","agent_id":"a_parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
		))
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitForRegistry(t, srv, "a_parent", 2*time.Second)

	// Wait for the sub-agent to register too. It will have a
	// loomcycle-generated agent_id; we find it via ListChildren.
	deadline := time.Now().Add(2 * time.Second)
	var subAgentID string
	for time.Now().Before(deadline) {
		children := srv.cancelReg.ListChildren("a_parent")
		if len(children) == 1 {
			subAgentID = children[0].AgentID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if subAgentID == "" {
		t.Skip("sub-agent did not register within deadline; the empty-script approach may not exercise the sub-loop reliably on this platform")
	}

	cancelResp, err := http.Post(ts.URL+"/v1/agents/a_parent/cancel", "application/json",
		strings.NewReader(`{"reason":"shutdown"}`))
	if err != nil {
		t.Fatalf("POST /v1/agents/a_parent/cancel: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		t.Fatalf("cancel status = %d", cancelResp.StatusCode)
	}
	var cr cancelResponse
	_ = json.NewDecoder(cancelResp.Body).Decode(&cr)
	if !cr.Cancelled {
		t.Errorf("cancelled = false, want true")
	}
	cascaded := false
	for _, c := range cr.Cascaded {
		if c == subAgentID {
			cascaded = true
		}
	}
	if !cascaded {
		t.Errorf("expected sub %s in cascaded list, got %v", subAgentID, cr.Cascaded)
	}
}

// Sub-agent inherits user_id and gets a fresh agent_id with
// parent_agent_id pointing at the parent.
func TestSubAgent_InheritsUserID_GeneratesFreshAgentID(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents["parent"] = config.AgentDef{
		Model: "stub-model", AllowedTools: []string{"Agent"},
	}
	cfg.Agents["child"] = config.AgentDef{
		Model: "stub-model", AllowedTools: []string{},
	}
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// parent: tool_call to spawn child
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			// child runs to completion
			{
				{Type: providers.EventText, Text: "child output"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
			// parent post-tool-result: text + done
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
			},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","user_id":"alice","agent_id":"a_parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Find the sub-run via parent_agent_id lookup in the store.
	subs, err := srv.store.ListRunsByParentAgentID(context.Background(), "a_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d sub-runs, want 1", len(subs))
	}
	sub := subs[0]
	if sub.UserID != "alice" {
		t.Errorf("sub user_id = %q, want alice (inheritance broken)", sub.UserID)
	}
	if !strings.HasPrefix(sub.AgentID, "a_") {
		t.Errorf("sub agent_id should be a_-prefixed, got %q", sub.AgentID)
	}
	if sub.AgentID == "a_parent" {
		t.Errorf("sub agent_id MUST be distinct from parent (got same: %q)", sub.AgentID)
	}
	if sub.ParentAgentID != "a_parent" {
		t.Errorf("sub parent_agent_id = %q, want a_parent", sub.ParentAgentID)
	}
}

// waitForRegistry polls the cancel registry until the agent_id appears
// or the deadline passes. Used by tests that need to synchronise on
// the goroutine running an HTTP request.
func waitForRegistry(t *testing.T, srv *Server, agentID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, ok := srv.cancelReg.Get(agentID); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent_id %q did not register within %s", agentID, within)
}
