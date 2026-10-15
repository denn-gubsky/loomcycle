package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A peer's credential that is a stored reference is resolved for the peer,
// never sent as written: for its endpoint when it is named by one, for no
// URL when it is found through its card (only an unrestricted credential
// then resolves). An unresolved one, or no resolver, refuses the call.
func TestTool_AStoredCredentialIsResolvedForThePeer(t *testing.T) {
	type seen struct{ s, target string }
	var got []seen
	resolver := func(_ context.Context, s, target string) (string, []string, error) {
		got = append(got, seen{s, target})
		if strings.Contains(s, "$cred:gone") {
			return s, []string{"gone"}, nil
		}
		return strings.ReplaceAll(s, "$cred:peer", "resolved-value"), nil, nil
	}
	auth := config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer_tok"}
	endpoint := config.A2AAgent{Endpoint: "https://peer.example/a2a", Binding: "jsonrpc", Auth: auth}
	card := config.A2AAgent{AgentCardURL: "https://peer.example/.well-known/agent-card.json", Auth: auth}
	call := func(def config.A2AAgent, cred string, opts ...Option) (*fakePeer, tools.Result) {
		t.Helper()
		fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}
		tool := NewTool("peer", "skill", "", staticResolver(def), factoryFor(fp), nil)
		for _, o := range opts {
			o(tool)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserCredentials: map[string]string{"peer_tok": cred}})
		res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		return fp, res
	}

	fp, res := call(endpoint, "$cred:peer", WithCredentialResolver(resolver))
	if res.IsError || fp.gotBearer != "resolved-value" {
		t.Fatalf("an endpoint peer: %q, bearer %q", res.Text, fp.gotBearer)
	}
	fp, res = call(card, "$cred:peer", WithCredentialResolver(resolver))
	if res.IsError || fp.gotBearer != "resolved-value" {
		t.Fatalf("a card peer: %q, bearer %q", res.Text, fp.gotBearer)
	}
	if len(got) != 2 || got[0].target != "https://peer.example/a2a" || got[1].target != "" {
		t.Errorf("resolved for %v, want the endpoint, then no URL for the card peer", got)
	}

	for name, tc := range map[string]struct {
		cred string
		opts []Option
	}{
		"unresolved":  {"$cred:gone", []Option{WithCredentialResolver(resolver)}},
		"no resolver": {"$cred:peer", nil},
	} {
		fp, res := call(endpoint, tc.cred, tc.opts...)
		if !res.IsError || fp.gotReq != nil || fp.gotBearer != "" {
			t.Errorf("%s: %q, sent=%t bearer %q; want the call refused", name, res.Text, fp.gotReq != nil, fp.gotBearer)
		}
	}
	// A literal credential is sent as before, without the resolver.
	if fp, res := call(endpoint, "literal-bearer"); res.IsError || fp.gotBearer != "literal-bearer" {
		t.Errorf("a literal credential: %q, bearer %q", res.Text, fp.gotBearer)
	}
}

// A request pinned by a restricted credential follows no redirect.
func TestHardenedPeerClient_APinnedRequestFollowsNoRedirect(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the pinned request reached the redirect target")
	}))
	defer sink.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer hop.Close()
	c := hardenedPeerClient(peerCallTimeout, []string{"127.0.0.1"})
	ctx := netguard.WithPinnable(context.Background())
	netguard.Pin(ctx)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hop.URL, nil)
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a pinned request was redirected")
	}
	if !strings.Contains(err.Error(), "not following a redirect") {
		t.Fatalf("the pinned request failed for another reason: %v", err)
	}
	// The control: unpinned, the same client follows it.
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, hop.URL, nil)
	var sinkHit atomic.Bool
	sink.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sinkHit.Store(true) })
	if resp, err := c.Do(req); err != nil {
		t.Fatalf("an unpinned request: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	if !sinkHit.Load() {
		t.Error("the unpinned control did not follow the redirect")
	}
}
