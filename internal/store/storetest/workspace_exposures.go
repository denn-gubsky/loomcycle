package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func mkExposure(id, tenant, workspaceID string, port int) store.WorkspaceExposure {
	return store.WorkspaceExposure{ID: id, TenantID: tenant, UserID: "alice", WorkspaceID: workspaceID, Port: port,
		Title: "t", Kind: store.WorkspaceExposurePage}
}

// An exposure reads back whole; its workspace holds at most max and one per
// port; another tenant reads none of it; touch and the idle list agree.
func testWorkspaceExposuresAreBoundedAndTenantScoped(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws1", "acme", "run1", "w", "wk1"))
	e, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex1", "acme", "ws1", 8000), 2)
	if err != nil {
		t.Fatal(err)
	}
	if e.Port != 8000 || e.UserID != "alice" || e.Kind != store.WorkspaceExposurePage || e.CreatedAt.IsZero() || e.LastUsedAt.IsZero() {
		t.Errorf("read back %+v", e)
	}
	var conflict *store.ErrConflict
	if _, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex2", "acme", "ws1", 8000), 2); !errors.As(err, &conflict) {
		t.Errorf("a second exposure of one port = %v, want a conflict", err)
	}
	if _, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex3", "acme", "ws1", 8001), 2); err != nil {
		t.Fatal(err)
	}
	var lim *store.ErrExposureLimit
	if _, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex4", "acme", "ws1", 8002), 2); !errors.As(err, &lim) {
		t.Errorf("a third exposure past max 2 = %v, want the limit", err)
	}
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceExposureGet(ctx, "other", "ex1"); !errors.As(err, &nf) {
		t.Errorf("another tenant's get = %v, want not found", err)
	}
	if l, _ := s.WorkspaceExposureListByWorkspace(ctx, "other", "ws1"); len(l) != 0 {
		t.Errorf("another tenant's list = %v", l)
	}
	if ok, _ := s.WorkspaceExposureClose(ctx, "other", "ex1", store.WorkspaceExposureClosedUnexposed, time.Now()); ok {
		t.Error("another tenant closed the exposure")
	}
	if l, _ := s.WorkspaceExposureListByWorkspace(ctx, "acme", "ws1"); len(l) != 2 {
		t.Errorf("list = %v", l)
	}
	later := time.Now().Add(time.Hour)
	if err := s.WorkspaceExposureTouch(ctx, "acme", "ex1", later); err != nil {
		t.Fatal(err)
	}
	idle, err := s.WorkspaceExposureListIdle(ctx, time.Now().Add(time.Minute))
	if err != nil || len(idle) != 1 || idle[0].ID != "ex3" {
		t.Errorf("idle = %v, %v; want ex3 only (ex1 was touched)", idle, err)
	}
	if ok, err := s.WorkspaceExposureClose(ctx, "acme", "ex3", store.WorkspaceExposureClosedIdle, time.Now()); !ok || err != nil {
		t.Errorf("close = %v, %v", ok, err)
	}
}

