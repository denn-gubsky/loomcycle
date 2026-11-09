package http

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A password reset removes the passkeys added since the password was last
// set: someone who knew the password could add one (a passkey step-up asks
// for the current password), and it must not outlive the reset. Passkeys
// from before the last password change stay.

// stepUpWithPasskey sends body to path with method; when the account asks for its second
// factor, it answers with a and sends again.
func stepUpWithPasskey(t *testing.T, b *browser, method, path string, body map[string]any, a *softAuthenticator) (int, map[string]any) {
	t.Helper()
	code, out := b.do(method, path, jsonBody(t, body))
	if code != http.StatusUnauthorized || out["code"] != "second_factor_required" {
		return code, out
	}
	with := map[string]any{"webauthn": a.get(out["webauthn"].(map[string]any))}
	for k, v := range body {
		with[k] = v
	}
	return b.do(method, path, jsonBody(t, with))
}

// registerAnotherPasskey registers a second passkey named name from b's
// session, stepping up with the account's password pass and its passkey a.
func registerAnotherPasskey(t *testing.T, b *browser, pass, name string, a *softAuthenticator) *softAuthenticator {
	t.Helper()
	code, offer := stepUpWithPasskey(t, b, "POST", "/v1/_auth/passkeys/register/begin", map[string]any{"current_password": pass}, a)
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, offer)
	}
	n := newSoftAuthenticator(t, testRPID, testOrigin)
	body := n.create(offer)
	body["name"] = name
	if code, out := b.do("POST", "/v1/_auth/passkeys/register/finish", jsonBody(t, body)); code != http.StatusCreated {
		t.Fatalf("register finish: %d %v", code, out)
	}
	return n
}

func passkeyNames(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := []string{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		out = append(out, name)
	}
	return out
}

func TestPasswordReset_RemovesPasskeysAddedSinceThePasswordWasSet(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "zoe@example.com")
	ctx := context.Background()
	old := registerPasskey(t, b) // "laptop", before the password below
	const changed = "zoe's changed passphrase, set after the laptop"
	if code, out := stepUpWithPasskey(t, b, "POST", "/v1/_auth/password",
		map[string]any{"current_password": goodPassword, "new_password": changed}, old); code != http.StatusNoContent {
		t.Fatalf("change password: %d %v", code, out)
	}
	registerAnotherPasskey(t, b, changed, "intruder", old) // after it

	rst := f.resetCode(t, bAcmeOp, "local", jsonBody(t, map[string]string{"subject": sub}))
	fresh := f.browser(t)
	const newPass = "a reset passphrase for zoe"
	_, out := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]any{"code": rst, "password": newPass}))
	code, body := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]any{"code": rst, "password": newPass,
		"webauthn": old.get(out["webauthn"].(map[string]any))}))
	if code != http.StatusOK {
		t.Fatalf("reset: %d %v", code, body)
	}

	rows, err := f.st.UserPasskeyList(ctx, "acme", sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "laptop" {
		names := []string{}
		for _, r := range rows {
			names = append(names, r.Name)
		}
		t.Fatalf("passkeys after the reset = %v, want only the laptop added before the last password change", names)
	}
	if got := passkeyNames(t, body["passkeys"]); len(got) != 1 || got[0] != "laptop" {
		t.Errorf("the reset answer's kept passkeys = %v, want [laptop]", got)
	}
	if got := passkeyNames(t, body["removed_passkeys"]); len(got) != 1 || got[0] != "intruder" {
		t.Errorf("the reset answer's removed passkeys = %v, want [intruder]", got)
	}
	if strings.Contains(jsonBody(t, body), base64.RawURLEncoding.EncodeToString(rows[0].CredentialID)) {
		t.Error("the reset answer carries a credential id")
	}
	removed := 0
	f.audit.mu.Lock()
	evs := append([]audit.Event(nil), f.audit.evs...)
	f.audit.mu.Unlock()
	for _, ev := range evs {
		if ev.Action == "auth_passkey_removed" && ev.TargetSubject == sub && ev.AuthReason == "password_reset" {
			removed++
		}
	}
	if removed != 1 {
		t.Errorf("audit records %d passkey removals by the reset, want 1", removed)
	}
}

// An account whose password was never stored keeps its passkeys: none was
// added under a password someone else may know.
func TestPasswordReset_KeepsEveryPasskeyWhenNoPasswordWasSet(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "yan@example.com")
	a := registerPasskey(t, b)
	ctx := context.Background()
	if _, err := f.st.UserCredentialDelete(ctx, "acme", sub, store.CredentialKindPassword); err != nil {
		t.Fatal(err)
	}
	rst := f.resetCode(t, bAcmeOp, "local", jsonBody(t, map[string]string{"subject": sub}))
	fresh := f.browser(t)
	const newPass = "a first passphrase for yan"
	_, out := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]any{"code": rst, "password": newPass}))
	if out["webauthn"] == nil {
		t.Fatalf("precondition: the reset did not ask for the passkey: %v", out)
	}
	code, body := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]any{"code": rst, "password": newPass,
		"webauthn": a.get(out["webauthn"].(map[string]any))}))
	if code != http.StatusOK {
		t.Fatalf("reset: %d %v", code, body)
	}
	if rows, _ := f.st.UserPasskeyList(ctx, "acme", sub); len(rows) != 1 {
		t.Fatalf("passkeys after the reset = %d, want the one kept", len(rows))
	}
	if got := passkeyNames(t, body["removed_passkeys"]); len(got) != 0 {
		t.Errorf("removed passkeys = %v, want none", got)
	}
}
