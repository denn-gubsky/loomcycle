package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/runner"
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
// The caller is admitted, charged and held to its key and budget rules by
// admitOffRunCall, as every model call a caller asks for with no run is.
//
// It does not wait out a runtime pause, like the LLM gateway and unlike a new
// run: a pause quiesces RUNS for a snapshot, and this call belongs to none.
func (s *Server) Decision(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	// A direct decision holds no run slot, so nothing else limits how many can
	// wait on a busy provider: with provider_queue_depth of them already
	// waiting, the next is refused instead of queueing. Set here, the one entry
	// every transport's direct decision comes through.
	ctx = lanegate.WithQueueBound(ctx, s.cfg().Concurrency.ProviderQueueDepthOrDefault())
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

	ctx, release, err := s.admitOffRunCall(ctx, "a decision", "")
	if err != nil {
		return connector.ToolResult{}, err
	}
	defer release()

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
	// No code, or one a later driver added: go by what kind of failure it is.
	if res.ErrorInfo != nil && res.ErrorInfo.Category == tools.CategoryValidation {
		if code == "" {
			code = builtin.DecisionCodeInvalidInput
		}
		return http.StatusBadRequest, code
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
		if errors.Is(err, runner.ErrRateLimited) {
			writeRateLimited(w, err)
			return
		}
		if concurrency.IsPerUserQuotaExhausted(err) {
			writeQuotaError(w, err)
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
