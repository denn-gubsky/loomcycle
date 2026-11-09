package http

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Removing a passkey ends the Web UI sessions signed in with it at once, and
// what such a session approved (an app sign-in code, a device login) is not
// honoured after the removal.

// sessionHashOf is the store key of the Web UI session b holds.
func sessionHashOf(t *testing.T, f *gateFixture, b *browser) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(sessionCookieOf(t, f, b).Value)
	if err != nil {
		t.Fatal(err)
	}
	return sessionHash(raw)
}

// addBoardClient configures the public app client "board".
func addBoardClient(t *testing.T, f *gateFixture) {
	t.Helper()
	cl, err := config.ResolveAppClient("test", "board", config.AuthClientConfig{Kind: "public", RedirectURIs: []string{appTestRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.cfg().ResolvedAuth.Clients = append(f.srv.cfg().ResolvedAuth.Clients, cl)
}

// approvedCode stores an app sign-in code for "board" approved by the
// session sessHash, as the authorize endpoint does, and returns the code.
func approvedCode(t *testing.T, f *gateFixture, code, sub, sessHash string) string {
	t.Helper()
	now := time.Now()
	if err := f.st.OAuthCodeCreate(context.Background(), store.OAuthCodeRow{CodeHash: hashOAuthSecret(code), ClientID: "board",
		RedirectURI: appTestRedirect, CodeChallenge: pkceChallenge(appTestVerifier), TenantID: "acme", Subject: sub,
		Gate: "keys", SessionHash: sessHash, ScopeCap: []string{auth.ScopeUser}, CreatedAt: now, ExpiresAt: now.Add(oauthCodeTTL)}); err != nil {
		t.Fatal(err)
	}
	return code
}

// passkeySessionCodes signs sub in with a, and returns that session's hash
// and a code it approved after checking that another code it approved is
// exchanged (so a later refusal is the passkey's, not the fixture's).
func passkeySessionCodes(t *testing.T, f *gateFixture, sub string, a *softAuthenticator) (string, string) {
	t.Helper()
	addBoardClient(t, f)
	p, code, out := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("passkey sign-in: %d %v", code, out)
	}
	hash := sessionHashOf(t, f, p)
	if status, body := exchangeCode(t, f, approvedCode(t, f, "code-control", sub, hash)); status != http.StatusOK {
		t.Fatalf("precondition: a code the passkey session approved is not exchanged: %d %s", status, body)
	}
	return hash, approvedCode(t, f, "code-after-removal", sub, hash)
}

func exchangeCode(t *testing.T, f *gateFixture, code string) (int, string) {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {appTestRedirect},
		"client_id": {"board"}, "code_verifier": {appTestVerifier}}
	r := httptest.NewRequest(http.MethodPost, "/v1/_oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.srv.handleOAuthToken(rec, r)
	return rec.Code, rec.Body.String()
}

// A session signed in with a passkey ends when the passkey is removed: it
// can no longer turn an app sign-in code it approved into a token, and its
// row is revoked rather than waiting for its next request.
func TestPasskeyRemoval_EndsTheSessionsSignedInWithIt(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "xia@example.com")
	a := registerPasskey(t, b)
	hash, c := passkeySessionCodes(t, f, sub, a)
	keys, _ := f.st.UserPasskeyList(context.Background(), "acme", sub)
	if len(keys) != 1 {
		t.Fatalf("precondition: %d passkeys", len(keys))
	}
	id := base64.RawURLEncoding.EncodeToString(keys[0].CredentialID)
	if code, out := stepUpWithPasskey(t, b, "DELETE", "/v1/_auth/passkeys/"+id, map[string]any{"current_password": goodPassword}, a); code != http.StatusNoContent {
		t.Fatalf("remove the passkey: %d %v", code, out)
	}
	if row, err := f.st.UISessionGet(context.Background(), hash); err != nil || row.RevokedAt.IsZero() {
		t.Errorf("the passkey's session after its removal: revoked=%v err=%v, want revoked", err == nil && !row.RevokedAt.IsZero(), err)
	}
	if status, body := exchangeCode(t, f, c); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("exchange of a code the removed passkey's session approved = %d %s, want invalid_grant", status, body)
	}
}

// The code exchange asks whether the passkey a session was made with is
// still the person's, so a code approved by that session is refused even if
// ending the session failed.
func TestOAuthCode_RefusedWhenTheApprovingSessionsPasskeyIsGone(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "wes@example.com")
	a := registerPasskey(t, b)
	_, c := passkeySessionCodes(t, f, sub, a)
	keys, _ := f.st.UserPasskeyList(context.Background(), "acme", sub)
	// Straight from the store: the session row stays live, as after a
	// removal whose session revoke faulted.
	if ok, err := f.st.UserPasskeyDelete(context.Background(), "acme", sub, keys[0].CredentialID); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if status, body := exchangeCode(t, f, c); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("exchange = %d %s, want invalid_grant", status, body)
	}
}

// sessionCookieOf is the Web UI session cookie b holds.
func sessionCookieOf(t *testing.T, f *gateFixture, b *browser) *http.Cookie {
	t.Helper()
	u, err := url.Parse(f.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.hc.Jar.Cookies(u) {
		if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil && len(raw) == sessionIDBytes {
			return c
		}
	}
	t.Fatal("the browser holds no session cookie")
	return nil
}

// A device login approved by a passkey session gets no token once that
// passkey is removed before the CLI polls.
func TestDeviceLogin_NoTokenWhenTheApprovingPasskeyWasRemovedBeforeThePoll(t *testing.T) {
	f := newPasskeyFixture(t)
	cfg := f.srv.cfg()
	cfg.ResolvedAuth.Gates = append(cfg.ResolvedAuth.Gates, config.AuthGate{Name: "cli", Kind: config.AuthGateKindDevice,
		Label: "cli", Device: &config.AuthDeviceGate{TokenTTL: 12 * time.Hour}})
	b, sub := f.passwordUser(t, "vic@example.com")
	a := registerPasskey(t, b)
	p, code, out := passkeySignIn(t, f, a.get)
	if code != http.StatusNoContent {
		t.Fatalf("passkey sign-in: %d %v", code, out)
	}
	c := sessionCookieOf(t, f, p)
	if got := approveAndRedeem(t, f.srv, c, nil); got.AccessToken == "" {
		t.Fatalf("precondition: a device login the passkey session approved got no token: %d %q", got.Status, got.Error)
	}
	keys, _ := f.st.UserPasskeyList(context.Background(), "acme", sub)
	id := base64.RawURLEncoding.EncodeToString(keys[0].CredentialID)
	got := approveAndRedeem(t, f.srv, c, func() {
		if code, out := stepUpWithPasskey(t, b, "DELETE", "/v1/_auth/passkeys/"+id, map[string]any{"current_password": goodPassword}, a); code != http.StatusNoContent {
			t.Fatalf("remove the passkey: %d %v", code, out)
		}
	})
	if got.AccessToken != "" {
		t.Fatalf("redeem after the approving passkey was removed = %d %q, want no token", got.Status, got.Error)
	}
}
