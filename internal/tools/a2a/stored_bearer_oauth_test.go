package a2a

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
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

// A peer credential naming a connected OAuth credential resolves, for the
// peer's endpoint, to the credential's access token.
func TestA2AOpenStoredBearer_ResolvesOAuthRef(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	f := oauthtest.New(t)
	f.Set(func(f *oauthtest.Server) { f.Interval = 1 })
	mgr := credential.NewOAuthManager(engine, f.Client(), "replica-test", nil)
	id := credential.Identity{TenantID: "t", Scope: "tenant", Name: "peer"}
	if _, err := engine.PutOAuth(context.Background(), id, credential.OAuthConfig{Resource: f.Resource(),
		Client: credential.OAuthClientConfig{Registration: credential.OAuthRegistrationDynamic, TokenEndpointAuth: credential.OAuthAuthNone}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if m, _ := mgr.ConnectStatus(context.Background(), id); m.OAuth.Status.State == credential.OAuthConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
	resolver := func(ctx context.Context, s, target string) (string, []string, error) {
		ri := tools.RunIdentity(ctx)
		return mgr.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, target, nil)
	}
	def := config.A2AAgent{Endpoint: f.Resource(), Binding: "jsonrpc", Auth: config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer_tok"}}
	fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}
	tool := NewTool("peer", "skill", "", staticResolver(def), factoryFor(fp), nil)
	WithCredentialResolver(resolver)(tool)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", UserCredentials: map[string]string{"peer_tok": "$oauth:peer"}})
	res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hi"}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %s", err, res.Text)
	}
	if fp.gotBearer != f.LastAccess() {
		t.Error("the peer was not sent the OAuth access token")
	}
}
