package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY access-control contract: ACL rule lists, groups, memberships,
// bindings and the per-tenant version, asserted against both backends.

const aclRulesA = `[{"effect":"allow","actions":["run"],"resources":["agent:chat/*"]},
 {"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`

const aclRulesB = `[{"effect":"allow","actions":["read"],"resources":["volume:shared-ro"]}]`

func mkAclDef(tenant, name, rules string) store.AclDefRow {
	return store.AclDefRow{TenantID: tenant, Name: name, Rules: json.RawMessage(rules), Description: "d", CreatedBy: "op"}
}

func testAclDefPutGetVersionAndRetire(t *testing.T, s store.Store) {
	ctx := context.Background()
	r1, err := s.AclDefPut(ctx, mkAclDef("acme", "support", aclRulesA))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if r1.Version != 1 || r1.ContentSHA256 == "" || r1.CatalogVersion == 0 || r1.CreatedAt.IsZero() || r1.Retired() || r1.CreatedBy != "op" {
		t.Errorf("first put = %+v", r1)
	}
	if !jsonEqual(r1.Rules, aclRulesA) {
		t.Errorf("rules round-trip: %s", r1.Rules)
	}
	// The same rules with different whitespace hash the same.
	spaced := strings.ReplaceAll(aclRulesA, ",", ", ")
	r2, err := s.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "support", Rules: json.RawMessage(spaced), Description: "d2", CreatedBy: "someone-else"})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if r2.Version != 2 || r2.ContentSHA256 != r1.ContentSHA256 || r2.Description != "d2" || r2.CreatedBy != "op" || !r2.CreatedAt.Equal(r1.CreatedAt) {
		t.Errorf("second put = %+v (first %+v)", r2, r1)
	}
	r3, _ := s.AclDefPut(ctx, mkAclDef("acme", "support", aclRulesB))
	if r3.Version != 3 || r3.ContentSHA256 == r1.ContentSHA256 {
		t.Errorf("third put = %+v", r3)
	}
	got, err := s.AclDefGet(ctx, "acme", "support")
	if err != nil || got.Version != 3 {
		t.Errorf("get = %+v, %v", got, err)
	}
	// Retire, then the live list omits it, the full list has it, Get still
	// returns it, and a Put un-retires it with a new version.
	if ok, err := s.AclDefRetire(ctx, "acme", "support"); !ok || err != nil {
		t.Fatalf("retire = %v, %v", ok, err)
	}
	if ok, _ := s.AclDefRetire(ctx, "acme", "support"); ok {
		t.Error("retiring twice reported a change")
	}
	live, _ := s.AclDefList(ctx, "acme", false)
	all, _ := s.AclDefList(ctx, "acme", true)
	if len(live) != 0 || len(all) != 1 || !all[0].Retired() {
		t.Errorf("live=%d all=%d retired=%v", len(live), len(all), all[0].Retired())
	}
	if got, err := s.AclDefGet(ctx, "acme", "support"); err != nil || !got.Retired() {
		t.Errorf("get after retire = %+v, %v", got, err)
	}
	r4, err := s.AclDefPut(ctx, mkAclDef("acme", "support", aclRulesA))
	if err != nil || r4.Version != 4 || r4.Retired() {
		t.Errorf("put after retire = %+v, %v", r4, err)
	}
	// A miss is an opaque not-found; an empty tenant lists an empty slice.
	var nf *store.ErrNotFound
	if _, err := s.AclDefGet(ctx, "acme", "nope"); !errors.As(err, &nf) {
		t.Errorf("miss err = %v", err)
	}
	if l, err := s.AclDefList(ctx, "empty", true); err != nil || l == nil || len(l) != 0 {
		t.Errorf("empty list = %v, %v", l, err)
	}
}

