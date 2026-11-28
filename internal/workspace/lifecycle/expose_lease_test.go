package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// RFC HI P2: a client's lease on a page (§2) and the parked run it keeps
// (D4).

// leaseFixture is alice's run with workspace web, leases bounded at 8 h, and
// a clock the test moves.
func leaseFixture(t *testing.T, maxLease time.Duration) (*fixture, Owner, *time.Time) {
	t.Helper()
	f, o := exposeStateFixture(t, ExposeLimits{IdleTimeout: 15 * time.Minute, MaxLease: maxLease})
	clock := time.Now()
	f.m.now = func() time.Time { return clock }
	return f, o, &clock
}

func mustLease(t *testing.T, f *fixture, id string, ttl time.Duration) time.Time {
	t.Helper()
	end, err := f.m.LeaseExposure(context.Background(), "acme", "alice", id, ttl)
	if err != nil {
		t.Fatalf("lease %s for %v: %v", id, ttl, err)
	}
	return end
}

// G38 acceptance: a page with a live lease and no requests is still open
// after its idle timeout; once the lease has expired it closes at the next
// sweep, with reason idle.
func TestExposureLease_KeepsAnIdlePageOpenUntilItEnds(t *testing.T) {
	f, o, clock := leaseFixture(t, 8*time.Hour)
	e := mustExpose(t, f, o, 8000, 0)
	end := mustLease(t, f, e.ID, 2*time.Hour)
	if want := clock.UTC().Add(2 * time.Hour); !end.Equal(want) {
		t.Errorf("lease end = %v, want now + 2h (%v)", end, want)
	}
	*clock = clock.Add(time.Hour)
	if n, err := f.m.SweepExposures(context.Background()); n != 0 || err != nil {
		t.Fatalf("sweep at +1h = %d, %v; want the leased page kept", n, err)
	}
	if _, _, err := f.m.OpenExposure(context.Background(), "acme", "alice", e.ID); err != nil {
		t.Fatalf("the leased page at +1h does not open: %v", err)
	}
	*clock = clock.Add(90 * time.Minute) // +2h30: the lease has ended
	if n, err := f.m.SweepExposures(context.Background()); n != 1 || err != nil {
		t.Fatalf("sweep after the lease = %d, %v; want the page closed", n, err)
	}
	wantClosed(t, f, e.ID, SurfaceIdle)
}

