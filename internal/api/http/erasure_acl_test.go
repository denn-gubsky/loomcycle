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
