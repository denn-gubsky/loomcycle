package a2a

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestHardenedPeerClient_RefusesEveryPrivateRangeTheGuardRefuses: the peer
// client dials through the shared guard, so it refuses the ranges a local
// classifier copy used to miss: CGNAT (tailnet peers, Alibaba metadata) and
// the metadata service spelled as NAT64. The refusal must be the guard's, not
// a timeout from actually trying.
func TestHardenedPeerClient_RefusesEveryPrivateRangeTheGuardRefuses(t *testing.T) {
	for _, target := range []string{
		"http://127.0.0.1:9/",
		"http://169.254.169.254:9/",
		"http://100.100.100.200:9/",
		"http://[64:ff9b::a9fe:a9fe]:9/",
	} {
		resp, err := hardenedPeerClient(2*time.Second, nil).Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: peer client connected; SSRF block missing", target)
			continue
		}
		if !strings.Contains(err.Error(), "blocked:") {
			t.Errorf("%s: err = %v, want the guard's refusal", target, err)
		}
	}
}

// TestFetchPeerCard_PrivateAllowlistAdmitsVouchedPeer: a peer on a private
// network is reachable once the operator vouches for it, by host or CIDR range,
// exactly as for the HTTP tool; a range entry does not admit other addresses.
func TestFetchPeerCard_PrivateAllowlistAdmitsVouchedPeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"sibling","version":"1.0.0"}`))
	}))
	defer srv.Close()

	if _, err := fetchPeerCard(context.Background(), srv.URL, []string{"100.64.0.0/10"}); err == nil {
		t.Fatal("a CIDR entry for another range admitted a loopback peer")
	}
	card, err := fetchPeerCard(context.Background(), srv.URL, []string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("vouched loopback peer refused: %v", err)
	}
	if card.Name != "sibling" {
		t.Fatalf("card name = %q, want sibling", card.Name)
	}
}

// TestRegisterTools_PeerFactoryCarriesOperatorPrivateAllowlist: the production
// wiring hands the operator's private-host allowlist to the peer client.
func TestRegisterTools_PeerFactoryCarriesOperatorPrivateAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"sibling","version":"1.0.0"}`))
	}))
	defer srv.Close()

	def := config.A2AAgent{AgentCardURL: srv.URL, ExpectedSkills: []config.A2AExpectedSkill{{ID: "ping"}}}
	// newPeer builds the client from the fetched card; this minimal card has no
	// interfaces, so building may fail after the fetch. Only a guard refusal
	// says whether the allowlist reached the dialer.
	newPeerErr := func(allow []string) error {
		cfg := &config.Config{A2AAgents: map[string]config.A2AAgent{"sib": {}}}
		cfg.Env.HTTPPrivateHostAllowlist = allow
		got := RegisterTools(context.Background(), cfg, nil, staticResolver(def), nil, nil)
		if len(got) != 1 {
			t.Fatalf("registered %d tools, want 1", len(got))
		}
		peer, err := got[0].(*Tool).newPeer(context.Background(), def, "")
		if err == nil {
			_ = peer.Close()
		}
		return err
	}
	if err := newPeerErr(nil); err == nil || !strings.Contains(err.Error(), "blocked:") {
		t.Fatalf("no allowlist: err = %v, want the guard's refusal", err)
	}
	if err := newPeerErr([]string{"127.0.0.0/8"}); err != nil && strings.Contains(err.Error(), "blocked:") {
		t.Fatalf("operator allowlist did not reach the peer dialer: %v", err)
	}
}

// TestFetchPeerCard_SSRFBlocksLoopbackPeer is regression-grade: an
// httptest server listens on loopback, and fetchPeerCard must REFUSE to
// reach it. On the unfixed code (plain http.Client) the fetch would
// succeed — reaching an internal address chosen via a model-authored
// agent_card_url.
func TestFetchPeerCard_SSRFBlocksLoopbackPeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"evil","version":"1.0.0"}`))
	}))
	defer srv.Close()

	_, err := fetchPeerCard(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("fetchPeerCard reached a loopback peer; SSRF block missing")
	}
}

// TestBodyLimitRoundTripper_ErrorsPastCap proves an over-cap response body
// fails loudly with errResponseTooLarge rather than being read unbounded.
func TestBodyLimitRoundTripper_ErrorsPastCap(t *testing.T) {
	rt := &bodyLimitRoundTripper{base: &stubRoundTripper{bodyLen: maxPeerResponseBytes + 1024}, limit: maxPeerResponseBytes}
	resp, err := rt.RoundTrip(httptest.NewRequest(http.MethodGet, "http://peer/x", nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("read err = %v, want errResponseTooLarge", err)
	}
}

// TestBodyLimitRoundTripper_PassesUnderCap confirms a normal small body is
// read through unchanged.
func TestBodyLimitRoundTripper_PassesUnderCap(t *testing.T) {
	rt := &bodyLimitRoundTripper{base: &stubRoundTripper{bodyLen: 512}, limit: maxPeerResponseBytes}
	resp, err := rt.RoundTrip(httptest.NewRequest(http.MethodGet, "http://peer/x", nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != 512 {
		t.Errorf("read %d bytes, want 512", len(body))
	}
}

// stubRoundTripper returns a response whose body is bodyLen zero bytes,
// without any real network — lets the cap be tested in isolation.
type stubRoundTripper struct{ bodyLen int }

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", s.bodyLen))),
		Header:     make(http.Header),
	}, nil
}

// TestNewSDKPeerClient_GRPCBindingIsNeverDialed pins why a gRPC peer needs no
// dial-time SSRF guard today: no gRPC client transport is registered (the SDK
// defaults are JSON-RPC and REST, and only those two get the guarded client),
// so a grpc binding fails at transport selection, before any connection. The
// card path selects transports the same way, so a card advertising only gRPC
// fails the same. If this starts building a client, a gRPC transport was
// wired: route its dial through the guard (grpc.WithContextDialer over
// netguard.GuardedDialContext with the operator's private-host allowlist)
// before shipping it, and replace this test with one that proves the refusal.
func TestNewSDKPeerClient_GRPCBindingIsNeverDialed(t *testing.T) {
	cl, err := newSDKPeerClient(context.Background(), config.A2AAgent{Endpoint: "169.254.169.254:443", Binding: "grpc"}, "", nil)
	if err == nil {
		_ = cl.Close()
		t.Fatal("a grpc binding built a peer client: a gRPC transport is wired, and it does not dial through the SSRF guard")
	}
	if !strings.Contains(err.Error(), "no compatible transports") {
		t.Fatalf("err = %v, want the SDK's transport-selection refusal (anything else may mean a dial was attempted)", err)
	}
}
