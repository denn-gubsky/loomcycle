package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY rows in a snapshot: SnapshotReadAcl reads every tenant's rows, and
// the SnapshotRestoreAcl* family writes them back keeping what the write
// surface would have stamped, leaving a held key alone.

var aclSnapT0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// seedAclSnapshotRows writes one of each row kind into tenant, plus a retired
// list, a retired binding, a nested group and an expiring binding.
func seedAclSnapshotRows(t *testing.T, s store.Store, tenant string) {
	t.Helper()
	ctx := context.Background()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s %s: %v", tenant, what, err)
		}
	}
	_, err := s.AclDefPut(ctx, mkAclDef(tenant, "support", aclRulesA))
	must("acl support", err)
	_, err = s.AclDefPut(ctx, mkAclDef(tenant, "support", aclRulesB)) // version 2
	must("acl support v2", err)
	_, err = s.AclDefPut(ctx, mkAclDef(tenant, "old", aclRulesA))
	must("acl old", err)
	_, err = s.AclDefRetire(ctx, tenant, "old")
	must("retire old", err)
	for _, g := range []string{"support", "contractors"} {
		_, err = s.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: g, Description: "g " + g, CreatedBy: "op"})
		must("group "+g, err)
	}
	must("member user", s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: "support", MemberKind: store.GroupMemberUser, MemberID: "alice", AddedBy: "op"}))
	must("member group", s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: "support", MemberKind: store.GroupMemberGroup, MemberID: "contractors", AddedBy: "op"}))
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_" + tenant + "_grp", TenantID: tenant, ACLName: "support", SubjectKind: "group", SubjectID: "support", CreatedBy: "op"})
	must("binding group", err)
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_" + tenant + "_exp", TenantID: tenant, ACLName: "support", SubjectKind: "user", SubjectID: "bob",
		ExpiresAt: aclSnapT0.Add(240 * time.Hour), CreatedBy: "op"})
	must("binding expiring", err)
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_" + tenant + "_ret", TenantID: tenant, ACLName: "old", SubjectKind: "tenant"})
	must("binding retired", err)
	_, err = s.AclBindingRetire(ctx, tenant, "aclb_"+tenant+"_ret")
	must("retire binding", err)
}