// Deleting a workspace closes its live exposures (released) and keeps the
// rows, and touches no other workspace's.
func testWorkspaceExposuresGoWithTheirWorkspace(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws1", "acme", "run1", "w", "wk1"))
	createWorkspace(t, s, mkWorkspace("ws2", "acme", "run1", "v", "wk1"))
	for _, e := range []store.WorkspaceExposure{mkExposure("ex1", "acme", "ws1", 8000), mkExposure("ex2", "acme", "ws2", 8000)} {
		if _, err := s.WorkspaceExposureCreate(ctx, e, 2); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.WorkspaceDelete(ctx, "acme", "ws1"); !ok || err != nil {
		t.Fatalf("delete workspace = %v, %v", ok, err)
	}
	got, err := s.WorkspaceExposureGet(ctx, "acme", "ex1")
	if err != nil || got.Live() || got.ClosedReason != store.WorkspaceExposureClosedReleased {
		t.Errorf("the deleted workspace's exposure = %+v, %v; want kept, closed released", got, err)
	}
	if l, _ := s.WorkspaceExposureListByUser(ctx, "acme", "alice"); len(l) != 1 || l[0].ID != "ex2" {
		t.Errorf("the user's live pages = %v, want ex2 only", l)
	}
	if e, err := s.WorkspaceExposureGet(ctx, "acme", "ex2"); err != nil || !e.Live() {
		t.Errorf("another workspace's exposure: %+v, %v", e, err)
	}
}

// RFC HI P1: a row carries its new fields both ways (nil and a value); a
// closed row is kept, read back by Get only, holds neither the count nor its
// port, is closed once, and is purged only after the given time.
func testWorkspaceExposuresKeepClosedRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws1", "acme", "run1", "w", "wk1"))
	plain, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex1", "acme", "ws1", 8000), 2)
	if err != nil {
		t.Fatal(err)
	}
	if plain.IdleTimeoutS != nil || plain.LeaseExpiresAt != nil || plain.ClosedAt != nil || plain.ClosedReason != "" ||
		plain.WorkspaceName != "" || plain.RunID != "" || !plain.Live() {
		t.Errorf("a row without the new fields read back %+v; want nil and empty", plain)
	}
	idle := 3600
	withAll := mkExposure("ex2", "acme", "ws1", 8001)
	withAll.WorkspaceName, withAll.RunID, withAll.IdleTimeoutS = "w", "run1", &idle
	got, err := s.WorkspaceExposureCreate(ctx, withAll, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdleTimeoutS == nil || *got.IdleTimeoutS != 3600 || got.WorkspaceName != "w" || got.RunID != "run1" {
		t.Errorf("a row with the new fields read back %+v", got)
	}
	closedAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	if ok, err := s.WorkspaceExposureClose(ctx, "acme", "ex1", store.WorkspaceExposureClosedUnexposed, closedAt); !ok || err != nil {
		t.Fatalf("close = %v, %v", ok, err)
	}
	if ok, _ := s.WorkspaceExposureClose(ctx, "acme", "ex1", store.WorkspaceExposureClosedIdle, time.Now()); ok {
		t.Error("a closed row closed again")
	}
	c, err := s.WorkspaceExposureGet(ctx, "acme", "ex1")
	if err != nil || c.Live() || !c.ClosedAt.Equal(closedAt) || c.ClosedReason != store.WorkspaceExposureClosedUnexposed {
		t.Errorf("the closed row = %+v, %v; want closed unexposed at %v", c, err, closedAt)
	}
	if l, _ := s.WorkspaceExposureListByWorkspace(ctx, "acme", "ws1"); len(l) != 1 || l[0].ID != "ex2" {
		t.Errorf("the workspace's list = %v, want the live ex2 only", l)
	}
	if l, _ := s.WorkspaceExposureListByUser(ctx, "acme", "alice"); len(l) != 1 || l[0].ID != "ex2" {
		t.Errorf("the user's list = %v, want the live ex2 only", l)
	}
	if l, _ := s.WorkspaceExposureListByUser(ctx, "other", "alice"); len(l) != 0 {
		t.Errorf("another tenant's user list = %v", l)
	}
	if l, _ := s.WorkspaceExposureListIdle(ctx, time.Now().Add(time.Hour)); len(l) != 1 || l[0].ID != "ex2" {
		t.Errorf("the idle list = %v, want the live ex2 only", l)
	}
	if err := s.WorkspaceExposureTouch(ctx, "acme", "ex1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c2, _ := s.WorkspaceExposureGet(ctx, "acme", "ex1"); !c2.LastUsedAt.Equal(c.LastUsedAt) {
		t.Error("a touch moved a closed row")
	}
	// The closed row holds neither the count (max 2) nor port 8000.
	if _, err := s.WorkspaceExposureCreate(ctx, mkExposure("ex3", "acme", "ws1", 8000), 2); err != nil {
		t.Fatalf("a new page on a closed page's port, within the live count: %v", err)
	}
	if n, err := s.WorkspaceExposurePurgeClosed(ctx, closedAt); n != 0 || err != nil {
		t.Errorf("purge before the close = %d, %v; want none", n, err)
	}
	if n, err := s.WorkspaceExposurePurgeClosed(ctx, time.Now()); n != 1 || err != nil {
		t.Errorf("purge after the close = %d, %v; want the one closed row", n, err)
	}
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceExposureGet(ctx, "acme", "ex1"); !errors.As(err, &nf) {
		t.Errorf("the purged row = %v, want not found", err)
	}
	for _, id := range []string{"ex2", "ex3"} {
		if _, err := s.WorkspaceExposureGet(ctx, "acme", id); err != nil {
			t.Errorf("the purge took live %s: %v", id, err)
		}
	}
}

// RFC HI §2: the lease is the latest end any client asked for, written on a
// live row only, cleared for every client at once, and read back by Get and
// the lists; another tenant and a closed row are not found.
func testWorkspaceExposureLeaseLatestExpiryWins(t *testing.T, s store.Store) {
	ctx := context.Background()
	createWorkspace(t, s, mkWorkspace("ws1", "acme", "run1", "w", "wk1"))
	for _, id := range []string{"ex1", "ex2"} {
		if _, err := s.WorkspaceExposureCreate(ctx, mkExposure(id, "acme", "ws1", map[string]int{"ex1": 8000, "ex2": 8001}[id]), 2); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Truncate(time.Millisecond)
	far, near := base.Add(2*time.Hour), base.Add(time.Hour)
	if end, err := s.WorkspaceExposureExtendLease(ctx, "acme", "ex1", far); err != nil || !end.Equal(far) {
		t.Fatalf("first lease = %v, %v; want %v", end, err, far)
	}
	if end, err := s.WorkspaceExposureExtendLease(ctx, "acme", "ex1", near); err != nil || !end.Equal(far) {
		t.Errorf("a shorter lease = %v, %v; want the later end %v kept", end, err, far)
	}
	further := far.Add(time.Hour)
	if end, err := s.WorkspaceExposureExtendLease(ctx, "acme", "ex1", further); err != nil || !end.Equal(further) {
		t.Errorf("a longer lease = %v, %v; want %v", end, err, further)
	}
	if e, _ := s.WorkspaceExposureGet(ctx, "acme", "ex1"); e.LeaseExpiresAt == nil || !e.LeaseExpiresAt.Equal(further) {
		t.Errorf("get reads lease %v, want %v", e.LeaseExpiresAt, further)
	}
	if l, _ := s.WorkspaceExposureListByWorkspace(ctx, "acme", "ws1"); len(l) != 2 || l[0].LeaseExpiresAt == nil || l[1].LeaseExpiresAt != nil {
		t.Errorf("list = %+v, want ex1 leased and ex2 not", l)
	}
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceExposureExtendLease(ctx, "other", "ex1", further); !errors.As(err, &nf) {
		t.Errorf("another tenant's lease = %v, want not found", err)
	}
	if _, err := s.WorkspaceExposureExtendLease(ctx, "acme", "nope", further); !errors.As(err, &nf) {
		t.Errorf("an unknown exposure's lease = %v, want not found", err)
	}
	if ok, _ := s.WorkspaceExposureClearLease(ctx, "other", "ex1"); ok {
		t.Error("another tenant cleared the lease")
	}
	if ok, err := s.WorkspaceExposureClearLease(ctx, "acme", "ex1"); !ok || err != nil {
		t.Fatalf("clear = %v, %v", ok, err)
	}
	if e, _ := s.WorkspaceExposureGet(ctx, "acme", "ex1"); e.LeaseExpiresAt != nil {
		t.Errorf("after clear the lease reads %v, want none", e.LeaseExpiresAt)
	}
	// After a clear a shorter end is taken again: nothing is left to win.
	if end, err := s.WorkspaceExposureExtendLease(ctx, "acme", "ex1", near); err != nil || !end.Equal(near) {
		t.Errorf("a lease after the clear = %v, %v; want %v", end, err, near)
	}
	if _, err := s.WorkspaceExposureClose(ctx, "acme", "ex1", store.WorkspaceExposureClosedIdle, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkspaceExposureExtendLease(ctx, "acme", "ex1", further); !errors.As(err, &nf) {
		t.Errorf("a closed exposure's lease = %v, want not found", err)
	}
	if ok, _ := s.WorkspaceExposureClearLease(ctx, "acme", "ex1"); ok {
		t.Error("a closed exposure's lease was cleared")
	}
	if e, _ := s.WorkspaceExposureGet(ctx, "acme", "ex1"); e.LeaseExpiresAt == nil || !e.LeaseExpiresAt.Equal(near) {
		t.Errorf("the closed row's lease = %v, want it as it closed (%v)", e.LeaseExpiresAt, near)
	}
}
