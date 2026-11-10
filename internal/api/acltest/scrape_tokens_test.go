package acltest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	googlegrpc "google.golang.org/grpc"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// newTokenEnv is newEnv with the token tool wired, as boot wires it.
func newTokenEnv(t *testing.T, mode authz.Mode) *env {
	t.Helper()
	return newEnvWith(t, mode, func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
		hasher, err := cfg.Env.TokenHasher()
		if err != nil {
			t.Fatal(err)
		}
		tool := &builtin.OperatorTokenDef{Store: st, Hasher: hasher, Audit: audit.NopSink{}}
		return nil, func(s *lchttp.Server) { s.SetOperatorTokenDefTool(tool) }
	})
}

func operatorTokenDefRPC(c loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error) {
	return c.OperatorTokenDef
}

// tokenCall is one OperatorTokenDef input on all three transports.
func tokenCall(input string) call {
	return call{family: "operatortokendef", input: input, method: http.MethodPost, path: "/v1/_operatortokendef", body: input, grpc: operatorTokenDefRPC}
}

// A tenant operator mints its own tenant's scrape token over HTTP, gRPC and
// MCP alike; the token reads only that tenant's series; and every refusal is
// a refusal on every transport.
func TestScrapeTokens_ATenantOperatorMintsOnEveryTransport(t *testing.T) {
	e := newTokenEnv(t, authz.ModeCompat)
	via := map[string]func(string, call) outcome{"http": e.viaHTTP, "grpc": e.viaGRPC, "mcp": e.viaMCP}
	for name, do := range via {
		o := do(operator, tokenCall(`{"op":"create","name":"`+name+`","scopes":["metrics:read"]}`))
		if !o.allowed {
			t.Fatalf("%s create: %s", name, o.raw)
		}
		m, _ := o.body.(map[string]any)
		r, _ := m["restrictions"].(map[string]any)
		if m["tenant_id"] != "acme" || r["tenant_metrics"] != true {
			t.Errorf("%s create: %s", name, o.raw)
		}
		token, _ := m["token"].(string)
		resp, raw := e.do(http.MethodGet, "/metrics", token, "", nil)
		if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), "loomcycle_process_") || strings.Contains(string(raw), "loomcycle_build_info") {
			t.Errorf("%s token's scrape: %d\n%s", name, resp.StatusCode, raw)
		}
	}
	for _, in := range []string{
		`{"op":"create","name":"x","scopes":["substrate:tenant"]}`,
		`{"op":"create","name":"x","scopes":["runs:create"]}`,
		`{"op":"create","name":"x","scopes":["metrics:read","runs:read"]}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"tenant_id":"beta"}`,
		`{"op":"create","name":"x","scopes":["metrics:read"],"tenant_metrics":false}`,
		`{"op":"get","def_id":"def_ops"}`, // the operator's own substrate:tenant token
	} {
		for name, do := range via {
			if o := do(operator, tokenCall(in)); o.allowed {
				t.Errorf("%s %s: allowed: %s", name, in, o.raw)
			}
		}
	}
	// A member reaches none of it, on any transport.
	for name, do := range via {
		if o := do(member, tokenCall(`{"op":"create","name":"m","scopes":["metrics:read"]}`)); o.allowed || o.code != "insufficient_scope" {
			t.Errorf("%s member create: %+v", name, o)
		}
	}
}

// Under enforce the tenant operator's baseline admits its scrape tokens and
// nothing more, and a deny bound to it refuses the mint on every transport.
func TestScrapeTokens_EnforceAdmitsByBaselineAndAnExplicitDenyRefuses(t *testing.T) {
	e := newTokenEnv(t, authz.ModeEnforce)
	via := map[string]func(string, call) outcome{"http": e.viaHTTP, "grpc": e.viaGRPC, "mcp": e.viaMCP}
	for name, do := range via {
		if o := do(delegate, tokenCall(`{"op":"create","name":"e-`+name+`","scopes":["metrics:read"]}`)); !o.allowed {
			t.Errorf("enforce, baseline only, %s create: %s", name, o.raw)
		}
	}
	e.bindDeny("delegate", "no-scrape-mint", `[{"effect":"deny","actions":["mint"],"resources":["principal:token:metrics"]}]`)
	for name, do := range via {
		if o := do(delegate, tokenCall(`{"op":"create","name":"d-`+name+`","scopes":["metrics:read"]}`)); o.allowed || o.code != "acl_denied" {
			t.Errorf("enforce, deny bound, %s create: %+v", name, o)
		}
		// Reading is not minting.
		if o := do(delegate, tokenCall(`{"op":"list"}`)); !o.allowed {
			t.Errorf("enforce, deny bound, %s list: %s", name, o.raw)
		}
	}
}
