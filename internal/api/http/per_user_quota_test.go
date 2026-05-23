package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestPerUserQuota_Refuses5thRunFromSameUser is the load-bearing
// integration test: with the per-user cap set to 2 and two in-flight
// runs from user_a (held open by the pausable provider), a third run
// from user_a returns 429 with `code: "per_user_quota_exhausted"` +
// `Retry-After: 5`. A concurrent run from user_b is accepted (the cap
// is per-user, not shared).
func TestPerUserQuota_Refuses5thRunFromSameUser(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := makeBaseConfig()
	dbPath := filepath.Join(t.TempDir(), "peruser.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Per-user cap = 2; global cap = 8 so the queue doesn't gate first.
	sem := concurrency.New(8, 8, time.Second).WithPerUserCap(2)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Fire 2 streaming runs as user_a. Both go through; the pausable
	// provider keeps them open so the per-user counter stays at 2.
	body := func(userID string) string {
		return `{"agent":"default","user_id":"` + userID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body("user_a")))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	// Poll until both goroutines have acquired their semaphore slot —
	// poll-until-condition replaces a fixed sleep so the test doesn't
	// flake under `-race` (which adds 2-5x scheduling overhead).
	// Same posture as the heartbeat-sweeper flake fix from PR #190.
	waitForActive(t, sem, 2, 2*time.Second)

	// Third run by user_a: expect 429 + per_user_quota_exhausted.
	resp3, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body("user_a")))
	if err != nil {
		t.Fatalf("third post: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp3.StatusCode)
	}
	if got := resp3.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
	respBody, _ := io.ReadAll(resp3.Body)
	var parsed struct {
		Code   string `json:"code"`
		UserID string `json:"user_id"`
		Cap    int    `json:"cap"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("body decode: %v; body=%s", err, respBody)
	}
	if parsed.Code != "per_user_quota_exhausted" {
		t.Errorf("code = %q, want per_user_quota_exhausted", parsed.Code)
	}
	if parsed.UserID != "user_a" || parsed.Cap != 2 {
		t.Errorf("body fields user_id=%q cap=%d, want user_a / 2", parsed.UserID, parsed.Cap)
	}

	// Concurrent run by user_b is unaffected — independent quota.
	// Fire as a goroutine so its blocking SSE drain doesn't deadlock
	// the test (it'll unblock when prov.release closes below).
	user_b_done := make(chan int, 1)
	go func() {
		resp4, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body("user_b")))
		if err != nil {
			user_b_done <- -1
			return
		}
		defer resp4.Body.Close()
		_, _ = io.Copy(io.Discard, resp4.Body)
		user_b_done <- resp4.StatusCode
	}()
	// Wait for user_b to acquire its slot before releasing the
	// providers — confirms the per-user cap on user_a didn't gate
	// user_b. Without this, user_b's post might still be in flight
	// when we close prov.release.
	waitForActive(t, sem, 3, 2*time.Second)

	// Release the held providers so all 3 in-flight runs can finish.
	close(prov.release)
	wg.Wait()
	if status := <-user_b_done; status != http.StatusOK {
		t.Errorf("user_b status = %d, want 200", status)
	}
}

// TestPerUserQuota_Disabled_NoBehaviorChange — when MaxConcurrentRunsPerUser=0
// (the default), the per-user check is fully bypassed. Many runs by
// the same user succeed up to the global cap. Back-compat guarantee.
func TestPerUserQuota_Disabled_NoBehaviorChange(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := makeBaseConfig()
	dbPath := filepath.Join(t.TempDir(), "noperuser.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// No WithPerUserCap call — default 0 = disabled.
	sem := concurrency.New(8, 8, time.Second)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"agent":"default","user_id":"user_a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`

	// Fire 4 runs by the same user — all should succeed (well under
	// the global cap of 8; per-user cap is 0 = no check).
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (per-user check should be disabled)", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	// Wait for all 4 to acquire — poll-until rather than fixed sleep
	// so -race doesn't flake.
	waitForActive(t, sem, 4, 2*time.Second)
	close(prov.release)
	wg.Wait()
}

// waitForActive polls the semaphore until active reaches `want` or
// the deadline elapses. Used in place of fixed `time.Sleep` for
// inter-goroutine synchronization — fixed sleeps flake under -race
// where scheduling overhead can stretch a 50ms wait past the
// "everything should be settled by now" assumption. Same pattern as
// the PR #190 heartbeat-sweeper flake fix.
func waitForActive(t *testing.T, sem *concurrency.Semaphore, want int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if st := sem.Stats(); st.Active == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := sem.Stats()
	t.Fatalf("waitForActive: timed out waiting for active=%d after %s; got active=%d queued=%d per_user=%v",
		want, deadline, st.Active, st.Queued, st.PerUser)
}
