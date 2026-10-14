package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// redirector answers every request with a 307 to target, and counts them.
func redirector(t *testing.T, target string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	n := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv, n
}

func counter(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	n := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, n
}

// A pinned request stops at the first redirect, whichever client sends it; an
// unpinned one follows it as before.
func TestPinnedRequest_FollowsNoRedirect(t *testing.T) {
	target, hits := counter(t)
	from, _ := redirector(t, target.URL)

	clients := map[string]*http.Client{
		"RedirectPolicy":        {CheckRedirect: RedirectPolicy(5)},
		"HonorPins, no policy":  HonorPins(&http.Client{}),
		"HonorPins, own policy": HonorPins(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}),
		"HonorPins, nil client": HonorPins(nil),
		// The test servers are on loopback, which the guard admits only when
		// the operator vouches for it.
		"NewGuardedClient": NewGuardedClient(0, []string{"127.0.0.1"}),
	}

	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			before := hits.Load()
			ctx := WithPinnable(context.Background())
			if !Pin(ctx) {
				t.Fatal("Pin on a pinnable context reported false")
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, from.URL, nil)
			resp, err := c.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			if !errors.Is(err, ErrPinnedRedirect) || hits.Load() != before {
				t.Errorf("pinned: err=%v, the target was reached %d time(s); want ErrPinnedRedirect and none", err, hits.Load()-before)
			}

			// Pinnable but not pinned: the redirect is followed.
			req, _ = http.NewRequestWithContext(WithPinnable(context.Background()), http.MethodGet, from.URL, nil)
			resp, err = c.Do(req)
			if err != nil {
				t.Fatalf("unpinned: %v", err)
			}
			resp.Body.Close()
			if hits.Load() != before+1 {
				t.Errorf("unpinned: the target was reached %d time(s), want 1", hits.Load()-before)
			}
		})
	}
}

// Pin needs a context made by WithPinnable, and reports when it has none, so
// the caller can refuse rather than send a request nothing will hold back.
// WithPinnable on an already pinnable context keeps the same record.
func TestPin_NeedsAPinnableContext(t *testing.T) {
	if Pin(context.Background()) || Pinned(context.Background()) {
		t.Error("a plain context was pinned")
	}
	ctx := WithPinnable(context.Background())
	again := WithPinnable(ctx)
	Pin(again)
	if !Pinned(ctx) {
		t.Error("a second WithPinnable started a new record: the first caller's request is not pinned")
	}
}

// HonorPins keeps the client's own policy for an unpinned request.
func TestHonorPins_KeepsTheClientsOwnPolicy(t *testing.T) {
	target, _ := counter(t)
	from, _ := redirector(t, target.URL)
	own := errors.New("the client's own policy")
	c := HonorPins(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return own }})
	resp, err := c.Get(from.URL)
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, own) {
		t.Errorf("err = %v, want the client's own policy", err)
	}
}
