package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// ledger collects what a table records.
type ledger struct {
	mu  sync.Mutex
	evs []providers.Event
}

func (l *ledger) record(ev providers.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

func (l *ledger) of(typ providers.EventType) []providers.SpawnChildEventInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []providers.SpawnChildEventInfo
	for _, ev := range l.evs {
		if ev.Type == typ && ev.SpawnChild != nil {
			out = append(out, *ev.SpawnChild)
		}
	}
	return out
}

// A child's end is recorded once, with what it handed back, and the parent's
// read of it once — a second read, a second Finish and anything after the
// table closed record nothing. These rows are what a resumed run rebuilds the
// table from.
func TestBackground_RecordsEachEndAndFirstReadOnce(t *testing.T) {
	b := NewBackground(context.Background())
	l := &ledger{}
	b.RecordTo(l.record)
	for _, s := range []ChildSpec{
		{RunID: "r_1", Agent: "writer", Index: 0, BatchID: "fan_1"},
		{RunID: "r_w", Agent: "team:triage", Index: -1, Kind: ChildKindTeam},
		{RunID: "r_late", Agent: "writer", Index: -1},
	} {
		if _, err := b.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	b.Finish("r_1", ChildCompleted, ChildResult{Output: "done", Structured: map[string]any{"k": "v"}})
	b.Finish("r_1", ChildFailed, ChildResult{Error: "late"})
	b.Finish("r_w", ChildFailed, ChildResult{Error: "run: capped", Detail: map[string]any{"status": "iteration_cap"}})
	b.MarkRead([]string{"r_1", "r_late"}) // r_late is running: not read
	b.MarkRead([]string{"r_1"})
	b.Close(errors.New("run ended"))
	b.Finish("r_late", ChildCancelled, ChildResult{Error: "cancelled"})

	res := l.of(providers.EventSpawnChildResult)
	if len(res) != 2 {
		t.Fatalf("recorded %d results, want 2 (none after the table closed): %+v", len(res), res)
	}
	if r := res[0]; r.RunID != "r_1" || r.Mode != "poll" || r.BatchID != "fan_1" || !r.Ok || r.Ended != ChildCompleted ||
		r.Output != "done" || r.State["k"] != "v" {
		t.Errorf("first result = %+v", r)
	}
	if r := res[1]; r.RunID != "r_w" || r.Kind != ChildKindTeam || r.Ok || r.Ended != ChildFailed ||
		r.Error != "run: capped" || r.Detail["status"] != "iteration_cap" {
		t.Errorf("walk result = %+v", r)
	}
	if reads := l.of(providers.EventSpawnChildRead); len(reads) != 1 || reads[0].RunID != "r_1" {
		t.Errorf("reads = %+v, want r_1 once", reads)
	}
}

// A restored table answers as the live one did: a child already read is not
// handed over by a bare select, a child already noted is not noted again, and
// an outstanding child holds the run until it is finished — then it is noted.
func TestBackground_RestoredChildrenKeepReadAndNotedState(t *testing.T) {
	b := NewBackground(context.Background())
	for _, r := range []RestoredChild{
		{ChildSpec: ChildSpec{RunID: "r_read", Agent: "a", Index: -1, Notify: true}, State: ChildCompleted, Read: true, Noted: true},
		{ChildSpec: ChildSpec{RunID: "r_noted", Agent: "a", Index: -1, Notify: true}, State: ChildCompleted, Noted: true,
			Result: ChildResult{Output: "kept"}},
		{ChildSpec: ChildSpec{RunID: "r_out", Agent: "a", Index: -1, Notify: true}, State: ChildRunning},
	} {
		b.Restore(r)
	}
	// A run id already filed is left alone.
	b.Restore(RestoredChild{ChildSpec: ChildSpec{RunID: "r_noted", Agent: "other", Index: -1}, State: ChildFailed})

	got, _ := b.Select(nil, "")
	if len(got) != 2 || got[0].RunID != "r_noted" || got[0].Result.Output != "kept" || got[1].RunID != "r_out" {
		t.Fatalf("bare select = %+v, want r_noted (kept) and r_out", got)
	}
	if n, _ := b.TakeNotes(); n != "" {
		t.Errorf("a child noted before the resume was noted again: %q", n)
	}
	if waiting, _ := b.Outstanding(); len(waiting) != 1 || waiting[0].RunID != "r_out" {
		t.Fatalf("outstanding = %+v, want r_out", waiting)
	}
	b.Finish("r_out", ChildCompleted, ChildResult{Output: "late"})
	if n, ids := b.TakeNotes(); n == "" || len(ids) != 1 || ids[0] != "r_out" {
		t.Errorf("note after the outstanding child ended = %q %v, want it alone", n, ids)
	}
}

// A restored child is cancelled through the cancel it was restored with —
// by a cancel naming it and by the run's lifetime ending — and never once it
// has ended: Finish frees nothing on it.
func TestBackground_RestoredChildIsCancelledThroughItsRun(t *testing.T) {
	run, endRun := context.WithCancelCause(context.Background())
	b := NewBackground(run)
	var mu sync.Mutex
	cancelled := map[string]error{}
	cancelOf := func(id string) func(error) {
		return func(cause error) {
			mu.Lock()
			defer mu.Unlock()
			cancelled[id] = cause
		}
	}
	for _, id := range []string{"r_named", "r_life", "r_done"} {
		b.Restore(RestoredChild{ChildSpec: ChildSpec{RunID: id, Index: -1}, State: ChildRunning, Cancel: cancelOf(id)})
	}
	b.Finish("r_done", ChildCompleted, ChildResult{})
	named := errors.New("cancelled by its parent")
	if !b.Cancel("r_named", named) {
		t.Fatal("a restored child is not this run's")
	}
	b.Finish("r_named", ChildCancelled, ChildResult{})
	life := errors.New("parent ended")
	endRun(life)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := cancelled["r_life"]
		mu.Unlock()
		if got != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(cancelled["r_named"], named) {
		t.Errorf("r_named cancelled with %v, want the parent's cancel", cancelled["r_named"])
	}
	if !errors.Is(cancelled["r_life"], life) {
		t.Errorf("r_life cancelled with %v, want the run's cause", cancelled["r_life"])
	}
	if _, ok := cancelled["r_done"]; ok {
		t.Error("an ended child was cancelled")
	}
}
