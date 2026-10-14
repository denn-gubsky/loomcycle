package http

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// startedRun is a RunOnce running on its own goroutine, with what its
// callbacks saw.
type startedRun struct {
	registered chan [2]string // agent_id, run_id
	done       chan error
	mu         sync.Mutex
	events     []providers.Event
	queuedCB   bool
}

func (r *startedRun) seen() []providers.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]providers.Event(nil), r.events...)
}

func startRunOnce(s *Server, ctx context.Context, in runner.RunInput) *startedRun {
	r := &startedRun{registered: make(chan [2]string, 1), done: make(chan error, 1)}
	cb := runner.RunCallbacks{
		OnQueued: func() {
			r.mu.Lock()
			r.queuedCB = true
			r.mu.Unlock()
		},
		OnRegistered: func(agentID, runID, _, _ string) { r.registered <- [2]string{agentID, runID} },
		OnEvent: func(ev providers.Event) {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		},
	}
	go func() { r.done <- s.RunOnce(ctx, in, cb) }()
	return r
}

func (r *startedRun) ids(t *testing.T) (agentID, runID string) {
	t.Helper()
	select {
	case ids := <-r.registered:
		return ids[0], ids[1]
	case err := <-r.done:
		t.Fatalf("run ended before it was registered: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("run was never registered")
	}
	return "", ""
}

func (r *startedRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return")
		return nil
	}
}

// waitEvents waits until the run has seen at least n events of typ (the
// queued event follows its registration on the admitting goroutine).
func (r *startedRun) waitEvents(t *testing.T, typ providers.EventType, n int) []providers.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		evs := eventsOfType(r.seen(), typ)
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func eventsOfType(evs []providers.Event, typ providers.EventType) []providers.Event {
	var out []providers.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// A run that cannot start at once is accepted as a queued run: it is
// registered (with its ids) and has a row in status queued before it waits,
// it reports a queued event, get_run reports its place, and once the slot
// frees it starts, reports admitted, and runs to completion.
func TestRunOnce_QueuedRunIsRegisteredReportedAndStarts(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)

	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	_, holderRun := holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("waits")})
	agentID, runID := queued.ids(t)
	if got, err := s.store.GetRun(context.Background(), runID); err != nil || got.Status != store.RunQueued {
		t.Fatalf("queued run's row = %q (%v), want queued", got.Status, err)
	}
	q := queued.waitEvents(t, providers.EventQueued, 1)
	if len(q) != 1 || q[0].Admission == nil || q[0].Admission.Reason != "global_cap" || q[0].Admission.Position != 1 {
		t.Fatalf("queued events = %+v, want one global_cap position 1", q)
	}
	queued.mu.Lock()
	if !queued.queuedCB {
		t.Error("OnQueued was not called before OnRegistered")
	}
	queued.mu.Unlock()
	run, err := s.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	resp := s.singleRunResponse(context.Background(), run, false)
	if resp.QueuePosition != 1 || resp.QueueReason != "global_cap" {
		t.Errorf("get_run queue_position/reason = %d %q, want 1 global_cap", resp.QueuePosition, resp.QueueReason)
	}
	if c := s.singleRunToConnector(context.Background(), run); c.QueuePosition != 1 || c.QueueReason != "global_cap" {
		t.Errorf("connector get_run queue_position/reason = %d %q, want 1 global_cap", c.QueuePosition, c.QueueReason)
	}
	// The queued run's transcript carries its queued event.
	if evs, err := s.store.GetRunEventsSince(context.Background(), runID, 0, 100); err != nil || len(evs) == 0 || evs[0].Type != string(providers.EventQueued) {
		t.Errorf("queued run's transcript = %+v (%v), want it to start with the queued event", evs, err)
	}

	gate <- struct{}{} // the holder ends; the queued run takes its slot
	if err := holder.wait(t); err != nil {
		t.Fatalf("holder: %v", err)
	}
	waitRunStatus(t, s.store, holderRun, store.RunCompleted)
	waitRunStatus(t, s.store, runID, store.RunRunning)
	gate <- struct{}{}
	if err := queued.wait(t); err != nil {
		t.Fatalf("queued run: %v", err)
	}
	waitRunStatus(t, s.store, runID, store.RunCompleted)
	a := queued.waitEvents(t, providers.EventAdmitted, 1)
	if len(a) != 1 || a[0].Admission == nil {
		t.Fatalf("admitted events = %+v, want one", a)
	}
	if _, ok := s.queuePositionOf(agentID); ok {
		t.Error("a started run still has a queue position")
	}
}

