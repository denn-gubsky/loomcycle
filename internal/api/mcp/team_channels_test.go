package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// teamChannelConnector answers the two team-channel reads and records what it
// was asked.
type teamChannelConnector struct {
	mockConnector
	listReq connector.TeamChannelsRequest
	peekReq connector.TeamChannelPeekRequest
	err     error
}

func (c *teamChannelConnector) ListTeamChannels(_ context.Context, r connector.TeamChannelsRequest) (connector.TeamChannelsResponse, error) {
	c.listReq = r
	return connector.TeamChannelsResponse{Team: r.Team, Channels: []connector.TeamChannelDescriptor{
		{Name: "journal", Scope: "user", DeclaredIn: connector.TeamChannelDeclaredActive, MessageCount: 3, HeldCount: 1},
	}}, c.err
}

func (c *teamChannelConnector) PeekTeamChannel(_ context.Context, r connector.TeamChannelPeekRequest) (connector.TeamChannelPeekResult, error) {
	c.peekReq = r
	return connector.TeamChannelPeekResult{Team: r.Team, Name: r.Name, Scope: "user", DeclaredIn: connector.TeamChannelDeclaredActive,
		Messages: []connector.ChannelMessage{{ID: "m1", Value: json.RawMessage(`{"note":"hi"}`), PublishedAt: "2026-10-08T00:00:00Z"}}}, c.err
}

// The two tools hand their arguments to the connector as written and return
// its answer: the team and the local name, never a stored channel name.
func TestTeamChannelTools_CarryTheRequestAndTheAnswer(t *testing.T) {
	c := &teamChannelConnector{}
	env := &handlerEnv{connector: c, session: NewSession()}

	res, err := handleListTeamChannels(context.Background(), env, json.RawMessage(`{"team":"triage"}`))
	if err != nil || res.IsError {
		t.Fatalf("list_team_channels: %v %+v", err, res)
	}
	if c.listReq.Team != "triage" {
		t.Errorf("the connector was asked for team %q", c.listReq.Team)
	}
	for _, want := range []string{`"name":"journal"`, `"declared_in":"active"`, `"message_count":3`, `"held_count":1`} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("list result %s lacks %s", res.Content[0].Text, want)
		}
	}

	res, err = handlePeekTeamChannel(context.Background(), env,
		json.RawMessage(`{"team":"triage","name":"journal","user_id":"alice","from_cursor":"cur_2","max_messages":5}`))
	if err != nil || res.IsError {
		t.Fatalf("peek_team_channel: %v %+v", err, res)
	}
	if got, want := c.peekReq, (connector.TeamChannelPeekRequest{Team: "triage", Name: "journal", UserID: "alice", FromCursor: "cur_2", MaxMessages: 5}); got != want {
		t.Errorf("the connector was asked %+v, want %+v", got, want)
	}
	if !strings.Contains(res.Content[0].Text, `"note":"hi"`) {
		t.Errorf("peek result %s lacks the message", res.Content[0].Text)
	}
}

// A refusal reaches the caller as a tool error that says which of the two
// not-founds it was.
func TestTeamChannelTools_ARefusalIsAToolError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{connector.ErrTeamNotFound, "no such team"},
		{connector.ErrTeamChannelNotDeclared, "declares no such channel"},
	} {
		env := &handlerEnv{connector: &teamChannelConnector{err: tc.err}, session: NewSession()}
		res, err := handlePeekTeamChannel(context.Background(), env, json.RawMessage(`{"team":"t","name":"n"}`))
		if err != nil || !res.IsError || !strings.Contains(res.Content[0].Text, tc.want) {
			t.Errorf("%v → err=%v result=%+v, want a tool error containing %q", tc.err, err, res, tc.want)
		}
	}
}

// Who is offered the tools: a tenant's operator and its members, as for the
// team itself; an isolated user is not.
func TestTeamChannelTools_AreOfferedLikeTheTeamTool(t *testing.T) {
	as := func(scopes ...string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: scopes})
	}
	for _, tool := range []string{"list_team_channels", "peek_team_channel"} {
		if !principalMayCallTool(context.Background(), tool) || !principalMayCallTool(as(auth.ScopeAdmin), tool) {
			t.Errorf("%s: withheld from an operator", tool)
		}
		if got, want := principalMayCallTool(as(auth.ScopeTenant), tool), principalMayCallTool(as(auth.ScopeTenant), "teamdef"); got != want || !got {
			t.Errorf("%s: offered to a tenant operator = %v, teamdef = %v", tool, got, want)
		}
		if principalMayCallTool(as(auth.ScopeUser), tool) {
			t.Errorf("%s: offered to an isolated user", tool)
		}
	}
}
