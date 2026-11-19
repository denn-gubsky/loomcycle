package grpc

import "github.com/denn-gubsky/loomcycle/internal/authz"

// The route table's scopes and run plane by RPC name, as the tests written
// against the hand-kept maps read them (RFC EZ P6 moved those maps into
// authz.GRPCRoute).
var grpcConsumerScopes, grpcRunPlaneRPCs = func() (map[string]string, map[string]bool) {
	scopes, plane := map[string]string{}, map[string]bool{}
	for name, r := range authz.GRPCRoutes() {
		scopes[name] = r.Scope
		if r.RunPlane {
			plane[name] = true
		}
	}
	return scopes, plane
}()
