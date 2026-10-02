package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A walk is a run, and its own transitions — start, the pauses it asks a
// person for, end — reach the run-state stream like any run's, on the stream
// filtered by its walk id included.

// streamRunStates subscribes req on the server's run-state stream and returns
// the events it delivers. The subscription ends with the test.
func streamRunStates(t *testing.T, srv *Server, req connector.StreamUserRunStatesRequest) <-chan connector.RunStateEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan connector.RunStateEvent, 256)
	go func() {
		_ = srv.StreamUserRunStates(ctx, req, func(evt connector.RunStateEvent) error {
			events <- evt
			return nil
		})
	}()
	return events
}

// waitForSubscribers waits until n run-state subscriptions are open, so a
// publish that follows cannot race past one of them.
func waitForSubscribers(t *testing.T, bus *runstate.Bus, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for bus.ActiveSubscriberCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d run-state subscribers never registered", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// walkFrames collects the frames of the walk's OWN run that pass the walk
// filter, up to and including the first that satisfies done.
func walkFrames(t *testing.T, events <-chan connector.RunStateEvent, walkID string, done func(connector.RunStateEvent) bool) []connector.RunStateEvent {
	t.Helper()
	var got []connector.RunStateEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.RunID != walkID || !walkIDMatches(walkID, evt.RunID, evt.ParentContext) {
				continue
			}
			got = append(got, evt)
			if done(evt) {
				return got
			}
		case <-deadline:
			t.Fatalf("walk %s: the frame waited for never came; got %+v", walkID, got)
			return nil
		}
	}
}

func hasStatus(status string) func(connector.RunStateEvent) bool {
	return func(evt connector.RunStateEvent) bool { return evt.Status == status }
}

// noWalkFrames fails when another tenant's subscription delivered any frame of
// the walk's own run.
func noWalkFrames(t *testing.T, events <-chan connector.RunStateEvent, walkID string) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-events:
			if evt.RunID == walkID {
				t.Fatalf("another tenant's stream saw walk %s: %+v", walkID, evt)
			}
		case <-deadline:
			return
		}
	}
}

// TestTeamDefRun_WalkStreamCarriesTheWalksOwnStartAndEnd: the walk's own run
// reaches the stream as running, then completed — and only its own tenant's.
// It published neither, so a stream watching the walk saw its members and
// never the walk.
//
// The walk id is not known before the walk starts, so the start frame is
// caught on the tenant's whole stream and put through the walk filter's match.
func TestTeamDefRun_WalkStreamCarriesTheWalksOwnStartAndEnd(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)
	events := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{UserID: "alice", TenantID: "acme", TenantScoped: true})
	other := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{UserID: "alice", TenantID: "evil", TenantScoped: true})
	waitForSubscribers(t, h.bus, 2)

	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)

	got := walkFrames(t, events, walkID, hasStatus("completed"))
	if len(got) != 2 || got[0].Status != "running" {
		t.Fatalf("walk frames = %+v, want running then completed", got)
	}
	for _, f := range got {
		if f.Agent != "team:solo" || f.UserID != "alice" || f.AwaitedState != "" {
			t.Errorf("walk frame %+v: want agent team:solo, user alice, no awaited state", f)
		}
	}
	noWalkFrames(t, other, walkID)
}

// TestTeamDefRun_WalkPausedAtABreakpointIsInterrupted: while a walk waits on
// the Interruption its breakpoint asked, its run streams and reads as
// awaited_state interrupted, and every way out of the pause — answered or the
// walk cancelled — streams a frame without it. The ask bypasses the model loop,
// whose recorded tool calls are where a wait was otherwise seen, so a paused
// walk read as one that was simply slow.
func TestTeamDefRun_WalkPausedAtABreakpointIsInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(h *walkHarness, walkID, interruptID string)
		// status is the walk's end once the pause is over.
		status string
	}{
		{"answered", func(h *walkHarness, walkID, interruptID string) {
			if _, err := h.srv.ResolveInterrupt(alicePrincipal(context.Background()), walkID, interruptID, "", "continue", "", ""); err != nil {
				t.Fatalf("resolve: %v", err)
			}
		}, "completed"},
		{"cancelled", func(h *walkHarness, walkID, _ string) {
			if stopped, _, err := h.srv.cancelTeamWalk(alicePrincipal(context.Background()), walkID, "stop"); !stopped || err != nil {
				t.Fatalf("cancel: stopped=%v err=%v", stopped, err)
			}
		}, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWalkHarness(t)
			seedTenantTeam(t, h.st, "acme", "wave", waveTeam)
			h.feed("acme")
			events := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{UserID: "alice", TenantID: "acme", TenantScoped: true})
			waitForSubscribers(t, h.bus, 1)

			walkID := h.postTeamDef(alicePrincipal,
				`{"op":"run","name":"wave","input":"go","mode":"detach","breakpoints":["wave:before_dispatch"]}`)
			pending := waitPendingInterrupt(t, h.st, walkID)

			paused := walkFrames(t, events, walkID, func(evt connector.RunStateEvent) bool { return evt.AwaitedState != "" })
			if last := paused[len(paused)-1]; last.Status != "running" || last.AwaitedState != "interrupted" || last.AwaitedOn != "question" {
				t.Errorf("pause frame = %+v, want running / interrupted / question", last)
			}
			if got := getRunAwaited(t, h, walkID); got != "interrupted/question" {
				t.Errorf("GET /v1/runs/%s awaited = %q while paused, want interrupted/question", walkID, got)
			}

			// From here the walk's own filter: the handle a caller holds.
			walkStream := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{
				UserID: "alice", TenantID: "acme", TenantScoped: true, WalkID: walkID})
			other := streamRunStates(t, h.srv, connector.StreamUserRunStatesRequest{
				UserID: "alice", TenantID: "evil", TenantScoped: true, WalkID: walkID})
			waitForSubscribers(t, h.bus, 3)

			tc.end(h, walkID, pending.InterruptID)

			after := walkFrames(t, walkStream, walkID, hasStatus(tc.status))
			if len(after) < 2 || after[0].Status != "running" || after[0].AwaitedState != "" {
				t.Fatalf("frames after the pause = %+v, want running with no awaited state, then %s", after, tc.status)
			}
			for _, f := range after {
				if f.AwaitedState != "" {
					t.Errorf("frame after the pause still awaited: %+v", f)
				}
			}
			// The transcript says the ask returned, so a read of the run would
			// not report it waiting.
			if state, on := awaited.ForRun(context.Background(), h.st, walkID); state != "" {
				t.Errorf("awaited.ForRun after the pause = %q/%q, want none", state, on)
			}
			noWalkFrames(t, other, walkID)
		})
	}
}

// getRunAwaited reads the walk through GET /v1/runs/{run_id} as alice and
// returns its awaited_state/awaited_on.
func getRunAwaited(t *testing.T, h *walkHarness, runID string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, nil)
	r.SetPathValue("run_id", runID)
	r = r.WithContext(alicePrincipal(r.Context()))
	rr := httptest.NewRecorder()
	h.srv.handleGetRun(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d: %s", runID, rr.Code, rr.Body.String())
	}
	var out struct {
		Status       string `json:"status"`
		AwaitedState string `json:"awaited_state"`
		AwaitedOn    string `json:"awaited_on"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != string(store.RunRunning) {
		t.Fatalf("GET /v1/runs/%s status = %q, want running", runID, out.Status)
	}
	return out.AwaitedState + "/" + out.AwaitedOn
}
