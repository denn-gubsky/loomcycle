package acltest

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC FY Part E: the gRPC RPCs and the MCP meta-tools ask the question their
// HTTP twin route asks. Before, MCP asked only `invoke mcp-tool:<name>`, gRPC
// Memory asked `read memory:*` for every op and the channel RPCs the
// type-level question, so a deny the route honoured did not hold there.

// bindMemberDeny writes one list as the operator and binds it to alice.
func (e *env) bindMemberDeny(name, rules string) {
	e.t.Helper()
	if resp, raw := e.do("POST", "/v1/_acldef", operator, `{"op":"create","name":"`+name+`","rules":`+rules+`}`, nil); resp.StatusCode != 201 {
		e.t.Fatalf("create %s: %d %s", name, resp.StatusCode, raw)
	}
	if resp, raw := e.do("POST", "/v1/_aclbinding", operator, `{"acl":"`+name+`","subject_kind":"user","subject_id":"alice"}`, nil); resp.StatusCode != 201 {
		e.t.Fatalf("bind %s: %d %s", name, resp.StatusCode, raw)
	}
}

func asAlice() context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+member)
}

func grpcDenied(err error) bool {
	return err != nil && strings.Contains(status.Convert(err).Message(), "acl_denied")
}

func TestACLTwins_ADeniedCancelHoldsOnEveryTransport(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindMemberDeny("no-cancel", `[{"effect":"deny","actions":["cancel"],"resources":["run:*"]}]`)
	if resp, raw := e.do("POST", "/v1/runs/r_x/cancel", member, `{}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP: %d %s", resp.StatusCode, raw)
	}
	if _, err := e.grpc.CancelTurn(asAlice(), &loomcyclepb.CancelTurnRequest{RunId: "r_x"}); !grpcDenied(err) {
		t.Errorf("gRPC: %v", err)
	}
	if o := e.viaMCP(member, call{family: "cancel_run", input: `{"agent_id":"a_x"}`}); o.code != "acl_denied" {
		t.Errorf("MCP cancel_run: %+v", o)
	}
}

func TestACLTwins_ADeniedMemoryWriteHoldsOnEveryTransport(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindMemberDeny("no-memory-write", `[{"effect":"deny","actions":["write"],"resources":["memory:*"]}]`)
	if resp, raw := e.do("PUT", "/v1/_memory/scopes/user/alice/keys/k", member, `{"value":"x"}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP: %d %s", resp.StatusCode, raw)
	}
	set := `{"op":"set","scope":"user","key":"k","value":"x"}`
	resp, err := e.grpc.Memory(asAlice(), &loomcyclepb.SubstrateRequest{InputJson: []byte(set)})
	if err != nil || !resp.GetIsError() || codeOf(resp.GetOutputJson()) != "acl_denied" {
		t.Errorf("gRPC Memory set: %v %s", err, resp.GetOutputJson())
	}
	if o := e.viaMCP(member, call{family: "memory", input: set}); o.code != "acl_denied" {
		t.Errorf("MCP memory set: %+v", o)
	}
	// A read is not a write: neither transport refuses it on this deny.
	get := `{"op":"get","scope":"user","key":"k"}`
	if resp, err := e.grpc.Memory(asAlice(), &loomcyclepb.SubstrateRequest{InputJson: []byte(get)}); err == nil && codeOf(resp.GetOutputJson()) == "acl_denied" {
		t.Errorf("gRPC Memory get was refused by a write deny: %s", resp.GetOutputJson())
	}
	if o := e.viaMCP(member, call{family: "memory", input: get}); o.code == "acl_denied" {
		t.Errorf("MCP memory get was refused by a write deny: %+v", o)
	}
}

func TestACLTwins_ADeniedChannelPublishHoldsOnEveryTransport(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindMemberDeny("no-alerts", `[{"effect":"deny","actions":["publish"],"resources":["channel:alerts"]}]`)
	if resp, raw := e.do("POST", "/v1/_channels/alerts/publish", member, `{"payload":{"x":1}}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP: %d %s", resp.StatusCode, raw)
	}
	if _, err := e.grpc.PublishChannel(asAlice(), &loomcyclepb.PublishChannelRequest{Channel: "alerts", Scope: "user", ScopeId: "alice", Payload: []byte(`{"x":1}`)}); !grpcDenied(err) {
		t.Errorf("gRPC: %v", err)
	}
	if o := e.viaMCP(member, call{family: "publish_channel", input: `{"channel":"alerts","payload":{"x":1}}`}); !strings.Contains(o.raw, "acl_denied") {
		t.Errorf("MCP publish_channel: %+v", o)
	}
	// Another channel is not covered by the deny.
	if _, err := e.grpc.PublishChannel(asAlice(), &loomcyclepb.PublishChannelRequest{Channel: "news", Scope: "user", ScopeId: "alice", Payload: []byte(`{"x":1}`)}); grpcDenied(err) {
		t.Errorf("gRPC publish to another channel was refused: %v", err)
	}
}
