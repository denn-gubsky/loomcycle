package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The acl section on both backends. The postgres half needs
// LOOMCYCLE_TEST_PG_DSN (see coverage_test.go).

const (
	aclTestRulesSupport = `[{"effect":"allow","actions":["run"],"resources":["agent:chat/*"]},{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	aclTestRulesStrict  = `[{"effect":"deny","actions":["run"],"resources":["agent:*"]}]`
)

// seedACLTenant writes, in tenant, two lists (one retired, one at version 2),
// a nested group pair with a user member, and three bindings (group, an
// expiring user binding, a retired tenant-wide one).
func seedACLTenant(t *testing.T, s store.Store, tenant string) {
	t.Helper()
	ctx := context.Background()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %q %s: %v", tenant, what, err)
		}
	}
	_, err := s.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: "support", Rules: json.RawMessage(aclTestRulesStrict), CreatedBy: "op"})
	must("support v1", err)
	_, err = s.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: "support", Rules: json.RawMessage(aclTestRulesSupport), Description: "tier 1", CreatedBy: "op"})
	must("support v2", err)
	_, err = s.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: "legacy", Rules: json.RawMessage(aclTestRulesStrict)})
	must("legacy", err)
	_, err = s.AclDefRetire(ctx, tenant, "legacy")
	must("retire legacy", err)
	for _, g := range []string{"contractors", "support"} {
		_, err = s.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: g, Description: g + " group", CreatedBy: "op"})
		must("group "+g, err)
	}
	must("member alice", s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: "contractors", MemberKind: store.GroupMemberUser, MemberID: "alice", AddedBy: "op"}))
	must("nest", s.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: "support", MemberKind: store.GroupMemberGroup, MemberID: "contractors", AddedBy: "op"}))
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_g_" + tenant, TenantID: tenant, ACLName: "support", SubjectKind: "group", SubjectID: "support", CreatedBy: "op"})
	must("binding group", err)
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_u_" + tenant, TenantID: tenant, ACLName: "support", SubjectKind: "user", SubjectID: "bob",
		ExpiresAt: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)})
	must("binding user", err)
	_, err = s.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_t_" + tenant, TenantID: tenant, ACLName: "legacy", SubjectKind: "tenant"})
	must("binding tenant", err)
	_, err = s.AclBindingRetire(ctx, tenant, "aclb_t_"+tenant)
	must("retire binding", err)
}

// aclState is a tenant's rows as the per-tenant reads return them, with the
// times in UTC so two backends' zones compare equal.
type aclState struct {
	Lists    []store.AclDefRow
	Groups   []store.GroupRow
	Members  map[string][]store.GroupMemberRow
	Bindings []store.AclBindingRow
}

func readACLState(t *testing.T, s store.Store, tenant string) aclState {
	t.Helper()
	ctx := context.Background()
	var st aclState
	var err error
	if st.Lists, err = s.AclDefList(ctx, tenant, true); err != nil {
		t.Fatal(err)
	}
	for i := range st.Lists {
		r := &st.Lists[i]
		r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
		if !r.RetiredAt.IsZero() {
			r.RetiredAt = r.RetiredAt.UTC()
		}
		var v any
		_ = json.Unmarshal(r.Rules, &v)
		r.Rules, _ = json.Marshal(v) // whitespace-insensitive
	}
	if st.Groups, err = s.GroupList(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	st.Members = map[string][]store.GroupMemberRow{}
	for i := range st.Groups {
		g := &st.Groups[i]
		g.CreatedAt, g.UpdatedAt = g.CreatedAt.UTC(), g.UpdatedAt.UTC()
		ms, err := s.GroupMembers(ctx, tenant, g.Name)
		if err != nil {
			t.Fatal(err)
		}
		for j := range ms {
			ms[j].AddedAt = ms[j].AddedAt.UTC()
		}
		st.Members[g.Name] = ms
	}
	if st.Bindings, err = s.AclBindingList(ctx, tenant, true); err != nil {
		t.Fatal(err)
	}
	for i := range st.Bindings {
		b := &st.Bindings[i]
		b.CreatedAt = b.CreatedAt.UTC()
		if !b.ExpiresAt.IsZero() {
			b.ExpiresAt = b.ExpiresAt.UTC()
		}
		if !b.RetiredAt.IsZero() {
			b.RetiredAt = b.RetiredAt.UTC()
		}
	}
	return st
}

// Every tenant's lists, groups, memberships and bindings come back under their
// own tenant with every field the source held — version, times, authors,
// expiry, retirement — the restore bumps each touched tenant's acl_version
// and names those tenants for the cache refresh, and a second restore of the
// same snapshot writes nothing.
func TestSnapshotACL_RoundTripsEveryTenantsRowsOnBothBackends(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			src, dst := b.open(t), b.open(t)
			seedACLTenant(t, src, "acme")
			seedACLTenant(t, src, "")

			raw := mustCapture(t, src)
			res := mustRestore(t, dst, raw, RestoreOptions{})
			if res.ACLListsRestored != 4 || res.ACLGroupsRestored != 4 || res.ACLMembersRestored != 4 || res.ACLBindingsRestored != 6 {
				t.Errorf("restored lists=%d groups=%d members=%d bindings=%d; want 4/4/4/6",
					res.ACLListsRestored, res.ACLGroupsRestored, res.ACLMembersRestored, res.ACLBindingsRestored)
			}
			if want := []string{"", "acme"}; !reflect.DeepEqual(res.Refresh.ACLTenants, want) {
				t.Errorf("Refresh.ACLTenants = %q, want %q", res.Refresh.ACLTenants, want)
			}
			if c := res.Counts(); c["acl_lists"] != 4 || c["acl_bindings"] != 6 {
				t.Errorf("Counts() acl_lists=%d acl_bindings=%d; the transports' restored map would miss them", c["acl_lists"], c["acl_bindings"])
			}
			for _, tenant := range []string{"", "acme"} {
				want, got := readACLState(t, src, tenant), readACLState(t, dst, tenant)
				// The restore recomputes the hash and stamps this build's
				// catalog version; with the same build, both must match.
				if !reflect.DeepEqual(got, want) {
					t.Errorf("tenant %q restored state differs:\n got %+v\nwant %+v", tenant, got, want)
				}
				if v, err := dst.AclVersion(ctx, tenant); err != nil || v == 0 {
					t.Errorf("tenant %q acl_version = %d, %v; a cached set keyed on 0 would miss the restored rows", tenant, v, err)
				}
			}
			if g, _ := dst.GroupsOfSubject(ctx, "acme", "alice"); !reflect.DeepEqual(g, []string{"contractors"}) {
				t.Errorf("alice's direct groups on the target = %v", g)
			}

			v0, _ := dst.AclVersion(ctx, "acme")
			again := mustRestore(t, dst, raw, RestoreOptions{})
			if again.ACLListsRestored+again.ACLGroupsRestored+again.ACLMembersRestored+again.ACLBindingsRestored != 0 || len(again.Refresh.ACLTenants) != 0 {
				t.Errorf("re-restore wrote acl rows: %+v tenants %q", again, again.Refresh.ACLTenants)
			}
			if v, _ := dst.AclVersion(ctx, "acme"); v != v0 {
				t.Errorf("re-restore moved acl_version %d → %d", v0, v)
			}
			for _, w := range again.Warnings {
				if strings.Contains(w, "acl ") {
					t.Errorf("re-restore warned: %s", w)
				}
			}
		})
	}
}

// A target that already holds rows on the snapshot's keys keeps them, as every
// section does: its own stricter list of the same name stands and the operator
// is told, its own extra rows are untouched, and the rows only the snapshot has
// are added.
func TestSnapshotACL_RestoreKeepsTheTargetsRowsOnHeldKeys(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			src, dst := b.open(t), b.open(t)
			seedACLTenant(t, src, "acme")

			// The target already has its own "support" list (different rules)
			// and a group and binding the snapshot does not.
			if _, err := dst.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "support", Rules: json.RawMessage(aclTestRulesStrict)}); err != nil {
				t.Fatal(err)
			}
			if _, err := dst.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "target-only"}); err != nil {
				t.Fatal(err)
			}
			if _, err := dst.AclBindingPut(ctx, store.AclBindingRow{BindingID: "aclb_target", TenantID: "acme", ACLName: "support", SubjectKind: "group", SubjectID: "target-only"}); err != nil {
				t.Fatal(err)
			}

			res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{})
			live, err := dst.AclDefGet(ctx, "acme", "support")
			if err != nil || live.Version != 1 || !jsonSame(live.Rules, aclTestRulesStrict) {
				t.Errorf("held list = %+v, %v; want the target's own version-1 strict list", live, err)
			}
			warned := false
			for _, w := range res.Warnings {
				if strings.Contains(w, "acl list acme/support") && strings.Contains(w, "live list was kept") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning names the held list; warnings: %q", res.Warnings)
			}
			if res.ACLListsRestored != 1 { // only "legacy"
				t.Errorf("lists restored = %d, want 1", res.ACLListsRestored)
			}
			if _, err := dst.GroupGet(ctx, "acme", "target-only"); err != nil {
				t.Errorf("target's own group lost: %v", err)
			}
			bs, _ := dst.AclBindingList(ctx, "acme", true)
			ids := map[string]bool{}
			for _, b := range bs {
				ids[b.BindingID] = true
			}
			for _, id := range []string{"aclb_target", "aclb_g_acme", "aclb_u_acme", "aclb_t_acme"} {
				if !ids[id] {
					t.Errorf("binding %s missing after restore (have %v)", id, ids)
				}
			}
		})
	}
}

func jsonSame(got json.RawMessage, want string) bool {
	var a, b any
	return json.Unmarshal(got, &a) == nil && json.Unmarshal([]byte(want), &b) == nil && reflect.DeepEqual(a, b)
}

// A hand-edited envelope cannot store what no author here could: a list with a
// verb this build's catalog lacks, and a membership in a group that is not in
// the snapshot, are skipped with a warning naming them; the rest lands.
func TestSnapshotACL_RefusedRowsAreSkippedWithAWarning(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	seedACLTenant(t, src, "acme")
	raw := editSection(t, mustCapture(t, src), "acl", func(sec map[string]any) {
		lists := sec["lists"].([]any)
		lists = append(lists, map[string]any{"tenant_id": "acme", "name": "future", "version": 1,
			"rules":      []any{map[string]any{"effect": "allow", "actions": []any{"teleport"}, "resources": []any{"agent:*"}}},
			"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"})
		sec["lists"] = lists
		members := sec["members"].([]any)
		sec["members"] = append(members, map[string]any{"tenant_id": "acme", "group": "ghost", "member_kind": "user",
			"member_id": "zed", "added_at": "2026-01-01T00:00:00Z"})
	})
	res := mustRestore(t, dst, raw, RestoreOptions{})
	var sawList, sawMember bool
	for _, w := range res.Warnings {
		sawList = sawList || strings.Contains(w, "acl list acme/future")
		sawMember = sawMember || strings.Contains(w, "acl group member acme/ghost user:zed")
	}
	if !sawList || !sawMember {
		t.Errorf("warnings %q; want the refused list and membership named", res.Warnings)
	}
	if _, err := dst.AclDefGet(context.Background(), "acme", "future"); err == nil {
		t.Error("a list with an unknown verb was stored")
	}
	if res.ACLListsRestored != 2 || res.ACLMembersRestored != 2 {
		t.Errorf("lists=%d members=%d; the valid rows must still land", res.ACLListsRestored, res.ACLMembersRestored)
	}
}

// A snapshot written before the section existed restores as it always did:
// no acl rows, no warning, no tenants to refresh.
func TestSnapshotACL_SnapshotWithoutTheSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	seedACLTenant(t, src, "acme")
	var env map[string]any
	if err := json.Unmarshal(mustCapture(t, src), &env); err != nil {
		t.Fatal(err)
	}
	delete(env["sections"].(map[string]any), "acl")
	delete(env, "checksum")
	raw, _ := json.Marshal(env)
	res := mustRestore(t, dst, raw, RestoreOptions{})
	if res.ACLListsRestored+res.ACLGroupsRestored+res.ACLMembersRestored+res.ACLBindingsRestored != 0 || len(res.Refresh.ACLTenants) != 0 {
		t.Errorf("restored acl rows from a snapshot without the section: %+v", res)
	}
	if l, _ := dst.AclDefList(context.Background(), "acme", true); len(l) != 0 {
		t.Errorf("target has %d lists", len(l))
	}
}
