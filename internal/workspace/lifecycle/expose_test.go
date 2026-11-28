package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC HI P1: a page's close is kept as state (D6), its own idle timeout
// (§3), and its state on request (§4).

func exposeStateFixture(t *testing.T, l ExposeLimits) (*fixture, Owner) {
	t.Helper()
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	l.Enabled = true
	if l.MaxPerWorkspace == 0 {
		l.MaxPerWorkspace = 2
	}
	f.m.SetExpose(l)
	o := f.run(t, "alice")
	f.open(t, o, "web")
	return f, o
}

func mustExpose(t *testing.T, f *fixture, o Owner, port, idleS int) store.WorkspaceExposure {
	t.Helper()
	e, err := f.m.Expose(context.Background(), o, "web", port, "Chart", "", idleS)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func wantClosed(t *testing.T, f *fixture, id, reason string) store.WorkspaceExposure {
	t.Helper()
	e, err := f.m.ExposureState(context.Background(), "acme", "alice", id)
	if err != nil || e.Live() || e.ClosedReason != reason || e.ClosedAt == nil {
		t.Fatalf("state = %+v, %v; want closed %s", e, err, reason)
	}
	if _, _, err := f.m.OpenExposure(context.Background(), "acme", "alice", id); !errors.Is(err, ErrExposureNotFound) {
		t.Errorf("the closed page still opens: %v", err)
	}
	return e
}

// An idle sweep closes the page and keeps its row: the state reads closed,
// idle, with no stream open, and the route refuses it as an unknown id.
func TestExposureState_ReportsIdleAfterTheSweep(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{IdleTimeout: 15 * time.Minute})
	e := mustExpose(t, f, o, 8000, 0)
	got, err := f.m.ExposureState(context.Background(), "acme", "alice", e.ID)
	if err != nil || !got.Live() || got.WorkspaceName != "web" || got.RunID != o.RunID || got.Title != "Chart" {
		t.Fatalf("live state = %+v, %v", got, err)
	}
	if want := got.LastUsedAt.Add(15 * time.Minute); !f.m.ExposureDeadline(got).Equal(want) {
		t.Errorf("deadline = %v, want last use + 15m (%v)", f.m.ExposureDeadline(got), want)
	}
	f.m.now = func() time.Time { return time.Now().Add(20 * time.Minute) }
	if n, err := f.m.SweepExposures(context.Background()); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	wantClosed(t, f, e.ID, SurfaceIdle)
}

// A page past its deadline the sweep has not reached yet reads closed idle,
// never live: the state read closes it as the route would.
func TestExposureState_ClosesAPagePastItsDeadline(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{IdleTimeout: 15 * time.Minute})
	e := mustExpose(t, f, o, 8000, 0)
	f.m.now = func() time.Time { return time.Now().Add(20 * time.Minute) }
	wantClosed(t, f, e.ID, SurfaceIdle)
	if list, _ := f.m.UserExposures(context.Background(), "acme", "alice"); len(list) != 0 {
		t.Errorf("the user's live pages = %v, want none", list)
	}
}

// The run's end releases its workspace, which deletes the workspace's row:
// the page's state still reads closed, released, naming its workspace and
// run.
func TestExposureState_ReportsReleasedAfterTheRunEnds(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{})
	e := mustExpose(t, f, o, 8000, 0)
	if err := f.m.ReleaseRun(context.Background(), o.RunID); err != nil {
		t.Fatal(err)
	}
	got := wantClosed(t, f, e.ID, SurfaceReleased)
	if got.WorkspaceName != "web" || got.RunID != o.RunID {
		t.Errorf("closed state names workspace %q run %q, want web and %s", got.WorkspaceName, got.RunID, o.RunID)
	}
}

// unexpose keeps the row closed unexposed; the workspace's live count no
// longer holds it, so a third page fits within max 2, on the closed page's
// port too.
func TestExposureState_UnexposeKeepsTheRowAndFreesTheCount(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{MaxPerWorkspace: 2})
	a := mustExpose(t, f, o, 8000, 0)
	mustExpose(t, f, o, 8001, 0)
	if ok, err := f.m.Unexpose(context.Background(), o, "web", 8000); !ok || err != nil {
		t.Fatalf("unexpose = %v, %v", ok, err)
	}
	wantClosed(t, f, a.ID, SurfaceUnexposed)
	if ok, _ := f.m.Unexpose(context.Background(), o, "web", 8000); ok {
		t.Error("a closed page was unexposed again")
	}
	mustExpose(t, f, o, 8000, 0)
	if _, err := f.m.Expose(context.Background(), o, "web", 8002, "", "", 0); !errors.Is(err, ErrExposureLimit) {
		t.Errorf("a third live page past max 2 = %v, want the limit", err)
	}
	if list, _ := f.m.Exposures(context.Background(), o, "web"); len(list) != 2 {
		t.Errorf("the workspace's pages = %d, want the 2 live ones", len(list))
	}
}

