package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FL P0b contract: runtime app clients.

func clientDef(id, name, tenant string) store.ClientDefRow {
	return store.ClientDefRow{DefID: id, Name: name, TenantID: tenant, Definition: `{"kind":"public"}`,
		ContentSHA256: "sha-" + id, Description: "d", CreatedAt: pwgT0, CreatedBy: "alice", CreatedByTenant: tenant}
}

func testClientDefVersionsAndTheNewestLiveIsActive(t *testing.T, s store.Store) {
	ctx := context.Background()
	v1, err := s.ClientDefCreate(ctx, clientDef("cd-1", "board", "acme"))
	if err != nil || v1.Version != 1 {
		t.Fatalf("v1 = %+v, %v", v1, err)
	}
	v2, err := s.ClientDefCreate(ctx, clientDef("cd-2", "board", "acme"))
	if err != nil || v2.Version != 2 {
		t.Fatalf("v2 = %+v, %v", v2, err)
	}
	got, err := s.ClientDefGetActive(ctx, "board")
	if err != nil || got.DefID != "cd-2" || got.TenantID != "acme" || got.Definition != `{"kind":"public"}` ||
		got.ContentSHA256 != "sha-cd-2" || got.Description != "d" || !got.CreatedAt.Equal(pwgT0) ||
		got.CreatedBy != "alice" || got.CreatedByTenant != "acme" || !got.RetiredAt.IsZero() {
		t.Fatalf("active = %+v, %v", got, err)
	}
	if got, err := s.ClientDefGet(ctx, "cd-1"); err != nil || got.Version != 1 {
		t.Fatalf("get v1 = %+v, %v", got, err)
	}
	if _, err := s.ClientDefCreate(ctx, clientDef("cd-o", "other", "")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ClientDefList(ctx)
	if err != nil || len(list) != 2 || list[0].DefID != "cd-2" || list[1].DefID != "cd-o" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var nf *store.ErrNotFound
	if _, err := s.ClientDefGetActive(ctx, "none"); !errors.As(err, &nf) {
		t.Fatalf("unknown: %v", err)
	}
}

// A name belongs to one tenant, even after it is retired.
func testClientDefNameBelongsToOneTenant(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.ClientDefCreate(ctx, clientDef("cd-a", "board", "acme")); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	for _, tenant := range []string{"globex", ""} {
		if _, err := s.ClientDefCreate(ctx, clientDef("cd-x-"+tenant, "board", tenant)); !errors.As(err, &conflict) {
			t.Fatalf("tenant %q took acme's name: %v", tenant, err)
		}
	}
	if ok, err := s.ClientDefRetire(ctx, "board", pwgT0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("retire: %v %v", ok, err)
	}
	if _, err := s.ClientDefCreate(ctx, clientDef("cd-g", "board", "globex")); !errors.As(err, &conflict) {
		t.Fatalf("another tenant took a retired name: %v", err)
	}
	// The owner may bring it back.
	if got, err := s.ClientDefCreate(ctx, clientDef("cd-b", "board", "acme")); err != nil || got.Version != 2 {
		t.Fatalf("owner recreate = %+v, %v", got, err)
	}
}

func testClientDefRetireTakesEveryVersionOutOfForce(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"cd-1", "cd-2"} {
		if _, err := s.ClientDefCreate(ctx, clientDef(id, "board", "acme")); err != nil {
			t.Fatal(err)
		}
	}
	at := pwgT0.Add(time.Hour)
	if ok, err := s.ClientDefRetire(ctx, "board", at); err != nil || !ok {
		t.Fatalf("retire: %v %v", ok, err)
	}
	if ok, err := s.ClientDefRetire(ctx, "board", at); err != nil || ok {
		t.Fatalf("second retire reported: %v %v", ok, err)
	}
	var nf *store.ErrNotFound
	if _, err := s.ClientDefGetActive(ctx, "board"); !errors.As(err, &nf) {
		t.Fatalf("retired client is active: %v", err)
	}
	if list, _ := s.ClientDefList(ctx); len(list) != 0 {
		t.Fatalf("retired client listed: %+v", list)
	}
	for _, id := range []string{"cd-1", "cd-2"} {
		if got, _ := s.ClientDefGet(ctx, id); !got.RetiredAt.Equal(at) {
			t.Fatalf("%s retired_at = %v", id, got.RetiredAt)
		}
	}
}

// Two tenants racing for a fresh name: exactly one wins it.
func testClientDefConcurrentCreatesHaveOneOwner(t *testing.T, s store.Store) {
	ctx := context.Background()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i, tenant := range []string{"t0", "t1", "t2", "t3", "t4", "t5"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ClientDefCreate(ctx, clientDef("cd-race-"+string(rune('a'+i)), "raced", tenant)); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d tenants created one name", wins.Load())
	}
}
