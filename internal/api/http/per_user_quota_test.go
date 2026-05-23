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

	// Give the two open runs time to register their semaphore slots.
	time.Sleep(50 * time.Millisecond)

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
	// Brief wait so user_b reaches the provider — confirms accept
	// happened. (If 429'd, the post returns immediately and we'd see
	// -1 / non-200 below.)
	time.Sleep(50 * time.Millisecond)

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
	time.Sleep(50 * time.Millisecond)
	close(prov.release)
	wg.Wait()
}

