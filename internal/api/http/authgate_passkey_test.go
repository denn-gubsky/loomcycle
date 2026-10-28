package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P6 passkeys. The fixture is the password-gate fixture plus a
// relying party (https://lc.example) and a passkey gate "keys"; the
// software authenticator answers offers like a browser would, so the
// server's go-webauthn verification runs for real.

const (
	testRPID   = "lc.example"
	testOrigin = "https://lc.example"
)

func newPasskeyFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := newGateFixture(t)
	cfg := f.srv.cfg()
	cfg.ResolvedAuth.Passkeys = &config.AuthPasskeyRP{ID: testRPID, Origin: testOrigin}
	cfg.ResolvedAuth.Gates = append(cfg.ResolvedAuth.Gates, config.AuthGate{Name: "keys", Kind: config.AuthGateKindPasskey, Label: "Passkey"})
	return f
}

// passwordUser signs a new person up on the local password gate and returns
// their browser (a fresh password session) and subject.
func (f *gateFixture) passwordUser(t *testing.T, login string) (*browser, string) {
	t.Helper()
	b := f.redeemed(t, "local", login, f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, sub := b.me()
	return b, sub
}

// registerPasskey registers a software passkey from b's session.
func registerPasskey(t *testing.T, b *browser) *softAuthenticator {
	t.Helper()
	code, offer := b.do("POST", "/v1/_auth/passkeys/register/begin", `{}`)
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, offer)
	}
	a := newSoftAuthenticator(t, testRPID, testOrigin)
	body := a.create(offer)
	body["name"] = "laptop"
	if code, out := b.do("POST", "/v1/_auth/passkeys/register/finish", jsonBody(t, body)); code != http.StatusCreated {
		t.Fatalf("register finish: %d %v", code, out)
	}
	return a
}

// passkeySignIn runs a password-less sign-in on a fresh browser with the
// answer a gives (answer may tamper with it) and returns the browser and the
// completion status.
func passkeySignIn(t *testing.T, f *gateFixture, answer func(offer map[string]any) map[string]any) (*browser, int, map[string]any) {
	t.Helper()
	b := f.browser(t)
	code, offer := b.do("POST", "/v1/_auth/passkey/keys/begin", `{}`)
	if code != http.StatusOK {
		t.Fatalf("login begin: %d %v", code, offer)
	}
	code, out := b.do("POST", "/v1/_auth/complete/keys", jsonBody(t, answer(offer)))
	return b, code, out
}

func TestPasskey_RegisteredFromAPasswordSessionSignsInWithoutAPassword(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	code, list := b.do("GET", "/v1/_auth/passkeys", "")
	keys, _ := list["passkeys"].([]any)
	if code != http.StatusOK || len(keys) != 1 || list["passwordless"] != true {
		t.Fatalf("list: %d %v", code, list)
	}
	if k := keys[0].(map[string]any); k["name"] != "laptop" || k["discoverable"] != true || k["public_key"] != nil {
		t.Fatalf("listed passkey: %v", k)
	}
	p, code, out := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("passkey sign-in: %d %v", code, out)
	}
	if code, got := p.me(); code != http.StatusOK || got != sub {
		t.Fatalf("/v1/_me after passkey sign-in: %d %q, want %q", code, got, sub)
	}
	rows, _ := f.st.UserPasskeyList(context.Background(), "acme", sub)
	if len(rows) != 1 || rows[0].SignCount != 1 || rows[0].LastUsedAt.IsZero() {
		t.Fatalf("stored passkey after use: %+v", rows)
	}
}

// A ceremony is answered once: replaying a valid answer is refused.
func TestPasskey_ACeremonyIsAnsweredOnce(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	p := f.browser(t)
	_, offer := p.do("POST", "/v1/_auth/passkey/keys/begin", `{}`)
	answer := jsonBody(t, a.get(offer))
	if code, out := p.do("POST", "/v1/_auth/complete/keys", answer); code != http.StatusNoContent {
		t.Fatalf("first answer: %d %v", code, out)
	}
	if code, _ := p.do("POST", "/v1/_auth/complete/keys", answer); code != http.StatusUnauthorized {
		t.Fatalf("replayed answer: %d, want 401", code)
	}
}

