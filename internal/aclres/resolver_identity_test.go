package aclres

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The compiled set records what the identity report reads without the
// store: the class baseline, each applied list with its version and the
// binding it came through, and how the subject is in each group.
func TestResolve_RecordsBaselineAppliedListsAndGroupVia(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, g := range []string{"staff", "support"} {
		if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: g}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "staff", MemberKind: "group", MemberID: "support"}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`, `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash","tool:Bashbox"]}]`} {
		if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(v)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "group", SubjectID: "staff"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "gone", SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
	p, err := New(st, authz.ModeCompat, 0).Resolve(ctx, member("alice"))
	if err != nil {
		t.Fatal(err)
	}
	eff := p.Effective
	if eff.Baseline() != authz.BaselineMember {
		t.Errorf("baseline = %q, want %q", eff.Baseline(), authz.BaselineMember)
	}
	got := map[string]authz.AppliedList{}
	for _, l := range eff.Lists() {
		got[l.Name] = l
	}
	if l, ok := got["no-shell"]; !ok || l.Version != 2 || l.Binding.SubjectKind != authz.SubjectGroup || l.Binding.SubjectID != "staff" {
		t.Errorf("no-shell applied as %+v (present %v), want version 2 through group staff", l, ok)
	}
	if _, ok := got[authz.BaselineMember]; !ok {
		t.Errorf("the baseline binding is not among the applied lists: %+v", eff.Lists())
	}
	if _, ok := got["gone"]; ok {
		t.Error("a binding to a missing list is reported as applied")
	}
	want := map[string]string{"support": authz.ViaDirect, "staff": "support"}
	if via := eff.GroupVia(); !reflect.DeepEqual(via, want) {
		t.Errorf("via = %v, want %v", via, want)
	}
}
