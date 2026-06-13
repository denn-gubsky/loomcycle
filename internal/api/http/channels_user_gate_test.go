package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// userChannelFixture builds a server with one declared USER-scoped channel and
// the channel publisher wired, so a peek on the declared channel returns 200
// (allow path) — distinguishable from the gate's opaque 404 (which is
// deliberately identical to an undeclared channel's 404).
func userChannelFixture(t *testing.T) *Server {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"inbox": {Scope: "user", Semantic: "broadcast", MaxMessages: 100},
		},
	}
	cfg.Env.ChannelsMaxValueBytes = 64 * 1024
	hookReg := hooks.NewRegistry()
	bus := channels.NewBus()
	srv := &Server{
		cfg:            cfg,
		store:          st,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	srv.SetSystemPublisher(&channels.StorePublisher{
		Store:     st,
		Bus:       bus,
		Scheduler: channels.NewScheduler(bus, 100),
	})
	return srv
}

// A channel-scoped tenant token must not read/write another subject's per-user
// channels by changing the {user_id} in the path (user_ids are not secret, and
// channel_messages have no tenant column to gate on). A non-admin principal may
// act only on its OWN subject; admin / legacy / open mode are unrestricted. The
// cross-subject refusal is an opaque 404 — identical to an undeclared channel,
// so it's not an existence oracle. Fails on the pre-gate wrappers, which passed
// the path user_id straight through as scope_id.
func TestHandleUserChannelPeek_RejectsCrossSubject(t *testing.T) {
	s := userChannelFixture(t)

	peek := func(pathUser, tenant, subject string, scopes []string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/users/"+pathUser+"/channels/inbox/peek", nil)
		r.SetPathValue("user_id", pathUser)
		r.SetPathValue("name", "inbox")
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: scopes,
		}))
		rr := httptest.NewRecorder()
		s.handleUserChannelPeek(rr, r)
		return rr
	}

	// Cross-subject (alice's token peeking bob's inbox) → opaque 404.
	if rr := peek("bob", "acme", "alice", []string{auth.ScopeChannelRead}); rr.Code != http.StatusNotFound {
		t.Errorf("cross-subject peek status=%d, want 404 (own-subject gate not enforced)\nbody=%s", rr.Code, rr.Body.String())
	}
	// Own subject → gate passes; declared channel → 200 (not the gate's 404).
	if rr := peek("alice", "acme", "alice", []string{auth.ScopeChannelRead}); rr.Code != http.StatusOK {
		t.Errorf("own-subject peek status=%d, want 200 (gate rejected a legitimate caller)\nbody=%s", rr.Code, rr.Body.String())
	}
	// Super-admin may peek any subject's channel.
	if rr := peek("bob", "x", "ops", []string{auth.ScopeAdmin}); rr.Code != http.StatusOK {
		t.Errorf("admin peek status=%d, want 200 (super-admin must cross subjects)\nbody=%s", rr.Code, rr.Body.String())
	}
}

// TestRequirePrincipalOwnsPathUser_Matrix locks the per-user ownership predicate
// directly (the per-subject mitigation for the tenant-column-less channels).
func TestRequirePrincipalOwnsPathUser_Matrix(t *testing.T) {
	cases := []struct {
		name      string
		principal *auth.Principal // nil = open mode (no principal)
		pathUser  string
		want      bool
	}{
		{"open mode (no principal)", nil, "anyone", true},
		{"legacy exempt", &auth.Principal{Subject: "default", Legacy: true}, "anyone", true},
		{"admin crosses subjects", &auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeAdmin}}, "anyone", true},
		{"own subject", &auth.Principal{Subject: "alice", Scopes: []string{auth.ScopeChannelRead}}, "alice", true},
		{"other subject blocked", &auth.Principal{Subject: "alice", Scopes: []string{auth.ScopeChannelRead}}, "bob", false},
	}
	for _, c := range cases {
		ctx := context.Background()
		if c.principal != nil {
			ctx = auth.WithPrincipal(ctx, *c.principal)
		}
		if got := requirePrincipalOwnsPathUser(ctx, c.pathUser); got != c.want {
			t.Errorf("%s: requirePrincipalOwnsPathUser = %v, want %v", c.name, got, c.want)
		}
	}
}
