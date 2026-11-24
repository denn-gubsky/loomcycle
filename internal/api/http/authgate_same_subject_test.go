package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// sameSubjectGates is RFC HD's pairing: "entra" makes the users (here an
// external gate standing in for the OIDC one; the resolver reads only its
// identity map and its user_identities rows), and "teams", trusted, links a
// first login to the user "entra:<value>".
func sameSubjectGates(t *testing.T, teamsMap func(*config.AuthIdentityMap)) (entra, teams *extGate, f *gateFixture) {
	t.Helper()
	entra = newExtGate(t, "entra", acmeMap(), config.AuthExternalGate{})
	m := acmeMap()
	m.Provision = config.ProvisionKnown
	m.Trusted = true
	m.SameSubjectAs = "entra"
	if teamsMap != nil {
		teamsMap(&m)
	}
	teams = newExtGate(t, "teams", m, config.AuthExternalGate{})
	return entra, teams, extFixture(t, entra, teams)
}

func lastAudit(f *gateFixture, action string) (audit.Event, bool) {
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	for i := len(f.audit.evs) - 1; i >= 0; i-- {
		if f.audit.evs[i].Action == action {
			return f.audit.evs[i], true
		}
	}
	return audit.Event{}, false
}

func asSub(sub string) func(*jwt.Claims) { return func(c *jwt.Claims) { c.Subject = sub } }

func TestSameSubjectAs_AFirstLoginLinksToTheUserTheNamedGateMade(t *testing.T) {
	entra, teams, f := sameSubjectGates(t, nil)
	ctx := context.Background()
	if code, body := f.assert(t, "entra", entra.sign(t, "assert", nil, asSub("abc"))); code != http.StatusOK || body["subject"] != "entra:abc" {
		t.Fatalf("entra login: %d %v", code, body)
	}
	code, body := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("abc")))
	if code != http.StatusOK || body["subject"] != "entra:abc" {
		t.Fatalf("first teams login: %d %v, want subject entra:abc", code, body)
	}
	ev, ok := lastAudit(f, "auth_identity_linked")
	if !ok || ev.AuthGate != "teams" || ev.AuthReason != "same subject" || ev.TargetSubject != "entra:abc" {
		t.Fatalf("link audit = %+v (found %v)", ev, ok)
	}
	if _, err := f.st.UserIdentityGet(ctx, "teams", "", "abc"); err != nil {
		t.Fatalf("no teams identity row: %v", err)
	}
	if _, err := f.st.UserGet(ctx, "acme", "teams:abc"); !isNotFound(err) {
		t.Fatalf("a separate teams user was made: %v", err)
	}
	// The second login answers from the identity row.
	if code, body := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("abc"))); code != http.StatusOK || body["subject"] != "entra:abc" {
		t.Fatalf("second teams login: %d %v", code, body)
	}
}

func TestSameSubjectAs_WithNoSuchUserProvisionKnownStillRefuses(t *testing.T) {
	_, teams, f := sameSubjectGates(t, nil)
	if code, _ := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("nobody"))); code != http.StatusUnauthorized {
		t.Fatalf("unknown identity: %d, want 401", code)
	}
}

// D2: a user an operator made under the named gate's name, never signed in
// through it, is not linked; the login goes on as if same_subject_as were
// unset.
func TestSameSubjectAs_AUserThatNeverSignedInThroughTheNamedGateIsNotLinked(t *testing.T) {
	_, teams, f := sameSubjectGates(t, nil)
	ctx := context.Background()
	if err := f.st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "entra:abc", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("abc"))); code != http.StatusUnauthorized {
		t.Fatalf("linked to a user with no entra identity: %d, want 401", code)
	}
}

