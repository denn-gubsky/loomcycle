package http

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// interleavingStore makes the first n GetRun calls after arm wait for each
// other, so n writers all read the SAME record before any of them writes —
// the interleaving that lost a field, made deterministic.
type interleavingStore struct {
	store.Store
	mu      sync.Mutex
	pending int
	all     chan struct{}
}

func (s *interleavingStore) arm(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending, s.all = n, make(chan struct{})
}

func (s *interleavingStore) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	s.mu.Lock()
	all := s.all
	if s.pending > 0 {
		s.pending--
		if s.pending == 0 {
			close(s.all)
		}
	} else {
		all = nil
	}
	s.mu.Unlock()
	if all != nil {
		select {
		case <-all:
		case <-time.After(5 * time.Second):
		}
	}
	return run, err
}

// refusingStore refuses every compare-and-set, as a record that some other
// writer changes between every read and write would.
type refusingStore struct{ store.Store }

func (refusingStore) SetRunConfigCAS(context.Context, string, json.RawMessage, json.RawMessage) (bool, error) {
	return false, nil
}

// seedRunConfig replaces a test run's record with cfg, whatever it held.
func seedRunConfig(t *testing.T, st store.Store, runID string, cfg json.RawMessage) {
	t.Helper()
	ctx := context.Background()
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetRunConfigCAS(ctx, runID, run.RunConfig, cfg); err != nil || !ok {
		t.Fatalf("seed run config: written=%v err=%v", ok, err)
	}
}

// The run record's writers — a retune, the review-arming record and the hook
// pins — each read the record, set their own field and write it back. Before,
// the write replaced the column, so of three that read the same record only
// the last one's field survived. Each now writes only over what it read and
// re-applies its change otherwise, so all three fields are kept.
func TestUpdateRunConfig_InterleavedWritersKeepEachOthersFields(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	cfg := *srv.cfg()
	cfg.Agents = map[string]config.AgentDef{"writer": {Model: "stub-model"}}
	srv.cfgHolder = config.NewHolder(&cfg)
	st := &interleavingStore{Store: srv.store}
	srv.store = st
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_rmw", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	st.arm(3)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		if _, err := srv.retuneRun(ctx, run, &runOverridesWire{MaxTokens: 512}); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if !srv.writeReviewRecord(ctx, run.ID, true, 90*time.Second) {
			errs <- errors.New("review arming was not recorded")
		}
	}()
	go func() {
		defer wg.Done()
		if err := srv.recordPinnedHooks(ctx, run.ID, &pinnedHooks{Agent: "fp"}); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	rec, ok := decodeRunConfig(mustGetRun(t, st, run.ID).RunConfig)
	if !ok {
		t.Fatal("the run has no readable record")
	}
	if rec.Resources == nil || rec.Resources.MaxTokens != 512 {
		t.Errorf("the retune was lost: resources %+v", rec.Resources)
	}
	if rec.Review == nil || !*rec.Review || rec.ReviewTTLSeconds != 90 {
		t.Errorf("the review arming was lost: review %v ttl %d", rec.Review, rec.ReviewTTLSeconds)
	}
	if rec.PinnedHooks == nil || rec.PinnedHooks.Agent != "fp" {
		t.Errorf("the hook pins were lost: %+v", rec.PinnedHooks)
	}
}

// A record that never stops changing under the writer is not written over,
// and the writer says so, rather than retrying forever or falling back to a
// blind replace.
func TestUpdateRunConfig_GivesUpWhenEveryWriteIsRefused(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_rmw_refused"})
	if err != nil {
		t.Fatal(err)
	}
	srv.store = refusingStore{Store: srv.store}
	calls := 0
	_, err = srv.updateRunConfig(ctx, run.ID, func(rec *runConfigRecord, _ bool) error {
		calls++
		rec.PinnedHooks = &pinnedHooks{Agent: "fp"}
		return nil
	})
	if !errors.Is(err, errRunConfigContended) {
		t.Fatalf("err %v, want contended", err)
	}
	if calls != runConfigWriteAttempts {
		t.Errorf("change applied %d times, want %d (one per fresh read)", calls, runConfigWriteAttempts)
	}
	if got := mustGetRun(t, srv.store, run.ID).RunConfig; len(got) != 0 {
		t.Errorf("record written despite every write being refused: %s", got)
	}
}