func testAclDefRefusesInvalidRulesAndBuiltinNames(t *testing.T, s store.Store) {
	ctx := context.Background()
	for name, rules := range map[string]string{
		"unknown type":  `[{"effect":"allow","actions":["run"],"resources":["nosuch:*"]}]`,
		"unknown field": `[{"effect":"allow","actions":["run"],"resources":["agent:*"],"extra":1}]`,
		"bad verb":      `[{"effect":"allow","actions":["read"],"resources":["tool:*"]}]`,
		"not json":      `nope`,
		"empty":         `[]`,
	} {
		if _, err := s.AclDefPut(ctx, mkAclDef("acme", "x", rules)); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	if _, err := s.AclDefPut(ctx, mkAclDef("acme", "builtin:admin", aclRulesA)); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Errorf("builtin name: %v", err)
	}
	if _, err := s.AclDefPut(ctx, mkAclDef("acme", "bad name!", aclRulesA)); err == nil {
		t.Error("bad name stored")
	}
	// Nothing was written.
	if l, _ := s.AclDefList(ctx, "acme", true); len(l) != 0 {
		t.Errorf("refused puts left rows: %+v", l)
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != 0 {
		t.Errorf("refused puts bumped the version to %d", v)
	}
}

func testAclDefTenantIsolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.AclDefPut(ctx, mkAclDef("a", "shared-name", aclRulesA)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AclDefPut(ctx, mkAclDef("b", "shared-name", aclRulesB)); err != nil {
		t.Fatal(err)
	}
	ra, _ := s.AclDefGet(ctx, "a", "shared-name")
	rb, _ := s.AclDefGet(ctx, "b", "shared-name")
	if ra.ContentSHA256 == rb.ContentSHA256 || ra.Version != 1 || rb.Version != 1 {
		t.Errorf("tenants share a row: %+v %+v", ra, rb)
	}
	var nf *store.ErrNotFound
	if _, err := s.AclDefGet(ctx, "c", "shared-name"); !errors.As(err, &nf) {
		t.Errorf("cross-tenant get = %v", err)
	}
	if ok, _ := s.AclDefRetire(ctx, "c", "shared-name"); ok {
		t.Error("cross-tenant retire changed a row")
	}
	if l, _ := s.AclDefList(ctx, "a", true); len(l) != 1 {
		t.Errorf("tenant a list = %d", len(l))
	}
	if va, _ := s.AclVersion(ctx, "a"); va != 1 {
		t.Errorf("tenant a version = %d", va)
	}
	if vc, _ := s.AclVersion(ctx, "c"); vc != 0 {
		t.Errorf("tenant c version = %d", vc)
	}
}

func testGroupPutGetListDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	g, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support", Description: "tier 1", CreatedBy: "op"})
	if err != nil || g.Name != "support" || g.CreatedAt.IsZero() || g.CreatedBy != "op" {
		t.Fatalf("put = %+v, %v", g, err)
	}
	g2, _ := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support", Description: "tier 1 (synced)", ExternalID: "okta:1", CreatedBy: "sync"})
	if g2.Description != "tier 1 (synced)" || g2.ExternalID != "okta:1" || g2.CreatedBy != "op" || !g2.CreatedAt.Equal(g.CreatedAt) {
		t.Errorf("update = %+v", g2)
	}
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "bad name!"}); err == nil {
		t.Error("bad name stored")
	}
	_, _ = s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "admins"})
	_, _ = s.GroupPut(ctx, store.GroupRow{TenantID: "other", Name: "support"})
	l, _ := s.GroupList(ctx, "acme")
	if len(l) != 2 || l[0].Name != "admins" || l[1].Name != "support" {
		t.Errorf("list = %+v", l)
	}
	var nf *store.ErrNotFound
	if _, err := s.GroupGet(ctx, "other", "admins"); !errors.As(err, &nf) {
		t.Errorf("cross-tenant get = %v", err)
	}
	if ok, err := s.GroupDelete(ctx, "acme", "admins"); !ok || err != nil {
		t.Errorf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.GroupDelete(ctx, "acme", "admins"); ok {
		t.Error("deleting twice reported a change")
	}
	if l, _ := s.GroupList(ctx, "empty"); l == nil || len(l) != 0 {
		t.Errorf("empty list = %v", l)
	}
}

