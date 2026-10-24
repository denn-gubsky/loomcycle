package grpc

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// grpcUngovernedRPCs are the RPCs no access-control question governs:
// Health (no auth at all), Config (a report narrowed to the caller's own
// scopes) and Authz (the questions a principal asks about itself, confined
// to its own subject by the shared implementation, as POST /v1/_authz/* is
// on HTTP).
var grpcUngovernedRPCs = map[string]bool{"Health": true, "Config": true, "Authz": true}

// serviceRPCs is every RPC the generated service registers, unary and
// streaming, read from the descriptor rather than listed.
func serviceRPCs() []string {
	var names []string
	for _, m := range loomcyclepb.Loomcycle_ServiceDesc.Methods {
		names = append(names, m.MethodName)
	}
	for _, s := range loomcyclepb.Loomcycle_ServiceDesc.Streams {
		names = append(names, s.StreamName)
	}
	return names
}

// Every RPC maps to a catalog question or is listed as ungoverned, so an RPC
// added later cannot land ungoverned by accident (RFC EZ §2, the gRPC half
// of the HTTP route census).
func TestGRPCSurface_EveryRPCIsGovernedOrListed(t *testing.T) {
	rpcs := serviceRPCs()
	if len(rpcs) < 60 {
		t.Fatalf("only %d RPCs in the service descriptor; the census is not reading it", len(rpcs))
	}
	registered := map[string]bool{}
	for _, name := range rpcs {
		registered[name] = true
		s, governed := authz.GRPCSurface(name)
		if grpcUngovernedRPCs[name] {
			if governed {
				t.Errorf("%s is listed as ungoverned but GRPCSurface maps it to %+v", name, s)
			}
			continue
		}
		if !governed {
			t.Errorf("RPC %s has no access-control question; map it in authz.GRPCSurface or list it in grpcUngovernedRPCs", name)
			continue
		}
		if !authz.ValidVerb(s.Type, s.Action) {
			t.Errorf("%s → %s %s is not a catalog pair", name, s.Action, s.Type)
		}
	}
	for name := range grpcUngovernedRPCs {
		if !registered[name] {
			t.Errorf("grpcUngovernedRPCs lists %s, which the service does not register", name)
		}
	}
	for name := range grpcConsumerScopes {
		if !registered[name] {
			t.Errorf("grpcConsumerScopes maps %s, which the service does not register", name)
		}
	}
}

// The access-control management RPCs: one per family of the op table, each
// gated as its HTTP routes are — substrate:tenant for the three management
// families (their routes are carved out of the member path, so a member is
// kept out on both transports), any authenticated principal for its own
// authz questions.
func TestACLRPCs_ExistPerFamilyAndAreScopedAsTheirRoutes(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range serviceRPCs() {
		registered[name] = true
	}
	rpcOf := map[string]string{
		connector.ACLFamilyGroupDef:   "GroupDef",
		connector.ACLFamilyAclDef:     "AclDef",
		connector.ACLFamilyAclBinding: "AclBinding",
		connector.ACLFamilyAuthz:      "Authz",
	}
	for _, family := range connector.ACLManagementFamilies() {
		rpc, ok := rpcOf[family]
		if !ok {
			t.Errorf("family %s has no RPC named here; add it to the proto and to this table", family)
			continue
		}
		if !registered[rpc] {
			t.Errorf("family %s: the service registers no %s RPC", family, rpc)
		}
		want := auth.ScopeTenant
		if family == connector.ACLFamilyAuthz {
			want = ""
		}
		if got := requiredScopeForRPC(grpcMethodPrefix + rpc); got != want {
			t.Errorf("%s requires %q, want %q (the scope of its HTTP routes)", rpc, got, want)
		}
	}
}
