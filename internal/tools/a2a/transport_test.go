package a2a

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsPrivatePeerIP table-checks the SSRF classifier, including the cloud
// metadata service address that is the highest-value SSRF target.
func TestIsPrivatePeerIP(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.169.254", true}, // AWS/GCP metadata service
		{"10.1.2.3", true},
		{"192.168.0.1", true},
		{"172.16.5.5", true},
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}
	for _, c := range cases {
		got := isPrivatePeerIP(net.ParseIP(c.ip))
		if got != c.private {
			t.Errorf("isPrivatePeerIP(%s) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// TestPeerDialContext_RefusesPrivateAddress proves the dialer refuses a
// loopback/private target before connecting — the SSRF block.
func TestPeerDialContext_RefusesPrivateAddress(t *testing.T) {
	_, err := peerDialContext(context.Background(), "tcp", "127.0.0.1:9")
	if err == nil {
		t.Fatal("peerDialContext dialed a loopback address; SSRF block missing")
	}
	if !strings.Contains(err.Error(), "no public addresses") && !strings.Contains(err.Error(), "private") {
		t.Errorf("unexpected error %v, want an SSRF-refusal", err)
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

	_, err := fetchPeerCard(context.Background(), srv.URL)
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
