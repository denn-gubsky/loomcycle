package memberpath

import (
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// The gRPC gate answers an RPC's member-path question from its twins (RPC)
// and holds a call to its own operation only for an op-keyed RPC (OpTwin). So
// an RPC whose operation the gate cannot see must have twins that agree, or
// the RPC-level answer would open an operation its route keeps closed.
func TestTwins_AnRPCWithoutOpsHasTwinsThatAgree(t *testing.T) {
	for rpc, ts := range twins {
		if len(ts) == 0 {
			t.Errorf("%s: listed with no twin; list it in noTwin with a reason instead", rpc)
			continue
		}
		keyed := ts[0].Op != ""
		ops := map[string]bool{}
		for _, tw := range ts {
			if (tw.Op != "") != keyed {
				t.Errorf("%s: some twins name an op and some do not", rpc)
			}
			if keyed && ops[tw.Op] {
				t.Errorf("%s: op %q has two twins", rpc, tw.Op)
			}
			ops[tw.Op] = true
		}
		if keyed {
			continue
		}
		first := Route(auth.ScopeTenant, ts[0].Method, ts[0].Path)
		for _, tw := range ts[1:] {
			if Route(auth.ScopeTenant, tw.Method, tw.Path) != first {
				t.Errorf("%s: its twins disagree on the member path (%s %s vs %s %s); name them by op so the gate can tell the calls apart",
					rpc, ts[0].Method, ts[0].Path, tw.Method, tw.Path)
			}
		}
	}
	for rpc := range noTwin {
		if _, dup := twins[rpc]; dup {
			t.Errorf("%s is listed both with twins and without", rpc)
		}
	}
}

// The member path opens exactly the substrate:tenant surfaces outside the
// carve-out: an admin-scoped surface is never opened, whatever its path.
func TestRoute_OpensOnlyUncarvedTenantSurfaces(t *testing.T) {
	for _, c := range []struct {
		required, method, path string
		want                   bool
	}{
		{auth.ScopeTenant, http.MethodPost, "/v1/_agentdef", true},
		{auth.ScopeTenant, http.MethodGet, "/v1/_limits", true},
		{auth.ScopeTenant, http.MethodPut, "/v1/_limits", false},
		{auth.ScopeTenant, http.MethodPost, "/v1/_crypto", false},
		{auth.ScopeTenant, http.MethodGet, "/v1/_groups/g", false},
		{auth.ScopeTenant, http.MethodPost, "/v1/_memory/search", true},
		{auth.ScopeTenant, http.MethodPost, "/v1/_memory/reembed", false},
		{auth.ScopeTenant, http.MethodPost, "/v1/_memory/home_facts", false},
		{auth.ScopeAdmin, http.MethodPost, "/v1/_operatortokendef", false},
		{auth.ScopeRunsCreate, http.MethodPost, "/v1/runs", false},
	} {
		if got := Route(c.required, c.method, c.path); got != c.want {
			t.Errorf("Route(%s, %s %s) = %v, want %v", c.required, c.method, c.path, got, c.want)
		}
	}
}

// A member admitted to TokenLimit by its list operation is held to the op it
// calls: set and delete are carved out, and an op not in the table has no twin.
func TestOpTwin_TokenLimitOperations(t *testing.T) {
	if !RPC("TokenLimit", auth.ScopeTenant) {
		t.Fatal("TokenLimit is not on the member path at all; its list operation should be")
	}
	for op, want := range map[string]bool{"list": true, "set": false, "delete": false} {
		tw, ok := OpTwin("TokenLimit", op)
		if !ok {
			t.Fatalf("TokenLimit op %q has no twin", op)
		}
		if got := Route(auth.ScopeTenant, tw.Method, tw.Path); got != want {
			t.Errorf("TokenLimit %s on the member path = %v, want %v", op, got, want)
		}
	}
	if _, ok := OpTwin("TokenLimit", "purge"); ok {
		t.Error("an unknown op has a twin")
	}
	if _, ok := OpTwin("AgentDef", "create"); ok {
		t.Error("AgentDef is not op-keyed, yet OpTwin answered")
	}
}
