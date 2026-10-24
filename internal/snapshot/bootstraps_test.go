package snapshot

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// emptiedTenant gives tenant one user and deletes it, leaving the
// tenant_bootstraps marker and no users row: the state the marker exists to
// keep from being bootstrapped a second time.
func emptiedTenant(t *testing.T, s store.Store, tenant string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UserCreate(ctx, store.UserRow{TenantID: tenant, Subject: "owner", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserDelete(ctx, tenant, "owner"); err != nil {
		t.Fatal(err)
	}
	if users, err := s.UserList(ctx, tenant); err != nil || len(users) != 0 {
		t.Fatalf("users of %s = %v, %v; want none", tenant, users, err)
	}
	if ok, err := s.TenantBootstrapped(ctx, tenant); err != nil || !ok {
		t.Fatalf("source %s bootstrapped = (%v, %v); want (true, nil)", tenant, ok, err)
	}
}

// A tenant whose users were all deleted is still bootstrapped after a
// snapshot round trip, so a website's manager cannot bootstrap a new owner
// into it on the target. Before the marker travelled, the restore recreated
// no user for it and so no marker: the tenant came back open.
func TestRoundTrip_EmptiedTenantStaysBootstrapped(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			src, dst := b.open(t), b.open(t)
			emptiedTenant(t, src, "acme")

			res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
			if res.UsersRestored != 0 || len(res.Warnings) != 0 {
				t.Fatalf("restore = users %d, warnings %v; want none", res.UsersRestored, res.Warnings)
			}
			if ok, err := dst.TenantBootstrapped(ctx, "acme"); err != nil || !ok {
				t.Fatalf("target acme bootstrapped = (%v, %v); want (true, nil): the emptied tenant was re-armed", ok, err)
			}
			marks, err := dst.TenantBootstrapList(ctx)
			if err != nil || len(marks) != 1 || marks[0].TenantID != "acme" || marks[0].Via != "users" {
				t.Errorf("target markers = %+v, %v; want the source's acme marker, via users", marks, err)
			}
			// A second restore is a no-op on an existing marker.
			mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
			if again, _ := dst.TenantBootstrapList(ctx); len(again) != 1 || !again[0].At.Equal(marks[0].At) {
				t.Errorf("markers after a re-restore = %+v; want %+v unchanged", again, marks)
			}
		})
	}
}

// A snapshot written before users.bootstraps existed still decodes and
// restores: its users come back (and with them their tenants' markers), and
// an emptied tenant in it is not marked, which is that format's documented
// limit.
func TestRestore_UsersSectionWithoutBootstrapsRestoresUsersAndTheirMarkers(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			src, dst := b.open(t), b.open(t)
			emptiedTenant(t, src, "acme")
			if err := src.UserCreate(ctx, store.UserRow{TenantID: "beta", Subject: "bob", AccessMode: "isolated", Status: "active"}); err != nil {
				t.Fatal(err)
			}
			raw := editSection(t, mustCapture(t, src), "users", func(sec map[string]any) {
				if _, ok := sec["bootstraps"]; !ok {
					t.Fatal("the capture has no bootstraps field to remove; the old-format case is not exercised")
				}
				delete(sec, "bootstraps")
			})
			res := mustRestore(t, dst, raw, RestoreOptions{})
			if res.UsersRestored != 1 || len(res.Warnings) != 0 {
				t.Fatalf("restore = users %d, warnings %v; want 1 and none", res.UsersRestored, res.Warnings)
			}
			if ok, err := dst.TenantBootstrapped(ctx, "beta"); err != nil || !ok {
				t.Errorf("beta bootstrapped = (%v, %v); its restored user must mark it", ok, err)
			}
			if ok, err := dst.TenantBootstrapped(ctx, "acme"); err != nil || ok {
				t.Errorf("acme bootstrapped = (%v, %v); an old-format snapshot carries no marker for it", ok, err)
			}
		})
	}
}
