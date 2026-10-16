package a2a

import (
	"context"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// On this plane a run parks only on an Interruption question, and a
// follow-up to a parked run resolves that question: the answer comes back as
// the Interruption tool's result inside the turn already running, exactly as
// an interrupt resolve does on HTTP, gRPC and MCP — where it is not a run
// start. So it is not counted here either, and a caller whose rate is spent
// can still answer its own run, which would otherwise hang on the question. A
// follow-up to a task that is NOT parked starts a fresh run, and is counted.
func TestExecutor_AnInterruptAnswerIsNotARunStart(t *testing.T) {
	pr := newParkingRunner("approve deploy?", "intr-1", []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn"},
	})
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-i": {ID: "run-i", AgentID: "task-i", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	resolver := &fakeResolver{pendingID: "intr-1", hasPending: true, resolved: make(chan struct{})}
	starts := 0
	ex := NewExecutor(pr, &fakeConnector{}, runs, "qa-agent").
		WithInterruptionBridge(resolver).
		// One start allowed: the first fresh run spends it.
		WithStartAdmission(func(context.Context, auth.Principal) error {
			starts++
			if starts > 1 {
				return runner.ErrRateLimited
			}
			return nil
		})
	ctx := WithPeerPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice"})

	if _, errs := collect(ex.Execute(ctx, &a2asrv.ExecutorContext{
		TaskID:  "task-i",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("deploy please")),
	})); len(errs) != 0 {
		t.Fatalf("the first start: %v", errs)
	}
	<-pr.started
	go func() {
		<-resolver.resolved
		close(pr.resume)
	}()

	// The answer, with the caller's rate spent.
	events, errs := collect(ex.Execute(ctx, &a2asrv.ExecutorContext{
		TaskID:     "task-i",
		StoredTask: &a2asdk.Task{ID: "task-i", ContextID: "c", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateInputRequired}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("yes, go")),
	}))
	if len(errs) != 0 {
		t.Fatalf("the answer was refused: %v", errs)
	}
	if starts != 1 {
		t.Errorf("the answer took a start: %d starts, want 1", starts)
	}
	if resolver.resolveCalls != 1 {
		t.Errorf("the answer resolved %d question(s), want 1", resolver.resolveCalls)
	}
	if last, ok := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent); !ok || last.Status.State != a2asdk.TaskStateCompleted {
		t.Fatalf("the answered run ended %#v, want COMPLETED", events[len(events)-1])
	}

	// A follow-up to the same task once nothing is parked is a fresh run:
	// counted, and refused with the rate spent.
	_, errs = collect(ex.Execute(ctx, &a2asrv.ExecutorContext{
		TaskID:     "task-i",
		StoredTask: &a2asdk.Task{ID: "task-i", ContextID: "c", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateCompleted}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("and again")),
	}))
	if starts != 2 || len(errs) != 1 || !errors.Is(errs[0], a2asdk.ErrUnauthorized) {
		t.Errorf("a fresh follow-up past the rate: %d starts, errs %v; want it counted and refused", starts, errs)
	}
}
