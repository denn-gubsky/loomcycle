package grpc

import (
	"context"
	"testing"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// principalRunStateStream is a server-side stream carrying a principal on its
// context, so the handler can be driven as a specific caller without the auth
// interceptor.
type principalRunStateStream struct {
	googlegrpc.ServerStream
	ctx  context.Context
	sent []*loomcyclepb.RunStateEvent
}

func (s *principalRunStateStream) Context() context.Context { return s.ctx }
func (s *principalRunStateStream) Send(e *loomcyclepb.RunStateEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

// TestGrpcStreamUserRunStates_IsolatedMemberCannotWatchAnotherUser — an
// isolated member (authority topped at substrate:user) holds runs:read, so it
// reaches this RPC; naming a co-tenant's user id must be the same opaque
// NotFound HTTP returns, and must not reach the run-state bus. Its own user id
// still streams.
func TestGrpcStreamUserRunStates_IsolatedMemberCannotWatchAnotherUser(t *testing.T) {
	mc := &n8nMock{streamEvents: []connector.RunStateEvent{{RunID: "r1", UserID: "alice", Status: "completed"}}}
	s := New(Config{Store: newTestStore(t), Connector: mc})
	isolated := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser},
	})

	stream := &principalRunStateStream{ctx: isolated}
	err := s.StreamUserRunStates(&loomcyclepb.StreamUserRunStatesRequest{UserId: "alice"}, stream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("isolated member naming another user: err=%v, want NotFound", err)
	}
	if len(stream.sent) != 0 || mc.lastStreamReq.UserID != "" {
		t.Fatalf("another user's run transitions reached an isolated member: sent=%d connector=%+v",
			len(stream.sent), mc.lastStreamReq)
	}

	own := &principalRunStateStream{ctx: isolated}
	if err := s.StreamUserRunStates(&loomcyclepb.StreamUserRunStatesRequest{UserId: "bob"}, own); err != nil {
		t.Fatalf("isolated member streaming its own user: %v", err)
	}
	if !mc.lastStreamReq.TenantScoped || mc.lastStreamReq.TenantID != "acme" {
		t.Errorf("own-user stream not tenant-scoped: %+v", mc.lastStreamReq)
	}
}
