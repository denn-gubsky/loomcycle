package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// raceStore runs a hook in the middle of a request, once per subject.
type raceStore struct {
	store.Store
	mu           sync.Mutex
	onTokenList  map[string]func()
	failGroupAdd map[string]bool
}

func (h *raceStore) OperatorTokenDefListBySubject(ctx context.Context, tenantID, subject string) ([]store.OperatorTokenDefRow, error) {
	h.mu.Lock()
	fn := h.onTokenList[subject]
	delete(h.onTokenList, subject)
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	return h.Store.OperatorTokenDefListBySubject(ctx, tenantID, subject)
}

func (h *raceStore) GroupMemberAdd(ctx context.Context, row store.GroupMemberRow) error {
	h.mu.Lock()
	fail := h.failGroupAdd[row.MemberID]
	h.mu.Unlock()
	if fail {
		return errors.New("injected: group member add failed")
	}
	return h.Store.GroupMemberAdd(ctx, row)
}

func race(f *gateFixture) *raceStore {
	h := &raceStore{Store: f.st, onTokenList: map[string]func(){}, failGroupAdd: map[string]bool{}}
	f.srv.store = h
	return h
}

func scimGate(f *gateFixture) *config.AuthSCIMGate {
	for i := range f.srv.cfg().ResolvedAuth.Gates {
		if g := f.srv.cfg().ResolvedAuth.Gates[i]; g.Name == "okta" {
			return g.SCIM
		}
	}
	return nil
}

var (
	scimOff = map[string]any{"op": "replace", "path": "active", "value": false}
	scimOn  = map[string]any{"op": "replace", "path": "active", "value": true}
)

// A SCIM re-enable that committed is recorded, even when joining the floor
// groups fails after it.
func TestSCIM_AReEnableIsRecordedEvenWhenTheFloorJoinFails(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOff)); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	h := race(f)
	h.failGroupAdd[sub] = true
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOn)); code != http.StatusInternalServerError {
		t.Fatalf("a re-enable whose floor join failed = %d, want 500", code)
	}
	if st := f.status(t, "acme", sub); st != "active" {
		t.Fatalf("status = %s", st)
	}
	if n := f.count("auth_user_reactivated"); n != 1 {
		t.Fatalf("auth_user_reactivated recorded %d times, want 1", n)
	}
}

// A user removed while pending comes back pending from an operator's
// re-enable, and a pending user is never disabled plainly (which would drop
// the approval it owes).
func TestUsers_APendingUserNeverBecomesActiveWithoutApproval(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{newUserStatus: store.UserStatusPending, authoritative: true})
	ctx := context.Background()
	// Removed by SCIM while pending, re-enabled by an operator.
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOff)); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	if code, out := f.call(t, bAcmeOp, "PATCH", "/v1/_users/"+sub, `{"status":"active"}`); code != http.StatusOK {
		t.Fatalf("operator re-enable = %d %v", code, out)
	}
	if st := f.status(t, "acme", sub); st != store.UserStatusPending {
		t.Fatalf("a SCIM-removed pending user came back %s", st)
	}
	// Removed by a revoke (or dormancy) while pending, re-enabled.
	if err := f.st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "bob", AccessMode: "isolated", Status: store.UserStatusPending}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Hour}, "acme", "bob", "corp", store.RemovalReasonRevoked, "gate:corp"); err != nil {
		t.Fatal(err)
	}
	if code, out := f.call(t, bAcmeOp, "PATCH", "/v1/_users/bob", `{"status":"active"}`); code != http.StatusOK {
		t.Fatalf("operator re-enable = %d %v", code, out)
	}
	if st := f.status(t, "acme", "bob"); st != store.UserStatusPending {
		t.Fatalf("a revoked pending user came back %s", st)
	}
	// A plain disable of a pending user is refused.
	if code, out := f.call(t, bAcmeOp, "PATCH", "/v1/_users/bob", `{"status":"disabled"}`); code != http.StatusConflict || out["code"] != "user_pending" {
		t.Fatalf("a plain disable of a pending user = %d %v", code, out)
	}
	if st := f.status(t, "acme", "bob"); st != store.UserStatusPending {
		t.Fatalf("status = %s", st)
	}
}

