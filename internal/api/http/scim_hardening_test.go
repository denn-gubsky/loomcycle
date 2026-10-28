package http

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

func (h *hookStore) SCIMRemovalDisown(ctx context.Context, tenantID, subject string) (bool, error) {
	if h.failDisown {
		return false, errors.New("injected: disown failed")
	}
	return h.Store.SCIMRemovalDisown(ctx, tenantID, subject)
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
