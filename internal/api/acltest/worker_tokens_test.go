package acltest

import (
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC GW Q2 on every transport: the operator mints a tenant-bound and a
// shared workspace-worker token, the minted token opens no API surface, and a
// tenant operator mints neither.
func TestWorkerTokens_AnAdminMintsSharedAndATenantOperatorCannot(t *testing.T) {
	e := newTokenEnv(t, authz.ModeCompat)
	via := map[string]func(string, call) outcome{"http": e.viaHTTP, "grpc": e.viaGRPC, "mcp": e.viaMCP}
	for name, do := range via {
		for _, c := range []struct {
			in         string
			wantShared bool
		}{
			{`{"op":"create","name":"wk-` + name + `","tenant_id":"acme","scopes":["workers:attach"]}`, false},
			{`{"op":"create","name":"pool-` + name + `","tenant_id":"ops","scopes":["workers:attach"],"worker_shared":true}`, true},
		} {
			o := do(legacy, tokenCall(c.in))
			if !o.allowed {
				t.Fatalf("%s %s: %s", name, c.in, o.raw)
			}
			m, _ := o.body.(map[string]any)
			r, _ := m["restrictions"].(map[string]any)
			if (r["worker_shared"] == true) != c.wantShared {
				t.Errorf("%s %s: %s, want worker_shared=%v", name, c.in, o.raw, c.wantShared)
			}
			token, _ := m["token"].(string)
			for _, p := range []struct{ method, path string }{{http.MethodGet, "/metrics"}, {http.MethodGet, "/v1/_me"}, {http.MethodGet, "/v1/runs"}} {
				if resp, raw := e.do(p.method, p.path, token, "", nil); resp.StatusCode != http.StatusForbidden {
					t.Errorf("%s worker token on %s %s: %d %s, want 403", name, p.method, p.path, resp.StatusCode, raw)
				}
			}
		}
	}
	for _, in := range []string{
		`{"op":"create","name":"w","scopes":["workers:attach"]}`,
		`{"op":"create","name":"w","scopes":["workers:attach"],"worker_shared":true}`,
		`{"op":"create","name":"w","scopes":["metrics:read"],"worker_shared":true}`,
	} {
		for name, do := range via {
			if o := do(operator, tokenCall(in)); o.allowed {
				t.Errorf("%s tenant operator %s: allowed: %s", name, in, o.raw)
			}
		}
	}
	for name, do := range via {
		if o := do(legacy, tokenCall(`{"op":"create","name":"x-`+name+`","tenant_id":"acme","scopes":["substrate:tenant"],"worker_shared":true}`)); o.allowed {
			t.Errorf("%s worker_shared on a tenant token: allowed: %s", name, o.raw)
		}
	}
}
