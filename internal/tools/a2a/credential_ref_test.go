package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// substCall is one call the fake credential substitute received.
type substCall struct {
	value, url, user, agent string
}

// fakeSubst resolves "$cred:tok" to a value that names the scope it was
// read at, so a test can tell the tenant's credential from a user's.
type fakeSubst struct {
	mu    sync.Mutex
	calls []substCall
	err   error
}

func (f *fakeSubst) fn(ctx context.Context, s, targetURL string) (string, []string, error) {
	ri := tools.RunIdentity(ctx)
	f.mu.Lock()
	f.calls = append(f.calls, substCall{value: s, url: targetURL, user: ri.UserID, agent: tools.AgentName(ctx)})
	f.mu.Unlock()
	if f.err != nil {
		return "", nil, f.err
	}
	if s != "$cred:tok" {
		return s, []string{"unknown"}, nil
	}
	if ri.UserID != "" {
		return ri.UserID + "s-own-value", nil, nil
	}
	return "tenant-value", nil, nil
}

func refPeerDef() config.A2AAgent {
	return config.A2AAgent{
		Endpoint: "https://peer.example/a2a",
		Binding:  "jsonrpc",
		Auth:     config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer_tok"},
	}
}

// A peer's bearer that the run carries as a stored-credential reference is
// resolved for the URL the call goes to — never sent as written. For a run a
// webhook or a schedule started it resolves at tenant level only; a run its
// user started resolves it for its full identity, as the MCP client does.
func TestTool_AStoredBearerIsResolvedForTheCallsURL(t *testing.T) {
	for _, atTenant := range []bool{true, false} {
		fs := &fakeSubst{}
		fp := &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}
		tool := NewTool("acme-peer", "research", "", staticResolver(refPeerDef()), factoryFor(fp), fs.fn, nil)

		ctx := tools.WithAgentName(context.Background(), "worker")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
			TenantID: "t", UserID: "bob",
			UserCredentials:        map[string]string{"peer_tok": "$cred:tok"},
			CredentialRefsAtTenant: atTenant,
		})
		res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hello"}`))
		if err != nil || res.IsError {
			t.Fatalf("atTenant=%v: Execute = %v / %q", atTenant, err, res.Text)
		}
		want := "bobs-own-value"
		if atTenant {
			want = "tenant-value"
		}
		if fp.gotBearer != want {
			t.Errorf("atTenant=%v: bearer = %q, want %q", atTenant, fp.gotBearer, want)
		}
		if len(fs.calls) != 1 || fs.calls[0].url != "https://peer.example/a2a" {
			t.Fatalf("atTenant=%v: substitute calls = %+v, want one for the peer's URL", atTenant, fs.calls)
		}
		if atTenant && (fs.calls[0].user != "" || fs.calls[0].agent != "") {
			t.Errorf("a trigger run's reference was resolved for user %q / agent %q, want the tenant only", fs.calls[0].user, fs.calls[0].agent)
		}
	}
}

// A credential restricted by allowed_urls pins the request it is resolved
// for, and the engine refuses one on a context that cannot be pinned (the
// caller would have no way to stop a redirect). The tool makes the call's
// context pinnable before the bearer resolves, so the call goes out pinned.
func TestTool_ARestrictedStoredBearerPinsTheCall(t *testing.T) {
	restricted := func(ctx context.Context, s, targetURL string) (string, []string, error) {
		if !netguard.Pin(ctx) {
			return "", nil, errors.New("not pinnable")
		}
		return "restricted-value", nil, nil
	}
	var pinned bool
	factory := func(ctx context.Context, def config.A2AAgent, bearer bearerFunc) (peerClient, error) {
		if _, err := bearer(ctx, def.Endpoint); err != nil {
			return nil, err
		}
		pinned = netguard.Pinned(ctx)
		return &fakePeer{result: a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart("ok"))}, nil
	}
	tool := NewTool("acme-peer", "research", "", staticResolver(refPeerDef()), factory, restricted, nil)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserCredentials: map[string]string{"peer_tok": "$cred:tok"},
	})
	res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hello"}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute = %v / %q, want the restricted bearer resolved", err, res.Text)
	}
	if !pinned {
		t.Error("the call carrying a restricted credential is not pinned, so it would follow a redirect")
	}
}

// A stored bearer the server cannot resolve refuses the call — before any
// client is built — rather than sending "$cred:<name>" to the peer.
func TestTool_AStoredBearerWithNoResolverIsRefused(t *testing.T) {
	called := false
	factory := func(context.Context, config.A2AAgent, bearerFunc) (peerClient, error) {
		called = true
		return &fakePeer{}, nil
	}
	tool := NewTool("acme-peer", "research", "", staticResolver(refPeerDef()), factory, nil, nil)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserCredentials: map[string]string{"peer_tok": "$cred:tok"},
	})
	res, err := tool.Execute(ctx, json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryBusiness {
		t.Fatalf("result = %+v, want a classified refusal", res)
	}
	if called {
		t.Error("the peer client was built for a bearer that cannot be resolved")
	}
	if strings.Contains(res.Text, "$cred:tok") {
		t.Errorf("the refusal repeats the reference: %q", res.Text)
	}
}

// The interceptor asks for the bearer with the URL the SDK is about to call
// (for a card-discovered peer, the endpoint the card names) and fails the
// call when it cannot be resolved there: no unauthenticated send, and the
// failure keeps the run-credential category.
func TestBearerInterceptor_ResolvesForTheRequestsURL(t *testing.T) {
	var asked string
	ok := &bearerInterceptor{bearer: func(_ context.Context, baseURL string) (string, error) {
		asked = baseURL
		return "the-value", nil
	}}
	req := &a2aclient.Request{BaseURL: "https://card-named.example/rpc", ServiceParams: a2aclient.ServiceParams{}}
	if _, _, err := ok.Before(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if asked != "https://card-named.example/rpc" {
		t.Errorf("resolved for %q, want the request's URL", asked)
	}
	if got := req.ServiceParams["Authorization"]; len(got) != 1 || got[0] != "Bearer the-value" {
		t.Errorf("Authorization = %v", got)
	}

	failing := &bearerInterceptor{bearer: func(context.Context, string) (string, error) {
		return "", tools.ErrRunCredentialUnavailable
	}}
	req = &a2aclient.Request{BaseURL: "https://elsewhere.example", ServiceParams: a2aclient.ServiceParams{}}
	if _, _, err := failing.Before(context.Background(), req); !errors.Is(err, tools.ErrRunCredentialUnavailable) {
		t.Errorf("Before = %v, want the run-credential error", err)
	}
	if _, set := req.ServiceParams["Authorization"]; set {
		t.Error("a bearer that did not resolve still set Authorization")
	}
}

// A request carrying a credential restricted to its URL (pinned) follows no
// redirect; an unpinned one still follows within the hop limit.
func TestHardenedPeerClient_APinnedRequestFollowsNoRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()
	hc := hardenedPeerClient(10*time.Second, []string{"127.0.0.0/8"})

	get := func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirector.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(context.Background()); err != nil {
		t.Fatalf("an unpinned request: %v", err)
	}
	pinned := netguard.WithPinnable(context.Background())
	netguard.Pin(pinned)
	if err := get(pinned); !errors.Is(err, netguard.ErrPinnedRedirect) {
		t.Errorf("a pinned request: err = %v, want ErrPinnedRedirect", err)
	}
}
