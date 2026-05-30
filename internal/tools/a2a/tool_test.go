package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fakePeer is an in-memory peerClient that records the request it was
// sent and replies with a scripted result, so the Tool's dispatch +
// credential-resolution logic is testable without a real A2A server.
type fakePeer struct {
	gotReq    *a2asdk.SendMessageRequest
	gotBearer string
	result    a2asdk.SendMessageResult
	sendErr   error
	closed    bool
}

func (f *fakePeer) SendMessage(ctx context.Context, req *a2asdk.SendMessageRequest) (a2asdk.SendMessageResult, error) {
	f.gotReq = req
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return f.result, nil
}

func (f *fakePeer) Close() error { f.closed = true; return nil }

// factoryFor returns a peerClientFactory that always yields fp and
// records the bearer it was constructed with (so a test can assert the
// resolved credential reached the transport seam).
func factoryFor(fp *fakePeer) peerClientFactory {
	return func(ctx context.Context, def config.A2AAgent, bearer string) (peerClient, error) {
		fp.gotBearer = bearer
		return fp, nil
	}
}

// staticResolver resolves any name to def (ok=true), mimicking an
// operator-registered peer.
func staticResolver(def config.A2AAgent) DefResolver {
	return func(ctx context.Context, name string) (config.A2AAgent, bool) {
		return def, true
	}
}

// TestTool_NameIsPeerSkillShape pins the synthetic tool name to the
// `a2a__<peer>__<skill>` form, mirroring `mcp__<server>__<tool>`.
func TestTool_NameIsPeerSkillShape(t *testing.T) {
	tool := NewTool("acme-peer", "research", "", staticResolver(config.A2AAgent{}), factoryFor(&fakePeer{}), nil)
	if got, want := tool.Name(), "a2a__acme-peer__research"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

// TestTool_ExecuteProxiesMessageAndResolvesCredential asserts the happy
// path: the tool resolves the bearer from the run's UserCredentials,
// passes it to the client factory, sends the message tagged with the
// remote skill id, and returns the peer's reply text.
func TestTool_ExecuteProxiesMessageAndResolvesCredential(t *testing.T) {
	fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("peer says hi"))}
	def := config.A2AAgent{
		Endpoint: "https://peer.example/a2a",
		Binding:  "jsonrpc",
		Auth:     config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer_tok"},
	}
	tool := NewTool("acme-peer", "research", "", staticResolver(def), factoryFor(fp), nil)

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserCredentials: map[string]string{"peer_tok": "s3cr3t-bearer"},
	})
	res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hello peer"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Text)
	}
	if res.Text != "peer says hi" {
		t.Errorf("result text = %q, want %q", res.Text, "peer says hi")
	}
	// Credential resolved from UserCredentials reached the factory.
	if fp.gotBearer != "s3cr3t-bearer" {
		t.Errorf("factory bearer = %q, want the resolved credential", fp.gotBearer)
	}
	// Message carried + tagged with the remote skill id.
	if fp.gotReq == nil || fp.gotReq.Message == nil {
		t.Fatal("peer received no message")
	}
	if sid, _ := fp.gotReq.Message.Metadata["skillId"].(string); sid != "research" {
		t.Errorf("message skillId = %v, want research", fp.gotReq.Message.Metadata["skillId"])
	}
	if !fp.closed {
		t.Error("peer client was not closed after the call")
	}
}

// TestTool_AbsentCredentialIsClearErrorNotEmptyBearer asserts the slice
// contract: a peer that DECLARES auth but whose run lacks the credential
// fails with a clear tool error — never a silent empty bearer that would
// call the peer unauthenticated. The factory must not be reached.
func TestTool_AbsentCredentialIsClearErrorNotEmptyBearer(t *testing.T) {
	fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("should not happen"))}
	called := false
	factory := func(ctx context.Context, def config.A2AAgent, bearer string) (peerClient, error) {
		called = true
		return fp, nil
	}
	def := config.A2AAgent{
		Auth: config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer_tok"},
	}
	tool := NewTool("acme-peer", "research", "", staticResolver(def), factory, nil)

	// Run identity carries NO "peer_tok" credential.
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{})
	res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing required credential")
	}
	if !strings.Contains(res.Text, "peer_tok") || !strings.Contains(res.Text, "absent") {
		t.Errorf("error text %q should name the missing credential ref and say it is absent", res.Text)
	}
	if called {
		t.Error("client factory must NOT be reached when the required credential is absent (would risk an empty bearer)")
	}
}

// TestTool_OpenPeerNeedsNoCredential asserts a peer that declares no auth
// scheme is called with an empty bearer and no error — open peers are
// valid.
func TestTool_OpenPeerNeedsNoCredential(t *testing.T) {
	fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}
	tool := NewTool("open-peer", "ping", "", staticResolver(config.A2AAgent{}), factoryFor(fp), nil)

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Text)
	}
	if fp.gotBearer != "" {
		t.Errorf("open peer should be called with empty bearer, got %q", fp.gotBearer)
	}
}

// TestTool_FailedPeerTaskSurfacesIsError asserts a peer task that
// terminates FAILED is reported as an error result so the model can
// self-correct.
func TestTool_FailedPeerTaskSurfacesIsError(t *testing.T) {
	fp := &fakePeer{result: &a2asdk.Task{
		ID:     "t1",
		Status: a2asdk.TaskStatus{State: a2asdk.TaskStateFailed, Message: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("peer failed"))},
	}}
	tool := NewTool("peer", "skill", "", staticResolver(config.A2AAgent{}), factoryFor(fp), nil)

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"go"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("FAILED peer task should yield IsError result, got %q", res.Text)
	}
}

// TestTool_EmptyMessageRejected asserts the input guard rejects an empty
// message field before any peer call.
func TestTool_EmptyMessageRejected(t *testing.T) {
	tool := NewTool("peer", "skill", "", staticResolver(config.A2AAgent{}), factoryFor(&fakePeer{}), nil)
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"   "}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty message should be an error result")
	}
}
