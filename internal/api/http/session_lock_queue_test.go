package http

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeClusterLocker stands in for the Postgres session locker. Each held lock
// is what pins one pool connection in production, so holding() is the number
// of connections the cluster lock would be pinning.
type fakeClusterLocker struct {
	mu    sync.Mutex
	held  map[string]bool
	takes int // successful TryLocks, ever
}

func newFakeClusterLocker() *fakeClusterLocker {
	return &fakeClusterLocker{held: map[string]bool{}}
}

func (l *fakeClusterLocker) TryLock(_ context.Context, id string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[id] {
		return nil, false
	}
	l.held[id] = true
	l.takes++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.held, id)
			l.mu.Unlock()
		})
	}, true
}

func (l *fakeClusterLocker) holding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

func (l *fakeClusterLocker) taken() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.takes
}

// sseFrames reads resp's event names onto a channel, closed at the stream's end.
func sseFrames(resp *http.Response) <-chan string {
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
	return frames
}

func nextFrame(t *testing.T, frames <-chan string) string {
	t.Helper()
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

// queuedSessionTurn starts a turn on a finished session while the only run
// slot is held, and returns once the turn has queued. url builds the request
// from the session id.
func queuedSessionTurn(t *testing.T, url func(base, sessionID string) (string, string)) (s *Server, gate chan struct{}, locker *fakeClusterLocker, ts *httptest.Server, sessionID string, holder *startedRun, frames <-chan string) {
	t.Helper()
	s, gate = newGatedBatchServer(t, 1)
	s.sem = concurrency.New(1, 8, 0)
	sessionID = finishedSession(t, s, gate)
	locker = newFakeClusterLocker()
	s.sessionLockPG = locker

	holder = startRunOnce(s, context.Background(), runner.RunInput{Agent: "r", Segments: oneUserSeg("holds the slot")})
	holder.ids(t)

	ts = httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	target, body := url(ts.URL, sessionID)
	resp := postStream(t, target, body)
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a stream)", resp.StatusCode)
	}
	frames = sseFrames(resp)
	for _, want := range []string{"session", "agent", "queued"} {
		if got := nextFrame(t, frames); got != want {
			t.Fatalf("frame %q, want %q", got, want)
		}
	}
	return s, gate, locker, ts, sessionID, holder, frames
}

func messagesTurn(agentID string) func(base, sessionID string) (string, string) {
	return func(base, sessionID string) (string, string) {
		return base + "/v1/sessions/" + sessionID + "/messages",
			`{"agent_id":"` + agentID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"queued turn"}]}]}`
	}
}

// A continuation waiting in the run queue holds no cluster session lock, so it
// pins no pool connection: a queue of them pinning one each could drain the
// pool the running runs need to finish, and so never get the slots they wait
// for. It still keeps the session from every other request on this replica,
// and takes the cluster lock back once it has its slot.
func TestHandleMessages_QueuedContinuationHoldsNoClusterLockWhileItWaits(t *testing.T) {
	s, gate, locker, ts, sessionID, holder, frames := queuedSessionTurn(t, messagesTurn("a_qlock"))

	if n := locker.holding(); n != 0 {
		t.Errorf("while the continuation is queued the cluster lock is held %d time(s), want 0", n)
	}
	second := postStream(t, ts.URL+"/v1/sessions/"+sessionID+"/messages",
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"second"}]}]}`)
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Errorf("a second turn on the queued continuation's session = %d, want 409 session_busy", second.StatusCode)
	}

	takesBefore := locker.taken()
	gate <- struct{}{} // the holder ends; the continuation takes its slot
	_ = holder.wait(t)
	if got := nextFrame(t, frames); got != "admitted" {
		t.Fatalf("frame after the slot freed = %q, want admitted", got)
	}
	gate <- struct{}{}
	for range frames {
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_qlock")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, s.store, run.ID, store.RunCompleted)
	if locker.taken() != takesBefore+1 {
		t.Errorf("the admitted continuation took the cluster lock %d time(s), want 1", locker.taken()-takesBefore)
	}
	if n := locker.holding(); n != 0 {
		t.Errorf("after the continuation ended the cluster lock is held %d time(s), want 0", n)
	}
}

// The same for POST /v1/runs naming a session.
func TestHandleRuns_QueuedSessionRunHoldsNoClusterLockWhileItWaits(t *testing.T) {
	s, gate, locker, _, _, holder, frames := queuedSessionTurn(t, func(base, sessionID string) (string, string) {
		return base + "/v1/runs",
			`{"agent":"r","agent_id":"a_qrlock","session_id":"` + sessionID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"queued turn"}]}]}`
	})

	if n := locker.holding(); n != 0 {
		t.Errorf("while the run is queued the cluster lock is held %d time(s), want 0", n)
	}
	gate <- struct{}{}
	_ = holder.wait(t)
	if got := nextFrame(t, frames); got != "admitted" {
		t.Fatalf("frame after the slot freed = %q, want admitted", got)
	}
	gate <- struct{}{}
	for range frames {
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_qrlock")
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, s.store, run.ID, store.RunCompleted)
	if n := locker.holding(); n != 0 {
		t.Errorf("after the run ended the cluster lock is held %d time(s), want 0", n)
	}
}

// A queued continuation whose session another replica took while it waited
// does not start: its row ends failed with the session-busy reason, its stream
// ends with an error, and its run slot is given back.
func TestHandleMessages_QueuedContinuationEndsFailedWhenAnotherReplicaTookTheSession(t *testing.T) {
	s, gate, locker, _, sessionID, holder, frames := queuedSessionTurn(t, messagesTurn("a_qtaken"))

	releaseOther, ok := locker.TryLock(context.Background(), sessionID)
	if !ok {
		t.Fatal("another replica could not take the session while the continuation was queued")
	}
	defer releaseOther()

	gate <- struct{}{}
	_ = holder.wait(t)
	var got []string
	for f := range frames {
		got = append(got, f)
	}
	if len(got) == 0 || got[len(got)-1] != "error" {
		t.Errorf("frames after the slot freed = %v, want them to end with error", got)
	}
	run, err := s.store.GetRunByAgentID(context.Background(), "a_qtaken")
	if err != nil {
		t.Fatal(err)
	}
	run = waitRunStatus(t, s.store, run.ID, store.RunFailed)
	if !strings.Contains(run.ErrorMsg, "in flight on session") {
		t.Errorf("row error = %q, want the session-busy reason", run.ErrorMsg)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.sem.Stats()
		if st.Active == 0 && st.Queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run slots after the refused start: active %d queued %d, want 0 and 0", st.Active, st.Queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
