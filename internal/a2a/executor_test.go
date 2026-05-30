package a2a

import (
	"context"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeRunner emits a scripted provider-event sequence through OnEvent,
// then returns runErr. It records the RunInput it was driven with so
// tests can assert prompt + identity translation.
type fakeRunner struct {
	script []providers.Event
	runErr error
	gotIn  runner.RunInput
}

func (f *fakeRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	f.gotIn = in
	if cb.OnRegistered != nil {
		cb.OnRegistered(in.AgentID, "run-1", "sess-1", "")
	}
	for _, ev := range f.script {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	return f.runErr
}

// fakeRuns is an in-memory RunReader keyed by agent_id.
type fakeRuns struct {
	byAgentID map[string]store.Run
}

func (f *fakeRuns) GetRun(ctx context.Context, runID string) (store.Run, error) {
	for _, r := range f.byAgentID {
		if r.ID == runID {
			return r, nil
		}
	}
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
}

func (f *fakeRuns) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	if r, ok := f.byAgentID[agentID]; ok {
		return r, nil
	}
	return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
}

// fakeConnector embeds the interface (so we only implement what the
// test exercises) and records CancelRun calls.
type fakeConnector struct {
	connector.Connector
	cancelledAgentID string
	cancelErr        error
}

func (f *fakeConnector) CancelRun(ctx context.Context, agentID, reason string) (connector.CancelRunResult, error) {
	f.cancelledAgentID = agentID
	if f.cancelErr != nil {
		return connector.CancelRunResult{}, f.cancelErr
	}
	return connector.CancelRunResult{Cancelled: true}, nil
}

// collect drains an executor iterator into slices of events and errors.
func collect(seq func(yield func(a2asdk.Event, error) bool)) ([]a2asdk.Event, []error) {
	var events []a2asdk.Event
	var errs []error
	seq(func(ev a2asdk.Event, err error) bool {
		if err != nil {
			errs = append(errs, err)
		}
		if ev != nil {
			events = append(events, ev)
		}
		return true
	})
	return events, errs
}

// TestExecutor_ExecuteHappyPathYieldsSubmittedWorkingArtifactCompleted
// is the executor happy-path: a fresh task drives a scripted run, and
// the A2A stream is SUBMITTED → WORKING → text-artifact → COMPLETED.
func TestExecutor_ExecuteHappyPathYieldsSubmittedWorkingArtifactCompleted(t *testing.T) {
	fr := &fakeRunner{script: []providers.Event{
		{Type: providers.EventStarted},
		{Type: providers.EventText, Text: "the answer is 42"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-abc": {ID: "run-1", AgentID: "task-abc", SessionID: "sess-1", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	ex := NewExecutor(fr, &fakeConnector{}, runs, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "task-abc",
		ContextID: "ctx-1",
		Message:   a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("what is the answer?")),
	}
	events, errs := collect(ex.Execute(context.Background(), execCtx))

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// SUBMITTED(task) → WORKING(status) → artifact → COMPLETED(status)
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %#v", len(events), events)
	}
	if _, ok := events[0].(*a2asdk.Task); !ok {
		t.Errorf("event[0] = %T, want *Task (submitted)", events[0])
	}
	if su, ok := events[1].(*a2asdk.TaskStatusUpdateEvent); !ok || su.Status.State != a2asdk.TaskStateWorking {
		t.Errorf("event[1] = %#v, want WORKING status", events[1])
	}
	au, ok := events[2].(*a2asdk.TaskArtifactUpdateEvent)
	if !ok || au.Artifact.Parts[0].Text() != "the answer is 42" {
		t.Errorf("event[2] = %#v, want text artifact", events[2])
	}
	su, ok := events[3].(*a2asdk.TaskStatusUpdateEvent)
	if !ok || su.Status.State != a2asdk.TaskStateCompleted {
		t.Errorf("event[3] = %#v, want COMPLETED status", events[3])
	}
	if su.Status.Message == nil || su.Status.Message.Parts[0].Text() != "end_turn" {
		t.Errorf("terminal status lost the stop reason: %#v", su.Status.Message)
	}

	// Prompt + identity translation.
	if fr.gotIn.Agent != "qa-agent" || fr.gotIn.AgentID != "task-abc" {
		t.Errorf("run input agent/agent_id = %q/%q, want qa-agent/task-abc", fr.gotIn.Agent, fr.gotIn.AgentID)
	}
	if len(fr.gotIn.Segments) != 1 || len(fr.gotIn.Segments[0].Content) != 1 {
		t.Fatalf("run input segments = %#v", fr.gotIn.Segments)
	}
	blk := fr.gotIn.Segments[0].Content[0]
	if blk.Type != "trusted-text" || blk.Text != "what is the answer?" {
		t.Errorf("prompt block = %#v, want trusted-text 'what is the answer?'", blk)
	}
}

// TestExecutor_ExecuteSkipsSubmittedWhenTaskAlreadyStored asserts the
// SUBMITTED beat is only for brand-new tasks (no stored task).
func TestExecutor_ExecuteSkipsSubmittedWhenTaskAlreadyStored(t *testing.T) {
	fr := &fakeRunner{script: []providers.Event{{Type: providers.EventStarted}}}
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-x": {ID: "run-2", AgentID: "task-x", Status: store.RunCompleted},
	}}
	ex := NewExecutor(fr, &fakeConnector{}, runs, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{
		TaskID:     "task-x",
		StoredTask: &a2asdk.Task{ID: "task-x", ContextID: "c", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}},
		Message:    a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("more")),
	}
	events, _ := collect(ex.Execute(context.Background(), execCtx))
	for _, ev := range events {
		if _, ok := ev.(*a2asdk.Task); ok {
			t.Fatalf("did not expect a submitted *Task event for a stored task")
		}
	}
}

