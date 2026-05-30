package a2a

import (
	"context"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// parkingRunner emits an EventInterruptionPending, then BLOCKS in RunOnce
// (simulating the Interruption tool parking on the bus inside the loop)
// until `resume` is closed, then emits the post-resume events and
// returns. This lets the executor's INPUT_REQUIRED ↔ resume bridge be
// exercised end-to-end with a real background goroutine, matching how a
// live run keeps draining across the park.
type parkingRunner struct {
	question    string
	intrID      string
	afterResume []providers.Event
	resume      chan struct{}
	started     chan struct{} // closed once the park event has been emitted
}

func newParkingRunner(question, intrID string, after []providers.Event) *parkingRunner {
	return &parkingRunner{
		question:    question,
		intrID:      intrID,
		afterResume: after,
		resume:      make(chan struct{}),
		started:     make(chan struct{}),
	}
}

func (p *parkingRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	if cb.OnEvent != nil {
		cb.OnEvent(providers.Event{
			Type: providers.EventInterruptionPending,
			Interruption: &providers.InterruptionEventInfo{
				InterruptID: p.intrID,
				Kind:        "question",
				Question:    p.question,
			},
		})
	}
	close(p.started)
	// Block as the real loop does while parked on intr:<id>.
	select {
	case <-p.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, ev := range p.afterResume {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	return nil
}

// TestExecutor_InputRequiredParkSurfacesQuestionAndPauses asserts a run
// that parks on an interruption surfaces TASK_STATE_INPUT_REQUIRED
// carrying the question text, and the first Execute returns there (the
// run lives on in the background).
func TestExecutor_InputRequiredParkSurfacesQuestionAndPauses(t *testing.T) {
	pr := newParkingRunner("approve deploy?", "intr-1", []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn"},
	})
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-i": {ID: "run-i", AgentID: "task-i", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	resolver := &fakeResolver{pendingID: "intr-1", hasPending: true}
	ex := NewExecutor(pr, &fakeConnector{}, runs, "qa-agent").WithInterruptionBridge(resolver)

	execCtx := &a2asrv.ExecutorContext{
		TaskID:  "task-i",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("deploy please")),
	}
	events, errs := collect(ex.Execute(context.Background(), execCtx))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Last event must be INPUT_REQUIRED carrying the question.
	last, ok := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2asdk.TaskStateInputRequired {
		t.Fatalf("terminal event = %#v, want INPUT_REQUIRED", events[len(events)-1])
	}
	if last.Status.Message == nil || last.Status.Message.Parts[0].Text() != "approve deploy?" {
		t.Errorf("INPUT_REQUIRED lost the question: %#v", last.Status.Message)
	}

	// The run was registered as parked under the task id. Release it so
	// the background goroutine does not leak past the test.
	close(pr.resume)
}

// TestExecutor_SameTaskFollowupResolvesAndResumesToTerminal asserts that
// a same-task follow-up message RESOLVES the pending interruption (calls
// resolver.Resolve with the human's answer) and resumes the parked run to
// its terminal status.
func TestExecutor_SameTaskFollowupResolvesAndResumesToTerminal(t *testing.T) {
	pr := newParkingRunner("approve deploy?", "intr-1", []providers.Event{
		{Type: providers.EventText, Text: "deploying now"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	})
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-i": {ID: "run-i", AgentID: "task-i", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	resolver := &fakeResolver{pendingID: "intr-1", hasPending: true, resolved: make(chan struct{})}
	ex := NewExecutor(pr, &fakeConnector{}, runs, "qa-agent").WithInterruptionBridge(resolver)

	// First message parks the run.
	first := &a2asrv.ExecutorContext{
		TaskID:  "task-i",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("deploy please")),
	}
	_, errs := collect(ex.Execute(context.Background(), first))
	if len(errs) != 0 {
		t.Fatalf("first Execute errors: %v", errs)
	}
	<-pr.started

	// When the resolver records the answer, unblock the parked runner —
	// the real bus.Notify would wake the loop the same way. Synchronise on
	// the resolver's `resolved` channel (no shared-field polling, which
	// would race the executor goroutine).
	go func() {
		<-resolver.resolved
		close(pr.resume)
	}()

	// Second message on the SAME task carries the answer + a stored task.
	second := &a2asrv.ExecutorContext{
		TaskID:     "task-i",
		StoredTask: &a2asdk.Task{ID: "task-i", ContextID: "c", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateInputRequired}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("yes, go")),
	}
	events, errs := collect(ex.Execute(context.Background(), second))
	if len(errs) != 0 {
		t.Fatalf("resume Execute errors: %v", errs)
	}

	if resolver.resolveCalls != 1 {
		t.Fatalf("resolver.Resolve called %d times, want 1", resolver.resolveCalls)
	}
	if resolver.gotAnswer != "yes, go" {
		t.Errorf("resolved answer = %q, want %q", resolver.gotAnswer, "yes, go")
	}
	last, ok := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2asdk.TaskStateCompleted {
		t.Fatalf("resumed run terminal = %#v, want COMPLETED", events[len(events)-1])
	}
}

// TestExecutor_UnknownTaskFollowupStartsFreshRun asserts that a follow-up
// carrying a stored task that is NOT parked starts a fresh run rather
// than resolving anything — the "unknown-taskId starts a new run"
// contract. resolver.Resolve must not be called.
func TestExecutor_UnknownTaskFollowupStartsFreshRun(t *testing.T) {
	fr := &fakeRunner{script: []providers.Event{
		{Type: providers.EventText, Text: "fresh answer"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-new": {ID: "run-new", AgentID: "task-new", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	resolver := &fakeResolver{} // no pending; should never be consulted for resolve
	ex := NewExecutor(fr, &fakeConnector{}, runs, "qa-agent").WithInterruptionBridge(resolver)

	execCtx := &a2asrv.ExecutorContext{
		TaskID:     "task-new",
		StoredTask: &a2asdk.Task{ID: "task-new", ContextID: "c", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hello")),
	}
	events, errs := collect(ex.Execute(context.Background(), execCtx))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolver.resolveCalls != 0 {
		t.Fatalf("resolver.Resolve was called %d times for an unknown task; want 0", resolver.resolveCalls)
	}
	if fr.gotIn.Agent != "qa-agent" {
		t.Error("a fresh run should have driven the runner")
	}
	last, ok := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2asdk.TaskStateCompleted {
		t.Fatalf("fresh run terminal = %#v, want COMPLETED", events[len(events)-1])
	}
}
