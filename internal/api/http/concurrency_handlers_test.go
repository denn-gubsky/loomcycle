package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestConcurrencyStats_DefaultShape — fresh server with no in-flight
// runs returns the lean shape `{"active":0,"queued":0}` (per_user is
// omitempty, absent when no per-user cap is configured AND no users
// have hit the substrate).
func TestConcurrencyStats_DefaultShape(t *testing.T) {
	cfg := makeBaseConfig()
	dbPath := filepath.Join(t.TempDir(), "concstats.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sem := concurrency.New(8, 8, time.Second)
	srv := New(cfg, &stubResolver{p: nil}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_concurrency/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got concurrencyStatsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Active != 0 || got.Queued != 0 {
		t.Errorf("active=%d queued=%d, want 0/0", got.Active, got.Queued)
	}
	if got.PerUser != nil {
		t.Errorf("PerUser should be omitted when no per-user activity; got %+v", got.PerUser)
	}
}

// TestConcurrencyStats_ReflectsLiveCounts — after one in-flight run
// from user_a (held by pausableProvider), the endpoint returns
// active=1 + per_user["user_a"]=1.
func TestConcurrencyStats_ReflectsLiveCounts(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := makeBaseConfig()
	dbPath := filepath.Join(t.TempDir(), "concstats2.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sem := concurrency.New(8, 8, time.Second).WithPerUserCap(4)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Fire one run as user_a, kept open by the pausable provider.
	// Hold the response body open until the main goroutine's stats
	// check finishes — closing it disconnects the client, cancels the
	// request ctx, releases the provider, and decrements the
	// semaphore before we can observe it.
	holdResponse := make(chan struct{})
	postDone := make(chan struct{})
	go func() {
		resp, _ := http.Post(ts.URL+"/v1/runs", "application/json",
			strings.NewReader(`{"agent":"default","user_id":"user_a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
		if resp != nil {
			<-holdResponse
			resp.Body.Close()
		}
		close(postDone)
	}()
	// Give the post time to land at the semaphore.
	time.Sleep(80 * time.Millisecond)

	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_concurrency/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got concurrencyStatsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Active != 1 {
		t.Errorf("active = %d, want 1", got.Active)
	}
	if got.PerUser["user_a"] != 1 {
		t.Errorf("per_user[user_a] = %d, want 1; full map: %+v", got.PerUser["user_a"], got.PerUser)
	}

	// Release the provider so the run completes; signal the post
	// goroutine to drain + close; then wait for it to exit.
	close(prov.release)
	close(holdResponse)
	<-postDone
}
