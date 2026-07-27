package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// configStubConnector stands in for the HTTP Server's connector implementation,
// echoing back the disclosure level it was asked to render so the RPC's plumbing
// can be asserted without standing up a full runtime.
type configStubConnector struct {
	mockConnector
}

func (c *configStubConnector) Config(ctx context.Context) ([]byte, error) {
	view := "admin"
	if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		view = "authenticated"
	}
	return json.Marshal(map[string]any{
		"view":     view,
		"instance": map[string]any{"version": "v1.38.0"},
		"features": map[string]any{"bash": map[string]any{"available": true}},
	})
}

// TestGrpcConfig_ReturnsJSONAtTheCallersLevel is the parity regression for the
// gRPC twin of GET /v1/config: the report comes back as JSON, and the disclosure
// level follows the caller's own scopes rather than being fixed.
func TestGrpcConfig_ReturnsJSONAtTheCallersLevel(t *testing.T) {
	stub := &configStubConnector{}
	adapter := New(Config{Connector: stub, CancelReg: nil})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"admin", scopedCtx("ops", "root", auth.ScopeAdmin), "admin"},
		{"tenant operator", scopedCtx("acme", "op@acme", auth.ScopeTenant), "authenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := adapter.Config(tc.ctx, &loomcyclepb.ConfigRequest{})
			if err != nil {
				t.Fatalf("Config: %v", err)
			}
			var out map[string]any
			if err := json.Unmarshal(resp.GetConfigJson(), &out); err != nil {
				t.Fatalf("config_json is not valid JSON (%q): %v", resp.GetConfigJson(), err)
			}
			if out["view"] != tc.want {
				t.Errorf("view = %v, want %q", out["view"], tc.want)
			}
		})
	}
}

// TestGrpcConfig_PublicViewIsUnreachable: the HTTP surface has a third, narrower
// `public` level reachable with NO bearer under LOOMCYCLE_PUBLIC_CONFIG. It must
// not exist over gRPC — the marker it keys on is stamped only by the HTTP
// middleware, and gRPC authenticates before dispatch. A caller that somehow
// reached this RPC unauthenticated would resolve to open mode (admin), never to
// a level that is DEFINED as "the least entitled reader".
func TestGrpcConfig_PublicViewIsUnreachable(t *testing.T) {
	stub := &configStubConnector{}
	adapter := New(Config{Connector: stub, CancelReg: nil})

	resp, err := adapter.Config(context.Background(), &loomcyclepb.ConfigRequest{})
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if strings.Contains(string(resp.GetConfigJson()), `"public"`) {
		t.Errorf("gRPC rendered the public view: %s", resp.GetConfigJson())
	}
}

// TestGrpcConfig_ScopeIsAnyAuthenticated pins the gate. An UNMAPPED RPC defaults
// to substrate:admin (deny-by-default), which would make the gRPC surface
// stricter than GET /v1/config — whose requiredScopeFor GET default is any
// authenticated principal. The report narrows itself by scope, so a floor would
// deny a caller a view it is entitled to.
func TestGrpcConfig_ScopeIsAnyAuthenticated(t *testing.T) {
	got, mapped := grpcConsumerScopes["Config"]
	if !mapped {
		t.Fatal(`Config is not in grpcConsumerScopes, so it defaults to substrate:admin — stricter than the HTTP route`)
	}
	if got != "" {
		t.Errorf("required scope = %q, want \"\" (any authenticated, matching the HTTP GET default)", got)
	}
}

// TestGrpcConfig_UnwiredConnectorIsUnimplemented: a degraded instance must say so
// rather than panic on a nil connector.
func TestGrpcConfig_UnwiredConnectorIsUnimplemented(t *testing.T) {
	adapter := New(Config{CancelReg: nil})
	if _, err := adapter.Config(context.Background(), &loomcyclepb.ConfigRequest{}); err == nil {
		t.Error("Config with no connector returned no error")
	}
}