func TestSameSubjectAs_ADisabledUserIsRefusedAndStaysDisabled(t *testing.T) {
	entra, teams, f := sameSubjectGates(t, func(m *config.AuthIdentityMap) { m.Provision = config.ProvisionAuto })
	ctx := context.Background()
	f.assert(t, "entra", entra.sign(t, "assert", nil, asSub("abc")))
	disabled := "disabled"
	if err := f.st.UserUpdate(ctx, "acme", "entra:abc", store.UserPatch{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("abc"))); code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's teams login: %d, want 401", code)
	}
	if u, _ := f.st.UserGet(ctx, "acme", "entra:abc"); u.Status != "disabled" {
		t.Fatalf("status %q, want disabled", u.Status)
	}
	// Even with provision: auto, no teams:abc is made to get round it.
	if _, err := f.st.UserGet(ctx, "acme", "teams:abc"); !isNotFound(err) {
		t.Fatalf("a separate teams user was made: %v", err)
	}
}

// D1: an uninstall (revoke) from teams ends the teams identity and the tokens
// teams issued; the user, who still signs in through entra, stays active.
func TestExternalRevoke_AUserWithAnotherSignInLosesOnlyThisGate(t *testing.T) {
	entra, teams, f := sameSubjectGates(t, nil)
	ctx := context.Background()
	_, eb := f.assert(t, "entra", entra.sign(t, "assert", nil, asSub("abc")))
	_, tb := f.assert(t, "teams", teams.sign(t, "assert", nil, asSub("abc")))
	entraTok, teamsTok := eb["token"].(string), tb["token"].(string)

	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "teams", "request": teams.sign(t, "revoke", nil, asSub("abc"))}); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if u, _ := f.st.UserGet(ctx, "acme", "entra:abc"); u.Status != "active" {
		t.Fatalf("user status %q, want active", u.Status)
	}
	if _, err := f.st.AuthRemovalGet(ctx, "acme", "entra:abc"); !isNotFound(err) {
		t.Fatalf("a removal was started: %v", err)
	}
	if _, err := f.st.UserIdentityGet(ctx, "teams", "", "abc"); !isNotFound(err) {
		t.Fatalf("the teams identity survived: %v", err)
	}
	if _, err := f.st.UserIdentityGet(ctx, "entra", "", "abc"); err != nil {
		t.Fatalf("the entra identity went too: %v", err)
	}
	if code, _ := f.call(t, teamsTok, "GET", "/v1/_me", ""); code != http.StatusUnauthorized {
		t.Fatalf("the teams token survived: %d", code)
	}
	if code, _ := f.call(t, entraTok, "GET", "/v1/_me", ""); code != http.StatusOK {
		t.Fatalf("the entra token was ended: %d", code)
	}
	if ev, ok := lastAudit(f, "auth_identity_unlinked"); !ok || ev.AuthGate != "teams" || ev.AuthReason != store.RemovalReasonRevoked {
		t.Fatalf("unlink audit = %+v (found %v)", ev, ok)
	}
}

func TestExternalSync_ACompleteSyncUnlinksAUserWithAnotherSignIn(t *testing.T) {
	entra, teams, f := sameSubjectGates(t, func(m *config.AuthIdentityMap) { m.AuthoritativeForUsers = true })
	ctx := context.Background()
	for _, sub := range []string{"a", "b"} {
		f.assert(t, "entra", entra.sign(t, "assert", nil, asSub(sub)))
		f.assert(t, "teams", teams.sign(t, "assert", nil, asSub(sub)))
	}
	req := teams.sign(t, "sync", map[string]any{"users": []map[string]any{{"sub": "a"}}, "complete": true}, asSub(""))
	if code, body := f.post(t, "/v1/_auth/sync", map[string]string{"gate": "teams", "request": req}); code != http.StatusOK || body["removed"] != float64(1) {
		t.Fatalf("sync: %d %v", code, body)
	}
	if u, _ := f.st.UserGet(ctx, "acme", "entra:b"); u.Status != "active" {
		t.Fatalf("entra:b is %q, want active", u.Status)
	}
	if _, err := f.st.UserIdentityGet(ctx, "teams", "", "b"); !isNotFound(err) {
		t.Fatalf("b's teams identity survived: %v", err)
	}
	if _, err := f.st.UserIdentityGet(ctx, "teams", "", "a"); err != nil {
		t.Fatalf("a's teams identity went: %v", err)
	}
}
