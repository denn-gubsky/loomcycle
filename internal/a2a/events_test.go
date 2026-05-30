package a2a

import (
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeInfo is a minimal TaskInfoProvider for stamping events in tests.
type fakeInfo struct {
	taskID    a2asdk.TaskID
	contextID string
}

func (f fakeInfo) TaskInfo() a2asdk.TaskInfo {
	return a2asdk.TaskInfo{TaskID: f.taskID, ContextID: f.contextID}
}

// TestTranslateEvent_PerEventTypeMapping is the event-translation table:
// each providers.Event type → the expected A2A event shape (or none).
func TestTranslateEvent_PerEventTypeMapping(t *testing.T) {
	info := fakeInfo{taskID: "task-1", contextID: "ctx-1"}

	t.Run("started → WORKING status", func(t *testing.T) {
		out := translateEvent(providers.Event{Type: providers.EventStarted}, info)
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		su, ok := out[0].(*a2asdk.TaskStatusUpdateEvent)
		if !ok {
			t.Fatalf("got %T, want *TaskStatusUpdateEvent", out[0])
		}
		if su.Status.State != a2asdk.TaskStateWorking {
			t.Errorf("state = %q, want WORKING", su.Status.State)
		}
		if su.TaskID != "task-1" || su.ContextID != "ctx-1" {
			t.Errorf("task/context = %q/%q, want task-1/ctx-1", su.TaskID, su.ContextID)
		}
	})

	t.Run("text → text artifact", func(t *testing.T) {
		out := translateEvent(providers.Event{Type: providers.EventText, Text: "hello"}, info)
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		au, ok := out[0].(*a2asdk.TaskArtifactUpdateEvent)
		if !ok {
			t.Fatalf("got %T, want *TaskArtifactUpdateEvent", out[0])
		}
		if au.Artifact == nil || len(au.Artifact.Parts) != 1 {
			t.Fatalf("artifact missing parts: %+v", au.Artifact)
		}
		if got := au.Artifact.Parts[0].Text(); got != "hello" {
			t.Errorf("artifact text = %q, want %q", got, "hello")
		}
	})

	t.Run("empty text → no event", func(t *testing.T) {
		if out := translateEvent(providers.Event{Type: providers.EventText, Text: ""}, info); len(out) != 0 {
			t.Fatalf("got %d events, want 0", len(out))
		}
	})

	t.Run("tool_result (error) → no event", func(t *testing.T) {
		out := translateEvent(providers.Event{Type: providers.EventToolResult, Text: "boom", IsError: true}, info)
		if len(out) != 0 {
			t.Fatalf("got %d events, want 0", len(out))
		}
	})

	// Loomcycle-internal events that have no standalone A2A artifact
	// representation — terminal status is the executor's job, not the
	// translator's.
	for _, et := range []providers.EventType{
		providers.EventToolCall,
		providers.EventToolResult,
		providers.EventUsage,
		providers.EventDone,
		providers.EventError,
	} {
		t.Run(string(et)+" → no event", func(t *testing.T) {
			if out := translateEvent(providers.Event{Type: et}, info); len(out) != 0 {
				t.Fatalf("got %d events for %q, want 0", len(out), et)
			}
		})
	}
}

// TestTerminalStatusForRun_MapsOutcomeToStatusEvent verifies the closing
// status event carries the right state and surfaces the detail string
// as the status message.
func TestTerminalStatusForRun_MapsOutcomeToStatusEvent(t *testing.T) {
	info := fakeInfo{taskID: "task-9", contextID: "ctx-9"}

	t.Run("completed carries stop reason", func(t *testing.T) {
		ev, ok := terminalStatusForRun(info, runOutcome{Status: store.RunCompleted, Detail: "end_turn"})
		if !ok {
			t.Fatal("expected a terminal event")
		}
		su := ev.(*a2asdk.TaskStatusUpdateEvent)
		if su.Status.State != a2asdk.TaskStateCompleted {
			t.Errorf("state = %q, want COMPLETED", su.Status.State)
		}
		if su.Status.Message == nil || su.Status.Message.Parts[0].Text() != "end_turn" {
			t.Errorf("status message did not carry the stop reason: %+v", su.Status.Message)
		}
	})

	t.Run("failed carries error detail", func(t *testing.T) {
		ev, ok := terminalStatusForRun(info, runOutcome{Status: store.RunFailed, Detail: "provider 500"})
		if !ok {
			t.Fatal("expected a terminal event")
		}
		su := ev.(*a2asdk.TaskStatusUpdateEvent)
		if su.Status.State != a2asdk.TaskStateFailed {
			t.Errorf("state = %q, want FAILED", su.Status.State)
		}
	})

	t.Run("unknown status → not terminal", func(t *testing.T) {
		if _, ok := terminalStatusForRun(info, runOutcome{Status: store.RunStatus("bogus")}); ok {
			t.Fatal("unknown status should not produce a terminal event")
		}
	})
}
