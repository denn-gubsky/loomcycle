package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// hookStore wraps the fixture's store to count reads and to act in the
// middle of a request.
type hookStore struct {
	store.Store
	mu                sync.Mutex
	onGroupsOfSubject map[string]func() // subject → once
	failRemovalDelete bool
	failDisown        bool
	scimUserGets      atomic.Int64
	groupLists        atomic.Int64
}

func (h *hookStore) GroupsOfSubject(ctx context.Context, tenantID, subject string) ([]string, error) {
	h.mu.Lock()
	fn := h.onGroupsOfSubject[subject]
	delete(h.onGroupsOfSubject, subject)
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	return h.Store.GroupsOfSubject(ctx, tenantID, subject)
}

func (h *hookStore) AuthRemovalDelete(ctx context.Context, tenantID, subject string) (bool, error) {
	if h.failRemovalDelete {
		return false, errors.New("injected: removal delete failed")
	}
	return h.Store.AuthRemovalDelete(ctx, tenantID, subject)
}

func (h *hookStore) OperatorDisableUser(ctx context.Context, tenantID, subject string) (bool, error) {
	if h.failDisown {
		return false, errors.New("injected: operator disable failed")
	}
	return h.Store.OperatorDisableUser(ctx, tenantID, subject)
}

func (h *hookStore) AuthRemovalDeleteIdle(ctx context.Context, tenantID, subject string) (bool, error) {
	if h.failRemovalDelete {
		return false, errors.New("injected: removal delete failed")
	}
	return h.Store.AuthRemovalDeleteIdle(ctx, tenantID, subject)
}

func (h *hookStore) SCIMUserGet(ctx context.Context, gate, tenantID, scimID string) (store.SCIMUserRow, error) {
	h.scimUserGets.Add(1)
	return h.Store.SCIMUserGet(ctx, gate, tenantID, scimID)
}

func (h *hookStore) GroupList(ctx context.Context, tenantID string) ([]store.GroupRow, error) {
	h.groupLists.Add(1)
	return h.Store.GroupList(ctx, tenantID)
}

func hook(f *gateFixture) *hookStore {
	h := &hookStore{Store: f.st, onGroupsOfSubject: map[string]func(){}}
	f.srv.store = h
	return h
}

// seedOldSCIMUser makes a SCIM user the gate linked before the removal
// window, as an earlier day's sync would have.
func seedOldSCIMUser(t *testing.T, f *gateFixture, id string) string {
	t.Helper()
	ctx := context.Background()
	old := time.Now().UTC().Add(-72 * time.Hour)
	sub := "okta:" + id
	if err := f.st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: "active", CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UserIdentityCreate(ctx, store.UserIdentityRow{TenantID: "acme", Subject: sub, Gate: "okta", Issuer: "scim",
		ExternalSubject: id, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SCIMUserCreate(ctx, store.SCIMUserRow{Gate: "okta", TenantID: "acme", SCIMID: id, Subject: sub,
		UserName: id + "@acme.test", UserNameKey: id + "@acme.test", Attributes: `{}`, CreatedAt: old}, time.Time{}, 1<<30); err != nil {
		t.Fatal(err)
	}
	return sub
}

// A percentage removal ceiling is of the users the gate had before the
// window: users the provider creates now never raise it.
func TestSCIM_CreatesInTheWindowDoNotRaiseTheRemovalCeiling(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	cfg := f.srv.cfg()
	for i := range cfg.ResolvedAuth.Gates {
		if cfg.ResolvedAuth.Gates[i].Name == "okta" {
			cfg.ResolvedAuth.Gates[i].SCIM.MaxSyncRemovalsPercent = 50
		}
	}
	old := []string{seedOldSCIMUser(t, f, "old1"), seedOldSCIMUser(t, f, "old2"), seedOldSCIMUser(t, f, "old3"), seedOldSCIMUser(t, f, "old4")}
	// Twenty fresh users would make a 50% ceiling 12 if they counted.
	for i := 0; i < 20; i++ {
		f.scimUser(t, bSCIMAcme, fmt.Sprintf("fresh%d@acme.test", i))
	}
	for i, sub := range old[:3] {
		want := http.StatusNoContent
		if i == 2 {
			want = http.StatusTooManyRequests // 50% of the four users before the window
		}
		if code, out := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+strings.TrimPrefix(sub, "okta:"), ""); code != want {
			t.Fatalf("removal %d = %d %v, want %d", i+1, code, out, want)
		}
	}
	if st := f.status(t, "acme", old[2]); st != "active" {
		t.Fatalf("the removal over the ceiling left the user %s", st)
	}
}

// A SCIM gate creates at most max_sync_creates users per window.
func TestSCIM_CreatesAreHeldToTheCeiling(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	cfg := f.srv.cfg()
	for i := range cfg.ResolvedAuth.Gates {
		if cfg.ResolvedAuth.Gates[i].Name == "okta" {
			cfg.ResolvedAuth.Gates[i].SCIM.MaxSyncCreates = 2
		}
	}
	f.scimUser(t, bSCIMAcme, "a@acme.test")
	f.scimUser(t, bSCIMAcme, "b@acme.test")
	code, out := f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{"userName": "c@acme.test"}))
	if code != http.StatusTooManyRequests {
		t.Fatalf("a create over the ceiling = %d %v, want 429", code, out)
	}
	if rows, _, _ := f.st.SCIMUserList(context.Background(), "okta", "acme", store.SCIMFilter{UserNameKey: "c@acme.test"}, 0, 10); len(rows) != 0 {
		t.Fatalf("the refused create left a resource: %+v", rows)
	}
	// Another tenant's ceiling is its own.
	f.scimUser(t, bSCIMOther, "c@other.test")
}

