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
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentNameMock records the agent name each Path / Document / Memory call
// reached the connector with ("<none>" when it was not reached).
type agentNameMock struct {
	*substrateMock
	reached []string
}

func (m *agentNameMock) note(ctx context.Context) (connector.ToolResult, error) {
	m.reached = append(m.reached, tools.AgentName(ctx))
	return connector.ToolResult{Text: `{}`}, nil
}
func (m *agentNameMock) Path(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return m.note(ctx)
}
func (m *agentNameMock) Document(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return m.note(ctx)
}
func (m *agentNameMock) Memory(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return m.note(ctx)
}

// RFC FY Part A: the substrate RPCs stamp one synthetic agent name for every
// caller, so its agent scope is one scope the whole tenant shares. A member
// has no such scope: its agent-scope Path, Document and Memory calls are
// refused NotFound before they reach the tool, and its other calls carry no
// agent name. The operator plane keeps its scope.
func TestGrpcDataRPCs_AMemberHasNoSharedAgentScope(t *testing.T) {
	member := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}
	op := auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	type call struct {
		rpc, input string
	}
	agentCalls := []call{
		{"Document", `{"op":"query_documents","scope":"agent"}`},
		{"Document", `{"op":"copy_documents","scope":"user","to_scope":"agent","document_ids":["d"]}`},
		{"Path", `{"op":"ls","path":"/"}`}, // Path's default scope is agent
		{"Path", `{"op":"ls","scope":"agent","path":"/"}`},
		{"Memory", `{"op":"get","scope":"agent","key":"k"}`},
	}
	invoke := func(s *Server, p auth.Principal, c call) error {
		ctx := auth.WithPrincipal(context.Background(), p)
		req := &loomcyclepb.SubstrateRequest{InputJson: []byte(c.input)}
		var err error
		switch c.rpc {
		case "Document":
			_, err = s.Document(ctx, req)
		case "Path":
			_, err = s.Path(ctx, req)
		case "Memory":
			_, err = s.Memory(ctx, req)
		}
		return err
	}
	for _, c := range agentCalls {
		m := &agentNameMock{substrateMock: &substrateMock{}}
		s := &Server{connector: m}
		if err := invoke(s, member, c); status.Code(err) != codes.NotFound {
			t.Errorf("member %s %s: err %v, want NotFound", c.rpc, c.input, err)
		}
		if len(m.reached) != 0 {
			t.Errorf("member %s %s reached the tool with agent %q", c.rpc, c.input, m.reached)
		}
		if err := invoke(s, op, c); err != nil {
			t.Errorf("operator %s %s: %v", c.rpc, c.input, err)
		}
		if len(m.reached) != 1 || m.reached[0] != grpcSubstrateAdminAgentName {
			t.Errorf("operator %s %s reached the tool with agent %q, want %q", c.rpc, c.input, m.reached, grpcSubstrateAdminAgentName)
		}
	}
	m := &agentNameMock{substrateMock: &substrateMock{}}
	s := &Server{connector: m}
	for _, c := range []call{
		{"Document", `{"op":"query_documents","scope":"user"}`},
		{"Document", `{"op":"get_document","id":"d"}`}, // Document's default scope is user
		{"Path", `{"op":"ls","scope":"user","path":"/"}`},
		{"Memory", `{"op":"get","scope":"tenant","key":"k"}`},
	} {
		if err := invoke(s, member, c); err != nil {
			t.Errorf("member %s %s: %v", c.rpc, c.input, err)
		}
	}
	for _, name := range m.reached {
		if name != "" {
			t.Errorf("a member's call reached the tool with agent name %q, want none", name)
		}
	}
}
