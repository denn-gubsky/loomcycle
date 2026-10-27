package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// clientDefAppClient resolves a runtime-registered client (a ClientDef).
func (s *Server) clientDefAppClient(ctx context.Context, id string) (config.AppClient, bool, error) {
	return config.AppClient{}, false, nil
}

// clientDefsAllowOrigin reports whether a live ClientDef lists origin.
func (s *Server) clientDefsAllowOrigin(ctx context.Context, origin string) bool {
	return false
}
