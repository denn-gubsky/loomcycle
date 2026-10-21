package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// endsOnStatusReadStore lets a run finish at the worst moment for a store tail:
// on the tail's first read of the run's status, it writes the run's last event
// and marks the run completed — after the tail has drained, before it reads the
// status that tells it to stop.
type endsOnStatusReadStore struct {
	store.Store
	once   sync.Once
	finish func()
}

func (s *endsOnStatusReadStore) GetRun(ctx context.Context, runID string) (store.Run, error) {
	s.once.Do(s.finish)
	return s.Store.GetRun(ctx, runID)
}

// A run that ends between the tail's drain and its status read still has its
// last event relayed: a terminal status means every event is written, so the
// tail drains once more before it stops. It used to stop at once, and the
// run's done never reached the client.
func TestStreamRunEvents_RelaysTheEventsWrittenJustBeforeTheTerminalStatus(t *testing.T) {
	base, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	ctx := context.Background()
	sess, err := base.CreateSession(ctx, "", "default", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := base.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_tail"})
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(ev providers.Event) {
		t.Helper()
		b, _ := json.Marshal(ev)
		if err := base.AppendEvent(ctx, run.ID, string(ev.Type), b); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(providers.Event{Type: providers.EventText, Text: "hello"})

	st := &endsOnStatusReadStore{Store: base, finish: func() {
		appendEvent(providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
		if err := base.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Error(err)
		}
	}}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"default": {Model: "stub-model"}},
	}
	srv := New(cfg, &stubResolver{p: &stubProvider{}}, nil, concurrency.New(1, 1, time.Second), st)

	tailCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var got []providers.EventType
	if err := srv.streamRunEvents(tailCtx, run.ID, 0, func(ev providers.Event) error {
		got = append(got, ev.Type)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if tailCtx.Err() != nil {
		t.Fatal("the tail did not stop on the run's terminal status")
	}
	if len(got) != 2 || got[0] != providers.EventText || got[1] != providers.EventDone {
		t.Errorf("relayed %v, want [text done]: the run's last event was dropped", got)
	}
}
