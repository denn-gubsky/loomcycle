package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memberScopes are the scopes a tenant-mode (non-isolated) member's token is
// minted with.
func memberScopes(t *testing.T) []string {
	t.Helper()
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	return scopes
}

// A person reads only its own conversations over gRPC as on HTTP: a
// non-isolated member reads, lists and streams the runs and sessions it
// started, not a colleague's and not a tenant-level run no user started. A
// tenant operator reads every run of its tenant and none of another's; an
// admin reads all.
func TestGrpcRunReads_AMemberReadsOnlyItsOwnRuns(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	runs := map[string]store.Run{}
	for _, row := range []struct{ name, tenant, user string }{
		{"alice", "acme", "alice"},
		{"bob", "acme", "bob"},
		{"tenant-level", "acme", ""},
		{"globex", "globex", "carol"},
	} {
		sess, err := st.CreateSession(ctx, row.tenant, "default", row.user)
		if err != nil {
			t.Fatal(err)
		}
		run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + row.user + "_" + row.tenant, UserID: row.user, TenantID: row.tenant})
		if err != nil {
			t.Fatal(err)
		}
		runs[row.name] = run
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		sees map[string]bool
	}{
		{"member", scopedCtx("acme", "bob", memberScopes(t)...), map[string]bool{"bob": true}},
		{"isolated member", scopedCtx("acme", "bob", auth.ScopeUser), map[string]bool{"bob": true}},
		{"tenant operator", scopedCtx("acme", "op", auth.ScopeTenant), map[string]bool{"alice": true, "bob": true, "tenant-level": true}},
		{"another tenant's operator", scopedCtx("globex", "op", auth.ScopeTenant), map[string]bool{"globex": true}},
		{"admin", scopedCtx("", "root", auth.ScopeAdmin), map[string]bool{"alice": true, "bob": true, "tenant-level": true, "globex": true}},
	} {
		for name, run := range runs {
			want := c.sees[name]
			_, err := adapter.GetAgent(c.ctx, &loomcyclepb.GetAgentRequest{AgentId: run.AgentID})
			if got := err == nil; got != want || (err != nil && status.Code(err) != codes.NotFound) {
				t.Errorf("%s GetAgent of %s's run: err=%v, want visible=%v (or NotFound)", c.name, name, err, want)
			}
			_, err = adapter.GetRun(c.ctx, &loomcyclepb.GetRunRequest{RunId: run.ID})
			if got := err == nil; got != want || (err != nil && status.Code(err) != codes.NotFound) {
				t.Errorf("%s GetRun of %s's run: err=%v, want visible=%v (or NotFound)", c.name, name, err, want)
			}
			_, err = adapter.GetTranscript(c.ctx, &loomcyclepb.GetTranscriptRequest{SessionId: run.SessionID})
			if got := err == nil; got != want || (err != nil && status.Code(err) != codes.NotFound) {
				t.Errorf("%s GetTranscript of %s's session: err=%v, want visible=%v (or NotFound)", c.name, name, err, want)
			}
		}
	}

	list := func(ctx context.Context, user string) int {
		t.Helper()
		resp, err := adapter.ListUserAgents(ctx, &loomcyclepb.ListUserAgentsRequest{UserId: user})
		if err != nil {
			t.Fatalf("ListUserAgents(%s): %v", user, err)
		}
		return len(resp.GetAgents())
	}
	member := scopedCtx("acme", "bob", memberScopes(t)...)
	if n := list(member, "alice"); n != 0 {
		t.Errorf("member listing alice's runs = %d, want none", n)
	}
	if n := list(member, "bob"); n != 1 {
		t.Errorf("member listing its own runs = %d, want 1", n)
	}
	if n := list(scopedCtx("acme", "op", auth.ScopeTenant), "alice"); n != 1 {
		t.Errorf("tenant operator listing alice's runs = %d, want 1", n)
	}
}

// A member may watch only its own runs' state changes; another user's id is
// the opaque NotFound HTTP gives, and never reaches the run-state bus.
func TestGrpcStreamUserRunStates_AMemberCannotWatchAnotherUser(t *testing.T) {
	mc := &n8nMock{streamEvents: []connector.RunStateEvent{{RunID: "r1", UserID: "alice", Status: "completed"}}}
	s := New(Config{Store: newTestStore(t), Connector: mc})
	member := scopedCtx("acme", "bob", memberScopes(t)...)

	stream := &principalRunStateStream{ctx: member}
	if err := s.StreamUserRunStates(&loomcyclepb.StreamUserRunStatesRequest{UserId: "alice"}, stream); status.Code(err) != codes.NotFound {
		t.Fatalf("member naming another user: err=%v, want NotFound", err)
	}
	if len(stream.sent) != 0 || mc.lastStreamReq.UserID != "" {
		t.Fatalf("another user's run transitions reached a member: sent=%d connector=%+v", len(stream.sent), mc.lastStreamReq)
	}
	if err := s.StreamUserRunStates(&loomcyclepb.StreamUserRunStatesRequest{UserId: "bob"}, &principalRunStateStream{ctx: member}); err != nil {
		t.Fatalf("member streaming its own user: %v", err)
	}
	if err := s.StreamUserRunStates(&loomcyclepb.StreamUserRunStatesRequest{UserId: "alice"}, &principalRunStateStream{ctx: scopedCtx("acme", "op", auth.ScopeTenant)}); err != nil {
		t.Fatalf("tenant operator streaming a member's runs: %v", err)
	}
}
