package grpc

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Operator-trust constants for gRPC substrate dispatches.
// Without these, the inbound gRPC ctx carries no policy and the
// in-process tools default-deny every call. Same pattern as the
// HTTP substrate_admin.go's substrateAdminCtx and the MCP
// operatorCtx — distinct synthetic identifiers so audit logs
// distinguish gRPC-admin from HTTP-admin from MCP-direct.
const (
	grpcSubstrateAdminAgentID   = "a_grpc-admin"
	grpcSubstrateAdminUserID    = "grpc-admin"
	grpcSubstrateAdminAgentName = "grpc-admin"
)

// substrateGRPCCtx stamps the operator-trust ctx for gRPC
// substrate admin dispatches. Mirror of HTTP substrateAdminCtx
// and MCP operatorCtx — without this every gRPC AgentDef /
// SkillDef call hits the in-process tool's default-deny scope
// gate and returns is_error=true with the "no scopes" refusal.
//
// Grants all six policies (Memory + Channel + AgentDef + SkillDef
// + Evaluation + History) even though only AgentDef + SkillDef
// have RPCs today. dispatchSubstrateRPC is a shared helper; a
// future RPC reusing it would silently default-deny without these
// grants. Keep symmetric with operatorCtx / substrateAdminCtx.
func substrateGRPCCtx(ctx context.Context) context.Context {
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:  grpcSubstrateAdminUserID,
		AgentID: grpcSubstrateAdminAgentID,
	})
	ctx = tools.WithAgentName(ctx, grpcSubstrateAdminAgentName)
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "global"},
	})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"*"},
		Subscribe: []string{"*"},
	})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	ctx = tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{
		Scopes: []string{"any"},
	})
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
		Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"},
	})
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{
		Scopes: []string{"any"},
	})
	return ctx
}

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
	// Stamp the operator-trust ctx so the in-process tool's
	// default-deny scope policy lets the call through. Without
	// this, every substrate call from gRPC returns is_error=true
	// with the "no scopes" refusal — invisible under mock-based
	// tests, broken in production.
	ctx = substrateGRPCCtx(ctx)
	out, isErr, err := callerFn(ctx, json.RawMessage(in))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %v", toolName, err)
	}
	return &loomcyclepb.SubstrateResponse{
		OutputJson: []byte(out),
		IsError:    isErr,
	}, nil
}