// The browser that began a sign-in is the only one that can finish it.
func TestPasskey_ASignInCeremonyIsBoundToTheBrowserThatBeganIt(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	began := f.browser(t)
	_, offer := began.do("POST", "/v1/_auth/passkey/keys/begin", `{}`)
	other := f.browser(t)
	if code, _ := other.do("POST", "/v1/_auth/complete/keys", jsonBody(t, a.get(offer))); code != http.StatusUnauthorized {
		t.Fatalf("another browser finished the ceremony: %d", code)
	}
}

// The relying party's origin and RP ID are checked: an assertion collected
// on another site, or for another RP ID, does not sign in.
func TestPasskey_AnAssertionForAnotherOriginOrRPIDIsRefused(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	for name, answer := range map[string]func(map[string]any) map[string]any{
		"origin": func(o map[string]any) map[string]any { return a.getWith(o, 7, "https://evil.example", testRPID) },
		"rp id":  func(o map[string]any) map[string]any { return a.getWith(o, 8, testOrigin, "evil.example") },
	} {
		if _, code, out := passkeySignIn(t, f, answer); code != http.StatusUnauthorized || out["code"] != "invalid_credentials" {
			t.Errorf("%s: %d %v, want 401 invalid_credentials", name, code, out)
		}
	}
}

// A password-less sign-in needs user verification: an authenticator that
// only proves presence does not sign anyone in.
func TestPasskey_APasswordlessSignInNeedsUserVerification(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	a.uv = false
	if _, code, _ := passkeySignIn(t, f, a.get); code != http.StatusUnauthorized {
		t.Fatalf("sign-in without user verification: %d, want 401", code)
	}
}

// A counter that does not move forward is a cloned authenticator: the
// sign-in is refused and the passkey flagged for good.
func TestPasskey_ASignCountThatGoesBackFlagsThePasskey(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	if _, code, out := passkeySignIn(t, f, func(o map[string]any) map[string]any { return a.getWith(o, 5, testOrigin, testRPID) }); code != http.StatusNoContent {
		t.Fatalf("count 5: %d %v", code, out)
	}
	if _, code, _ := passkeySignIn(t, f, func(o map[string]any) map[string]any { return a.getWith(o, 3, testOrigin, testRPID) }); code != http.StatusUnauthorized {
		t.Fatalf("count 3 after 5: %d, want 401", code)
	}
	rows, _ := f.st.UserPasskeyList(context.Background(), "acme", sub)
	if len(rows) != 1 || rows[0].FlaggedAt.IsZero() || rows[0].FlagReason != "sign_count_regressed" {
		t.Fatalf("the passkey was not flagged: %+v", rows)
	}
	if _, code, _ := passkeySignIn(t, f, func(o map[string]any) map[string]any { return a.getWith(o, 50, testOrigin, testRPID) }); code != http.StatusUnauthorized {
		t.Fatalf("a flagged passkey signed in: %d", code)
	}
}

// An unknown passkey and a known one with a bad signature are refused with
// the same answer: no credential enumeration.
func TestPasskey_RefusalsDoNotSayWhichPasskeyOrWhy(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	stranger := newSoftAuthenticator(t, testRPID, testOrigin)
	stranger.handle = a.handle
	_, c1, unknown := passkeySignIn(t, f, stranger.get)
	forged := newSoftAuthenticator(t, testRPID, testOrigin)
	forged.credID, forged.handle = a.credID, a.handle // right id, wrong key
	_, c2, badSig := passkeySignIn(t, f, forged.get)
	if c1 != http.StatusUnauthorized || c2 != http.StatusUnauthorized || unknown["code"] != badSig["code"] || unknown["error"] != badSig["error"] {
		t.Fatalf("unknown = %d %v, bad signature = %d %v", c1, unknown, c2, badSig)
	}
}