// TestExecutor_ExecuteRunErrorYieldsFailed asserts a RunOnce error
// surfaces as a FAILED status carrying the cause.
func TestExecutor_ExecuteRunErrorYieldsFailed(t *testing.T) {
	fr := &fakeRunner{runErr: errors.New("loop blew up")}
	ex := NewExecutor(fr, &fakeConnector{}, &fakeRuns{byAgentID: map[string]store.Run{}}, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{
		TaskID:  "task-err",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("go")),
	}
	events, _ := collect(ex.Execute(context.Background(), execCtx))
	last := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if last.Status.State != a2asdk.TaskStateFailed {
		t.Fatalf("terminal state = %q, want FAILED", last.Status.State)
	}
	if last.Status.Message == nil || last.Status.Message.Parts[0].Text() != "loop blew up" {
		t.Errorf("FAILED status lost the cause: %#v", last.Status.Message)
	}
}

// TestExecutor_ExecuteRejectsNonTextParts asserts a message with an
// unsupported part type is rejected (FAILED) rather than silently
// dropping content.
func TestExecutor_ExecuteRejectsNonTextParts(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, &fakeConnector{}, &fakeRuns{byAgentID: map[string]store.Run{}}, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{
		TaskID:  "task-data",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewDataPart(map[string]any{"k": "v"})),
	}
	events, _ := collect(ex.Execute(context.Background(), execCtx))
	last := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if last.Status.State != a2asdk.TaskStateFailed {
		t.Fatalf("terminal state = %q, want FAILED for unsupported part", last.Status.State)
	}
	if fr.gotIn.Agent != "" {
		t.Errorf("runner should not have been driven on a rejected message")
	}
}

// TestExecutor_RejectedBrandNewMessageYieldsTaskBeforeFailed asserts a
// rejection on a brand-new message (no stored task) emits a SUBMITTED
// *Task* BEFORE the FAILED status. The SDK's event aggregation rejects a
// bare status update as the first event ("first event must be a Task or
// a message"), so a rejection that skipped the Task beat surfaced as an
// opaque transport error instead of a terminal FAILED task. Regression
// guard for that — the end-to-end SDK test exercises the full path; this
// pins the invariant cheaply.
func TestExecutor_RejectedBrandNewMessageYieldsTaskBeforeFailed(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, &fakeConnector{}, &fakeRuns{byAgentID: map[string]store.Run{}}, "fallback").
		WithAgentResolver(func(string) (string, bool) { return "", false })

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hi"))
	msg.Metadata = map[string]any{"skillId": "nope"}
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-rej", Message: msg}

	events, _ := collect(ex.Execute(context.Background(), execCtx))
	if len(events) < 2 {
		t.Fatalf("got %d events, want >=2 (Task then FAILED)", len(events))
	}
	if _, ok := events[0].(*a2asdk.Task); !ok {
		t.Fatalf("event[0] = %T, want *Task (SUBMITTED) so the SDK has a task to fail", events[0])
	}
	last := events[len(events)-1].(*a2asdk.TaskStatusUpdateEvent)
	if last.Status.State != a2asdk.TaskStateFailed {
		t.Errorf("final state = %q, want FAILED", last.Status.State)
	}
}