func testGroupMembersNestingAndCycleRefusal(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, g := range []string{"all", "staff", "support"} {
		if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(group, kind, id string) error {
		return s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: group, MemberKind: kind, MemberID: id, AddedBy: "op"})
	}
	if err := add("support", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := add("support", "user", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := add("staff", "group", "support"); err != nil {
		t.Fatal(err)
	}
	if err := add("all", "group", "staff"); err != nil {
		t.Fatal(err)
	}
	// A duplicate edge is a conflict; a missing group or member group is a
	// not-found; a cycle and self-membership are refused.
	var conflict *store.ErrConflict
	if err := add("support", "user", "alice"); !errors.As(err, &conflict) {
		t.Errorf("dup = %v", err)
	}
	var nf *store.ErrNotFound
	if err := add("nosuch", "user", "alice"); !errors.As(err, &nf) {
		t.Errorf("missing group = %v", err)
	}
	if err := add("support", "group", "nosuch"); !errors.As(err, &nf) {
		t.Errorf("missing member group = %v", err)
	}
	if err := add("support", "group", "all"); err == nil || !strings.Contains(err.Error(), "nested inside itself") {
		t.Errorf("cycle = %v", err)
	}
	if err := add("support", "group", "support"); err == nil {
		t.Error("self membership stored")
	}
	if err := add("support", "role", "x"); err == nil {
		t.Error("bad kind stored")
	}
	members, _ := s.GroupMembers(ctx, "acme", "support")
	if len(members) != 2 || members[0].MemberID != "alice" || members[1].MemberID != "bob" || members[0].AddedAt.IsZero() || members[0].AddedBy != "op" {
		t.Errorf("members = %+v", members)
	}
	graph, _ := s.GroupGraph(ctx, "acme")
	want := map[string][]string{"all": {"staff"}, "staff": {"support"}, "support": nil}
	if !reflect.DeepEqual(graph, want) {
		t.Errorf("graph = %v, want %v", graph, want)
	}
	direct, _ := s.GroupsOfSubject(ctx, "acme", "alice")
	if !reflect.DeepEqual(direct, []string{"support"}) {
		t.Errorf("groups of alice = %v", direct)
	}
	if direct, _ := s.GroupsOfSubject(ctx, "acme", "nobody"); direct == nil || len(direct) != 0 {
		t.Errorf("groups of nobody = %v", direct)
	}
	if direct, _ := s.GroupsOfSubject(ctx, "other", "alice"); len(direct) != 0 {
		t.Errorf("cross-tenant groups of alice = %v", direct)
	}
	if ok, err := s.GroupMemberRemove(ctx, "acme", "support", "user", "bob"); !ok || err != nil {
		t.Errorf("remove = %v, %v", ok, err)
	}
	if ok, _ := s.GroupMemberRemove(ctx, "acme", "support", "user", "bob"); ok {
		t.Error("removing twice reported a change")
	}
}

func testGroupDeleteRemovesEdgesBothWays(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, g := range []string{"all", "staff", "support"} {
		_, _ = s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g})
	}
	_ = s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "group", MemberID: "support"})
	_ = s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "all", MemberKind: "group", MemberID: "staff"})
	_ = s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "user", MemberID: "carol"})
	if ok, _ := s.GroupDelete(ctx, "acme", "staff"); !ok {
		t.Fatal("delete")
	}
	graph, _ := s.GroupGraph(ctx, "acme")
	if !reflect.DeepEqual(graph, map[string][]string{"all": nil, "support": nil}) {
		t.Errorf("graph after delete = %v", graph)
	}
	if m, _ := s.GroupMembers(ctx, "acme", "staff"); len(m) != 0 {
		t.Errorf("members of a deleted group = %+v", m)
	}
	if g, _ := s.GroupsOfSubject(ctx, "acme", "carol"); len(g) != 0 {
		t.Errorf("carol still in %v", g)
	}
}

