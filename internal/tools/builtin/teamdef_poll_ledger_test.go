package builtin

import (
	"reflect"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ledgerRecorder is a run's event emitter that keeps its spawn-ledger
// events, started and result alike.
type ledgerRecorder struct {
	mu     sync.Mutex
	events []providers.Event
}

func (l *ledgerRecorder) emit(ev providers.Event) {
	if ev.Type != providers.EventSpawnChildStarted && ev.Type != providers.EventSpawnChildResult {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *ledgerRecorder) all() []providers.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]providers.Event(nil), l.events...)
}

// A walk run in poll mode is recorded on its caller's transcript as a spawn
// ledger row — the one Agent poll mode records for a child — naming the call,
// the walk's run, mode poll, kind team and the team version that runs. A
// waited-for or detached walk is not the caller's background child and
// records none, nor does a poll-mode walk refused after it was filed: its id
// was never handed out.
func TestTeamDefPoll_TheWalkIsRecordedOnTheCallersSpawnLedger(t *testing.T) {
	tool, ctx, bg, g, _, done := pollFixture(t)
	defer done()
	led := &ledgerRecorder{}
	ctx = tools.WithEventEmitter(tools.WithToolUseID(ctx, "tu_walk"), led.emit)

	started := decodeResult(t, runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"the diff","mode":"poll"}`).Text)
	id, _ := started["run_id"].(string)
	defID, _ := started["def_id"].(string)
	events := led.all()
	if len(events) != 1 || events[0].Type != providers.EventSpawnChildStarted || id == "" || defID == "" {
		t.Fatalf("ledger after the poll-mode run %v = %+v, want one spawn_child_started", started, events)
	}
	want := providers.SpawnChildEventInfo{ToolUseID: "tu_walk", RunID: id, Agent: "team:rev", Mode: "poll",
		Kind: "team", Team: "rev", DefID: defID}
	if !reflect.DeepEqual(*events[0].SpawnChild, want) {
		t.Errorf("ledger row = %+v, want %+v", *events[0].SpawnChild, want)
	}

	close(g.release)
	waitUntil(t, func() bool { v, _ := bg.Lookup(id); return v.Ended() })
	decodeWalkPoll(t, runTD(t, tool, ctx, `{"op":"poll","run_ids":["`+id+`"]}`))
	if res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x"}`); res.IsError {
		t.Fatalf("waited-for run: %s", res.Text)
	}
	if res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x","mode":"detach"}`); res.IsError {
		t.Fatalf("detached run: %s", res.Text)
	}
	if res := runTD(t, tool, ctx, `{"op":"run","name":"rev","input":"x","mode":"poll","breakpoints":["nowhere"]}`); !res.IsError {
		t.Fatalf("want the bad breakpoint refused, got %s", res.Text)
	}
	if events := led.all(); len(events) != 1 {
		t.Errorf("ledger = %+v; reading the walk, a waited-for, a detached and a refused walk must add none", events)
	}
}
