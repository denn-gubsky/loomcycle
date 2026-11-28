package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC GW P12 plan §3.4 (PR 4): a worker that restarts. RFC GW §11
// "Failure": restarted, a durable workspace answers again with its files and
// its pin.

func (f *fixture) openDurable(t *testing.T, o Owner, name string) store.WorkspaceRow {
	t.Helper()
	f.m.SetDurable(DurableLimits{Enabled: true, MaxPerUser: 8})
	r, err := f.m.Open(context.Background(), o, OpenRequest{Name: name, Toolchains: []string{"python", "node"}, Lifetime: store.WorkspaceLifetimeDurable})
	if err != nil {
		t.Fatalf("open durable %s: %v", name, err)
	}
	return r
}

func TestRestart_DurableAnswersAgainWithFilesAndPin(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{})
	ctx := context.Background()
	o := f.run(t, "alice")
	r := f.openDurable(t, o, "work")
	if err := os.WriteFile(filepath.Join(f.h.Dir(r.ID), "kept.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.StopWorker()
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, _ := f.m.MarkLost(ctx); n != 1 {
		t.Fatalf("marked lost %d, want 1", n)
	}
	f.m.now = time.Now
	f.h.StartWorker()
	waitState(t, f, r.ID, store.WorkspaceReady)
	got, err := f.m.Get(ctx, o, "work")
	if err != nil || got.ID != r.ID {
		t.Fatalf("the run's pin after the restart: %+v, %v", got, err)
	}
	if b, err := os.ReadFile(filepath.Join(f.h.Dir(r.ID), "kept.txt")); err != nil || string(b) != "kept" {
		t.Errorf("the file after the restart: %q, %v", b, err)
	}
}

// One whose run ended while its worker was away comes back, and the
// sweeper parks it: it is never removed.
func TestRestart_DurableOfAnEndedRunComesBackParked(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{})
	ctx := context.Background()
	o := f.run(t, "alice")
	r := f.openDurable(t, o, "work")
	f.h.StopWorker()
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, _ := f.m.MarkLost(ctx); n != 1 {
		t.Fatalf("marked lost %d, want 1", n)
	}
	f.m.now = time.Now
	f.finish(t, o.RunID, store.RunCompleted)
	f.h.StartWorker()
	waitState(t, f, r.ID, store.WorkspaceReady)
	if _, err := f.m.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.WorkspaceGet(ctx, "acme", r.ID)
	if err != nil || got.OwnerKind != store.WorkspaceOwnerParked {
		t.Fatalf("after the sweep: %+v, %v; want parked", got, err)
	}
	if _, err := os.Stat(f.h.Dir(r.ID)); err != nil {
		t.Errorf("its directory: %v", err)
	}
}

// A workspace marked lost closes its pages, and its run is told so
// (surface_closed, reason lost: ruling Q6).
func TestRestart_LostClosesItsPages(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{})
	ctx := context.Background()
	f.m.SetExpose(ExposeLimits{Enabled: true, MaxPerWorkspace: 2, IdleTimeout: time.Hour})
	var mu sync.Mutex
	var told []SurfaceClosed
	f.m.SetSurfaceNotifier(func(_ context.Context, _ string, c SurfaceClosed) {
		mu.Lock()
		told = append(told, c)
		mu.Unlock()
	})
	o := f.run(t, "alice")
	r := f.open(t, o, "web")
	e, err := f.m.Expose(ctx, o, "web", 8000, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.h.StopWorker()
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, _ := f.m.MarkLost(ctx); n != 1 {
		t.Fatalf("marked lost %d, want 1", n)
	}
	if list, _ := f.st.WorkspaceExposureListByWorkspace(ctx, "acme", r.ID); len(list) != 0 {
		t.Errorf("exposures of a lost workspace: %d, want none", len(list))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || told[0].ExposureID != e.ID || told[0].Reason != SurfaceLost {
		t.Errorf("told %+v, want one close of %s with reason lost", told, e.ID)
	}
}

func waitState(t *testing.T, f *fixture, id string, want store.WorkspaceState) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := f.st.WorkspaceGet(context.Background(), "acme", id)
		if err == nil && got.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%v), want %s", id, got.State, err, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