// A closed row stays a day for its client to read, then the sweep purges it.
func TestExposureState_ClosedRowsArePurgedAfterADay(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{})
	e := mustExpose(t, f, o, 8000, 0)
	if _, err := f.m.Unexpose(context.Background(), o, "web", 8000); err != nil {
		t.Fatal(err)
	}
	f.m.now = func() time.Time { return time.Now().Add(23 * time.Hour) }
	if _, err := f.m.SweepExposures(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.WorkspaceExposureGet(context.Background(), "acme", e.ID); err != nil {
		t.Fatalf("a row closed 23h ago was purged: %v", err)
	}
	f.m.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if _, err := f.m.SweepExposures(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nf *store.ErrNotFound
	if _, err := f.st.WorkspaceExposureGet(context.Background(), "acme", e.ID); !errors.As(err, &nf) {
		t.Errorf("a row closed 25h ago = %v, want purged", err)
	}
	if _, err := f.m.ExposureState(context.Background(), "acme", "alice", e.ID); !errors.Is(err, ErrExposureNotFound) {
		t.Errorf("the purged page's state = %v, want not found", err)
	}
}

// An agent's idle_timeout_s within max_idle_timeout_s is stored and the
// sweep and the deadline use it; one outside the bound is refused, naming
// it.
func TestExposureState_PageIdleTimeoutWithinTheOperatorsBound(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{IdleTimeout: 15 * time.Minute, MaxIdleTimeout: 2 * time.Hour})
	for _, s := range []int{7201, 59, -1} {
		_, err := f.m.Expose(context.Background(), o, "web", 8000, "", "", s)
		if !errors.Is(err, ErrExposeArgument) || !strings.Contains(err.Error(), "7200") {
			t.Errorf("idle_timeout_s %d = %v, want refused naming the bound 7200", s, err)
		}
	}
	long := mustExpose(t, f, o, 8000, 7200)
	plain := mustExpose(t, f, o, 8001, 0)
	if long.IdleTimeoutS == nil || *long.IdleTimeoutS != 7200 || plain.IdleTimeoutS != nil {
		t.Fatalf("stored timeouts %v and %v, want 7200 and none", long.IdleTimeoutS, plain.IdleTimeoutS)
	}
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, _ := f.m.SweepExposures(context.Background()); n != 1 {
		t.Fatalf("sweep at +1h closed %d, want only the operator-timed page", n)
	}
	wantClosed(t, f, plain.ID, SurfaceIdle)
	got, err := f.m.ExposureState(context.Background(), "acme", "alice", long.ID)
	if err != nil || !got.Live() || !f.m.ExposureDeadline(got).Equal(got.LastUsedAt.Add(2*time.Hour)) {
		t.Fatalf("the 2h page at +1h = %+v, %v; want live until last use + 2h", got, err)
	}
	f.m.now = func() time.Time { return time.Now().Add(2*time.Hour + time.Minute) }
	if n, _ := f.m.SweepExposures(context.Background()); n != 1 {
		t.Fatalf("sweep past 2h closed %d, want the 2h page", n)
	}
	wantClosed(t, f, long.ID, SurfaceIdle)
}

// With no max_idle_timeout_s an agent may ask for no more than the
// operator's own timeout.
func TestExposureState_MaxIdleTimeoutDefaultsToTheOperatorsTimeout(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{IdleTimeout: 15 * time.Minute})
	if _, err := f.m.Expose(context.Background(), o, "web", 8000, "", "", 901); !errors.Is(err, ErrExposeArgument) {
		t.Errorf("901 over the default 900 = %v, want refused", err)
	}
	mustExpose(t, f, o, 8000, 900)
}

// The state is the page's own user's alone: another user, another tenant,
// an unknown id and the operator's switch off all read as an unknown id;
// the user's list holds their own live pages only.
func TestExposureState_OnlyForThePagesOwnUser(t *testing.T) {
	f, o := exposeStateFixture(t, ExposeLimits{})
	e := mustExpose(t, f, o, 8000, 0)
	ctx := context.Background()
	for _, c := range []struct{ name, tenant, user, id string }{
		{"bob", "acme", "bob", e.ID},
		{"tenant", "other", "alice", e.ID},
		{"unknown", "acme", "alice", "ffffffffffffffffffffffff"},
		{"bad id", "acme", "alice", "x"},
	} {
		if _, err := f.m.ExposureState(ctx, c.tenant, c.user, c.id); !errors.Is(err, ErrExposureNotFound) {
			t.Errorf("%s = %v, want not found", c.name, err)
		}
	}
	if l, _ := f.m.UserExposures(ctx, "acme", "bob"); len(l) != 0 {
		t.Errorf("bob's pages = %v", l)
	}
	if l, _ := f.m.UserExposures(ctx, "acme", "alice"); len(l) != 1 || l[0].ID != e.ID || l[0].WorkspaceName != "web" {
		t.Errorf("alice's pages = %+v", l)
	}
	f.m.SetExpose(ExposeLimits{Enabled: false})
	if _, err := f.m.ExposureState(ctx, "acme", "alice", e.ID); !errors.Is(err, ErrExposureNotFound) {
		t.Errorf("with the switch off = %v, want not found", err)
	}
}
