package http

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A password account whose last usable passkey goes while flagged ones stay
// would answer every password sign-in with second_factor_unavailable: the
// removal is refused (the flagged keys keep counting — no MFA downgrade).
func TestPasskey_TheLastUsablePasskeyStaysWhileFlaggedOnesRemain(t *testing.T) {
	f := newPasskeyFixture(t)
	b, _ := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	code, out := b.do("POST", "/v1/_auth/passkeys/register/begin", passwordStepUp)
	if code != http.StatusUnauthorized || out["code"] != "second_factor_required" {
		t.Fatalf("second registration step-up: %d %v", code, out)
	}
	code, offer := b.do("POST", "/v1/_auth/passkeys/register/begin", jsonBody(t, map[string]any{
		"current_password": goodPassword, "webauthn": a.get(out["webauthn"].(map[string]any))}))
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, offer)
	}
	second := newSoftAuthenticator(t, testRPID, testOrigin)
	if code, out := b.do("POST", "/v1/_auth/passkeys/register/finish", jsonBody(t, second.create(offer))); code != http.StatusCreated {
		t.Fatalf("register finish: %d %v", code, out)
	}
	if _, err := f.st.UserPasskeyFlag(context.Background(), second.credID, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	path := "/v1/_auth/passkeys/" + b64u.EncodeToString(a.credID)
	code, out = b.do("DELETE", path, passwordStepUp)
	if code != http.StatusUnauthorized {
		t.Fatalf("remove step-up: %d %v", code, out)
	}
	code, out = b.do("DELETE", path, jsonBody(t, map[string]any{"current_password": goodPassword, "webauthn": a.get(out["webauthn"].(map[string]any))}))
	if code != http.StatusConflict || out["code"] != "flagged_passkeys_remain" {
		t.Fatalf("removing the last usable passkey beside a flagged one: %d %v", code, out)
	}
	if code, _ := f.browser(t).login("local", "alice@example.com", goodPassword, ""); code == http.StatusForbidden {
		t.Fatal("the account is locked out of its password sign-in")
	}
}
