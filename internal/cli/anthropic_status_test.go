package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	oauthdev "github.com/denn-gubsky/loomcycle/internal/providers/anthropic_oauth_dev"
)

func storeWithToken(t *testing.T, tok oauthdev.Token) *oauthdev.TokenStore {
	t.Helper()
	store := oauthdev.NewTokenStore(t.TempDir() + "/tokens.json")
	if err := store.Save(tok); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return store
}

// F6: --probe confirms server-side validity by refreshing; a 200 means the
// session is alive, and the probe rotates + persists the fresh token.
func TestAnthropicStatus_ProbeValid(t *testing.T) {
	store := storeWithToken(t, oauthdev.NewToken("old-at", "old-rt", "user:inference", 3600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600,"scope":"user:inference"}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := anthropicStatus(&out, &errb, store, true, oauthdev.ExchangeOptions{Endpoint: srv.URL})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "✓ valid") {
		t.Errorf("output missing ✓ valid:\n%s", out.String())
	}
	// The probe healed the store: the rotated token is persisted.
	if got, _ := store.Load(); got.AccessToken != "new-at" {
		t.Errorf("store not rotated: access token = %q, want new-at", got.AccessToken)
	}
}

// A dead session (the token endpoint rejects the refresh token) → ✗ INVALID + exit 1.
func TestAnthropicStatus_ProbeInvalid(t *testing.T) {
	store := storeWithToken(t, oauthdev.NewToken("old-at", "dead-rt", "user:inference", 3600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := anthropicStatus(&out, &errb, store, true, oauthdev.ExchangeOptions{Endpoint: srv.URL})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "✗ INVALID") || !strings.Contains(out.String(), "invalid_grant") {
		t.Errorf("output missing ✗ INVALID / invalid_grant:\n%s", out.String())
	}
}

// Plain status (no probe) must say it did NOT confirm server-side validity (F6).
func TestAnthropicStatus_NoProbe_NotChecked(t *testing.T) {
	store := storeWithToken(t, oauthdev.NewToken("at", "rt", "user:inference", 3600))
	var out, errb bytes.Buffer
	code := anthropicStatus(&out, &errb, store, false, oauthdev.ExchangeOptions{})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not checked") {
		t.Errorf("plain status should say server-side not checked:\n%s", out.String())
	}
}

// --probe with no refresh token in the store can't verify → exit 1.
func TestAnthropicStatus_ProbeNoRefreshToken(t *testing.T) {
	store := storeWithToken(t, oauthdev.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour), ObtainedAt: time.Now()})
	var out, errb bytes.Buffer
	code := anthropicStatus(&out, &errb, store, true, oauthdev.ExchangeOptions{})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no refresh token") {
		t.Errorf("output missing 'no refresh token':\n%s", out.String())
	}
}