// Registering needs the person's own session — not a bearer, not a pasted
// token's session — and a recent sign-in.
func TestPasskey_RegistrationNeedsAUserSessionAndARecentSignIn(t *testing.T) {
	f := newPasskeyFixture(t)
	if code, out := f.call(t, bAcmeOp, "POST", "/v1/_auth/passkeys/register/begin", `{}`); code != http.StatusForbidden || out["code"] != "session_required" {
		t.Fatalf("a bearer: %d %v", code, out)
	}
	tb := f.browser(t)
	f.addUser(t, "acme", "tok-user", "isolated")
	seedToken(t, f.st, "tok-user-bearer", "acme", "tok-user", []string{"runs:create", "runs:read"}, time.Time{})
	req, _ := http.NewRequest("POST", f.ts.URL+"/ui/session", nil)
	req.Header.Set("Authorization", "Bearer tok-user-bearer")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", testUA)
	resp, err := tb.hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("token sign-in: %v %v", resp, err)
	}
	resp.Body.Close()
	if code, out := tb.do("POST", "/v1/_auth/passkeys/register/begin", `{}`); code != http.StatusForbidden || out["code"] != "passkey_needs_user_session" {
		t.Fatalf("a pasted token's session: %d %v", code, out)
	}
	b, _ := f.passwordUser(t, "alice@example.com")
	f.srv.cfg().ResolvedAuth.ReauthWindow = time.Nanosecond
	time.Sleep(time.Millisecond)
	if code, out := b.do("POST", "/v1/_auth/passkeys/register/begin", `{}`); code != http.StatusForbidden || out["code"] != "reauth_required" {
		t.Fatalf("a stale sign-in: %d %v", code, out)
	}
}

// A registration ceremony belongs to the session that began it.
func TestPasskey_ARegistrationCeremonyIsBoundToItsSession(t *testing.T) {
	f := newPasskeyFixture(t)
	alice, _ := f.passwordUser(t, "alice@example.com")
	bob, bobSub := f.passwordUser(t, "bob@example.com")
	_, offer := alice.do("POST", "/v1/_auth/passkeys/register/begin", `{}`)
	a := newSoftAuthenticator(t, testRPID, testOrigin)
	body := a.create(offer)
	if code, out := bob.do("POST", "/v1/_auth/passkeys/register/finish", jsonBody(t, body)); code != http.StatusBadRequest {
		t.Fatalf("bob finished alice's ceremony: %d %v", code, out)
	}
	if rows, _ := f.st.UserPasskeyList(context.Background(), "acme", bobSub); len(rows) != 0 {
		t.Fatalf("a passkey reached bob: %+v", rows)
	}
}

// The passkey endpoints refuse a cross-site request (CSRF).
func TestPasskey_CrossSiteRequestsAreRefused(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	for _, path := range []string{"/v1/_auth/passkey/keys/begin", "/v1/_auth/passkeys/register/begin", "/v1/_auth/complete/keys"} {
		req, _ := http.NewRequest("POST", f.ts.URL+path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set(uiRequestHeader, "1")
		req.Header.Set("User-Agent", testUA)
		resp, err := b.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s cross-site: %d, want 403", path, resp.StatusCode)
		}
	}
}

// At a password gate a passkey is a second factor: the password alone gets
// a passkey offer, the answer signs in, and a wrong one is refused.
func TestPasskey_IsASecondFactorAtThePasswordGate(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	nb := f.browser(t)
	code, out := nb.login("local", "alice@example.com", goodPassword, "")
	if code != http.StatusUnauthorized || out["code"] != "second_factor_required" || out["webauthn"] == nil {
		t.Fatalf("password alone: %d %v", code, out)
	}
	offer := out["webauthn"].(map[string]any)
	wrong := newSoftAuthenticator(t, testRPID, testOrigin)
	wrong.credID = a.credID
	bad := map[string]any{"login": "alice@example.com", "password": goodPassword, "webauthn": wrong.get(offer)}
	if code, out := nb.do("POST", "/v1/_auth/complete/local", jsonBody(t, bad)); code != http.StatusUnauthorized || out["code"] != "invalid_otp" {
		t.Fatalf("a wrong passkey answer: %d %v", code, out)
	}
	_, out = nb.login("local", "alice@example.com", goodPassword, "")
	offer = out["webauthn"].(map[string]any)
	good := map[string]any{"login": "alice@example.com", "password": goodPassword, "webauthn": a.get(offer)}
	if code, out := nb.do("POST", "/v1/_auth/complete/local", jsonBody(t, good)); code != http.StatusNoContent {
		t.Fatalf("password + passkey: %d %v", code, out)
	}
	if code, got := nb.me(); code != http.StatusOK || got != sub {
		t.Fatalf("/v1/_me: %d %q", code, got)
	}
	// A second-factor ceremony cannot finish a password-less sign-in.
	_, out = f.browser(t).login("local", "alice@example.com", goodPassword, "")
	stolen := out["webauthn"].(map[string]any)
	p := f.browser(t)
	p.do("POST", "/v1/_auth/passkey/keys/begin", `{}`)
	if code, _ := p.do("POST", "/v1/_auth/complete/keys", jsonBody(t, a.get(stolen))); code != http.StatusUnauthorized {
		t.Fatalf("a second-factor ceremony signed in password-less: %d", code)
	}
}

