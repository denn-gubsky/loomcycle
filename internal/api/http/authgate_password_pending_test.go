package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// pendingStatus is the "awaiting approval" marker GET /v1/_me carries for a
// session whose user is pending ("" otherwise).
func (b *browser) pendingStatus() string {
	b.t.Helper()
	code, me := b.do("GET", "/v1/_me", "")
	if code != http.StatusOK {
		b.t.Fatalf("/v1/_me = %d %v", code, me)
	}
	a, _ := me["auth"].(map[string]any)
	st, _ := a["status"].(string)
	return st
}

// A website's sign-up on a gate with new_user.status: pending (loomcycle.cloud,
// one shared tenant) redeems into a user at the gate's floor that signs in to
// the awaiting-approval page and nothing else, by the invite's own session and
// by a later password login, until a tenant operator approves it.
func TestPasswordGate_WebsiteSignupWaitsForApproval(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bSite, "queue", `{"tenant":"acme","login":"pat@example.com"}`)
	b := f.redeemed(t, "queue", "pat@example.com", inv)
	_, subject := b.me()
	u, err := f.st.UserGet(context.Background(), "acme", subject)
	if err != nil || u.Status != store.UserStatusPending || u.AccessMode != "tenant" {
		t.Fatalf("user row = %+v, %v", u, err)
	}
	if st := b.pendingStatus(); st != store.UserStatusPending {
		t.Fatalf("/v1/_me status after redeem = %q", st)
	}
	if code, _ := b.do("GET", "/v1/_auth/account", ""); code != http.StatusForbidden {
		t.Fatalf("pending session reached /v1/_auth/account: %d", code)
	}
	// The password signs in again, to the same waiting room.
	again := f.browser(t)
	if code, body := again.login("queue", "pat@example.com", goodPassword, ""); code != http.StatusNoContent {
		t.Fatalf("pending login: %d %v", code, body)
	}
	if st := again.pendingStatus(); st != store.UserStatusPending {
		t.Fatalf("/v1/_me status after login = %q", st)
	}
	if code, _ := again.do("GET", "/v1/_auth/account", ""); code != http.StatusForbidden {
		t.Fatalf("pending login reached /v1/_auth/account: %d", code)
	}

	if code, body := f.call(t, bAcmeOp, "POST", "/v1/_users/"+subject+"/approve", ""); code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, body)
	}
	// Both sessions are the user now: no waiting marker, the API answers.
	for _, s := range []*browser{b, again} {
		if st := s.pendingStatus(); st != "" {
			t.Fatalf("approved session still %q", st)
		}
		if code, body := s.do("GET", "/v1/_auth/account", ""); code != http.StatusOK {
			t.Fatalf("approved session refused /v1/_auth/account: %d %v", code, body)
		}
	}
}

// Only a website's invite waits. An operator's own invite is the approval,
// and an attach invite never changes an existing user's status, so on the
// same pending gate both end active.
func TestPasswordGate_OperatorAndAttachInvitesOnAPendingGateStayActive(t *testing.T) {
	f := newGateFixture(t)
	b := f.redeemed(t, "queue", "olga@example.com", f.inviteCode(t, bAcmeOp, "queue", `{"login":"olga@example.com"}`))
	_, subject := b.me()
	if u, err := f.st.UserGet(context.Background(), "acme", subject); err != nil || u.Status != "active" {
		t.Fatalf("operator-invited user = %+v, %v", u, err)
	}
	if st := b.pendingStatus(); st != "" {
		t.Fatalf("operator-invited session is %q", st)
	}

	f.addUser(t, "acme", "legacy", "tenant")
	inv := f.inviteCode(t, bSite, "queue", `{"tenant":"acme","subject":"legacy","login":"legacy@example.com"}`)
	lb := f.redeemed(t, "queue", "legacy@example.com", inv)
	if u, err := f.st.UserGet(context.Background(), "acme", "legacy"); err != nil || u.Status != "active" {
		t.Fatalf("attached user = %+v, %v", u, err)
	}
	if code, _ := lb.do("GET", "/v1/_auth/account", ""); code != http.StatusOK {
		t.Fatalf("attached user refused /v1/_auth/account: %d", code)
	}
}

// Pending signs in; disabled still does not: a disabled user's right password
// gets the same answer as a wrong one.
func TestPasswordGate_DisabledUserStillCannotSignIn(t *testing.T) {
	f := newGateFixture(t)
	b := f.redeemed(t, "queue", "dora@example.com", f.inviteCode(t, bSite, "queue", `{"tenant":"acme","login":"dora@example.com"}`))
	_, subject := b.me()
	off := "disabled"
	if err := f.st.UserUpdate(context.Background(), "acme", subject, store.UserPatch{Status: &off}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.browser(t).login("queue", "dora@example.com", goodPassword, ""); code != http.StatusUnauthorized || body["code"] != "invalid_credentials" {
		t.Fatalf("disabled login: %d %v", code, body)
	}
	if code, _ := b.do("GET", "/v1/_me", ""); code != http.StatusUnauthorized {
		t.Fatalf("disabled user's session still answers /v1/_me: %d", code)
	}
}
