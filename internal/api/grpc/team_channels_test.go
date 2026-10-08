package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

type teamChannelMock struct {
	mockConnector
	listReq    connector.TeamChannelsRequest
	peekReq    connector.TeamChannelPeekRequest
	releaseReq connector.TeamChannelReleaseRequest
	err        error
}

func (m *teamChannelMock) ReleaseTeamChannel(_ context.Context, r connector.TeamChannelReleaseRequest) (connector.TeamChannelReleaseResult, error) {
	m.releaseReq = r
	return connector.TeamChannelReleaseResult{Team: r.Team, Name: r.Name, Released: []string{"m1", "m2"}, ReleasedCount: 2, StillHeld: 4}, m.err
}

func (m *teamChannelMock) ListTeamChannels(_ context.Context, r connector.TeamChannelsRequest) (connector.TeamChannelsResponse, error) {
	m.listReq = r
	return connector.TeamChannelsResponse{Team: r.Team, Channels: []connector.TeamChannelDescriptor{{
		Name: "journal", Scope: "user", Semantic: "queue", Hold: true, DefaultTTL: 60, MaxMessages: 50,
		DeclaredIn: connector.TeamChannelDeclaredRetired, DefID: "tdf_1", Version: 3,
		MessageCount: 4, HeldCount: 2, AwaitingHooksCount: 1, OldestVisibleAt: "2026-10-08T00:00:00Z", NewestVisibleAt: "2026-10-08T01:00:00Z",
	}}}, m.err
}

func (m *teamChannelMock) PeekTeamChannel(_ context.Context, r connector.TeamChannelPeekRequest) (connector.TeamChannelPeekResult, error) {
	m.peekReq = r
	return connector.TeamChannelPeekResult{Team: r.Team, Name: r.Name, Scope: "user", DeclaredIn: connector.TeamChannelDeclaredActive,
		Messages: []connector.ChannelMessage{{ID: "m1", Value: json.RawMessage(`{"note":"hi"}`), PublishedAt: "2026-10-08T00:00:00Z"}}}, m.err
}

// Every field of the connector's answer reaches the wire, and every field of
// the request reaches the connector.
func TestTeamChannelRPCs_CarryEveryField(t *testing.T) {
	mc := &teamChannelMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	ctx := context.Background()

	list, err := client.ListTeamChannels(ctx, &loomcyclepb.ListTeamChannelsRequest{Team: "triage", Tenant: "acme"})
	if err != nil {
		t.Fatalf("ListTeamChannels: %v", err)
	}
	if mc.listReq != (connector.TeamChannelsRequest{Team: "triage", Tenant: "acme"}) {
		t.Errorf("the connector was asked %+v", mc.listReq)
	}
	if list.GetTeam() != "triage" || len(list.GetChannels()) != 1 {
		t.Fatalf("list = %v", list)
	}
	c := list.GetChannels()[0]
	if c.GetName() != "journal" || c.GetScope() != "user" || c.GetSemantic() != "queue" || !c.GetHold() ||
		c.GetDefaultTtl() != 60 || c.GetMaxMessages() != 50 || c.GetDeclaredIn() != "retired" || c.GetDefId() != "tdf_1" || c.GetVersion() != 3 ||
		c.GetMessageCount() != 4 || c.GetHeldCount() != 2 || c.GetAwaitingHooksCount() != 1 ||
		c.GetOldestVisibleAt() == "" || c.GetNewestVisibleAt() == "" {
		t.Errorf("a field of the descriptor was lost on the wire: %v", c)
	}

	peek, err := client.PeekTeamChannel(ctx, &loomcyclepb.PeekTeamChannelRequest{
		Team: "triage", Name: "journal", Tenant: "acme", UserId: "alice", FromCursor: "cur_2", MaxMessages: 5})
	if err != nil {
		t.Fatalf("PeekTeamChannel: %v", err)
	}
	if want := (connector.TeamChannelPeekRequest{Team: "triage", Name: "journal", Tenant: "acme", UserID: "alice", FromCursor: "cur_2", MaxMessages: 5}); mc.peekReq != want {
		t.Errorf("the connector was asked %+v, want %+v", mc.peekReq, want)
	}
	if peek.GetTeam() != "triage" || peek.GetName() != "journal" || peek.GetScope() != "user" || peek.GetDeclaredIn() != "active" ||
		len(peek.GetMessages()) != 1 || peek.GetMessages()[0].GetId() != "m1" || string(peek.GetMessages()[0].GetValue()) != `{"note":"hi"}` {
		t.Errorf("peek = %v", peek)
	}

	rel, err := client.ReleaseTeamChannel(ctx, &loomcyclepb.ReleaseTeamChannelRequest{
		Team: "triage", Name: "journal", Tenant: "acme", UserId: "alice", Count: 2})
	if err != nil {
		t.Fatalf("ReleaseTeamChannel: %v", err)
	}
	if want := (connector.TeamChannelReleaseRequest{Team: "triage", Name: "journal", Tenant: "acme", UserID: "alice", Count: 2}); mc.releaseReq != want {
		t.Errorf("the connector was asked %+v, want %+v", mc.releaseReq, want)
	}
	if rel.GetTeam() != "triage" || rel.GetName() != "journal" || len(rel.GetReleased()) != 2 || rel.GetReleased()[1] != "m2" ||
		rel.GetReleasedCount() != 2 || rel.GetStillHeld() != 4 {
		t.Errorf("release = %v", rel)
	}
}

func TestTeamChannelRPCs_MapRefusalsToStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{connector.ErrTeamNotFound, codes.NotFound},
		{connector.ErrTeamChannelNotDeclared, codes.NotFound},
		{connector.ErrTeamChannelUserRequired, codes.InvalidArgument},
		{connector.ErrTeamChannelBadCount, codes.InvalidArgument},
	} {
		client, cleanup := startTestServerWithConnector(t, &teamChannelMock{err: tc.err})
		if _, err := client.PeekTeamChannel(context.Background(), &loomcyclepb.PeekTeamChannelRequest{Team: "t", Name: "n"}); status.Code(err) != tc.want {
			t.Errorf("%v → %s, want %s", tc.err, status.Code(err), tc.want)
		}
		if _, err := client.ReleaseTeamChannel(context.Background(), &loomcyclepb.ReleaseTeamChannelRequest{Team: "t", Name: "n"}); status.Code(err) != tc.want {
			t.Errorf("release: %v → %s, want %s", tc.err, status.Code(err), tc.want)
		}
		if _, err := client.ListTeamChannels(context.Background(), &loomcyclepb.ListTeamChannelsRequest{Team: "t"}); status.Code(err) != tc.want {
			t.Errorf("list: %v → %s, want %s", tc.err, status.Code(err), tc.want)
		}
		cleanup()
	}
}

// Gated like the team's own RPC, not left to the admin-only default.
func TestTeamChannelRPCs_AreGatedLikeTeamDef(t *testing.T) {
	for _, m := range []string{"ListTeamChannels", "PeekTeamChannel", "ReleaseTeamChannel"} {
		if got, ok := grpcConsumerScopes[m]; !ok || got != auth.ScopeTenant || got != grpcConsumerScopes["TeamDef"] {
			t.Errorf("%s scope = %q (mapped %v), want TeamDef's %q", m, got, ok, grpcConsumerScopes["TeamDef"])
		}
	}
}
