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
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A draft saved interactive (or for review) and started later must behave as
// the same run started by POST /v1/runs: it outlives the call that started
// it, parks for input, and its turn can be stopped.

type detachedDraftHarness struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	st   store.Store
	gate chan struct{} // one token per turn that should complete
}

func newDetachedDraftHarness(t *testing.T) *detachedDraftHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		// Named "agent" so createDraft can build the draft.
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "be brief", UnboundedIterations: true}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "detached-draft.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gate := make(chan struct{}, 4)
	srv := New(cfg, &stubResolver{p: &gatedProvider{text: "working", gate: gate}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &detachedDraftHarness{t: t, srv: srv, ts: ts, st: st, gate: gate}
}

// start starts the draft on a connection the returned func drops, and returns
// its SSE frames.
func (h *detachedDraftHarness) start(runID string) (<-chan [2]string, func()) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.ts.URL+"/v1/runs/"+runID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("start: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		h.t.Fatalf("start = %d", resp.StatusCode)
	}
	return sseLines(h.t, resp.Body), func() { cancel(); _ = resp.Body.Close() }
}

func (h *detachedDraftHarness) waitFrame(frames <-chan [2]string, want string) {
	h.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				h.t.Fatalf("stream closed before %s", want)
			}
			if f[0] == want {
				return
			}
			if f[0] == "done" || f[0] == "error" {
				h.t.Fatalf("run ended (%s: %s) before %s", f[0], f[1], want)
			}
		case <-deadline:
			h.t.Fatalf("no %s frame", want)
		}
	}
}

// waitStarterGone waits until the server has seen the starting call end: the
// run's admission slot is released when that call returns, whether the run
// ended with it or goes on without it.
func (h *detachedDraftHarness) waitStarterGone() {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.srv.sem.Stats().Active != 0 {
		if time.Now().After(deadline) {
			h.t.Fatal("the starting call never returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *detachedDraftHarness) post(path, body string) (int, string) {
	h.t.Helper()
	return do(h.t, http.MethodPost, h.ts.URL+path, body)
}

// parks counts the run's persisted awaiting_input events.
func (h *detachedDraftHarness) parks(runID string) int {
	h.t.Helper()
	evs, err := h.st.GetRunEventsSince(context.Background(), runID, 0, 500)
	if err != nil {
		h.t.Fatalf("events: %v", err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == string(providers.EventAwaitingInput) {
			n++
		}
	}
	return n
}

func (h *detachedDraftHarness) waitParks(runID string, want int) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.parks(runID) < want {
		if time.Now().After(deadline) {
			run, _ := h.st.GetRun(context.Background(), runID)
			h.t.Fatalf("run parked %d times, want %d (status %q)", h.parks(runID), want, run.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stop cancels the whole run so its goroutine does not outlive the test's store.
func (h *detachedDraftHarness) stop(c createdRun) {
	h.t.Helper()
	_, _ = h.post("/v1/agents/"+c.AgentID+"/cancel", `{}`)
	waitRunStatus(h.t, h.st, c.RunID, store.RunCancelled)
}

// The starter leaving does not end an interactive draft: it stays running,
// parked, takes the next instruction and parks again.
func TestConfiguredRun_InteractiveDraftOutlivesTheCallThatStartedIt(t *testing.T) {
	h := newDetachedDraftHarness(t)
	c := createDraft(t, h.ts, `,"interactive":true`)
	h.gate <- struct{}{} // the first turn completes
	frames, leave := h.start(c.RunID)
	h.waitFrame(frames, string(providers.EventAwaitingInput))

	leave()
	h.waitStarterGone()
	if run, _ := h.st.GetRun(context.Background(), c.RunID); run.Status != store.RunRunning {
		t.Fatalf("after its starter left the run is %q, want still running", run.Status)
	}
	h.gate <- struct{}{} // the steered turn completes
	if code, body := h.post("/v1/runs/"+c.RunID+"/input", `{"text":"keep going"}`); code != http.StatusOK {
		t.Fatalf("input after the starter left = %d %s, want the run to take it", code, body)
	}
	h.waitParks(c.RunID, 2)
	if run, _ := h.st.GetRun(context.Background(), c.RunID); run.Status != store.RunRunning {
		t.Errorf("after its second turn the run is %q, want running and parked", run.Status)
	}
	h.stop(c)
}

// A turn of a started interactive draft can be stopped: the turn ends and the
// run parks, rather than the cancel being refused or ending the run.
func TestConfiguredRun_InteractiveDraftTurnCancelStopsTheTurnAndParks(t *testing.T) {
	h := newDetachedDraftHarness(t)
	c := createDraft(t, h.ts, `,"interactive":true`)
	h.gate <- struct{}{}
	frames, leave := h.start(c.RunID)
	defer leave()
	h.waitFrame(frames, string(providers.EventAwaitingInput))

	// No token: the steered turn holds in the provider until it is cancelled.
	if code, body := h.post("/v1/runs/"+c.RunID+"/input", `{"text":"slow work"}`); code != http.StatusOK {
		t.Fatalf("input = %d %s", code, body)
	}
	h.waitFrame(frames, string(providers.EventText))

	code, body := h.post("/v1/runs/"+c.RunID+"/cancel", `{"reason":"too slow"}`)
	if code != http.StatusOK {
		t.Fatalf("turn cancel = %d %s, want 200", code, body)
	}
	var out struct {
		Stopped bool `json:"stopped"`
		Parked  bool `json:"parked"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || !out.Stopped || !out.Parked {
		t.Fatalf("turn cancel = %s (%v), want stopped and parked", body, err)
	}
	h.waitFrame(frames, string(providers.EventAwaitingInput))
	if run, _ := h.st.GetRun(context.Background(), c.RunID); run.Status != store.RunRunning {
		t.Errorf("after the turn cancel the run is %q, want running and parked", run.Status)
	}
	h.stop(c)
}

// A draft held for review outlives the call that started it too, as a run
// started by POST /v1/runs with review does.
func TestReview_HeldDraftSurvivesTheStarterLeaving(t *testing.T) {
	h := newReviewHarness(t)
	code, body := do(t, http.MethodPost, h.ts.URL+"/v1/runs",
		`{"agent":"writer","start":false,"review":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var c createdRun
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	runID, _, frames, leave := h.startAt("/v1/runs/"+c.RunID+"/start", "")
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)
	leave()
	deadline := time.Now().Add(3 * time.Second)
	for h.srv.sem.Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the starting call never returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run, _ := h.st.GetRun(context.Background(), runID); run.Status != store.RunRunning {
		t.Fatalf("after its starter left the held draft is %q, want still running and held", run.Status)
	}
	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	h.waitStatus(runID, store.RunCompleted)
}

// Once detach returns, nothing more reaches the caller: a caller may return
// while its run goes on.
func TestForwardWhileAttached_DeliversNothingAfterDetach(t *testing.T) {
	var got []string
	forward, detach := forwardWhileAttached(func(ev providers.Event) { got = append(got, ev.Text) })
	forward(providers.Event{Text: "before"})
	detach()
	forward(providers.Event{Text: "after"})
	if strings.Join(got, ",") != "before" {
		t.Errorf("delivered %q, want only the event before detach", got)
	}
}
