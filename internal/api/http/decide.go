package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// decisionTool is the registered Decision tool, or nil on a deployment that
// declares no decision models (main registers it only for a declared block).
func (s *Server) decisionTool() *builtin.Decision {
	for _, t := range s.tools {
		if d, ok := t.(*builtin.Decision); ok {
			return d
		}
	}
	return nil
}

// Decision asks a decision model on behalf of a caller that has no run: the
// ONE path for it, behind POST /v1/_decide, the gRPC Decide RPC and the MCP
// `decision` tool. input and the result are the in-run Decision tool's, and
// the tool itself produces the result.
//
// A run's start is what normally holds a model call to its rules; with no run,
// this function does each by hand, in this order:
//
//  1. The caller is the authenticated principal: its tenant and subject. With
//     no principal (open mode, the stdio MCP server) the call is the
//     operator's, filed under the shared tenant with no user, as a run started
//     without a user_id is. Nothing in the request can name another caller.
//  2. The operator's provider key: whether this caller may spend it is derived
//     from the principal and stamped EXPLICITLY either way. The driver's check
//     reads a bit that means "allowed" when nothing stamped it, so a path that
//     stamped nothing would hand a restricted tenant the operator's key. The
//     credential resolver is stamped beside it, as at a run start, so a
//     restricted tenant holding its own key for the provider is still served,
//     on that key.
//  3. The budget: a caller at a hard token ceiling is refused before any call
//     (*connector.TokenLimitError), by the check run admission uses.
//  4. The bill: the context names who is charged (tools.WithMeteredOffRunCall),
//     which is what makes RecordRunSideCallUsage write the usage row and count
//     the tokens, and what lifts the tool's own refusal of a call with no run.
//
// It does not wait out a runtime pause, like the LLM gateway and unlike a new
// run: a pause quiesces RUNS for a snapshot, and this call belongs to none.
func (s *Server) Decision(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	tool := s.decisionTool()
	if tool == nil {
		// Answered by the tool itself (decision_not_configured), so a caller can
		// tell a deployment without decision models from a broken one. No model
		// can be reached, so there is nothing to admit or meter.
		res, err := s.execBuiltin(ctx, &builtin.Decision{}, input)
		if err != nil {
			return connector.ToolResult{}, err
		}
		return connector.ToolResult{Text: res.Text, IsError: res.IsError, ErrorInfo: res.Error}, nil
	}

	p, authed := auth.PrincipalFromContext(ctx)
	var tenant, user string
	if authed {
		tenant, user = p.TenantID, p.Subject
	}
	// The more restrictive of the principal and any run bit already on ctx: an
	// OR can only add a restriction.
	restricted := tools.AuthorOperatorKeyRestricted(ctx, s.cfg().Env.OperatorKeyRestriction)

	if dec := s.limits.Check(tenant, user); !dec.Allowed {
		return connector.ToolResult{}, &connector.TokenLimitError{Info: *dec.Refusal}
	}

	// The identity the credential resolver looks a tenant's own key up under.
	// The agent name is cleared: an off-run call belongs to no agent, and a
	// transport's synthetic one (the MCP operator's) must not match an
	// agent-scoped credential on one surface and not on the others.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		TenantID:              tenant,
		UserID:                user,
		OperatorKeyRestricted: restricted,
		Isolated:              auth.IsIsolated(p, authed),
	})
	ctx = tools.WithAgentName(ctx, "")
	ctx = tools.WithDecisionPolicy(ctx, nil) // no agent, so no agent's narrowing
	ctx = providers.WithCredentialResolver(ctx, s.credResolver)
	ctx = providers.WithOperatorKeyAllowed(ctx, !restricted)
	ctx = tools.WithMeteredOffRunCall(ctx, tools.MeteredOffRunCallValue{TenantID: tenant, UserID: user})

	res, err := s.execBuiltin(ctx, tool, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError, ErrorInfo: res.Error}, nil
}

// DecisionModels lists the decision models a caller may name and the default.
// ok is false on a deployment that declares none.
func (s *Server) DecisionModels(context.Context) (connector.DecisionModelList, bool) {
	tool := s.decisionTool()
	if tool == nil || tool.Service == nil {
		return connector.DecisionModelList{}, false
	}
	return connector.DecisionModelList{Default: tool.Service.Default(), Models: tool.Service.Models()}, true
}

const decisionNotConfiguredMsg = "this deployment declares no decision models"

// decideFailureStatus maps a failed Decision result to the HTTP status and the
// code its body carries. The code is the tool's own, read from the result's
// text; a refusal the dispatcher made before the tool ran (an unknown
// argument) has none and is the caller's input.
func decideFailureStatus(res connector.ToolResult) (status int, code string) {
	code = builtin.DecisionFailureCode(res.Text)
	switch code {
	case builtin.DecisionCodeInvalidInput, decision.CodeBadQuestion, decision.CodeBadOptions,
		decision.CodeTooManyQuestions, decision.CodeModelNotAllowed:
		return http.StatusBadRequest, code
	case decision.CodePromptTooLarge:
		return http.StatusRequestEntityTooLarge, code
	case builtin.DecisionCodeKeyRestricted:
		return http.StatusForbidden, code
	case builtin.DecisionCodeNotConfigured:
		// The status the other "this deployment has none configured" refusals
		// use (embedder_not_configured, pause_not_configured).
		return http.StatusServiceUnavailable, code
	case decision.CodeTimeout:
		return http.StatusGatewayTimeout, code
	case decision.CodeCallFailed, decision.CodeModelNotFound:
		return http.StatusBadGateway, code
	}
	if res.ErrorInfo != nil && res.ErrorInfo.Category == tools.CategoryValidation {
		return http.StatusBadRequest, builtin.DecisionCodeInvalidInput
	}
	if code == "" {
		code = decision.CodeCallFailed
	}
	return http.StatusBadGateway, code
}

// handleDecide serves POST /v1/_decide: the body is the Decision tool's input
// ({model?, state, questions}) and a 200 is the tool's output, byte for byte.
// A failure is {code, error} plus the failure's structure, at the status
// decideFailureStatus gives the tool's code; a caller over a hard token budget
// gets the 429 run admission gives.
func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxRequestBytes()))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds the %d-byte limit", maxErr.Limit))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body: expected {state, questions}")
		return
	}
	res, err := s.Decision(r.Context(), body)
	if err != nil {
		var over *connector.TokenLimitError
		if errors.As(err, &over) {
			writeTokenLimitError(w, &over.Info)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if res.IsError {
		status, code := decideFailureStatus(res)
		env := map[string]any{"code": code, "error": res.Text}
		addErrorInfo(env, res.Text, res.ErrorInfo)
		writeJSONErrorBody(w, status, env)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(res.Text))
}

// handleDecideModels serves GET /v1/_decide/models: the decision models a call
// may name and the default.
func (s *Server) handleDecideModels(w http.ResponseWriter, r *http.Request) {
	list, ok := s.DecisionModels(r.Context())
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, builtin.DecisionCodeNotConfigured, decisionNotConfiguredMsg)
		return
	}
	writeJSONOK(w, list)
}
