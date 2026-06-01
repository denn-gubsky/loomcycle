package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// ResolveProbe — mirrors POST /v1/_resolve/probe. Thin translation
// between the proto wire shape and the Connector, same pattern as the
// pause/snapshot handlers. The two resolver sentinels both map to
// Unavailable (the feature can't run on this deployment); per-provider
// probe failures are carried in the matrix, not surfaced as an error.
func (s *Server) ResolveProbe(ctx context.Context, _ *loomcyclepb.ResolveProbeRequest) (*loomcyclepb.ResolverMatrixResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	res, err := s.connector.ResolveProbe(ctx)
	if err != nil {
		if errors.Is(err, connector.ErrResolverUnavailable) || errors.Is(err, connector.ErrResolveProbeUnavailable) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return resolverMatrixToProto(res), nil
}

func resolverMatrixToProto(m connector.ResolverMatrix) *loomcyclepb.ResolverMatrixResponse {
	out := &loomcyclepb.ResolverMatrixResponse{
		GeneratedAt: timestamppb.New(m.GeneratedAt),
		Providers:   make(map[string]*loomcyclepb.ResolverProviderAvailability, len(m.Providers)),
	}
	for name, p := range m.Providers {
		models := make(map[string]*loomcyclepb.ResolverModelStatus, len(p.Models))
		for mn, st := range p.Models {
			models[mn] = &loomcyclepb.ResolverModelStatus{Listed: st.Listed, Stalled: st.Stalled}
		}
		out.Providers[name] = &loomcyclepb.ResolverProviderAvailability{
			Excluded:  p.Excluded,
			Reachable: p.Reachable,
			Models:    models,
			LastCheck: timestamppb.New(p.LastCheck),
			LastError: p.LastError,
		}
	}
	return out
}
