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
	if ok, _ := s.WorkspaceExposureDelete(ctx, "other", "ex1"); ok {
		t.Error("another tenant deleted the exposure")
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
	if ok, err := s.WorkspaceExposureDelete(ctx, "acme", "ex3"); !ok || err != nil {
		t.Errorf("delete = %v, %v", ok, err)
	}
}

// Deleting a workspace deletes its exposures with it, and no other's.
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
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceExposureGet(ctx, "acme", "ex1"); !errors.As(err, &nf) {
		t.Errorf("the deleted workspace's exposure = %v, want not found", err)
	}
	if _, err := s.WorkspaceExposureGet(ctx, "acme", "ex2"); err != nil {
		t.Errorf("another workspace's exposure: %v", err)
	}
}
