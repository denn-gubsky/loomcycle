package http

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// numberedProvider answers "answer N" and records the last user text of each
// request, so a test can tell which answer a run ended on and whether feedback
// reached the model.
type numberedProvider struct {
	mu        sync.Mutex
	lastUsers []string
}

func (p *numberedProvider) ID() string                  { return "stub" }
func (p *numberedProvider) Probe(context.Context) error { return nil }
func (p *numberedProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *numberedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *numberedProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	last := ""
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		for _, c := range req.Messages[n-1].Content {
			last += c.Text
		}
	}
	p.mu.Lock()
	p.lastUsers = append(p.lastUsers, last)
	n := len(p.lastUsers)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d", n)}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

func (p *numberedProvider) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lastUsers...)
}

type reviewHarness struct {
	t    *testing.T
	ts   *httptest.Server
	st   store.Store
	prov *numberedProvider
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &numberedProvider{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &reviewHarness{t: t, ts: ts, st: st, prov: prov}
}

// start posts a run and returns its ids plus a reader of its SSE frame types.
// cancelStream drops the client connection.
func (h *reviewHarness) start(body string) (runID, agentID string, frames <-chan string, cancelStream func()) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.ts.URL+"/v1/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("run status = %d", resp.StatusCode)
	}
	out := make(chan string, 128)
	ids := make(chan [2]string, 1)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		typ := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				typ = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:") && typ == "agent":
				var a struct {
					RunID   string `json:"run_id"`
					AgentID string `json:"agent_id"`
				}
				_ = json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &a)
				ids <- [2]string{a.RunID, a.AgentID}
			case line == "" && typ != "":
				out <- typ
				typ = ""
			}
		}
	}()
	select {
	case got := <-ids:
		return got[0], got[1], out, cancel
	case <-time.After(3 * time.Second):
		h.t.Fatal("no agent frame")
	}
	return "", "", nil, cancel
}

func (h *reviewHarness) waitFrame(frames <-chan string, want string) {
	h.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				h.t.Fatalf("stream closed before %s", want)
			}
			if f == want {
				return
			}
		case <-deadline:
			h.t.Fatalf("no %s frame", want)
		}
	}
}

// waitHeld waits for the run's latest persisted event to be the given hold —
// the state the review verb gates on.
func (h *reviewHarness) waitHeld(runID string, round int) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ev, err := h.st.GetLastEventForRun(context.Background(), runID); err == nil && ev.Type == string(providers.EventAwaitingReview) {
			var p providers.Event
			if json.Unmarshal(ev.Payload, &p) == nil && p.AwaitingReview != nil && p.AwaitingReview.Round == round {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("run %s never held at round %d", runID, round)
}

func (h *reviewHarness) review(runID, body string) (int, string) {
	h.t.Helper()
	resp, err := http.Post(h.ts.URL+"/v1/runs/"+runID+"/review", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text())
	}
	return resp.StatusCode, b.String()
}

func (h *reviewHarness) waitStatus(runID string, want store.RunStatus) store.Run {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := h.st.GetRun(context.Background(), runID); err == nil && run.Status == want {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := h.st.GetRun(context.Background(), runID)
	h.t.Fatalf("run status = %q, want %q", run.Status, want)
	return run
}

const reviewRunBody = `{"agent":"writer","review":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`

// The whole hold through the real server: held, rejected with feedback, the
// revision held again, approved — and the run completes on the revision.
func TestReview_RejectWithFeedbackThenApprove(t *testing.T) {
	h := newReviewHarness(t)
	runID, agentID, frames, stop := h.start(reviewRunBody)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)

	// While held the run reports what it is waiting for.
	resp, err := http.Get(h.ts.URL + "/v1/agents/" + agentID)
	if err != nil {
		t.Fatal(err)
	}
	var a agentResponse
	_ = json.NewDecoder(resp.Body).Decode(&a)
	resp.Body.Close()
	if a.Status != store.RunRunning || a.AwaitedState != awaitedStateReview {
		t.Errorf("held run reads status=%q awaited_state=%q, want running/review", a.Status, a.AwaitedState)
	}

	if code, body := h.review(runID, `{"decision":"reject","feedback":"cover the rollback"}`); code != http.StatusOK {
		t.Fatalf("reject = %d %s", code, body)
	}
	h.waitHeld(runID, 2)
	if seen := h.prov.seen(); len(seen) != 2 || seen[1] != "cover the rollback" {
		t.Errorf("the model was sent %q, want the feedback as the revision's last user turn", seen)
	}

	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	h.waitFrame(frames, "done")
	run := h.waitStatus(runID, store.RunCompleted)
	if !strings.Contains(string(run.Result), "answer 2") {
		t.Errorf("result = %s, want the approved revision", run.Result)
	}
}

