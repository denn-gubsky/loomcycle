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

// A peer credential that holds an $oauth: reference goes through the
// resolver like a $cred: one — never out as written — and an unresolved one
// refuses the call. The run's user_bearer may not be one: it takes a $cred:
// reference or a literal.
func TestA2AOpenStoredBearer_OAuthRefNeverSentAsWritten(t *testing.T) {
	resolver := func(_ context.Context, s, _ string) (string, []string, error) {
		return s, []string{"docs"}, nil // nothing resolves $oauth: yet
	}
	call := func(ref string, ident tools.RunIdentityValue) (*fakePeer, tools.Result) {
		t.Helper()
		def := config.A2AAgent{Endpoint: "https://peer.example/a2a", Binding: "jsonrpc", Auth: config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: ref}}
		fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}
		tool := NewTool("peer", "skill", "", staticResolver(def), factoryFor(fp), nil)
		WithCredentialResolver(resolver)(tool)
		res, err := tool.Execute(tools.WithRunIdentity(context.Background(), ident), json.RawMessage(`{"message":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		return fp, res
	}
	fp, res := call("peer_tok", tools.RunIdentityValue{UserCredentials: map[string]string{"peer_tok": "$oauth:docs"}})
	if !res.IsError || fp.gotReq != nil || strings.Contains(fp.gotBearer, "$oauth:") {
		t.Errorf("an unresolved $oauth: credential: %q, sent=%t bearer %q; want the call refused", res.Text, fp.gotReq != nil, fp.gotBearer)
	}
	fp, res = call("default", tools.RunIdentityValue{UserID: "alice", UserBearer: "$oauth:docs",
		UserCredentials: map[string]string{"default": "$oauth:docs"}})
	if !res.IsError || fp.gotReq != nil {
		t.Errorf("an $oauth: user_bearer: %q, sent=%t; want the call refused", res.Text, fp.gotReq != nil)
	}
}
