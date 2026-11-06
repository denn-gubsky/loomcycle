package credential

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingSink keeps the audit events it is given.
type recordingSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *recordingSink) Record(ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingSink) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Action)
	}
	return out
}

// oauthHarness is a manager over a fresh engine and a fake authorization
// server, on a fake clock whose waits return at once (advancing the clock)
// unless gated.
type oauthHarness struct {
	f     *oauthtest.Server
	e     *Engine
	st    Store
	m     *OAuthManager
	clock *fakeClock
	sink  *recordingSink

	mu     sync.Mutex
	waits  []time.Duration
	gate   chan struct{} // nil: waits return at once
	waited chan struct{} // signalled each time a wait begins
}

func newOAuthHarness(t *testing.T) *oauthHarness {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return newOAuthHarnessOn(t, st, oauthtest.New(t), "replica-a")
}

// newOAuthHarnessOn is a harness over st and f, as replica holder: two
// harnesses on one store are two replicas.
func newOAuthHarnessOn(t *testing.T, st Store, f *oauthtest.Server, holder string) *oauthHarness {
	t.Helper()
	sealer, err := NewSealer(testKey(1), "")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(st, sealer)
	clock := &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	e.now = clock.now
	h := &oauthHarness{f: f, e: e, st: st, clock: clock, sink: &recordingSink{}, waited: make(chan struct{}, 1000)}
	h.m = NewOAuthManager(e, f.Client(), holder, h.sink)
	h.m.now = clock.now
	h.m.client.now = clock.now
	h.m.wait = h.wait
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		h.m.mu.Lock()
		ps := make([]*oauthPoller, 0, len(h.m.pollers))
		for _, p := range h.m.pollers {
			ps = append(ps, p)
		}
		h.m.mu.Unlock()
		for _, p := range ps {
			<-p.done
		}
	})
	h.m.SetBackground(ctx)
	return h
}

func (h *oauthHarness) wait(ctx context.Context, d time.Duration) error {
	h.mu.Lock()
	h.waits = append(h.waits, d)
	gate := h.gate
	h.mu.Unlock()
	h.waited <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.clock.advance(d)
	return ctx.Err()
}

func (h *oauthHarness) waitLog() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration{}, h.waits...)
}

// awaitPoller waits for the poller of id to finish.
func (h *oauthHarness) awaitPoller(t *testing.T, id Identity) {
	t.Helper()
	h.m.mu.Lock()
	p := h.m.pollers[oauthRow{row: rowKey(id)}.key()]
	h.m.mu.Unlock()
	if p == nil {
		return
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the poller did not finish")
	}
}

// create stores an OAuth credential for the fake's resource.
func (h *oauthHarness) create(t *testing.T, id Identity) {
	t.Helper()
	if _, err := h.e.PutOAuth(context.Background(), id, OAuthConfig{Resource: h.f.Resource(),
		Client: OAuthClientConfig{Registration: OAuthRegistrationDynamic, TokenEndpointAuth: OAuthAuthNone}}, nil); err != nil {
		t.Fatal(err)
	}
}

// connect creates (when absent) and connects id, waiting for the grant.
func (h *oauthHarness) connect(t *testing.T, id Identity) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.e.Get(ctx, id); err != nil {
		h.create(t, id)
	}
	if _, err := h.m.ConnectStart(ctx, id); err != nil {
		t.Fatalf("ConnectStart: %v", err)
	}
	h.awaitPoller(t, id)
	r, err := h.e.getOAuth(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.def.Status.State != OAuthConnected {
		t.Fatalf("state = %q (%s), want connected", r.def.Status.State, r.def.Status.LastError)
	}
}

// secretOf opens the stored secret of id.
func (h *oauthHarness) secretOf(t *testing.T, id Identity) (oauthRow, oauthSecret) {
	t.Helper()
	r, err := h.e.getOAuth(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := h.e.openOAuthSecret(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func rowKey(id Identity) credentialdef.Row {
	return credentialdef.Row{TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name}
}
