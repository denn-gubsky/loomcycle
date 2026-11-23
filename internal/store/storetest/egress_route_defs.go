package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P4 — the egress routes a tenant writes (store.EgressRouteDefStore).

// RunEgressRouteDefs asserts the egress_route_defs contract against a backend,
// each case on a fresh store from factory. Its own runner beside Run, as
// RunWorkspaces is, so Run stays in its length budget.
func RunEgressRouteDefs(t *testing.T, factory Factory) {
	t.Helper()
	for name, fn := range map[string]func(*testing.T, store.Store){
		"CreateReadsBackEveryField": testEgressRouteDefCreateReadsBackEveryField,
		"CreateRefusesAHeldName":    testEgressRouteDefCreateRefusesAHeldName,
		"TenantsDoNotSeeEachOther":  testEgressRouteDefTenantsDoNotSeeEachOther,
	} {
		t.Run(name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			fn(t, s)
		})
	}
}

func mkEgressRoute(tenant, name, url string) store.EgressRouteDefRow {
	return store.EgressRouteDefRow{TenantID: tenant, Name: name, URL: url, Methods: []string{"GET", "POST"},
		Credential: "$cred:crm", Auth: "header", Header: "X-Api-Key", CreatedBy: "alice"}
}

// testEgressRouteDefCreateReadsBackEveryField: what a create stores is what
// get and list return, timestamps stamped by the store.
func testEgressRouteDefCreateReadsBackEveryField(t *testing.T, s store.Store) {
	ctx := context.Background()
	want := mkEgressRoute("acme", "crm", "https://api.crm.example/v2/")
	got, err := s.EgressRouteDefCreate(ctx, want)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.CreatedAt.IsZero() || !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Errorf("timestamps created=%v updated=%v, want set and equal", got.CreatedAt, got.UpdatedAt)
	}
	read, err := s.EgressRouteDefGet(ctx, "acme", "crm")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want.CreatedAt, want.UpdatedAt = read.CreatedAt, read.UpdatedAt
	if !reflect.DeepEqual(read, want) {
		t.Errorf("get = %+v\nwant %+v", read, want)
	}
	list, err := s.EgressRouteDefList(ctx, "acme")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], read) {
		t.Errorf("list = %+v, %v; want the one row", list, err)
	}
}

// testEgressRouteDefCreateRefusesAHeldName: a create never moves an existing
// name to another URL; the first row stands.
func testEgressRouteDefCreateRefusesAHeldName(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.EgressRouteDefCreate(ctx, mkEgressRoute("acme", "crm", "https://api.crm.example/v2/")); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := s.EgressRouteDefCreate(ctx, mkEgressRoute("acme", "crm", "https://elsewhere.example/"))
	var conflict *store.ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("second create err = %v, want *ErrConflict", err)
	}
	got, err := s.EgressRouteDefGet(ctx, "acme", "crm")
	if err != nil || got.URL != "https://api.crm.example/v2/" {
		t.Errorf("after refused create: url=%q err=%v, want the first URL", got.URL, err)
	}
}

// testEgressRouteDefTenantsDoNotSeeEachOther: two tenants hold one name
// independently, and get, list and delete each reach only the caller's row.
func testEgressRouteDefTenantsDoNotSeeEachOther(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.EgressRouteDefCreate(ctx, mkEgressRoute("acme", "crm", "https://a.example/")); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if _, err := s.EgressRouteDefCreate(ctx, mkEgressRoute("globex", "crm", "https://g.example/")); err != nil {
		t.Fatalf("create globex: %v", err)
	}
	if _, err := s.EgressRouteDefGet(ctx, "initech", "crm"); !isNotFound(err) {
		t.Errorf("get from a third tenant err = %v, want ErrNotFound", err)
	}
	if list, err := s.EgressRouteDefList(ctx, "initech"); err != nil || len(list) != 0 {
		t.Errorf("list from a third tenant = %+v, %v; want none", list, err)
	}
	if found, err := s.EgressRouteDefDelete(ctx, "initech", "crm"); err != nil || found {
		t.Errorf("delete from a third tenant found=%v err=%v, want nothing found", found, err)
	}
	if found, err := s.EgressRouteDefDelete(ctx, "acme", "crm"); err != nil || !found {
		t.Fatalf("delete acme: found=%v err=%v", found, err)
	}
	if got, err := s.EgressRouteDefGet(ctx, "globex", "crm"); err != nil || got.URL != "https://g.example/" {
		t.Errorf("globex after acme's delete: %+v, %v", got, err)
	}
	if found, err := s.EgressRouteDefDelete(ctx, "acme", "crm"); err != nil || found {
		t.Errorf("second delete found=%v err=%v, want nothing found", found, err)
	}
}

func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}
