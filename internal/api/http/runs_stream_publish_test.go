package http

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// receiveAny blocks for up to 1s waiting for one event from sub.C.
func receiveAny(t *testing.T, sub *runstate.Subscription) (runstate.RunStateEvent, bool) {
	t.Helper()
	select {
	case evt, ok := <-sub.C:
		return evt, ok
	case <-time.After(time.Second):
		return runstate.RunStateEvent{}, false
	}
}

func TestFinishRun_PublishesCompletedToBus(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("user-a")
	defer sub.Close()

	// Create a session + run row so FinishRun has something to write.
	ctx := t.Context()
	sess, err := srv.store.CreateSession(ctx, "tenant-a", "test-agent", "user-a")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "ag1", UserID: "user-a", Model: "test-model",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	meta := runStateMeta{RunID: run.ID, AgentID: "ag1", Agent: "test-agent", UserID: "user-a"}
	srv.finishRun(ctx, run.ID, loop.RunResult{StopReason: "end_turn"}, nil, meta)

	evt, ok := receiveAny(t, sub)
	if !ok {
		t.Fatal("no event delivered after finishRun")
	}
	if evt.Status != "completed" || evt.RunID != run.ID || evt.StopReason != "end_turn" {
		t.Errorf("wrong event: %+v", evt)
	}
}

func TestFinishRunFailedReason_PublishesFailedToBus(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("user-a")
	defer sub.Close()

	ctx := t.Context()
	sess, _ := srv.store.CreateSession(ctx, "tenant-a", "test-agent", "user-a")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "ag1", UserID: "user-a", Model: "test-model",
	})

	meta := runStateMeta{RunID: run.ID, AgentID: "ag1", Agent: "test-agent", UserID: "user-a"}
	srv.finishRunFailedReason(run.ID, "registry collision", meta)

	evt, ok := receiveAny(t, sub)
	if !ok {
		t.Fatal("no event delivered after finishRunFailedReason")
	}
	if evt.Status != "failed" || evt.Error != "registry collision" {
		t.Errorf("wrong event: %+v", evt)
	}
}

func TestFinishRunCancelled_PublishesCancelledToBus(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("user-a")
	defer sub.Close()

	ctx := t.Context()
	sess, _ := srv.store.CreateSession(ctx, "tenant-a", "test-agent", "user-a")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "ag1", UserID: "user-a", Model: "test-model",
	})

	meta := runStateMeta{RunID: run.ID, AgentID: "ag1", Agent: "test-agent", UserID: "user-a"}
	srv.finishRunCancelled(ctx, run.ID, loop.RunResult{}, "cancelled by api", meta)

	evt, ok := receiveAny(t, sub)
	if !ok {
		t.Fatal("no event delivered after finishRunCancelled")
	}
	if evt.Status != "cancelled" || evt.StopReason != "cancelled by api" {
		t.Errorf("wrong event: %+v", evt)
	}
}

func TestFinishRun_NoEventWhenBusUnwired(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	// Note: no SetRunStateBus call.

	ctx := t.Context()
	sess, _ := srv.store.CreateSession(ctx, "tenant-a", "test-agent", "user-a")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "ag1", UserID: "user-a", Model: "test-model",
	})
	meta := runStateMeta{RunID: run.ID, AgentID: "ag1", UserID: "user-a"}
	// Must not panic / no-op when bus is nil.
	srv.finishRun(ctx, run.ID, loop.RunResult{StopReason: "end_turn"}, nil, meta)
}