// Rejected without feedback: the run ends in its own status, not completed.
func TestReview_RejectWithoutFeedbackEndsRejected(t *testing.T) {
	h := newReviewHarness(t)
	runID, agentID, frames, stop := h.start(reviewRunBody)
	defer stop()
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)
	if code, body := h.review(runID, `{"decision":"reject"}`); code != http.StatusOK {
		t.Fatalf("reject = %d %s", code, body)
	}
	run := h.waitStatus(runID, store.RunRejected)
	if run.StopReason != "rejected" {
		t.Errorf("stop_reason = %q", run.StopReason)
	}
	resp, err := http.Get(h.ts.URL + "/v1/agents/" + agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a agentResponse
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if a.Status != store.RunRejected {
		t.Errorf("GET /v1/agents status = %q, want rejected", a.Status)
	}
	// A finished run is no longer reviewable.
	if code, _ := h.review(runID, `{"decision":"approve"}`); code != http.StatusNotFound {
		t.Errorf("review of a finished run = %d, want 404", code)
	}
}

// A held run outlives the caller's connection: a human's review can take far
// longer than a client keeps a stream open.
func TestReview_HeldRunSurvivesTheClientLeaving(t *testing.T) {
	h := newReviewHarness(t)
	runID, _, frames, stop := h.start(reviewRunBody)
	h.waitFrame(frames, "awaiting_review")
	h.waitHeld(runID, 1)
	stop()
	time.Sleep(100 * time.Millisecond)
	if run, _ := h.st.GetRun(context.Background(), runID); run.Status != store.RunRunning {
		t.Fatalf("after the client left the run is %q, want still running and held", run.Status)
	}
	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	h.waitStatus(runID, store.RunCompleted)
}

// The verb refuses what it cannot act on, in ways a caller can tell apart.
func TestReview_Refusals(t *testing.T) {
	h := newReviewHarness(t)
	// A live run that is not held: an interactive run waiting for input.
	runID, _, frames, stop := h.start(`{"agent":"writer","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)
	defer stop()
	h.waitFrame(frames, "awaiting_input")
	if code, body := h.review(runID, `{"decision":"approve"}`); code != http.StatusConflict || !strings.Contains(body, "not_held") {
		t.Errorf("review of a run not held = %d %s, want 409 not_held", code, body)
	}
	for name, body := range map[string]string{
		"unknown decision":        `{"decision":"maybe"}`,
		"feedback on an approval": `{"decision":"approve","feedback":"nice"}`,
	} {
		if code, _ := h.review(runID, body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if code, _ := h.review("r_missing", `{"decision":"approve"}`); code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", code)
	}
}

// Another tenant's held run is an opaque 404, and the verdict does not reach
// it. The same gate stops an isolated member reviewing a colleague's run.
func TestReviewRun_OnlyTheOwnersMayDeliverAVerdict(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.SetSteerRegistry(steer.NewRegistry(0))
	runID := seedRunInTenant(t, st, "acme", "alice", "a_held")
	run, _ := st.GetRun(context.Background(), runID)
	q, dereg := s.steerReg.Register(steer.Entry{RunID: runID, SessionID: run.SessionID, UserID: "alice"})
	defer dereg()
	payload, _ := json.Marshal(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1}})
	if err := st.AppendEvent(context.Background(), runID, string(providers.EventAwaitingReview), payload); err != nil {
		t.Fatal(err)
	}

	for name, ctx := range map[string]context.Context{
		"another tenant": tenantPrincipalCtx("evil", "mallory", auth.ScopeRunsCreate),
		"an isolated colleague": auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}),
	} {
		if _, err := s.ReviewRun(ctx, runID, "approve", "", "api"); !errors.Is(err, connector.ErrRunNotInFlight) {
			t.Errorf("%s: err = %v, want the opaque not-in-flight", name, err)
		}
	}
	select {
	case m := <-q:
		t.Fatalf("a refused verdict reached the run: %+v", m)
	default:
	}

	if _, err := s.ReviewRun(tenantPrincipalCtx("acme", "alice", auth.ScopeRunsCreate), runID, "reject", "  redo  ", "api"); err != nil {
		t.Fatalf("owner's verdict: %v", err)
	}
	if m := <-q; m.Kind != steer.KindReject || m.Text != "redo" || m.Source != "api" {
		t.Errorf("delivered %+v, want a trimmed reject from the api", m)
	}
}

// The verb is a run write, gated like steering — not the read scope, and not
// the admin default an unmapped route falls to.
func TestReview_RequiresRunsCreate(t *testing.T) {
	if got := requiredScopeFor(http.MethodPost, "/v1/runs/r_1/review"); got != auth.ScopeRunsCreate {
		t.Errorf("scope = %q, want %q", got, auth.ScopeRunsCreate)
	}
}
