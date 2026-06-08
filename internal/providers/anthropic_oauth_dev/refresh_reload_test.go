package anthropic_oauth_dev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingTokenServer returns a fresh token and counts how many refresh POSTs
// it received — so a test can assert whether refreshLocked hit the network.
func countingTokenServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"POSTED","refresh_token":"POSTED-rt","expires_in":3600,"scope":"user:inference"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// F7: when another process has already rotated the on-disk token (a NEWER
// ObtainedAt) while we held the cross-process lock, refreshLocked must ADOPT it
// and skip the network refresh — POSTing now would use a refresh token the peer
// already invalidated. On the unfixed code this POSTs unconditionally.
func TestRefresh_AdoptsNewerOnDiskTokenWithoutPost(t *testing.T) {
	store := NewTokenStore(t.TempDir() + "/tokens.json")
	// Our process loaded a stale (needs-refresh) token at startup.
	stale := Token{AccessToken: "stale-at", RefreshToken: "stale-rt",
		ObtainedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)}
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}
	refresher := NewRefresher(store, ExchangeOptions{}, func(string, ...any) {})

	// Another process rotated the file to a NEWER token after we loaded.
	newer := NewToken("newer-at", "newer-rt", "user:inference", 3600)
	if err := store.Save(newer); err != nil {
		t.Fatalf("Save newer: %v", err)
	}

	var hits atomic.Int32
	refresher.httpClient = ExchangeOptions{Endpoint: countingTokenServer(t, &hits).URL}

	if err := refresher.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("expected NO refresh POST (adopt the peer's newer token), got %d", n)
	}
	if got := refresher.Token().AccessToken; got != "newer-at" {
		t.Errorf("Token = %q, want the adopted on-disk token %q", got, "newer-at")
	}
}

// Counterpart: when the on-disk token is NOT newer than ours (no peer
// refreshed), refreshLocked still performs the network refresh — the adopt
// path must not suppress a genuine refresh.
func TestRefresh_PostsWhenNoNewerOnDiskToken(t *testing.T) {
	store := NewTokenStore(t.TempDir() + "/tokens.json")
	stale := Token{AccessToken: "stale-at", RefreshToken: "stale-rt",
		ObtainedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)}
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save: %v", err)
	}
	refresher := NewRefresher(store, ExchangeOptions{}, func(string, ...any) {})

	var hits atomic.Int32
	refresher.httpClient = ExchangeOptions{Endpoint: countingTokenServer(t, &hits).URL}

	if err := refresher.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected exactly 1 refresh POST, got %d", n)
	}
	if got := refresher.Token().AccessToken; got != "POSTED" {
		t.Errorf("Token = %q, want POSTED", got)
	}
}
