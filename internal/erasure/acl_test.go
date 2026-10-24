package erasure_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedACL gives alice two stored memberships and two user-kind bindings (one
// retired) in tenant, plus rows that are NOT hers: bob's membership and
// binding, and a group-kind binding to a group named "alice".
func seedACL(t *testing.T, st store.Store, tenant string) {
	t.Helper()
	ctx := context.Background()
	for _, g := range []string{"support", "ops", "alice"} {
		if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct{ group, id string }{{"support", "alice"}, {"ops", "alice"}, {"support", "bob"}} {
		if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: m.group, MemberKind: "user", MemberID: m.id}); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(acl, kind, id string) store.AclBindingRow {
		b, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: acl, SubjectKind: kind, SubjectID: id})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bind("contractor", "user", "alice")
	old := bind("legacy", "user", "alice")
	if _, err := st.AclBindingRetire(ctx, tenant, old.BindingID); err != nil {
		t.Fatal(err)
	}
	bind("contractor", "user", "bob")
	bind("contractor", "group", "alice")
}

// configACL declares alice in a config group and binds a config list to her.
func configACL() func() *authz.Static {
	st := &authz.Static{
		Groups:   []authz.StaticGroup{{Tenant: "acme", Name: "yaml-team", Members: []string{"alice"}}},
		Bindings: []authz.StaticBinding{{Tenant: "acme", Binding: authz.Binding{ACL: "yaml-list", SubjectKind: authz.SubjectUser, SubjectID: "alice"}}},
	}
	return func() *authz.Static { return st }
}

func noteContaining(notes []string, sub string) string {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return n
		}
	}
	return ""
}

// TestService_ReportListsTheSubjectsAccessControl: the report counts and names
// the subject's memberships and bindings, and names the config-declared ones
// as something only the config file can remove.
func TestService_ReportListsTheSubjectsAccessControl(t *testing.T) {
	s := newSvc(t)
	s.ACLStatic = configACL()
	seedACL(t, s.Store, "acme")
	ctx := context.Background()
	before, _ := s.Store.AclVersion(ctx, "acme")

	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Tier2.Counts["acl_memberships"]; got != 2 {
		t.Errorf("acl_memberships = %d, want 2", got)
	}
	if got := rep.Tier2.Counts["acl_bindings"]; got != 2 {
		t.Errorf("acl_bindings = %d, want 2 (the retired row still names her)", got)
	}
	stored := noteContaining(rep.Notes, "access control (stored")
	for _, want := range []string{"[ops, support]", "[contractor, legacy]"} {
		if !strings.Contains(stored, want) {
			t.Errorf("stored note %q does not name %s", stored, want)
		}
	}
	cfg := noteContaining(rep.Notes, "declared in config; remove from the config file")
	for _, want := range []string{"[yaml-team]", "[yaml-list]"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config note %q does not name %s", cfg, want)
		}
	}
	if after, _ := s.Store.AclVersion(ctx, "acme"); after != before {
		t.Errorf("a report moved acl_version %d → %d", before, after)
	}
}

// TestService_ErasureRemovesOnlyTheSubjectsAccessControl: a live erasure
// removes the subject's memberships and user-kind bindings, bumps the
// tenant's acl_version, tells the transport (cache invalidation + audit), and
// leaves another user's rows, a group's rows, another tenant's rows and the
// config-declared ones — the last named in Retained.
func TestService_ErasureRemovesOnlyTheSubjectsAccessControl(t *testing.T) {
	s := newSvc(t)
	s.ACLStatic = configACL()
	var changed []string
	s.ACLChanged = func(_ context.Context, tenant, subject string) { changed = append(changed, tenant+"/"+subject) }
	seedACL(t, s.Store, "acme")
	seedACL(t, s.Store, "other")
	ctx := context.Background()
	before, _ := s.Store.AclVersion(ctx, "acme")
	otherBefore, _ := s.Store.AclVersion(ctx, "other")

	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if res.Deleted["acl_memberships"] != 2 || res.Deleted["acl_bindings"] != 2 {
		t.Errorf("deleted = memberships %d, bindings %d; want 2, 2", res.Deleted["acl_memberships"], res.Deleted["acl_bindings"])
	}
	if g, _ := s.Store.GroupsOfSubject(ctx, "acme", "alice"); len(g) != 0 {
		t.Errorf("alice is still a member of %v", g)
	}
	all, _ := s.Store.AclBindingList(ctx, "acme", true)
	if len(all) != 2 {
		t.Errorf("acme bindings left = %d, want 2 (bob's, the group's)", len(all))
	}
	for _, r := range all {
		if r.SubjectKind == "user" && r.SubjectID == "alice" {
			t.Errorf("binding %s to alice survived", r.BindingID)
		}
	}
	if g, _ := s.Store.GroupsOfSubject(ctx, "acme", "bob"); len(g) != 1 {
		t.Errorf("bob's membership = %v", g)
	}
	if g, _ := s.Store.GroupsOfSubject(ctx, "other", "alice"); len(g) != 2 {
		t.Errorf("another tenant's alice lost memberships: %v", g)
	}
	if v, _ := s.Store.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("acl_version = %d, want %d", v, before+1)
	}
	if v, _ := s.Store.AclVersion(ctx, "other"); v != otherBefore {
		t.Errorf("another tenant's acl_version moved %d → %d", otherBefore, v)
	}
	if len(changed) != 1 || changed[0] != "acme/alice" {
		t.Errorf("ACLChanged calls = %v, want [acme/alice]", changed)
	}
	if r := res.Retained["access_control_config"]; !strings.Contains(r, "remove from the config file") || !strings.Contains(r, "yaml-team") {
		t.Errorf("config-declared access control not reported as retained: %q", r)
	}

	// Idempotent: a re-run removes nothing and does not invalidate again.
	res, err = s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil || res.Deleted["acl_memberships"] != 0 || res.Deleted["acl_bindings"] != 0 || len(changed) != 1 {
		t.Errorf("re-run: deleted %v, changed %v, err %v", res.Deleted, changed, err)
	}
}

// TestService_DryRunLeavesAccessControlInPlace: the preview counts exactly
// what the live erasure removes and writes nothing — no row, no version bump,
// no invalidation.
func TestService_DryRunLeavesAccessControlInPlace(t *testing.T) {
	s := newSvc(t)
	calls := 0
	s.ACLChanged = func(context.Context, string, string) { calls++ }
	seedACL(t, s.Store, "acme")
	ctx := context.Background()
	before, _ := s.Store.AclVersion(ctx, "acme")

	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["acl_memberships"] != 2 || res.Deleted["acl_bindings"] != 2 {
		t.Errorf("dry run counted memberships %d, bindings %d; want 2, 2", res.Deleted["acl_memberships"], res.Deleted["acl_bindings"])
	}
	if g, _ := s.Store.GroupsOfSubject(ctx, "acme", "alice"); len(g) != 2 {
		t.Errorf("a dry run removed memberships: %v", g)
	}
	if all, _ := s.Store.AclBindingList(ctx, "acme", true); len(all) != 4 {
		t.Errorf("a dry run removed bindings: %d left, want 4", len(all))
	}
	if v, _ := s.Store.AclVersion(ctx, "acme"); v != before {
		t.Errorf("a dry run moved acl_version %d → %d", before, v)
	}
	if calls != 0 {
		t.Errorf("a dry run invalidated the cache %d time(s)", calls)
	}
}
