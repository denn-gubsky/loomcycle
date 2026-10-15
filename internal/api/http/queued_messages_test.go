package http

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// finishedSession runs one turn of agent "r" to its end and returns its
// session, for a continuation to continue.
func finishedSession(t *testing.T, s *Server, gate chan struct{}) string {
	t.Helper()
	first := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("first turn")})
	_, runID := first.ids(t)
	gate <- struct{}{}
	if err := first.wait(t); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	run := waitRunStatus(t, s.store, runID, store.RunCompleted)
	return run.SessionID
}

// postStream POSTs body and returns once the response headers arrive: a
// queued continuation answers with its stream at once, so a handler that
// instead holds the request until it has a slot fails here rather than hanging.
func postStream(t *testing.T, url, body string) *http.Response {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 3 * time.Second}}
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v (a continuation that has to wait should answer with its stream at once)", url, err)
	}
	return resp
}

// POST /v1/sessions/{id}/messages queues like POST /v1/runs: a continuation
// that cannot start at once opens its stream when it queues — session, agent
// and queued frames while it waits, its row in status queued with its request
// stored for a restart — then admitted once a slot frees, then the run, in the
// same session. It used to wait 30 s (the old queue_timeout_ms) and answer 429.
func TestHandleMessages_QueuedContinuationStreamsWhileItWaits(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	sessionID := finishedSession(t, s, gate)

	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	holder.ids(t)

	ts := httptest.NewServer(s.Mux())
	defer ts.Close()
	body := `{"agent_id":"a_qmsg","segments":[{"role":"user","content":[{"type":"trusted-text","text":"continued while full"}]}]}`
	resp := postStream(t, ts.URL+"/v1/sessions/"+sessionID+"/messages", body)
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

	run, err := s.store.GetRunByAgentID(context.Background(), "a_qmsg")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunQueued || run.SessionID != sessionID {
		t.Fatalf("queued continuation's row = status %q session %q, want queued in %q", run.Status, run.SessionID, sessionID)
	}
	if _, ok := s.queuePositionOf("a_qmsg"); !ok {
		t.Error("the queued continuation has no place in the queue")
	}
	// A restart would start it again as a continuation of the same session.
	in, why := s.restartInput(store.QueuedRun{Run: run, Input: queuedRowInput(t, s, run.ID)})
	if why != "" || in.SessionID != sessionID || in.Agent != "r" || !strings.Contains(firstUserText(in.Segments), "continued while full") {
		t.Errorf("restart input = session %q agent %q prompt %q (%s), want a continuation of %q by r with its prompt",
			in.SessionID, in.Agent, firstUserText(in.Segments), why, sessionID)
	}

	gate <- struct{}{} // the holder ends; the continuation takes its slot
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
		t.Error("the queued continuation's stream never reached done")
	}
	done := waitRunStatus(t, s.store, run.ID, store.RunCompleted)
	if done.SessionID != sessionID {
		t.Errorf("the continuation ran in session %q, want %q", done.SessionID, sessionID)
	}
}

// A continuation queued and then cancelled ends cancelled and leaves the
// queue; the session is free for the next turn.
func TestHandleMessages_CancelledWhileQueuedEndsCancelled(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	sessionID := finishedSession(t, s, gate)
	holder := startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds")})
	holder.ids(t)

	ts := httptest.NewServer(s.Mux())
	defer ts.Close()
	resp := postStream(t, ts.URL+"/v1/sessions/"+sessionID+"/messages",
		`{"agent_id":"a_qmsgcancel","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "event: "+string(providers.EventQueued) {
			break
		}
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_qmsgcancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.cancelReg.Cancel("a_qmsgcancel", "test"); !ok {
		t.Fatal("cancel registry has no entry for the queued continuation")
	}
	waitRunStatus(t, s.store, run.ID, store.RunCancelled)
	for sc.Scan() {
	}
	if st := s.sem.Stats(); st.Queued != 0 {
		t.Errorf("queue = %d after the cancel, want 0", st.Queued)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
}

// A queued continuation's restart request goes through runRequest, so every
// messagesRequest field must be a runRequest field of the same JSON name and
// type; one that is not would be dropped from a restarted continuation.
func TestMessagesRequest_EveryFieldIsARunRequestField(t *testing.T) {
	fields := func(v any) map[string]reflect.Type {
		out := map[string]reflect.Type{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				t.Fatalf("%s.%s has no JSON name", rt.Name(), f.Name)
			}
			out[name] = f.Type
		}
		return out
	}
	run := fields(runRequest{})
	for name, typ := range fields(messagesRequest{}) {
		if got, ok := run[name]; !ok {
			t.Errorf("messagesRequest field %q is not a runRequest field: a restarted queued continuation would drop it", name)
		} else if got != typ {
			t.Errorf("messagesRequest field %q is %s, runRequest's is %s", name, typ, got)
		}
	}
}
