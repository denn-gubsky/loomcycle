package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// ReconcileConfigTeams writes the yaml's teams into the store's shared tier
// (RFC GS P2) through the TeamDef tool, under the operator plane's own grants:
// the substrate admin ctx an operator's off-run create gets, so a configured
// team passes exactly the gates the same team created over the API would. ctx
// must carry no principal — it is the boot's, not a request's — which makes it
// the operator's (open mode) and leaves the promoter unrestricted.
//
// A server with no TeamDef tool reconciles nothing.
func (s *Server) ReconcileConfigTeams(ctx context.Context, teams map[string]config.Team) (builtin.ConfigTeamsReport, error) {
	td, ok := s.teamDefTool.(*builtin.TeamDef)
	if !ok || td == nil {
		return builtin.ConfigTeamsReport{}, nil
	}
	return td.ReconcileConfigTeams(substrateAdminCtx(ctx), teams)
}
