package limits

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// midReadStore is a fakeStore whose TokenLimitsAll runs a hook between taking
// its answer and returning it, so a test can land a PutLimit in the window a
// reload reads the store in.
type midReadStore struct {
	fakeStore
	during func()
}

func (m *midReadStore) TokenLimitsAll(ctx context.Context) ([]store.TokenLimitRow, error) {
	rows, err := m.fakeStore.TokenLimitsAll(ctx)
	if m.during != nil {
		m.during()
	}
	return rows, err
}

// TestTracker_AReloadPicksUpALimitAnotherReplicaWrote: a limit row written
// after this replica seeded (by another replica's PUT /v1/_limits, or a gate
// provisioning a user there) is enforced here once the cache reloads.
func TestTracker_AReloadPicksUpALimitAnotherReplicaWrote(t *testing.T) {
	fs := &fakeStore{aggs: []store.UsageAggregate{agg("acme", "u1", 900, 0)}}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatalf("no row yet: want allowed, got %+v", dec)
	}

	fs.limits = []store.TokenLimitRow{{TenantID: "acme", Scope: "user", ScopeID: "u1", HardLimit: i64(500)}}
	if err := tr.ReloadLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); dec.Allowed {
		t.Fatalf("after reload the other replica's hard limit 500 (used 900) must refuse; got %+v", dec)
	}

	// A row another replica deleted stops applying too.
	fs.limits = nil
	if err := tr.ReloadLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); !dec.Allowed {
		t.Fatalf("after the row is gone the user is unlimited again; got %+v", dec)
	}
}

// TestTracker_AFailedReloadKeepsTheCachedLimits: a store fault during a reload
// must not leave a persisted budget unenforced.
func TestTracker_AFailedReloadKeepsTheCachedLimits(t *testing.T) {
	fs := &fakeStore{
		aggs:   []store.UsageAggregate{agg("acme", "u1", 900, 0)},
		limits: []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64(500)}},
	}
	tr := New(fs)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs.err = errors.New("store down")
	if err := tr.ReloadLimits(context.Background()); err == nil {
		t.Fatal("a failed read must be reported")
	}
	if dec := tr.Check("acme", "u1"); dec.Allowed {
		t.Fatalf("a failed reload dropped the cached hard limit; got %+v", dec)
	}
}

// TestTracker_AReloadNeverUndoesAWriteItRaced: a PutLimit or DeleteLimit that
// lands while a reload is reading the store is newer than what the reload
// read, so the reload must not overwrite it with the older snapshot.
func TestTracker_AReloadNeverUndoesAWriteItRaced(t *testing.T) {
	ms := &midReadStore{fakeStore: fakeStore{aggs: []store.UsageAggregate{agg("acme", "u1", 900, 0)}}}
	tr := New(ms)
	if err := tr.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms.during = func() {
		tr.PutLimit(store.TokenLimitRow{TenantID: "acme", Scope: "user", ScopeID: "u1", HardLimit: i64(500)})
	}
	if err := tr.ReloadLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec := tr.Check("acme", "u1"); dec.Allowed {
		t.Fatalf("the reload's older snapshot undid a limit written during it; got %+v", dec)
	}
}

// TestRunLimitsReloader_ReloadsOnEveryTickUntilCancelled: the loop reloads on
// its interval and stops with its context.
func TestRunLimitsReloader_ReloadsOnEveryTickUntilCancelled(t *testing.T) {
	var reads atomic.Int64
	ms := &midReadStore{during: func() { reads.Add(1) }}
	tr := New(ms)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLimitsReloader(ctx, tr, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("reloader read the store %d times in 5s, want at least 3", reads.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reloader did not stop when its context was cancelled")
	}
}