// admission "reject" keeps the old answer: refused at once, no row, no
// registration.
func TestRunOnce_AdmissionRejectRefusesWithoutARow(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	err := s.RunOnce(context.Background(), runner.RunInput{
		Agent: "r", AgentID: "a_rejected", Admission: runner.AdmissionReject, Segments: oneUserSeg("refused"),
	}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrBackpressure) {
		t.Fatalf("err = %v, want ErrBackpressure", err)
	}
	if _, err := s.store.GetRunByAgentID(context.Background(), "a_rejected"); err == nil {
		t.Error("a rejected run has a row")
	}
	if err := s.RunOnce(context.Background(), runner.RunInput{Agent: "r", Admission: "later", Segments: oneUserSeg("x")}, runner.RunCallbacks{}); !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("unknown admission value: err = %v, want ErrInvalidArgument", err)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
}

// A queued run is cancellable while it waits: the cancel ends its wait, its
// row ends cancelled, and it leaves the queue.
func TestRunOnce_CancelledWhileQueuedEndsCancelled(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("waits")})
	agentID, runID := queued.ids(t)
	if _, ok := s.cancelReg.Cancel(agentID, "test"); !ok {
		t.Fatal("cancel registry has no entry for the queued run")
	}
	if err := queued.wait(t); err == nil {
		t.Fatal("a cancelled queued run returned nil")
	}
	waitRunStatus(t, s.store, runID, store.RunCancelled)
	if st := s.sem.Stats(); st.Queued != 0 {
		t.Errorf("queue = %d after the cancel, want 0", st.Queued)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
}

// Over its user's cap a run queues (user_cap) instead of being refused, when
// the operator lets users queue.
func TestRunOnce_OverUserCapQueues(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	s.sem = concurrency.New(4, 8, 0).WithPerUserCap(1).WithQueuedPerUserCap(2)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", UserID: "u", Segments: oneUserSeg("holds")})
	holder.ids(t)

	queued := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", UserID: "u", Segments: oneUserSeg("waits")})
	_, runID := queued.ids(t)
	q := queued.waitEvents(t, providers.EventQueued, 1)
	if len(q) != 1 || q[0].Admission.Reason != "user_cap" || q[0].Admission.Position != 1 {
		t.Fatalf("queued events = %+v, want one user_cap position 1", q)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
	gate <- struct{}{}
	if err := queued.wait(t); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, s.store, runID, store.RunCompleted)
}

// POST /v1/runs: a run that has to wait opens its stream when it queues — the
// caller gets the session and agent frames and a queued event while it waits
// — then admitted, then the run.
func TestHandleRuns_QueuedRunStreamsWhileItWaits(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	ts := httptest.NewServer(s.Mux())
	defer ts.Close()
	body := `{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"waits"}]}]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a stream)", resp.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event: ") {
				frames <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(frames)
	}()
	next := func() string {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream ended")
			}
			return f
		case <-time.After(3 * time.Second):
			t.Fatal("no frame")
		}
		return ""
	}
	for _, want := range []string{"session", "agent", "queued"} {
		if got := next(); got != want {
			t.Fatalf("frame %q, want %q", got, want)
		}
	}
	gate <- struct{}{} // the holder ends
	_ = holder.wait(t)
	if got := next(); got != "admitted" {
		t.Fatalf("frame after the slot freed = %q, want admitted", got)
	}
	gate <- struct{}{}
	sawDone := false
	for f := range frames {
		if f == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Error("the queued run's stream never reached done")
	}
}

// POST /v1/runs with admission "reject" answers 429 at once, as before.
func TestHandleRuns_AdmissionRejectAnswers429(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)
	rec := doJSON(t, s, "POST", "/v1/runs", `{"agent":"r","admission":"reject","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "backpressure" {
		t.Errorf("code = %q, want backpressure", e.Code)
	}
	rec = doJSON(t, s, "POST", "/v1/runs", `{"agent":"r","admission":"maybe","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown admission: status = %d, want 400", rec.Code)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
}