// TestExecutor_CancelRoutesThroughConnectorAndYieldsCanceled asserts
// Cancel calls Connector.CancelRun keyed by the task id (== agent_id)
// and emits CANCELED.
func TestExecutor_CancelRoutesThroughConnectorAndYieldsCanceled(t *testing.T) {
	fc := &fakeConnector{}
	ex := NewExecutor(&fakeRunner{}, fc, &fakeRuns{byAgentID: map[string]store.Run{}}, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{TaskID: "task-cancel", ContextID: "c"}
	events, errs := collect(ex.Cancel(context.Background(), execCtx))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if fc.cancelledAgentID != "task-cancel" {
		t.Errorf("cancelled agent_id = %q, want task-cancel", fc.cancelledAgentID)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	su := events[0].(*a2asdk.TaskStatusUpdateEvent)
	if su.Status.State != a2asdk.TaskStateCanceled {
		t.Errorf("state = %q, want CANCELED", su.Status.State)
	}
}

// TestExecutor_CancelSurfacesConnectorError asserts a CancelRun failure
// propagates as an iterator error.
func TestExecutor_CancelSurfacesConnectorError(t *testing.T) {
	fc := &fakeConnector{cancelErr: errors.New("no such run")}
	ex := NewExecutor(&fakeRunner{}, fc, &fakeRuns{byAgentID: map[string]store.Run{}}, "qa-agent")

	execCtx := &a2asrv.ExecutorContext{TaskID: "task-gone"}
	_, errs := collect(ex.Cancel(context.Background(), execCtx))
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1", len(errs))
	}
}

// TestExecutor_ResolverDispatchesKnownSkillToItsAgent asserts that with
// a skill→agent resolver installed, an inbound message carrying a known
// skillId routes to the mapped loomcycle agent (multi-agent server).
func TestExecutor_ResolverDispatchesKnownSkillToItsAgent(t *testing.T) {
	fr := &fakeRunner{script: []providers.Event{{Type: providers.EventDone, StopReason: "end_turn"}}}
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-r": {ID: "run-r", AgentID: "task-r", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	skillToAgent := map[string]string{"research": "researcher", "write": "writer"}
	ex := NewExecutor(fr, &fakeConnector{}, runs, "fallback").
		WithAgentResolver(func(skillID string) (string, bool) {
			a, ok := skillToAgent[skillID]
			return a, ok
		})

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("write a poem"))
	msg.Metadata = map[string]any{"skillId": "write"}
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-r", Message: msg}

	_, errs := collect(ex.Execute(context.Background(), execCtx))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if fr.gotIn.Agent != "writer" {
		t.Errorf("dispatched agent = %q, want writer (skillId=write)", fr.gotIn.Agent)
	}
}

// TestExecutor_ResolverRejectsUnknownSkill asserts an unexposed skill is
// rejected (FAILED status) rather than silently falling back to the
// default agent — a peer must not reach an unexposed agent.
func TestExecutor_ResolverRejectsUnknownSkill(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, &fakeConnector{}, &fakeRuns{byAgentID: map[string]store.Run{}}, "fallback").
		WithAgentResolver(func(string) (string, bool) { return "", false })

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hi"))
	msg.Metadata = map[string]any{"skillId": "nope"}
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-bad", Message: msg}

	events, _ := collect(ex.Execute(context.Background(), execCtx))
	if fr.gotIn.Agent != "" {
		t.Errorf("runner must not be driven for an unknown skill; got agent %q", fr.gotIn.Agent)
	}
	if len(events) == 0 {
		t.Fatal("expected a FAILED status event for the rejected skill")
	}
	last := events[len(events)-1]
	su, ok := last.(*a2asdk.TaskStatusUpdateEvent)
	if !ok || su.Status.State != a2asdk.TaskStateFailed {
		t.Errorf("final event = %#v, want FAILED status", last)
	}
}

// TestExecutor_ResolverRejectsMissingSkillId asserts a multi-agent
// server rejects a request that omits the skillId entirely.
func TestExecutor_ResolverRejectsMissingSkillId(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, &fakeConnector{}, &fakeRuns{byAgentID: map[string]store.Run{}}, "fallback").
		WithAgentResolver(func(string) (string, bool) { return "writer", true })

	execCtx := &a2asrv.ExecutorContext{
		TaskID:  "task-nometa",
		Message: a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hi")),
	}
	_, _ = collect(ex.Execute(context.Background(), execCtx))
	if fr.gotIn.Agent != "" {
		t.Errorf("runner must not be driven when skillId is absent; got agent %q", fr.gotIn.Agent)
	}
}

// TestExecutor_NoResolverUsesFixedAgent confirms back-compat: without a
// resolver, every request routes to the fixed agentName regardless of
// any skillId metadata.
func TestExecutor_NoResolverUsesFixedAgent(t *testing.T) {
	fr := &fakeRunner{script: []providers.Event{{Type: providers.EventDone, StopReason: "end_turn"}}}
	runs := &fakeRuns{byAgentID: map[string]store.Run{
		"task-f": {ID: "run-f", AgentID: "task-f", Status: store.RunCompleted, StopReason: "end_turn"},
	}}
	ex := NewExecutor(fr, &fakeConnector{}, runs, "only-agent")

	msg := a2asdk.NewMessage(a2asdk.MessageRoleUser, a2asdk.NewTextPart("hi"))
	msg.Metadata = map[string]any{"skillId": "ignored"}
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-f", Message: msg}
	_, errs := collect(ex.Execute(context.Background(), execCtx))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if fr.gotIn.Agent != "only-agent" {
		t.Errorf("agent = %q, want only-agent (no resolver ⇒ fixed)", fr.gotIn.Agent)
	}
}
