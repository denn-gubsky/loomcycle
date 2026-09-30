package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A compare-and-set of the record writes only over the record the caller
// read: a stale prev is refused and leaves the newer record in place, a run
// with no record matches an empty prev, and an unknown run is not found.
func testSetRunConfigCASRefusesAStalePrev(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	a := json.RawMessage(`{"review":true}`)
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_cas", RunConfig: a})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if ok, err := s.SetRunConfigCAS(ctx, run.ID, a, json.RawMessage(`{"review":false}`)); err != nil || !ok {
		t.Fatalf("CAS over the record as written: ok=%v err=%v, want applied", ok, err)
	}
	if ok, err := s.SetRunConfigCAS(ctx, run.ID, a, json.RawMessage(`{"interactive":true}`)); err != nil || ok {
		t.Fatalf("CAS over a stale record: ok=%v err=%v, want refused", ok, err)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !jsonEqual(got.RunConfig, `{"review":false}`) {
		t.Fatalf("after a refused CAS the record is %s, want the applied one", got.RunConfig)
	}
	// What a read returned is always a valid prev, whatever the backend did to
	// the text (Postgres re-serialises jsonb).
	if ok, err := s.SetRunConfigCAS(ctx, run.ID, got.RunConfig, json.RawMessage(`{"review":true}`)); err != nil || !ok {
		t.Fatalf("CAS over the record as read back: ok=%v err=%v, want applied", ok, err)
	}

	bare, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_cas_bare"})
	if err != nil {
		t.Fatalf("CreateRun(no config): %v", err)
	}
	if ok, err := s.SetRunConfigCAS(ctx, bare.ID, json.RawMessage(`{}`), a); err != nil || ok {
		t.Fatalf("CAS expecting a record on a run with none: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := s.SetRunConfigCAS(ctx, bare.ID, nil, a); err != nil || !ok {
		t.Fatalf("CAS expecting no record on a run with none: ok=%v err=%v, want applied", ok, err)
	}

	var nf *store.ErrNotFound
	if _, err := s.SetRunConfigCAS(ctx, "r_missing", nil, a); !errors.As(err, &nf) {
		t.Fatalf("CAS on an unknown run: err=%v, want not found", err)
	}
}

// Writers that each read the record, set their own field and compare-and-set
// it back, retrying on a refusal, all keep their field. A plain replace kept
// only the last writer's read, so the others' fields were lost.
func testSetRunConfigCASConcurrentWritersAllSurvive(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_cas_conc"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for attempt := 0; attempt < 200; attempt++ {
				cur, err := s.GetRun(ctx, run.ID)
				if err != nil {
					errs <- err
					return
				}
				rec := map[string]any{}
				if len(cur.RunConfig) > 0 {
					if err := json.Unmarshal(cur.RunConfig, &rec); err != nil {
						errs <- err
						return
					}
				}
				rec[key] = true
				next, _ := json.Marshal(rec)
				ok, err := s.SetRunConfigCAS(ctx, run.ID, cur.RunConfig, next)
				if err != nil {
					errs <- err
					return
				}
				if ok {
					return
				}
			}
			errs <- fmt.Errorf("%s: never won the compare-and-set", key)
		}(fmt.Sprintf("k%d", i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(got.RunConfig, &rec); err != nil {
		t.Fatalf("record %q: %v", got.RunConfig, err)
	}
	for i := 0; i < writers; i++ {
		if rec[fmt.Sprintf("k%d", i)] != true {
			t.Errorf("k%d was lost: record %s", i, got.RunConfig)
		}
	}
}