func testAclBindingPutListRetireAndFor(t *testing.T, s store.Store) {
	ctx := context.Background()
	put := func(acl, kind, id string, exp time.Time) store.AclBindingRow {
		t.Helper()
		b, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: acl, SubjectKind: kind, SubjectID: id, ExpiresAt: exp, CreatedBy: "op"})
		if err != nil {
			t.Fatalf("put %s %s %s: %v", acl, kind, id, err)
		}
		return b
	}
	tenantWide := put("read-only", "tenant", "", time.Time{})
	alice := put("support", "user", "alice", time.Time{})
	group := put("support", "group", "support", time.Now().Add(time.Hour).UTC())
	token := put("ci", "token", "def-1", time.Time{})
	_ = put("other", "user", "bob", time.Time{})
	if tenantWide.BindingID == "" || !strings.HasPrefix(tenantWide.BindingID, "aclb_") || tenantWide.CreatedAt.IsZero() || tenantWide.CreatedBy != "op" {
		t.Errorf("minted = %+v", tenantWide)
	}
	if group.ExpiresAt.IsZero() || !alice.ExpiresAt.IsZero() {
		t.Errorf("expiry round-trip: %+v / %+v", group, alice)
	}
	// A supplied id is kept; a second put with it conflicts.
	custom, err := s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_custom", TenantID: "acme", ACLName: "x", SubjectKind: "tenant"})
	if err != nil || custom.BindingID != "aclb_custom" {
		t.Errorf("custom id = %+v, %v", custom, err)
	}
	var conflict *store.ErrConflict
	if _, err := s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_custom", TenantID: "acme", ACLName: "x", SubjectKind: "tenant"}); !errors.As(err, &conflict) {
		t.Errorf("dup id = %v", err)
	}
	// Invalid shapes are refused.
	for _, bad := range []store.AclBindingRow{
		{TenantID: "acme", ACLName: "x", SubjectKind: "tenant", SubjectID: "x"},
		{TenantID: "acme", ACLName: "x", SubjectKind: "user"},
		{TenantID: "acme", ACLName: "x", SubjectKind: "role", SubjectID: "x"},
		{TenantID: "acme", ACLName: "", SubjectKind: "tenant"},
	} {
		if _, err := s.AclBindingPut(ctx, bad); err == nil {
			t.Errorf("stored %+v", bad)
		}
	}
	// For: tenant-wide + own + groups + token, never another user's.
	got, err := s.AclBindingsFor(ctx, "acme", "alice", []string{"support", "all"}, "def-1")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rows []store.AclBindingRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.BindingID)
		}
		return out
	}
	want := []string{tenantWide.BindingID, alice.BindingID, group.BindingID, token.BindingID, "aclb_custom"}
	if !reflect.DeepEqual(ids(got), want) {
		t.Errorf("for alice = %v, want %v", ids(got), want)
	}
	// No groups, no token: tenant-wide + own only.
	got, _ = s.AclBindingsFor(ctx, "acme", "alice", nil, "")
	if !reflect.DeepEqual(ids(got), []string{tenantWide.BindingID, alice.BindingID, "aclb_custom"}) {
		t.Errorf("for alice (no groups) = %v", ids(got))
	}
	// Another tenant sees nothing.
	if got, _ := s.AclBindingsFor(ctx, "other", "alice", []string{"support"}, "def-1"); len(got) != 0 {
		t.Errorf("cross-tenant for = %v", ids(got))
	}
	// Retire drops it from For and from the live list; the full list keeps it.
	if ok, err := s.AclBindingRetire(ctx, "acme", alice.BindingID); !ok || err != nil {
		t.Errorf("retire = %v, %v", ok, err)
	}
	if ok, _ := s.AclBindingRetire(ctx, "acme", alice.BindingID); ok {
		t.Error("retiring twice reported a change")
	}
	if ok, _ := s.AclBindingRetire(ctx, "other", tenantWide.BindingID); ok {
		t.Error("cross-tenant retire changed a row")
	}
	got, _ = s.AclBindingsFor(ctx, "acme", "alice", nil, "")
	if !reflect.DeepEqual(ids(got), []string{tenantWide.BindingID, "aclb_custom"}) {
		t.Errorf("for alice after retire = %v", ids(got))
	}
	live, _ := s.AclBindingList(ctx, "acme", false)
	all, _ := s.AclBindingList(ctx, "acme", true)
	if len(live) != 5 || len(all) != 6 {
		t.Errorf("live=%d all=%d", len(live), len(all))
	}
	for _, r := range all {
		if r.BindingID == alice.BindingID && r.RetiredAt.IsZero() {
			t.Error("retired binding has no retired_at")
		}
	}
	if l, _ := s.AclBindingList(ctx, "empty", true); l == nil || len(l) != 0 {
		t.Errorf("empty list = %v", l)
	}
}

