package anthropic_oauth_dev

import (
	"context"
	"log"
	"sync"
	"time"
)

// Refresher rotates the access token before it expires. Started by the
// driver on registration; stopped via Stop() at shutdown. Runs a
// 30-second tick, checks the persisted token's NeedsRefresh(), rotates
// proactively. Single-flight via the mutex — concurrent calls can't
// double-refresh.
type Refresher struct {
	store      *TokenStore
	httpClient ExchangeOptions
	logf       func(string, ...any) // nil = log.Printf

	mu       sync.Mutex
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	// Token is the in-memory cache the driver reads on every request.
	// kept in sync with the persisted file by the refresh goroutine.
	// Reads via Token(); writes are gated by mu.
	cached Token
}

// NewRefresher builds a Refresher that reads/writes via store, calls
// the OAuth token endpoint via opts.HTTPClient (nil = default client),
// and logs via logf (nil = log.Printf).
//
// The initial token is loaded from the store; if absent, the Refresher
// constructs in a usable state but Token() returns an empty Token until
// the operator logs in.
func NewRefresher(store *TokenStore, opts ExchangeOptions, logf func(string, ...any)) *Refresher {
	if logf == nil {
		logf = log.Printf
	}
	r := &Refresher{
		store:      store,
		httpClient: opts,
		logf:       logf,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	if t, err := store.Load(); err == nil {
		r.cached = t
	}
	return r
}

// Start launches the background refresh goroutine. Must be called
// EXACTLY ONCE per Refresher lifetime; subsequent Start() calls panic
// on the deferred close(doneCh) once the first goroutine exits. Stop()
// must also be called exactly once, AFTER Start().
//
// Used by the v0.11.9 provider registration in cmd/loomcycle/main.go
// (one Start at boot, one Stop at shutdown). Callers needing
// hot-reload semantics should construct a fresh Refresher rather than
// re-starting an existing one.
func (r *Refresher) Start(ctx context.Context) {
	go r.loop(ctx)
}

// Stop signals the refresh goroutine to exit + waits for it to finish.
// Safe to call multiple times. Always safe to defer.
func (r *Refresher) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	// Block until loop returns. Bounded by the 30-second tick + the
	// refresh HTTP call's 30-second timeout.
	<-r.doneCh
}

// Token returns the most recent cached token. Safe for concurrent use.
// Returns the zero Token before the first successful Load() — callers
// check t.AccessToken == "" to detect "not logged in."
func (r *Refresher) Token() Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cached
}

// RefreshNow forces an immediate refresh attempt, bypassing the 5-min
// slack check. Used by the `login` CLI path (the just-exchanged token
// is fresh; this is a no-op) and by request paths that observe a 401
// from Anthropic (token may have been revoked server-side).
//
// Concurrent calls are coalesced via the mutex — only one HTTP refresh
// runs at a time; later callers see the result of the first.
func (r *Refresher) RefreshNow(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshLocked(ctx)
}

// loop is the background tick. Exits when ctx cancels OR stopCh closes.
func (r *Refresher) loop(ctx context.Context) {
	defer close(r.doneCh)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			needs := r.cached.AccessToken != "" && r.cached.NeedsRefresh()
			r.mu.Unlock()
			if !needs {
				continue
			}
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			r.mu.Lock()
			err := r.refreshLocked(refreshCtx)
			r.mu.Unlock()
			cancel()
			if err != nil {
				// Log + continue. Next tick retries. If refresh keeps
				// failing past the access token's actual expiry,
				// subsequent requests fail with 401 and the caller
				// gets a clear error pointing at `loomcycle anthropic
				// login`.
				r.logf("anthropic-oauth-dev: refresh failed: %v (will retry on next tick)", err)
			}
		}
	}
}

// refreshLocked performs one refresh attempt. Caller holds r.mu.
func (r *Refresher) refreshLocked(ctx context.Context) error {
	if r.cached.RefreshToken == "" {
		// Nothing to refresh — operator hasn't logged in yet.
		return nil
	}
	fresh, err := RefreshAccessToken(ctx, r.cached.RefreshToken, r.httpClient)
	if err != nil {
		return err
	}
	// Anthropic MAY rotate the refresh token; if so, the fresh response
	// carries a new value. Persist whatever came back.
	if err := r.store.Save(fresh); err != nil {
		return err
	}
	r.cached = fresh
	r.logf("anthropic-oauth-dev: token refreshed (expires_at=%s)", fresh.ExpiresAt.Format(time.RFC3339))
	return nil
}