// testAclSnapshotReadsEveryTenantAndRestoresEveryField: the read spans
// tenants and includes retired rows; restoring the rows under a fresh tenant
// keeps every field the write surface stamped (version, times, authors,
// expiry, retirement); the restored set is what the per-tenant reads return.
func testAclSnapshotReadsEveryTenantAndRestoresEveryField(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedAclSnapshotRows(t, s, "acme")
	seedAclSnapshotRows(t, s, "")
	snap, err := s.SnapshotReadAcl(ctx)
	if err != nil {
		t.Fatalf("SnapshotReadAcl: %v", err)
	}
	if len(snap.Defs) != 4 || len(snap.Groups) != 4 || len(snap.Members) != 4 || len(snap.Bindings) != 6 {
		t.Fatalf("read defs=%d groups=%d members=%d bindings=%d; want 4/4/4/6 across both tenants",
			len(snap.Defs), len(snap.Groups), len(snap.Members), len(snap.Bindings))
	}
	if snap.Defs[0].TenantID != "" || snap.Defs[len(snap.Defs)-1].TenantID != "acme" {
		t.Errorf("defs not ordered by tenant: first %q last %q", snap.Defs[0].TenantID, snap.Defs[len(snap.Defs)-1].TenantID)
	}

	// Restore acme's rows as tenant "copy", in the restore order: groups,
	// members, lists, bindings.
	const dst = "copy"
	v0, _ := s.AclVersion(ctx, dst)
	for _, g := range snap.Groups {
		if g.TenantID != "acme" {
			continue
		}
		g.TenantID = dst
		if ok, err := s.SnapshotRestoreGroup(ctx, g); err != nil || !ok {
			t.Fatalf("restore group %s = (%v, %v)", g.Name, ok, err)
		}
	}
	for _, m := range snap.Members {
		if m.TenantID != "acme" {
			continue
		}
		m.TenantID = dst
		if ok, err := s.SnapshotRestoreGroupMember(ctx, m); err != nil || !ok {
			t.Fatalf("restore member %s/%s = (%v, %v)", m.GroupName, m.MemberID, ok, err)
		}
	}
	for _, d := range snap.Defs {
		if d.TenantID != "acme" {
			continue
		}
		d.TenantID = dst
		if ok, err := s.SnapshotRestoreAclDef(ctx, d); err != nil || !ok {
			t.Fatalf("restore acl %s = (%v, %v)", d.Name, ok, err)
		}
	}
	for _, b := range snap.Bindings {
		if b.TenantID != "acme" {
			continue
		}
		b.TenantID = dst
		b.BindingID += "_copy" // ids are global
		if ok, err := s.SnapshotRestoreAclBinding(ctx, b); err != nil || !ok {
			t.Fatalf("restore binding %s = (%v, %v)", b.BindingID, ok, err)
		}
	}
	if v, _ := s.AclVersion(ctx, dst); v <= v0 {
		t.Errorf("acl_version of %q did not move (%d → %d): a cached set would miss the restored rows", dst, v0, v)
	}

	// Every field the source held, as the per-tenant reads see it.
	src, _ := s.AclDefList(ctx, "acme", true)
	got, _ := s.AclDefList(ctx, dst, true)
	if len(got) != len(src) {
		t.Fatalf("restored lists = %d, want %d", len(got), len(src))
	}
	for i := range src {
		a, b := src[i], got[i]
		if a.Name != b.Name || a.Version != b.Version || a.ContentSHA256 != b.ContentSHA256 || !jsonEqual(b.Rules, string(a.Rules)) ||
			a.Description != b.Description || a.CreatedBy != b.CreatedBy || !a.CreatedAt.Equal(b.CreatedAt) ||
			!a.UpdatedAt.Equal(b.UpdatedAt) || !a.RetiredAt.Equal(b.RetiredAt) || a.CatalogVersion != b.CatalogVersion {
			t.Errorf("list %s: restored %+v, source %+v", a.Name, b, a)
		}
	}
	if got[0].Name != "old" || !got[0].Retired() || got[1].Version != 2 {
		t.Errorf("restored lists lost retirement or version: %+v", got)
	}
	srcG, _ := s.GroupList(ctx, "acme")
	gotG, _ := s.GroupList(ctx, dst)
	for i := range srcG {
		a, b := srcG[i], gotG[i]
		if a.Name != b.Name || a.Description != b.Description || a.CreatedBy != b.CreatedBy || !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) {
			t.Errorf("group %s: restored %+v, source %+v", a.Name, b, a)
		}
	}
	srcM, _ := s.GroupMembers(ctx, "acme", "support")
	gotM, _ := s.GroupMembers(ctx, dst, "support")
	if len(gotM) != 2 || len(srcM) != 2 {
		t.Fatalf("members: restored %d, source %d; want 2", len(gotM), len(srcM))
	}
	for i := range srcM {
		a, b := srcM[i], gotM[i]
		if a.MemberKind != b.MemberKind || a.MemberID != b.MemberID || a.AddedBy != b.AddedBy || !a.AddedAt.Equal(b.AddedAt) {
			t.Errorf("member %s: restored %+v, source %+v", a.MemberID, b, a)
		}
	}
	if groups, _ := s.GroupsOfSubject(ctx, dst, "alice"); len(groups) != 1 || groups[0] != "support" {
		t.Errorf("alice's groups in %q = %v", dst, groups)
	}
	srcB, _ := s.AclBindingList(ctx, "acme", true)
	gotB, _ := s.AclBindingList(ctx, dst, true)
	if len(gotB) != len(srcB) {
		t.Fatalf("bindings: restored %d, source %d", len(gotB), len(srcB))
	}
	byID := map[string]store.AclBindingRow{}
	for _, b := range gotB {
		byID[strings.TrimSuffix(b.BindingID, "_copy")] = b
	}
	for _, a := range srcB {
		b, ok := byID[a.BindingID]
		if !ok || a.ACLName != b.ACLName || a.SubjectKind != b.SubjectKind || a.SubjectID != b.SubjectID ||
			!a.ExpiresAt.Equal(b.ExpiresAt) || !a.CreatedAt.Equal(b.CreatedAt) || a.CreatedBy != b.CreatedBy || !a.RetiredAt.Equal(b.RetiredAt) {
			t.Errorf("binding %s: restored %+v, source %+v", a.BindingID, b, a)
		}
	}
	if live, _ := s.AclBindingList(ctx, dst, false); len(live) != 2 {
		t.Errorf("live restored bindings = %d, want 2 (the retired one stays retired)", len(live))
	}
}