// A revoke racing a SCIM re-enable never answers "removed" over an active
// user: the disable and the removal row are one write, so the re-enable's
// claim is taken over and it stops.
func TestAuthGate_ARevokeRacingASCIMReEnableLeavesTheUserDisabled(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	ctx := context.Background()
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOff)); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	now := time.Now().UTC()
	if ok, err := f.st.SCIMReactivateClaim(ctx, "okta", "acme", sub, now, now.Add(-time.Hour), 100); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	h := race(f)
	finished := false
	h.onTokenList[sub] = func() {
		_, finished, _ = f.st.SCIMReactivate(ctx, "okta", "acme", sub)
	}
	if err := f.srv.startRemovalWith(ctx, lifecycle.Policy{UnbindAfter: time.Hour}, "acme", sub, "corp", store.RemovalReasonRevoked, "gate:corp"); err != nil {
		t.Fatal(err)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Fatalf("the revoke answered with the user %s (the re-enable finished: %v)", st, finished)
	}
	if rm, err := f.st.AuthRemovalGet(ctx, "acme", sub); err != nil || rm.Reason != store.RemovalReasonRevoked {
		t.Fatalf("removal = %+v %v", rm, err)
	}
}

// A SCIM re-enable never races a running removal stage: while the sweeper
// unbinds, the re-enable is refused (409) and the user stays disabled.
func TestSCIM_AReEnableNeverRacesARemovalStage(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true})
	id, sub := f.scimUser(t, bSCIMAcme, "alice@acme.test")
	if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOff)); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	h := hook(f)
	got := 0
	h.onGroupsOfSubject[sub] = func() { // inside the sweeper's unbind
		got, _ = f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOn))
	}
	f.srv.AuthLifecycleSweep(context.Background(), time.Now().Add(200*time.Hour))
	if got != http.StatusConflict {
		t.Fatalf("a re-enable while the unbind ran = %d, want 409", got)
	}
	if st := f.status(t, "acme", sub); st != "disabled" {
		t.Fatalf("status = %s after a re-enable raced the unbind", st)
	}
	if rm, err := f.st.AuthRemovalGet(context.Background(), "acme", sub); err != nil || rm.UnboundAt.IsZero() || rm.Stage != "" {
		t.Fatalf("removal after the stage = %+v %v", rm, err)
	}
}

// Concurrent creates never pass max_sync_creates together.
func TestSCIM_ConcurrentCreatesAreHeldToTheCeiling(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{})
	scimGate(f).MaxSyncCreates = 3
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = f.call(t, bSCIMAcme, "POST", "/scim/v2/Users", jsonBody(t, map[string]any{"userName": fmt.Sprintf("u%d@acme.test", i)}))
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	if created != 3 {
		t.Fatalf("%d of 10 concurrent creates passed a ceiling of 3: %v", created, codes)
	}
}

// SCIM re-enables are held to the removal ceiling per window.
func TestSCIM_ReEnablesAreHeldToTheCeiling(t *testing.T) {
	f := newSCIMFixture(t, scimOpts{authoritative: true, maxRemovals: 3})
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		id, _ := f.scimUser(t, bSCIMAcme, n+"@acme.test")
		if code, _ := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOff)); code != http.StatusOK {
			t.Fatalf("remove %s = %d", n, code)
		}
		ids = append(ids, id)
	}
	scimGate(f).MaxSyncRemovals = 2
	for i, id := range ids {
		want := http.StatusOK
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if code, out := f.call(t, bSCIMAcme, "PATCH", "/scim/v2/Users/"+id, scimPatchBody(t, scimOn)); code != want {
			t.Fatalf("re-enable %d = %d %v, want %d", i+1, code, out, want)
		}
	}
	if st := f.status(t, "acme", "okta:"+ids[2]); st != "disabled" {
		t.Fatalf("the re-enable over the ceiling left the user %s", st)
	}
}