func testAclVersionBumpsOnEveryWrite(t *testing.T, s store.Store) {
	ctx := context.Background()
	expect := func(step string, want int64) {
		t.Helper()
		if v, err := s.AclVersion(ctx, "acme"); err != nil || v != want {
			t.Errorf("after %s: version = %d, %v; want %d", step, v, err, want)
		}
	}
	expect("nothing", 0)
	_, _ = s.AclDefPut(ctx, mkAclDef("acme", "a", aclRulesA))
	expect("acl put", 1)
	_, _ = s.AclDefPut(ctx, mkAclDef("acme", "a", aclRulesB))
	expect("acl update", 2)
	_, _ = s.AclDefRetire(ctx, "acme", "a")
	expect("acl retire", 3)
	_, _ = s.AclDefRetire(ctx, "acme", "a") // no-op: no bump
	expect("acl retire again", 3)
	_, _ = s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "g"})
	expect("group put", 4)
	_ = s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "g", MemberKind: "user", MemberID: "u"})
	expect("member add", 5)
	_ = s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "g", MemberKind: "user", MemberID: "u"}) // conflict: no bump
	expect("member add dup", 5)
	_, _ = s.GroupMemberRemove(ctx, "acme", "g", "user", "u")
	expect("member remove", 6)
	b, _ := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "a", SubjectKind: "group", SubjectID: "g"})
	expect("binding put", 7)
	_, _ = s.AclBindingRetire(ctx, "acme", b.BindingID)
	expect("binding retire", 8)
	_, _ = s.GroupDelete(ctx, "acme", "g")
	expect("group delete", 9)
	// Another tenant's writes do not move this tenant's version.
	_, _ = s.AclDefPut(ctx, mkAclDef("other", "a", aclRulesA))
	expect("other tenant write", 9)
}

// testAclEraseSubjectRemovesOnlyThatSubjectsRows pins subject erasure's ACL
// step: the subject's user-kind memberships and user-kind bindings (retired
// ones too) go, in its tenant only, with one version bump. Each survivor is
// the minimal pair of a removed row — another user, a GROUP named like the
// subject, the same subject in another tenant — so an over-broad predicate
// fails here rather than in production.
func testAclEraseSubjectRemovesOnlyThatSubjectsRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, tenant := range []string{"acme", "other"} {
		for _, g := range []string{"support", "ops", "alice"} {
			if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: g}); err != nil {
				t.Fatal(err)
			}
		}
	}
	member := func(tenant, group, kind, id string) {
		t.Helper()
		if err := s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: group, MemberKind: kind, MemberID: id}); err != nil {
			t.Fatalf("member %s/%s %s:%s: %v", tenant, group, kind, id, err)
		}
	}
	member("acme", "support", "user", "alice")
	member("acme", "ops", "user", "alice")
	member("acme", "support", "user", "bob")
	member("acme", "support", "group", "alice") // the GROUP "alice", not the user
	member("other", "support", "user", "alice")
	bind := func(tenant, kind, id string) store.AclBindingRow {
		t.Helper()
		b, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: "l", SubjectKind: kind, SubjectID: id})
		if err != nil {
			t.Fatalf("bind %s %s:%s: %v", tenant, kind, id, err)
		}
		return b
	}
	bind("acme", "user", "alice")
	retired := bind("acme", "user", "alice")
	if ok, err := s.AclBindingRetire(ctx, "acme", retired.BindingID); !ok || err != nil {
		t.Fatalf("retire = %v, %v", ok, err)
	}
	keep := map[string]bool{
		bind("acme", "user", "bob").BindingID:    true,
		bind("acme", "group", "alice").BindingID: true,
		bind("acme", "token", "alice").BindingID: true,
		bind("acme", "tenant", "").BindingID:     true,
	}
	otherAlice := bind("other", "user", "alice")
	before, _ := s.AclVersion(ctx, "acme")
	otherBefore, _ := s.AclVersion(ctx, "other")

	m, b, err := s.AclEraseSubject(ctx, "acme", "alice")
	if err != nil || m != 2 || b != 2 {
		t.Fatalf("erase = (%d memberships, %d bindings, %v), want (2, 2, nil)", m, b, err)
	}
	if g, _ := s.GroupsOfSubject(ctx, "acme", "alice"); len(g) != 0 {
		t.Errorf("alice still a member of %v", g)
	}
	if g, _ := s.GroupsOfSubject(ctx, "acme", "bob"); !reflect.DeepEqual(g, []string{"support"}) {
		t.Errorf("bob's membership = %v, want [support]", g)
	}
	if graph, _ := s.GroupGraph(ctx, "acme"); !reflect.DeepEqual(graph["support"], []string{"alice"}) {
		t.Errorf("the group alice's edge into support = %v, want [alice]", graph["support"])
	}
	all, _ := s.AclBindingList(ctx, "acme", true)
	if len(all) != len(keep) {
		t.Errorf("acme bindings after erase = %d, want %d", len(all), len(keep))
	}
	for _, r := range all {
		if !keep[r.BindingID] {
			t.Errorf("binding %s (%s:%s) survived the erasure", r.BindingID, r.SubjectKind, r.SubjectID)
		}
	}
	if g, _ := s.GroupsOfSubject(ctx, "other", "alice"); !reflect.DeepEqual(g, []string{"support"}) {
		t.Errorf("another tenant's alice lost her membership: %v", g)
	}
	if got, _ := s.AclBindingList(ctx, "other", true); len(got) != 1 || got[0].BindingID != otherAlice.BindingID {
		t.Errorf("another tenant's bindings = %+v", got)
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("acme version = %d, want %d (one bump)", v, before+1)
	}
	if v, _ := s.AclVersion(ctx, "other"); v != otherBefore {
		t.Errorf("other tenant's version moved: %d → %d", otherBefore, v)
	}
	// Repeating it is a no-op and does not bump.
	if m, b, err := s.AclEraseSubject(ctx, "acme", "alice"); err != nil || m != 0 || b != 0 {
		t.Errorf("second erase = (%d, %d, %v), want (0, 0, nil)", m, b, err)
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("a no-op erase bumped the version to %d", v)
	}
	if _, _, err := s.AclEraseSubject(ctx, "acme", ""); err == nil {
		t.Error("an empty subject was accepted")
	}
}

