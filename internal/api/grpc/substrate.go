package grpc

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// v0.8.22 substrate admin RPCs. Mirror of the
// /v1/_agentdef + /v1/_skilldef HTTP endpoints — different
// transport, identical semantics. Both dispatch through the
// Connector (same path the MCP `agentdef` / `skilldef`
// meta-tools take).
//
// Each handler is intentionally trivial: validate the input
// is well-formed JSON, forward to the Connector method, wrap
// the typed result back into the proto response shape.
// Tool-level refusals come back as is_error=true on the
// response; transport-level errors come back as gRPC status
// codes.

// AgentDef serves the AgentDef gRPC RPC.
func (s *Server) AgentDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "AgentDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.AgentDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// SkillDef serves the SkillDef gRPC RPC.
func (s *Server) SkillDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "SkillDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.SkillDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// dispatchSubstrateRPC is the shared body of the two substrate
// handlers. callerFn closes over the specific Connector method
// (AgentDef or SkillDef).
func (s *Server) dispatchSubstrateRPC(
	ctx context.Context,
	toolName string,
	req *loomcyclepb.SubstrateRequest,
	callerFn func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error),
) (*loomcyclepb.SubstrateResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	in := req.GetInputJson()
	if len(in) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "%s: empty input_json", toolName)
	}
	if !json.Valid(in) {
		return nil, status.Errorf(codes.InvalidArgument, "%s: input_json is not valid JSON", toolName)
	}
	out, isErr, err := callerFn(ctx, json.RawMessage(in))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %v", toolName, err)
	}
	return &loomcyclepb.SubstrateResponse{
		OutputJson: []byte(out),
		IsError:    isErr,
	}, nil
}
