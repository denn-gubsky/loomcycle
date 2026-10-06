package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type ctxKeyProbe struct{}

// A background child keeps the spawning call's values but not its
// cancellation, and is cancelled — with the run's cause — when the run's
// lifetime ends.
func TestBackground_ChildOutlivesItsCallButNotItsRun(t *testing.T) {
	run, endRun := context.WithCancelCause(context.Background())
	b := NewBackground(run)
	call, endCall := context.WithCancel(context.WithValue(context.Background(), ctxKeyProbe{}, "v"))
	cctx, err := b.Start(call, ChildSpec{RunID: "r_1", Agent: "a", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	endCall()
	if cctx.Err() != nil {
		t.Fatal("the child was cancelled when the call that started it returned")
	}
	if cctx.Value(ctxKeyProbe{}) != "v" {
		t.Error("the child lost the call's values")
	}
	why := errors.New("parent cancelled")
	endRun(why)
	select {
	case <-cctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the child outlived its run")
	}
	if !errors.Is(context.Cause(cctx), why) {
		t.Errorf("child cause = %v, want the run's", context.Cause(cctx))
	}
}

// Outstanding holds queued, running and held children; a finished one, a
// resident one and one its parent's turn end is cancelling do not.
func TestBackground_OutstandingIsOnlyWhatHoldsTheRun(t *testing.T) {
	b := NewBackground(context.Background())
	for _, s := range []ChildSpec{
		{RunID: "r_q", Index: -1},
		{RunID: "r_h", Index: -1},
		{RunID: "r_done", Index: -1},
		{RunID: "r_end", Index: -1, CancelOnParentEnd: true},
	} {
		if _, err := b.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	b.AddResident("r_res", "res")
	b.SetState("r_h", ChildHeld)
	b.Finish("r_done", ChildCompleted, ChildResult{Output: "ok"})
	b.EndTurn(errors.New("turn ended"))
	got, _ := b.Outstanding()
	var ids []string
	for _, v := range got {
		ids = append(ids, v.RunID+"="+v.State)
	}
	if strings.Join(ids, ",") != "r_q=queued,r_h=held" {
		t.Errorf("outstanding = %v, want [r_q=queued r_h=held]", ids)
	}
}

// Finish wakes a watcher and counts once; a later Finish or SetState does not
// change the final state.
func TestBackground_FinishIsFinalAndWakesWatchers(t *testing.T) {
	b := NewBackground(context.Background())
	if _, err := b.Start(context.Background(), ChildSpec{RunID: "r_1", Index: -1}); err != nil {
		t.Fatal(err)
	}
	_, changed := b.Outstanding()
	b.Finish("r_1", ChildTimeout, ChildResult{Status: "timeout"})
	select {
	case <-changed:
	default:
		t.Fatal("Finish did not wake the watcher")
	}
	b.Finish("r_1", ChildCompleted, ChildResult{Output: "late"})
	b.SetState("r_1", ChildRunning)
	if v, _ := b.Lookup("r_1"); v.State != ChildTimeout || v.Result.Output != "" {
		t.Errorf("child = %+v, want it to stay timed out", v)
	}
}

// A note names each ended child once, skips one started with notify off or
// already read, and several share one note.
func TestBackground_NotesNameEachEndedChildOnce(t *testing.T) {
	b := NewBackground(context.Background())
	for _, s := range []ChildSpec{
		{RunID: "r_1", Agent: "researcher", Index: -1, Notify: true},
		{RunID: "r_2", Agent: "writer", Index: -1, Notify: true},
		{RunID: "r_3", Agent: "quiet", Index: -1},
		{RunID: "r_4", Agent: "read", Index: -1, Notify: true},
	} {
		if _, err := b.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := b.TakeNotes(); n != "" {
		t.Fatalf("note before any child ended: %q", n)
	}
	b.Finish("r_1", ChildCompleted, ChildResult{})
	if n, _ := b.TakeNotes(); n != "Background child r_1 (researcher) finished: completed. Use Agent poll to read its result." {
		t.Errorf("one child: %q", n)
	}
	b.Finish("r_2", ChildFailed, ChildResult{})
	b.Finish("r_3", ChildCompleted, ChildResult{})
	b.Finish("r_4", ChildCompleted, ChildResult{})
	b.MarkRead([]string{"r_4"})
	if n, _ := b.TakeNotes(); n != "Background child r_2 (writer) finished: failed. Use Agent poll to read its result." {
		t.Errorf("after notify-off and read children: %q", n)
	}
	if n, _ := b.TakeNotes(); n != "" {
		t.Errorf("a child was noted twice: %q", n)
	}
}

// With no ids, Select returns the background children not read yet — so a
// loop of bare polls hands each result over once; by id it returns them every
// time, and an id this run did not start is unknown.
func TestBackground_SelectUnreadOnceByIDAlways(t *testing.T) {
	b := NewBackground(context.Background())
	for _, s := range []ChildSpec{{RunID: "r_1", Index: 0, BatchID: "fan_1"}, {RunID: "r_2", Index: 1, BatchID: "fan_1"}} {
		if _, err := b.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	b.AddResident("r_res", "res")
	b.Finish("r_1", ChildCompleted, ChildResult{Output: "one"})
	got, _ := b.Select(nil, "")
	if len(got) != 2 {
		t.Fatalf("unread = %d children, want 2 (the resident child is not listed)", len(got))
	}
	b.MarkRead([]string{"r_1", "r_2"}) // r_2 is still running: not read
	if got, _ := b.Select(nil, ""); len(got) != 1 || got[0].RunID != "r_2" {
		t.Errorf("after reading r_1, unread = %+v, want only r_2", got)
	}
	if got, _ := b.Select([]string{"r_1"}, ""); len(got) != 1 || got[0].Result.Output != "one" {
		t.Errorf("by id = %+v, want r_1's result again", got)
	}
	if got, _ := b.Select(nil, "fan_1"); len(got) != 2 {
		t.Errorf("by batch = %d children, want 2", len(got))
	}
	if _, unknown := b.Select([]string{"r_1", "r_other"}, ""); len(unknown) != 1 || unknown[0] != "r_other" {
		t.Errorf("unknown = %v, want [r_other]", unknown)
	}
	if _, unknown := b.Select(nil, "fan_other"); len(unknown) != 1 {
		t.Errorf("an unknown batch was not reported: %v", unknown)
	}
}

// Close cancels every outstanding child and refuses new ones.
func TestBackground_CloseCancelsOutstandingAndRefusesMore(t *testing.T) {
	b := NewBackground(context.Background())
	cctx, err := b.Start(context.Background(), ChildSpec{RunID: "r_1", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	why := errors.New("run ended")
	b.Close(why)
	if !errors.Is(context.Cause(cctx), why) {
		t.Errorf("outstanding child cause = %v, want the close cause", context.Cause(cctx))
	}
	if _, err := b.Start(context.Background(), ChildSpec{RunID: "r_2", Index: -1}); err == nil {
		t.Error("a child was started after Close")
	}
}

// A run that ended because it was cancelled passes ITS cancel to the children
// it closes over, even when Close runs before the lifetime watch fires — so a
// child of a cancelled parent is recorded cancelled for the parent's reason.
func TestBackground_CloseAfterACancelPassesTheRunsCause(t *testing.T) {
	run, endRun := context.WithCancelCause(context.Background())
	b := NewBackground(run)
	cctx, err := b.Start(context.Background(), ChildSpec{RunID: "r_1", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	b.byID["r_1"].stop() // hold the watch off: Close runs first
	why := errors.New("operator cancelled the parent")
	endRun(why)
	b.Close(errors.New("run ended"))
	if !errors.Is(context.Cause(cctx), why) {
		t.Errorf("child cause = %v, want the run's cancel", context.Cause(cctx))
	}
}

// A team walk is read with TeamDef poll, so a note naming one says so — a run
// that started a walk need not have the Agent tool. A mixed note names both.
func TestBackground_NotesNameTheCallThatReadsEachKind(t *testing.T) {
	b := NewBackground(context.Background())
	for _, s := range []ChildSpec{
		{RunID: "r_w", Agent: "team:triage", Index: -1, Notify: true, Kind: ChildKindTeam},
		{RunID: "r_a", Agent: "writer", Index: -1, Notify: true},
	} {
		if _, err := b.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	b.Finish("r_w", ChildCompleted, ChildResult{Output: "done"})
	if n, _ := b.TakeNotes(); n != "Background child r_w (team:triage) finished: completed. Use TeamDef poll to read its result." {
		t.Errorf("one walk: %q", n)
	}
	b.Finish("r_a", ChildCompleted, ChildResult{})
	wake, _ := b.WakeNote([]string{"r_w", "r_a"})
	if !strings.HasSuffix(wake, "Use Agent poll to read the sub-agents' results and TeamDef poll to read the team walks'.") {
		t.Errorf("mixed wake note: %q", wake)
	}
}

// A child withdrawn after Start — its start was refused, its id never handed
// out — is not this run's: no poll selects it, no note names it, nothing
// waits for it, and its ctx is released.
func TestBackground_AWithdrawnChildIsNeverReported(t *testing.T) {
	b := NewBackground(context.Background())
	cctx, err := b.Start(context.Background(), ChildSpec{RunID: "r_1", Agent: "team:x", Index: -1, Notify: true, Kind: ChildKindTeam})
	if err != nil {
		t.Fatal(err)
	}
	b.Withdraw("r_1")
	if cctx.Err() == nil {
		t.Error("a withdrawn child's ctx was not released")
	}
	if _, ok := b.Lookup("r_1"); ok {
		t.Error("a withdrawn child can still be looked up")
	}
	if out, unknown := b.Select(nil, ""); len(out) != 0 || len(unknown) != 0 {
		t.Errorf("a bare select found %v (unknown %v)", out, unknown)
	}
	if waiting, _ := b.Outstanding(); len(waiting) != 0 {
		t.Errorf("a withdrawn child holds the run: %v", waiting)
	}
	b.Finish("r_1", ChildCompleted, ChildResult{})
	if n, _ := b.TakeNotes(); n != "" {
		t.Errorf("a withdrawn child was noted: %q", n)
	}
}
