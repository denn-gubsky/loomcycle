package http

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// heldChild starts a writer child through the Agent tool's path, under an
// agent_stop hook that holds its answer, and returns the parent's events, a
// channel with the call's outcome, and the child's run id once the parent's
// stream reports it held.
func heldChild(t *testing.T) (*reviewHarness, *eventLog, chan error, chan string, string) {
	h, events, done, out, childRun, _ := heldChildCancelable(t)
	return h, events, done, out, childRun
}

func heldChildCancelable(t *testing.T) (*reviewHarness, *eventLog, chan error, chan string, string, context.CancelFunc) {
	t.Helper()
	h := newReviewHarness(t)
	hold := newRecordingHook(t, `{"decision":"hold","reason":"a person reads this"}`)
	register(t, h.srv, &hooks.Hook{Owner: "ops", Name: "review", Phase: hooks.PhaseAgentStop, Agents: []string{"writer"}, CallbackURL: hold.srv.URL})
	ctx, events := lockedParentCtx(t, h.srv)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	done, out := make(chan error, 1), make(chan string, 1)
	go func() {
		o, _, _, err := h.srv.runAgentToolChild(ctx, "writer", "write the plan", "")
		out <- o
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range events.snapshot() {
			if ev.SubagentHold != nil && ev.SubagentHold.State == providers.SubagentHoldHeld {
				return h, events, done, out, ev.SubagentHold.SubagentRunID, cancel
			}
		}
		select {
		case err := <-done:
			t.Fatalf("the child ended before it was held: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the parent's stream never reported the child held")
	return nil, nil, nil, nil, "", nil
}

// eventLog is the parent's stream, safe to read while the child runs.
type eventLog struct {
	mu     sync.Mutex
	events []providers.Event
}

func (l *eventLog) add(ev providers.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) snapshot() []providers.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]providers.Event(nil), l.events...)
}

// lockedParentCtx is parentCtx with an event log the test may read while the
// child runs.
func lockedParentCtx(t *testing.T, s *Server) (context.Context, *eventLog) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.store.CreateSession(ctx, "", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_lead", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	log := &eventLog{}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_lead"})
	ctx = tools.WithRunID(ctx, run.ID)
	ctx = tools.WithAgentName(ctx, "lead")
	ctx = tools.WithEventEmitter(ctx, log.add)
	return ctx, log
}

func releasedStatus(events *eventLog) string {
	for _, ev := range events.snapshot() {
		if ev.SubagentHold != nil && ev.SubagentHold.State == providers.SubagentHoldReleased {
			return ev.SubagentHold.Status
		}
	}
	return ""
}

// A child the Agent tool started, held by an agent_stop hook, waits for a
// verdict instead of ending rejected, and its parent's stream says so: the
// hold (naming the child's run, where the verdict goes), then its release.
// An approval hands the parent the held answer.
func TestHeldSubagent_WaitsForAVerdictAndTheParentSeesIt(t *testing.T) {
	h, events, done, out, childRun := heldChild(t)
	if childRun == "" {
		t.Fatal("the hold names no child run")
	}
	if _, err := h.srv.ReviewRun(context.Background(), childRun, "approve", "", "api"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the approved child failed: %v", err)
	}
	if got := <-out; !strings.Contains(got, "answer 1") {
		t.Errorf("the parent got %q, want the held answer", got)
	}
	if st := releasedStatus(events); st != "completed" {
		t.Errorf("released status = %q, want completed", st)
	}
}

// A rejection with no feedback ends the child rejected, which the parent's
// call reports as a failure, and the release says rejected.
func TestHeldSubagent_ARejectionFailsTheParentsCall(t *testing.T) {
	h, events, done, _, childRun := heldChild(t)
	if _, err := h.srv.ReviewRun(context.Background(), childRun, "reject", "", "api"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err = %v, want a rejection", err)
	}
	if st := releasedStatus(events); st != "rejected" {
		t.Errorf("released status = %q, want rejected", st)
	}
}

// The held child's queue takes a verdict and nothing else: its parent drives
// it, so an operator can neither steer nor retune it.
func TestHeldSubagent_TakesNoSteerOrRetune(t *testing.T) {
	h, _, done, _, childRun := heldChild(t)
	if _, err := h.srv.SteerRun(context.Background(), childRun, "do something else", "api"); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("steer: err %v, want not in flight", err)
	}
	if _, err := h.srv.runForSteer(context.Background(), childRun); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("retune gate: err %v, want not in flight", err)
	}
	if _, err := h.srv.ReviewRun(context.Background(), childRun, "approve", "", "api"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	<-done
}

// The hold has no deadline of its own, but a cancelled parent cancels its
// held child: the call returns, and the release says cancelled.
func TestHeldSubagent_ACancelledParentEndsTheHold(t *testing.T) {
	_, events, done, _, _, cancel := heldChildCancelable(t)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the call succeeded after its parent was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held child outlived its cancelled parent")
	}
	if st := releasedStatus(events); st != "cancelled" {
		t.Errorf("released status = %q, want cancelled", st)
	}
}