// testAclEraseTokenBindingsRemovesOnlyThoseTokensRows pins the token half of
// subject erasure's ACL step: the token-kind bindings (retired ones too) of
// the named token def ids go, in the tenant only, with one version bump. The
// survivors are minimal pairs of a removed row — another token, a USER whose
// id equals a removed def id, the same def id in another tenant.
func testAclEraseTokenBindingsRemovesOnlyThoseTokensRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	bind := func(tenant, kind, id string) store.AclBindingRow {
		t.Helper()
		b, err := s.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: "l", SubjectKind: kind, SubjectID: id})
		if err != nil {
			t.Fatalf("bind %s %s:%s: %v", tenant, kind, id, err)
		}
		return b
	}
	bind("acme", "token", "tok_a")
	retired := bind("acme", "token", "tok_b")
	if ok, err := s.AclBindingRetire(ctx, "acme", retired.BindingID); !ok || err != nil {
		t.Fatalf("retire = %v, %v", ok, err)
	}
	keep := map[string]bool{
		bind("acme", "token", "tok_c").BindingID: true,
		bind("acme", "user", "tok_a").BindingID:  true,
	}
	otherA := bind("other", "token", "tok_a")
	before, _ := s.AclVersion(ctx, "acme")
	otherBefore, _ := s.AclVersion(ctx, "other")

	n, err := s.AclEraseTokenBindings(ctx, "acme", []string{"tok_a", "tok_b", "tok_unbound"})
	if err != nil || n != 2 {
		t.Fatalf("erase = (%d, %v), want (2, nil)", n, err)
	}
	all, _ := s.AclBindingList(ctx, "acme", true)
	if len(all) != len(keep) {
		t.Errorf("acme bindings after erase = %d, want %d", len(all), len(keep))
	}
	for _, r := range all {
		if !keep[r.BindingID] {
			t.Errorf("binding %s (%s:%s) survived the erasure", r.BindingID, r.SubjectKind, r.SubjectID)
		}
	}
	if got, _ := s.AclBindingList(ctx, "other", true); len(got) != 1 || got[0].BindingID != otherA.BindingID {
		t.Errorf("another tenant's bindings = %+v", got)
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("acme version = %d, want %d (one bump)", v, before+1)
	}
	if v, _ := s.AclVersion(ctx, "other"); v != otherBefore {
		t.Errorf("other tenant's version moved: %d → %d", otherBefore, v)
	}
	// Repeating it, and an empty list, are no-ops that do not bump.
	if n, err := s.AclEraseTokenBindings(ctx, "acme", []string{"tok_a", "tok_b"}); err != nil || n != 0 {
		t.Errorf("second erase = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := s.AclEraseTokenBindings(ctx, "acme", nil); err != nil || n != 0 {
		t.Errorf("empty erase = (%d, %v), want (0, nil)", n, err)
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != before+1 {
		t.Errorf("a no-op erase bumped the version to %d", v)
	}
}