// A released lease no longer holds the page: it closes idle at its next
// deadline, its last use plus its idle timeout.
func TestExposureLease_AReleasedPageClosesAtItsNextIdleDeadline(t *testing.T) {
	f, o, clock := leaseFixture(t, 8*time.Hour)
	e := mustExpose(t, f, o, 8000, 0)
	mustLease(t, f, e.ID, 2*time.Hour)
	if err := f.m.ReleaseExposureLease(context.Background(), "acme", "alice", e.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.m.ExposureState(context.Background(), "acme", "alice", e.ID)
	if got.LeaseExpiresAt != nil || !f.m.ExposureDeadline(got).Equal(got.LastUsedAt.Add(15*time.Minute)) {
		t.Fatalf("after release: lease %v, deadline %v; want none and last use + 15m", got.LeaseExpiresAt, f.m.ExposureDeadline(got))
	}
	*clock = clock.Add(20 * time.Minute)
	if n, _ := f.m.SweepExposures(context.Background()); n != 1 {
		t.Fatalf("sweep at +20m closed %d, want the released page", n)
	}
	wantClosed(t, f, e.ID, SurfaceIdle)
}

// The lease is the latest end any client asked for: a shorter renewal
// keeps the longer lease.
func TestExposureLease_LatestExpiryWins(t *testing.T) {
	f, o, clock := leaseFixture(t, 8*time.Hour)
	e := mustExpose(t, f, o, 8000, 0)
	long := mustLease(t, f, e.ID, 4*time.Hour)
	if end := mustLease(t, f, e.ID, time.Hour); !end.Equal(long) {
		t.Errorf("a 1h renewal = %v, want the 4h lease kept (%v)", end, long)
	}
	*clock = clock.Add(2 * time.Hour)
	if end := mustLease(t, f, e.ID, 3*time.Hour); !end.Equal(clock.UTC().Add(3 * time.Hour)) {
		t.Errorf("a renewal past the lease = %v, want now + 3h", end)
	}
}

// Another user's page, another tenant's, an unknown id and a closed page
// are all the unknown id's answer, for taking and for releasing.
func TestExposureLease_RefusedAsUnknownForAnythingButTheUsersLivePage(t *testing.T) {
	f, o, _ := leaseFixture(t, 8*time.Hour)
	e := mustExpose(t, f, o, 8000, 0)
	closed := mustExpose(t, f, o, 8001, 0)
	if _, err := f.m.Unexpose(context.Background(), o, "web", 8001); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct{ name, tenant, user, id string }{
		{"bob", "acme", "bob", e.ID},
		{"tenant", "other", "alice", e.ID},
		{"unknown", "acme", "alice", "ffffffffffffffffffffffff"},
		{"bad id", "acme", "alice", "x"},
		{"closed", "acme", "alice", closed.ID},
	} {
		if _, err := f.m.LeaseExposure(ctx, c.tenant, c.user, c.id, time.Hour); !errors.Is(err, ErrExposureNotFound) {
			t.Errorf("lease %s = %v, want not found", c.name, err)
		}
		if err := f.m.ReleaseExposureLease(ctx, c.tenant, c.user, c.id); !errors.Is(err, ErrExposureNotFound) {
			t.Errorf("release %s = %v, want not found", c.name, err)
		}
	}
	if got, _ := f.st.WorkspaceExposureGet(ctx, "acme", e.ID); got.LeaseExpiresAt != nil {
		t.Errorf("a refused lease was written: %v", got.LeaseExpiresAt)
	}
}

// A lease longer than max_lease_s, or shorter than a second, is refused
// naming the bound, before the page is looked up.
func TestExposureLease_LengthWithinTheOperatorsBound(t *testing.T) {
	f, o, _ := leaseFixture(t, 8*time.Hour)
	e := mustExpose(t, f, o, 8000, 0)
	for _, ttl := range []time.Duration{8*time.Hour + time.Second, 0, -time.Hour, time.Millisecond} {
		for _, id := range []string{e.ID, "ffffffffffffffffffffffff"} {
			_, err := f.m.LeaseExposure(context.Background(), "acme", "alice", id, ttl)
			if !errors.Is(err, ErrLeaseTTL) || !strings.Contains(err.Error(), "28800") {
				t.Errorf("ttl %v on %s = %v, want refused naming 28800", ttl, id, err)
			}
		}
	}
	mustLease(t, f, e.ID, 8*time.Hour)
}

// max_lease_s 0 turns leases off: a lease is refused as off, not as an
// unknown page, and no run is held. workspace.expose off answers every page
// as unknown, a lease too.
func TestExposureLease_OffWhenTheOperatorSetsZero(t *testing.T) {
	f, o, _ := leaseFixture(t, 0)
	e := mustExpose(t, f, o, 8000, 0)
	ctx := context.Background()
	if f.m.ExposeMaxLease() != 0 {
		t.Errorf("ExposeMaxLease = %v, want 0 (off)", f.m.ExposeMaxLease())
	}
	if _, err := f.m.LeaseExposure(ctx, "acme", "alice", e.ID, time.Hour); !errors.Is(err, ErrLeasesOff) {
		t.Errorf("lease = %v, want leases off", err)
	}
	if err := f.m.ReleaseExposureLease(ctx, "acme", "alice", e.ID); !errors.Is(err, ErrLeasesOff) {
		t.Errorf("release = %v, want leases off", err)
	}
	// A lease written while leases were on holds no run once they are off.
	if _, err := f.st.WorkspaceExposureExtendLease(ctx, "acme", e.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if end, err := f.m.RunLeaseEnd(ctx, o.RunID); err != nil || !end.IsZero() {
		t.Errorf("RunLeaseEnd with leases off = %v, %v; want none", end, err)
	}
	f.m.SetExpose(ExposeLimits{Enabled: false, MaxLease: time.Hour})
	if f.m.ExposeMaxLease() != 0 {
		t.Errorf("ExposeMaxLease with expose off = %v, want 0", f.m.ExposeMaxLease())
	}
	if _, err := f.m.LeaseExposure(ctx, "acme", "alice", e.ID, time.Minute); !errors.Is(err, ErrExposureNotFound) {
		t.Errorf("lease with expose off = %v, want not found", err)
	}
}

// RunLeaseEnd is the latest live lease of the run's own pages: none, then
// the lease's end; another run's lease does not count; an ended or released
// one does not count; a lease is counted no further than max_lease_s from
// now.
func TestExposureLease_RunLeaseEndIsTheLatestLiveLeaseOfTheRunsPages(t *testing.T) {
	f, o, clock := leaseFixture(t, 8*time.Hour)
	ctx := context.Background()
	a := mustExpose(t, f, o, 8000, 0)
	b := mustExpose(t, f, o, 8001, 0)
	if end, err := f.m.RunLeaseEnd(ctx, o.RunID); err != nil || !end.IsZero() {
		t.Fatalf("no lease = %v, %v; want none", end, err)
	}
	mustLease(t, f, a.ID, time.Hour)
	bEnd := mustLease(t, f, b.ID, 3*time.Hour)
	if end, err := f.m.RunLeaseEnd(ctx, o.RunID); err != nil || !end.Equal(bEnd) {
		t.Errorf("two leases = %v, %v; want the later %v", end, err, bEnd)
	}
	bob := f.run(t, "bob")
	f.open(t, bob, "web")
	if end, _ := f.m.RunLeaseEnd(ctx, bob.RunID); !end.IsZero() {
		t.Errorf("bob's run = %v, want none: alice's leases are not his", end)
	}
	if err := f.m.ReleaseExposureLease(ctx, "acme", "alice", b.ID); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * time.Hour) // a's lease ended an hour ago
	if end, _ := f.m.RunLeaseEnd(ctx, o.RunID); !end.IsZero() {
		t.Errorf("ended and released leases = %v, want none", end)
	}
	if _, err := f.st.WorkspaceExposureExtendLease(ctx, "acme", a.ID, clock.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if end, _ := f.m.RunLeaseEnd(ctx, o.RunID); !end.Equal(clock.UTC().Add(8 * time.Hour)) {
		t.Errorf("a lease past the bound = %v, want now + max_lease_s", end)
	}
	if end, _ := f.m.RunLeaseEnd(ctx, "no-such-run"); !end.IsZero() {
		t.Errorf("an unknown run = %v, want none", end)
	}
}