// A refused status change writes none of the request's attributes.
func TestSCIM_ARefusedStatusChangeWritesNoAttribute(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true, maxRemovals: 1})
	ctx := context.Background()
	// A re-enable of a user an operator disabled.
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	disabled := "disabled"
	if err := f.st.UserUpdate(ctx, "acme", sub, store.UserPatch{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Users/"+id, `{"userName":"renamed@acme.test","displayName":"Renamed","active":true}`); code != http.StatusForbidden {
		t.Fatalf("re-enable of an operator's disable = %d, want 403", code)
	}
	if row, _ := f.st.SCIMUserGet(ctx, "okta", "acme", id); row.UserName != "alice@acme.test" {
		t.Fatalf("a refused re-enable renamed the resource to %q", row.UserName)
	}
	if u, _ := f.st.UserGet(ctx, "acme", sub); u.DisplayName == "Renamed" {
		t.Fatal("a refused re-enable changed the display name")
	}
	// A removal over the ceiling.
	first, _ := f.scimUser(t, bSCIMAcme, "bob@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+first, ""); code != http.StatusNoContent {
		t.Fatalf("first removal = %d", code)
	}
	id2, _ := f.scimUser(t, bSCIMAcme, "carol@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Users/"+id2, `{"userName":"carol2@acme.test","active":false}`); code != http.StatusTooManyRequests {
		t.Fatalf("a removal over the ceiling = %d, want 429", code)
	}
	if row, _ := f.st.SCIMUserGet(ctx, "okta", "acme", id2); row.UserName != "carol@acme.test" {
		t.Fatalf("a refused removal renamed the resource to %q", row.UserName)
	}
}

// A SCIM re-enable gives the user nothing before it owns the re-enable: an
// operator who takes the removal over while the user is being reset keeps a
// disabled user that has joined no floor group.
func TestSCIM_AReEnableGivesNothingBeforeItOwnsTheUser(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	ctx := context.Background()
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	off := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, off); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	h := hook(f)
	h.onGroupsOfSubject[sub] = func() {
		now := time.Now().UTC()
		if ok, err := f.st.AuthRemovalStart(ctx, store.AuthRemovalRow{TenantID: "acme", Subject: sub, Reason: store.RemovalReasonOperator,
			DisabledAt: now, UnbindDueAt: now.Add(time.Hour), CreatedBy: "op"}); !ok || err != nil {
			t.Errorf("operator takeover: %v %v", ok, err)
		}
	}
	on := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true})
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, on); code != http.StatusForbidden {
		t.Fatalf("a re-enable an operator took over = %d, want 403", code)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Fatalf("status = %s", st)
	}
	if groups, _ := f.st.GroupsOfSubject(ctx, "acme", sub); slices.Contains(groups, "everyone") {
		t.Fatalf("a re-enable SCIM did not own joined the floor groups: %v", groups)
	}
}

// An operator's plain disable takes the user from SCIM: a SCIM removal row
// behind it goes, so the identity provider cannot re-enable the user.
func TestSCIM_AnOperatorsDisableTakesTheUserFromSCIM(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	off := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": false})
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, off); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	if code, out := f.call(t, bAcmeOp, "PATCH", "/v1/_users/"+sub, `{"status":"disabled"}`); code != http.StatusOK {
		t.Fatalf("operator disable = %d %v", code, out)
	}
	on := scimPatchBody(t, map[string]any{"op": "replace", "path": "active", "value": true})
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, on); code != http.StatusForbidden {
		t.Fatalf("SCIM re-enable after an operator's disable = %d, want 403", code)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Fatalf("status = %s", st)
	}
}

