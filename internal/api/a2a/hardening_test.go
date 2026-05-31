package a2a

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
)

// TestServer_GRPCDisabledUnderHostPathTenancy pins the fail-closed gRPC
// gate: under host/path tenancy the routed tenant cannot be derived from
// the gRPC transport, so the binding must NOT be registered and the served
// card must NOT advertise it (otherwise a peer could spoof its tenant via
// the gRPC request body). REST + JSON-RPC remain.
func TestServer_GRPCDisabledUnderHostPathTenancy(t *testing.T) {
	for _, tenancy := range []string{"host", "path"} {
		srv := newTestServer(t, tenancy, "https://agents.example")
		if srv.grpcEnabled {
			t.Errorf("tenancy=%s: grpcEnabled=true, want false", tenancy)
		}
		if srv.grpc != nil {
			t.Errorf("tenancy=%s: gRPC handler built despite disabled binding", tenancy)
		}
		card := buildAgentCard(fixtureCard(), "https://agents.example", "", false, srv.grpcEnabled)
		if len(card.SupportedInterfaces) != 2 {
			t.Errorf("tenancy=%s: card advertises %d interfaces, want 2 (REST+JSON-RPC, no gRPC)", tenancy, len(card.SupportedInterfaces))
		}
		for _, iface := range card.SupportedInterfaces {
			if iface.ProtocolBinding == a2asdk.TransportProtocolGRPC {
				t.Errorf("tenancy=%s: card still advertises the gRPC interface", tenancy)
			}
		}
	}
}

// TestServer_GRPCEnabledUnderSingleTenant confirms the binding is served in
// none/single-tenant mode (the common deployment), with all 3 interfaces.
func TestServer_GRPCEnabledUnderSingleTenant(t *testing.T) {
	srv := newTestServer(t, "none", "https://agents.example")
	if !srv.grpcEnabled || srv.grpc == nil {
		t.Fatal("none tenancy must serve the gRPC binding")
	}
	card := buildAgentCard(fixtureCard(), "https://agents.example", "", false, srv.grpcEnabled)
	if len(card.SupportedInterfaces) != 3 {
		t.Errorf("none tenancy: card advertises %d interfaces, want 3", len(card.SupportedInterfaces))
	}
}

// TestCapBody_RejectsOversizedBody pins the unauthenticated-body cap: a
// body over maxA2ABodyBytes must fail the handler's read rather than being
// buffered in full. Regression-grade: without capBody, io.ReadAll succeeds.
func TestCapBody_RejectsOversizedBody(t *testing.T) {
	var readErr error
	h := capBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	body := bytes.NewReader(make([]byte, maxA2ABodyBytes+1))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", body))
	if readErr == nil {
		t.Fatal("capBody allowed an over-cap body to be read in full")
	}
}

// TestCapBody_AllowsUnderCapBody confirms a normal small body passes.
func TestCapBody_AllowsUnderCapBody(t *testing.T) {
	var n int
	h := capBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n = len(b)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, 1024))))
	if n != 1024 {
		t.Errorf("read %d bytes, want 1024", n)
	}
}
