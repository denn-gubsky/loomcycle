package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// substrateMock is a programmable Connector stub purpose-built
// for the substrate RPC tests. Embeds mockConnector so the
// unrelated methods come for free; overrides AgentDef + SkillDef
// to stage inputs/outputs.
type substrateMock struct {
	mockConnector

	gotAgentDefInput json.RawMessage
	gotSkillDefInput json.RawMessage

	agentDefResult connector.ToolResult
	skillDefResult connector.ToolResult

	agentDefErr error
	skillDefErr error
}

func (m *substrateMock) AgentDef(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	m.gotAgentDefInput = in
	return m.agentDefResult, m.agentDefErr
}

func (m *substrateMock) SkillDef(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	m.gotSkillDefInput = in
	return m.skillDefResult, m.skillDefErr
}

func TestGrpcAgentDef_HappyPath(t *testing.T) {
	mc := &substrateMock{
		agentDefResult: connector.ToolResult{
			Text:    `{"def_id":"def_abc","name":"reviewer","version":1}`,
			IsError: false,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.AgentDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: []byte(`{"op":"create","name":"reviewer","overlay":{"system_prompt":"hi"}}`),
	})
	if err != nil {
		t.Fatalf("AgentDef: %v", err)
	}
	if resp.GetIsError() {
		t.Errorf("is_error = true, want false")
	}
	if string(mc.gotAgentDefInput) == "" {
		t.Errorf("connector wasn't called with the input")
	}
	if string(resp.GetOutputJson()) != `{"def_id":"def_abc","name":"reviewer","version":1}` {
		t.Errorf("output_json = %s", resp.GetOutputJson())
	}
}

func TestGrpcSkillDef_HappyPath(t *testing.T) {
	mc := &substrateMock{
		skillDefResult: connector.ToolResult{
			Text:    `{"def_id":"sdf_abc","name":"voice-applier","version":1}`,
			IsError: false,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.SkillDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: []byte(`{"op":"create","name":"voice-applier","overlay":{"body":"VOICE"}}`),
	})
	if err != nil {
		t.Fatalf("SkillDef: %v", err)
	}
	if resp.GetIsError() {
		t.Errorf("is_error = true, want false")
	}
	if string(resp.GetOutputJson()) != `{"def_id":"sdf_abc","name":"voice-applier","version":1}` {
		t.Errorf("output_json = %s", resp.GetOutputJson())
	}
}

func TestGrpcSubstrate_PropagatesToolRefusal(t *testing.T) {
	mc := &substrateMock{
		skillDefResult: connector.ToolResult{
			Text:    "skill 'foo' refused: empty body",
			IsError: true,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.SkillDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: []byte(`{"op":"create","name":"foo","overlay":{"body":""}}`),
	})
	if err != nil {
		t.Fatalf("SkillDef: %v", err)
	}
	if !resp.GetIsError() {
		t.Errorf("is_error = false, want true for tool refusal")
	}
	if string(resp.GetOutputJson()) == "" {
		t.Error("expected error text in output_json")
	}
}

func TestGrpcSubstrate_RejectsEmptyInput(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &substrateMock{})
	defer cleanup()

	_, err := client.SkillDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: nil,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument; got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("status code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGrpcSubstrate_RejectsMalformedJSON(t *testing.T) {
	client, cleanup := startTestServerWithConnector(t, &substrateMock{})
	defer cleanup()

	_, err := client.AgentDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: []byte(`not json`),
	})
	if err == nil {
		t.Fatal("expected InvalidArgument; got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("status code = %v, want InvalidArgument", status.Code(err))
	}
}

// realToolConnector is a tiny Connector that dispatches SkillDef
// to a real in-process *builtin.SkillDef tool. Used by the ctx-
// synthesis regression test below — substrateMock bypasses the
// in-process policy gate, which is exactly what we DON'T want to
// test here. This mock makes the policy gate fire if-and-only-if
// the gRPC handler forgets to stamp the operator-trust ctx.
type realToolConnector struct {
	mockConnector

	skillTool *builtin.SkillDef
}

func (c *realToolConnector) SkillDef(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	res, err := c.skillTool.Execute(ctx, in)
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, err
}

// TestGrpcSubstrate_OperatorCtxLetsRealToolThrough is the
// regression test for the CRITICAL bug surfaced by code review:
// without operator-trust ctx synthesis in the gRPC handler, every
// substrate call from gRPC hits the in-process tool's default-deny
// scope gate and returns is_error=true. This test wires a real
// SkillDef tool (not the substrateMock) and asserts a `list` op
// succeeds — proving the ctx synthesis actually delivers the
// "any" scope.
func TestGrpcSubstrate_OperatorCtxLetsRealToolThrough(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	skillTool := &builtin.SkillDef{Store: st}
	mc := &realToolConnector{skillTool: skillTool}

	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	// `list` on an unknown name returns an empty version slice on
	// the happy path (no rows). Anything else means either the
	// scope gate refused (is_error=true) or the dispatcher mis-
	// routed.
	resp, err := client.SkillDef(context.Background(), &loomcyclepb.SubstrateRequest{
		InputJson: []byte(`{"op":"list","name":"unknown-skill"}`),
	})
	if err != nil {
		t.Fatalf("SkillDef: %v", err)
	}
	if resp.GetIsError() {
		t.Errorf("is_error = true with output_json = %s — gRPC ctx synthesis didn't grant scope=[any]", resp.GetOutputJson())
	}
}