// An operator's re-enable that cannot end the removal leaves the user
// disabled: an active user behind a SCIM removal row is one the provider
// could later re-enable over an operator's next disable.
func TestUsers_AReactivationThatCannotEndTheRemovalLeavesTheUserDisabled(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Users/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("remove = %d", code)
	}
	h := hook(f)
	h.failRemovalDelete = true
	if code, _ := f.call(t, bAcmeOp, "PATCH", "/v1/_users/"+sub, `{"status":"active"}`); code != http.StatusInternalServerError {
		t.Fatalf("a re-enable whose removal row stays = %d, want 500", code)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Fatalf("status = %s, want disabled", st)
	}
}

// An operator's disable retires the user's delegated tokens even when the
// disable's store write (which also takes the user from SCIM) fails: the
// revocation never waits on it.
func TestUsers_ADisableRetiresTokensEvenWhenSCIMDisownFails(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	ctx := context.Background()
	if err := f.st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "bob", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	seedToken(t, f.st, "bob-bearer", "acme", "bob", []string{"runs:create"}, time.Time{})
	h := hook(f)
	h.failDisown = true
	if code, _ := f.call(t, bAcmeOp, "PATCH", "/v1/_users/bob", `{"status":"disabled"}`); code != http.StatusInternalServerError {
		t.Fatalf("a disable whose disown failed = %d, want 500", code)
	}
	row, err := f.st.OperatorTokenDefGet(ctx, "def_bob")
	if err != nil {
		t.Fatal(err)
	}
	if row.RetiredAt.IsZero() {
		t.Fatal("the disabled user's token stayed live after the disown failed")
	}
}

// A group delete needs group sync like every other group write.
func TestSCIM_AGroupDeleteNeedsGroupSync(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	ctx := context.Background()
	if err := f.st.SCIMGroupCreate(ctx, store.SCIMGroupRow{Gate: "okta", TenantID: "acme", SCIMID: "g1", DisplayName: "Admins", DisplayKey: "admins"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(t, bSCIMAcme, "DELETE", "/scim/v2/Groups/g1", ""); code != http.StatusForbidden {
		t.Fatalf("group delete on a gate without group sync = %d, want 403", code)
	}
	if _, err := f.st.SCIMGroupGet(ctx, "okta", "acme", "g1"); err != nil {
		t.Fatalf("the group went: %v", err)
	}
}

// One request resolves at most scimMaxGroupMembers member references,
// across all of its operations.
func TestSCIM_APatchIsHeldToOneMemberBudget(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{trusted: true, groups: true})
	aliceID, _ := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	code, body := f.call(t, bSCIMAcme, "POST", "/scim/v2/Groups", `{"displayName":"Sales"}`)
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %v", code, body)
	}
	half := make([]map[string]string, scimMaxGroupMembers/2+1)
	for i := range half {
		half[i] = map[string]string{"value": aliceID}
	}
	op := map[string]any{"op": "add", "path": "members", "value": half}
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Groups/"+body["id"].(string), scimPatchBody(t, op, op)); code != http.StatusBadRequest {
		t.Fatalf("a PATCH over the member budget = %d, want 400", code)
	}
}

// A group write resolves its members in one read and lists the tenant's
// groups once for the whole re-sync, not once per member.
func TestSCIM_AGroupWriteReadsMembersAndGroupsOnce(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{trusted: true, groups: true})
	var members []map[string]string
	for i := 0; i < 5; i++ {
		id, _ := f.scimUser(t, bSCIMAcme, fmt.Sprintf("u%d@acme.test", i))
		members = append(members, map[string]string{"value": id})
	}
	code, body := f.call(t, bSCIMAcme, "POST", "/scim/v2/Groups", `{"displayName":"Sales"}`)
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %v", code, body)
	}
	h := hook(f)
	if code, out := f.call(t, bSCIMAcme, "PUT", "/scim/v2/Groups/"+body["id"].(string), jsonBody(t, map[string]any{
		"displayName": "Sales", "members": members})); code != http.StatusOK {
		t.Fatalf("replace group: %d %v", code, out)
	}
	if n := h.scimUserGets.Load(); n != 0 {
		t.Fatalf("the members were read one by one: %d reads", n)
	}
	// The first member's sync creates the group: one listing before it, one
	// after, then none.
	if n := h.groupLists.Load(); n > 2 {
		t.Fatalf("the tenant's groups were listed %d times for 5 members", n)
	}
}
