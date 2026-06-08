package grpc

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
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
	// RFC N tenant invariant: tenant from the authoritative principal the
	// gRPC interceptor stamped on ctx, NEVER the wire. Mirrors HTTP
	// substrateAdminCtx — without carrying TenantID the def-tools stamp
	// every gRPC-admin-registered def into the shared "" tenant. Zero
	// value "" when no principal → shared tenant (correct single-tenant).
	principal, _ := auth.PrincipalFromContext(ctx)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:   grpcSubstrateAdminUserID,
		AgentID:  grpcSubstrateAdminAgentID,
		TenantID: principal.TenantID,
	})
	ctx = tools.WithAgentName(ctx, grpcSubstrateAdminAgentName)
	// AgentTools wildcard ceiling (F11): operator-trust path → mirror HTTP
	// substrateAdminCtx + MCP operatorCtx so a gRPC `agentdef`/`skilldef`
	// create with a tool-bearing allowed_tools overlay validates instead of
	// refusing "caller's effective allowed_tools not on ctx". Per-run contexts
	// keep the agent's actual list, so the in-loop escalation guard is unchanged.
	ctx = tools.WithAgentTools(ctx, []string{"*"})
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
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	// WebhookDef: same operator-trust posture; "any" scope lets the
	// gRPC-admin call create/fork/retire any webhook name
	// (RFC H WH-3 / mirrors A2AAgentDef).
	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	// MemoryBackendDef: same operator-trust posture; "any" scope lets the
	// gRPC-admin call create/fork/retire any backend name
	// (RFC I MR-3a / mirrors WebhookDef).
	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: grpcSubstrateAdminAgentName,
	})
	// OperatorTokenDef: gRPC substrate admin is operator-trust → grant
	// admin (RFC L).
	ctx = tools.WithOperatorTokenDefPolicy(ctx, tools.OperatorTokenDefPolicyValue{Admin: true})
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

// MCPServerDef serves the v0.9.x MCPServerDef gRPC RPC — dynamic MCP
// server registration substrate. Mirror of AgentDef + SkillDef RPC
// shape; the body is op-discriminated input_json the Connector method
// dispatches.
func (s *Server) MCPServerDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "MCPServerDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.MCPServerDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// ScheduleDef serves the v1.x RFC E ScheduleDef gRPC RPC —
// scheduled-runs substrate. Same shape as the other three substrate
// RPCs; op-discriminated input_json (create / fork / get / list /
// retire) routes via the Connector to the in-process tool.
func (s *Server) ScheduleDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "ScheduleDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.ScheduleDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// A2AServerCardDef serves the v1.x RFC G A2AServerCardDef gRPC RPC —
// A2A-server-card substrate. Same shape as the other substrate RPCs;
// op-discriminated input_json routes via the Connector to the in-process
// tool.
func (s *Server) A2AServerCardDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "A2AServerCardDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.A2AServerCardDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// A2AAgentDef serves the v1.x RFC G A2AAgentDef gRPC RPC — A2A-agent
// substrate. Same shape as the other substrate RPCs.
func (s *Server) A2AAgentDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "A2AAgentDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.A2AAgentDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// WebhookDef serves the v1.x RFC H WebhookDef gRPC RPC — inbound-webhook
// substrate. Same shape as the other substrate RPCs.
// (RFC H WH-3 / mirrors A2AAgentDef.)
func (s *Server) WebhookDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "WebhookDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.WebhookDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// MemoryBackendDef serves the RFC I MR-3a MemoryBackendDef gRPC RPC —
// memory-backend substrate. Same shape as the other substrate RPCs.
// (RFC I MR-3a / mirrors WebhookDef.)
func (s *Server) MemoryBackendDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "MemoryBackendDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.MemoryBackendDef(ctx, in)
		if err != nil {
			return nil, false, err
		}
		return json.RawMessage(res.Text), res.IsError, nil
	})
}

// OperatorTokenDef serves the RFC L OperatorTokenDef gRPC RPC — auth
// token minting/rotation/retirement. Same shape as the other substrate
// RPCs; operator-admin-only.
func (s *Server) OperatorTokenDef(ctx context.Context, req *loomcyclepb.SubstrateRequest) (*loomcyclepb.SubstrateResponse, error) {
	return s.dispatchSubstrateRPC(ctx, "OperatorTokenDef", req, func(ctx context.Context, in json.RawMessage) (json.RawMessage, bool, error) {
		res, err := s.connector.OperatorTokenDef(ctx, in)
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
