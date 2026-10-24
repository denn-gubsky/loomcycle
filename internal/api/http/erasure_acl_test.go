package http

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestErasureExecute_DropsTheSubjectsCachedACLSet: a live erasure removes the
// subject's group membership AND drops the compiled set this replica cached
// for it. The resolver re-reads acl_version only once per TTL, so without the
// invalidation the erased membership would keep applying for up to an hour.
// The config-declared membership is reported for the operator to remove from
// the file, and is not something the erasure claims to have removed.
func TestErasureExecute_DropsTheSubjectsCachedACLSet(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.ResolvedACL = &authz.Static{
		Groups: []authz.StaticGroup{{Tenant: "acme", Name: "yaml-team", Members: []string{"alice"}}},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	// A long TTL: the cached version is trusted for the whole test, so only an
	// explicit invalidation can make the next Resolve see the erasure.
	srv.SetACLResolver(aclres.New(srv.store, authz.ModeCompat, time.Hour))
	eraseLog, aclLog := &recordingSink{}, &recordingSink{}
	srv.SetEraseAudit(eraseLog)
	srv.SetTokenAudit(aclLog)
	ctx := context.Background()
	if _, err := srv.store.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	alice := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}
	p, err := srv.aclResolver.Resolve(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Groups, []string{"support", "yaml-team"}) {
		t.Fatalf("groups before erasure = %v", p.Groups)
	}
	before, _ := srv.store.AclVersion(ctx, "acme")

	rec, res := erasePost(t, srv, "acme", map[string]any{"subject": "alice", "dry_run": false, "confirm": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if res.Deleted["acl_memberships"] != 1 {
		t.Errorf("acl_memberships deleted = %d, want 1", res.Deleted["acl_memberships"])
	}
	if r := res.Retained["access_control_config"]; !strings.Contains(r, "yaml-team") || !strings.Contains(r, "remove from the config file") {
		t.Errorf("config-declared membership not reported: %q", r)
	}
	if v, _ := srv.store.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("acl_version = %d, want %d", v, before+1)
	}
	// Audited twice, like the other planes and like an ACL write: the
	// erase_result record carries the plane counts, and the ACL audit trail
	// records the write against the subject.
	var planes map[string]int
	for _, ev := range eraseLog.events {
		if ev.Action == "erase_result" {
			planes = ev.ErasePlanes
		}
	}
	if planes["acl_memberships"] != 1 {
		t.Errorf("erase_result planes = %v, want acl_memberships 1", planes)
	}
	if n := len(aclLog.events); n != 1 || aclLog.events[0].Action != "subject_access_erased" ||
		aclLog.events[0].TargetName != "alice" || aclLog.events[0].TargetTenant != "acme" {
		t.Errorf("ACL audit = %+v, want one subject_access_erased for acme/alice", aclLog.events)
	}
	p, err = srv.aclResolver.Resolve(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Groups, []string{"yaml-team"}) {
		t.Errorf("groups after erasure = %v, want [yaml-team] — the cached set still carries the erased membership", p.Groups)
	}
}

// TestErasure_NamesTheRemovedGroupsAndListsInAStructuredField: the groups and
// lists an erasure removes (or a dry run / report would) travel as data under
// `access`, not only inside a prose note a client would have to parse. The
// config-declared ones are not the store's to remove and stay out of it.
func TestErasure_NamesTheRemovedGroupsAndListsInAStructuredField(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.ResolvedACL = &authz.Static{
		Groups: []authz.StaticGroup{{Tenant: "acme", Name: "yaml-team", Members: []string{"alice"}}},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	srv.SetEraseAudit(&recordingSink{})
	ctx := context.Background()
	for _, g := range []string{"support", "ops"} {
		if _, err := srv.store.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: g, MemberKind: "user", MemberID: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []string{"reviewers", "readers", "reviewers"} {
		if _, err := srv.store.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: l, SubjectKind: "user", SubjectID: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	wantGroups, wantLists := []string{"ops", "support"}, []string{"readers", "reviewers"}

	rep, err := srv.ErasureReport(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Access.Groups, wantGroups) || !reflect.DeepEqual(rep.Access.Lists, wantLists) {
		t.Errorf("report access = %+v, want groups %v lists %v", rep.Access, wantGroups, wantLists)
	}

	rec, dry := erasePost(t, srv, "acme", map[string]any{"subject": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run status = %d: %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(dry.Access.Groups, wantGroups) || !reflect.DeepEqual(dry.Access.Lists, wantLists) {
		t.Errorf("dry-run access = %+v", dry.Access)
	}
	if g, _ := srv.store.GroupsOfSubject(ctx, "acme", "alice"); len(g) != 2 {
		t.Fatalf("a dry run removed memberships: %v", g)
	}

	rec, res := erasePost(t, srv, "acme", map[string]any{"subject": "alice", "dry_run": false, "confirm": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(res.Access.Groups, wantGroups) || !reflect.DeepEqual(res.Access.Lists, wantLists) {
		t.Errorf("execute access = %+v", res.Access)
	}
	// The wire carries `access` with arrays, never null: an empty footprint
	// is [] so a client can iterate without a nil check.
	if !strings.Contains(rec.Body.String(), `"access":{"groups":["ops","support"],"lists":["readers","reviewers"]`) {
		t.Errorf("wire access field missing or misshapen: %s", rec.Body.String())
	}
	_, again := erasePost(t, srv, "acme", map[string]any{"subject": "alice", "dry_run": false, "confirm": "alice"})
	if again.Access.Groups == nil || again.Access.Lists == nil || len(again.Access.Groups)+len(again.Access.Lists) != 0 {
		t.Errorf("a repeat erasure's access = %+v, want empty non-nil lists", again.Access)
	}
}