// Removing a passkey ends the sessions made with it.
func TestPasskey_RemovingAPasskeyEndsItsSessions(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	p, code, _ := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("sign-in: %d", code)
	}
	if code, _ := p.me(); code != http.StatusOK {
		t.Fatalf("passkey session: %d", code)
	}
	if code, out := b.do("DELETE", "/v1/_auth/passkeys/"+b64u.EncodeToString(a.credID), ""); code != http.StatusNoContent {
		t.Fatalf("remove: %d %v", code, out)
	}
	if code, _ := p.me(); code != http.StatusUnauthorized {
		t.Fatalf("the passkey's session outlived it: %d", code)
	}
}

// The last way into an account is not removed.
func TestPasskey_TheLastWayInIsNotRemoved(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	ctx := context.Background()
	if _, err := f.st.UserCredentialDelete(ctx, "acme", sub, store.CredentialKindPassword); err != nil {
		t.Fatal(err)
	}
	ids, _ := f.st.UserIdentityListSubject(ctx, "acme", sub)
	for _, id := range ids {
		if _, err := f.st.UserIdentityDelete(ctx, "acme", sub, id.Gate); err != nil {
			t.Fatal(err)
		}
	}
	// The password session ended with its password; sign in by passkey.
	p, code, _ := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("sign-in: %d", code)
	}
	if code, out := p.do("DELETE", "/v1/_auth/passkeys/"+b64u.EncodeToString(a.credID), ""); code != http.StatusConflict || out["code"] != "last_sign_in_method" {
		t.Fatalf("removing the last way in: %d %v", code, out)
	}
}

// An operator removing a user's credentials (a lost authenticator) removes
// the passkeys too, and the sessions made with them end.
func TestPasskey_AnOperatorsCredentialRemovalTakesThePasskeys(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	p, code, _ := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("sign-in: %d", code)
	}
	if code, out := f.call(t, bAcmeOp, "DELETE", "/v1/_authgate/local/credentials/"+sub, ""); code != http.StatusNoContent {
		t.Fatalf("remove credentials: %d %v", code, out)
	}
	if rows, _ := f.st.UserPasskeyList(context.Background(), "acme", sub); len(rows) != 0 {
		t.Fatalf("passkeys survived: %+v", rows)
	}
	if code, _ := p.me(); code != http.StatusUnauthorized {
		t.Fatalf("the passkey session survived: %d", code)
	}
}

// Another user's passkey is neither renamed nor removed through its id.
func TestPasskey_AnotherUsersPasskeyIsOutOfReach(t *testing.T) {
	f := newPasskeyFixture(t)
	alice, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, alice)
	bob, _ := f.passwordUser(t, "bob@example.com")
	id := b64u.EncodeToString(a.credID)
	if code, _ := bob.do("PATCH", "/v1/_auth/passkeys/"+id, `{"name":"mine"}`); code != http.StatusNotFound {
		t.Fatalf("bob renamed alice's passkey: %d", code)
	}
	if code, _ := bob.do("DELETE", "/v1/_auth/passkeys/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("bob removed alice's passkey: %d", code)
	}
}