// testAclSnapshotRestoreLeavesHeldRowsAlone: a row the store holds on the same
// key stands — a different rule list, a different group description, the
// same binding id — and a write that changes nothing does not move the
// version.
func testAclSnapshotRestoreLeavesHeldRowsAlone(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedAclSnapshotRows(t, s, "acme")
	snap, err := s.SnapshotReadAcl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v0, _ := s.AclVersion(ctx, "acme")
	for _, g := range snap.Groups {
		if ok, err := s.SnapshotRestoreGroup(ctx, g); err != nil || ok {
			t.Errorf("re-restore group %s = (%v, %v); want (false, nil)", g.Name, ok, err)
		}
	}
	for _, m := range snap.Members {
		if ok, err := s.SnapshotRestoreGroupMember(ctx, m); err != nil || ok {
			t.Errorf("re-restore member %s = (%v, %v); want (false, nil)", m.MemberID, ok, err)
		}
	}
	for _, d := range snap.Defs {
		if ok, err := s.SnapshotRestoreAclDef(ctx, d); err != nil || ok {
			t.Errorf("re-restore acl %s = (%v, %v); want (false, nil)", d.Name, ok, err)
		}
	}
	for _, b := range snap.Bindings {
		if ok, err := s.SnapshotRestoreAclBinding(ctx, b); err != nil || ok {
			t.Errorf("re-restore binding %s = (%v, %v); want (false, nil)", b.BindingID, ok, err)
		}
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != v0 {
		t.Errorf("a restore that wrote nothing moved acl_version %d → %d", v0, v)
	}

	// A DIFFERENT row on a held key: the live one stands.
	other := mkAclDef("acme", "support", aclRulesA)
	other.Version = 9
	if ok, err := s.SnapshotRestoreAclDef(ctx, other); err != nil || ok {
		t.Errorf("restore over a held list = (%v, %v); want (false, nil)", ok, err)
	}
	if live, _ := s.AclDefGet(ctx, "acme", "support"); live.Version != 2 || !jsonEqual(live.Rules, aclRulesB) {
		t.Errorf("held list changed by a restore: %+v", live)
	}
	if ok, err := s.SnapshotRestoreGroup(ctx, store.GroupRow{TenantID: "acme", Name: "support", Description: "replaced"}); err != nil || ok {
		t.Errorf("restore over a held group = (%v, %v)", ok, err)
	}
	if g, _ := s.GroupGet(ctx, "acme", "support"); g.Description != "g support" {
		t.Errorf("held group changed by a restore: %+v", g)
	}
	// A binding id another tenant holds is refused, not skipped silently.
	if ok, err := s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{BindingID: "aclb_acme_grp", TenantID: "other", ACLName: "x", SubjectKind: "tenant"}); err == nil || ok {
		t.Errorf("restore of another tenant's binding id = (%v, %v); want an error", ok, err)
	}
}

// testAclSnapshotRestoreRefusesWhatNoAuthorCouldWrite: the restore re-runs the
// write surface's validation, so a hand-edited envelope stores nothing an
// author here could not have.
func testAclSnapshotRestoreRefusesWhatNoAuthorCouldWrite(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "a", MemberKind: store.GroupMemberGroup, MemberID: "b"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		write func() (bool, error)
	}{
		{"acl unknown verb", func() (bool, error) {
			return s.SnapshotRestoreAclDef(ctx, store.AclDefRow{TenantID: "acme", Name: "x", Version: 1,
				Rules: json.RawMessage(`[{"effect":"allow","actions":["fly"],"resources":["agent:*"]}]`)})
		}},
		{"acl empty rules", func() (bool, error) {
			return s.SnapshotRestoreAclDef(ctx, store.AclDefRow{TenantID: "acme", Name: "x", Version: 1, Rules: json.RawMessage(`[]`)})
		}},
		{"acl builtin name", func() (bool, error) {
			return s.SnapshotRestoreAclDef(ctx, store.AclDefRow{TenantID: "acme", Name: "builtin:admin", Version: 1, Rules: json.RawMessage(aclRulesA)})
		}},
		{"acl zero version", func() (bool, error) {
			return s.SnapshotRestoreAclDef(ctx, store.AclDefRow{TenantID: "acme", Name: "x", Rules: json.RawMessage(aclRulesA)})
		}},
		{"group bad name", func() (bool, error) {
			return s.SnapshotRestoreGroup(ctx, store.GroupRow{TenantID: "acme", Name: "a b"})
		}},
		{"member of a missing group", func() (bool, error) {
			return s.SnapshotRestoreGroupMember(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "nope", MemberKind: store.GroupMemberUser, MemberID: "u"})
		}},
		{"member closing a cycle", func() (bool, error) {
			return s.SnapshotRestoreGroupMember(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "b", MemberKind: store.GroupMemberGroup, MemberID: "a"})
		}},
		{"binding bad kind", func() (bool, error) {
			return s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{BindingID: "aclb_x", TenantID: "acme", ACLName: "x", SubjectKind: "robot", SubjectID: "r"})
		}},
		{"binding without id", func() (bool, error) {
			return s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "x", SubjectKind: "tenant"})
		}},
	}
	v0, _ := s.AclVersion(ctx, "acme")
	for _, c := range cases {
		if ok, err := c.write(); err == nil || ok {
			t.Errorf("%s: (%v, %v); want refused", c.name, ok, err)
		}
	}
	if v, _ := s.AclVersion(ctx, "acme"); v != v0 {
		t.Errorf("refused restores moved acl_version %d → %d", v0, v)
	}
}
