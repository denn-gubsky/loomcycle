package a2a

import (
	"context"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestAnswerFromMessage_RejectsNonTextPart pins the helper-level contract:
// a non-text part yields an error (not "") so the resume path can refuse.
// Regression-grade: the pre-fix helper returned ("", nil) here.
func TestAnswerFromMessage_RejectsNonTextPart(t *testing.T) {
	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewDataPart(map[string]any{"k": "v"}))
	if _, err := answerFromMessage(msg); err == nil {
		t.Fatal("answerFromMessage accepted a non-text part; want an error")
	}
	got, err := answerFromMessage(a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hi")))
	if err != nil || got != "hi" {
		t.Fatalf("text answer = (%q, %v), want (\"hi\", nil)", got, err)
	}
}

// TestExecutor_ResumeWithNonTextPartFailsWithoutResolving pins the bridge
// behaviour: a resume carrying a non-text part must FAIL the task and must
// NOT resolve the interruption with an empty answer.
//
// Regression-grade: on the pre-fix code resumeParkedRun resolved with ""
// (resolver.resolveCalls would be 1) and the run proceeded.
func TestExecutor_ResumeWithNonTextPartFailsWithoutResolving(t *testing.T) {
	pr := newParkingRunner("approve?", "intr-1", []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn"},
	})
	defer close(pr.resume) // release the parked background goroutine at test end
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-i": {ID: "run-i", AgentID: "task-i", Status: store.RunRunning},
	}}
	resolver := &fakeResolver{pendingID: "intr-1", hasPending: true}
	ex := NewExecutor(pr, &fakeConnector{}, runs, "qa-agent").WithInterruptionBridge(resolver)

	// First message parks the run.
	first := &a2asrv.ExecutorContext{
		TaskID:  "task-i",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("deploy please")),
	}
	if _, errs := collect(ex.Execute(context.Background(), first)); len(errs) != 0 {
		t.Fatalf("first Execute errors: %v", errs)
	}
	<-pr.started

	// Resume with a NON-TEXT part.
	second := &a2asrv.ExecutorContext{
		TaskID:     "task-i",
		StoredTask: &a2asdk.Task{ID: "task-i", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateInputRequired}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewDataPart(map[string]any{"file": "x"})),
	}
	events, _ := collect(ex.Execute(context.Background(), second))

	if resolver.resolveCalls != 0 {
		t.Errorf("resolver.Resolve called %d times for an unusable resume; want 0 (no empty-answer resolve)", resolver.resolveCalls)
	}
	last, ok := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2asdk.TaskStateFailed {
		t.Fatalf("terminal event = %#v, want FAILED", events[len(events)-1])
	}
}
