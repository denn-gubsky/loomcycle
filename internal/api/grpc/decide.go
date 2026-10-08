package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Decide serves the Decide RPC: a decision model asked with no run behind it.
// The request is rebuilt as the Decision tool's input and goes through the
// connector's one run-less path (Connector.Decision), which decides from the
// caller's principal whose key may be spent, checks the budget and charges the
// tokens. This handler only translates the wire shape and the failure.
func (s *Server) Decide(ctx context.Context, req *loomcyclepb.DecideRequest) (*loomcyclepb.DecideResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	input, err := decideInputFromProto(req)
	if err != nil {
		return nil, decideStatus(codes.InvalidArgument, builtin.DecisionCodeInvalidInput, err.Error(), nil)
	}
	res, err := s.connector.Decision(ctx, input)
	if err != nil {
		if errors.Is(err, runner.ErrTokenLimitExceeded) {
			// The refusal, code and details run admission gives.
			return nil, mapRunnerErr(err)
		}
		return nil, status.Errorf(codes.Internal, "Decide: %v", err)
	}
	if res.IsError {
		code, reason := decideFailureCode(res)
		return nil, decideStatus(code, reason, res.Text, res.ErrorInfo)
	}
	var out struct {
		Model       string                     `json:"model"`
		Provider    string                     `json:"provider"`
		ServedModel string                     `json:"served_model"`
		Answers     map[string]json.RawMessage `json:"answers"`
		Usage       struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return nil, status.Errorf(codes.Internal, "Decide: the tool's result is not readable: %v", err)
	}
	resp := &loomcyclepb.DecideResponse{
		Model: out.Model, Provider: out.Provider, ServedModel: out.ServedModel,
		Answers: make(map[string][]byte, len(out.Answers)),
		Usage:   &loomcyclepb.DecisionUsage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens},
	}
	// Each answer is the tool's own bytes for it: a RawMessage is not re-encoded.
	for name, raw := range out.Answers {
		resp.Answers[name] = raw
	}
	return resp, nil
}

// ListDecisionModels serves the ListDecisionModels RPC.
func (s *Server) ListDecisionModels(ctx context.Context, _ *loomcyclepb.ListDecisionModelsRequest) (*loomcyclepb.ListDecisionModelsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	list, ok := s.connector.DecisionModels(ctx)
	if !ok {
		return nil, decideStatus(codes.FailedPrecondition, builtin.DecisionCodeNotConfigured,
			"this deployment declares no decision models", nil)
	}
	resp := &loomcyclepb.ListDecisionModelsResponse{DefaultModel: list.Default}
	for _, m := range list.Models {
		resp.Models = append(resp.Models, &loomcyclepb.DecisionModel{
			Name: m.Name, Provider: m.Provider, Model: m.Model,
			Limits: &loomcyclepb.DecisionLimits{
				MaxQuestions: int32(m.Limits.MaxQuestions),
				MinOptions:   int32(m.Limits.MinOptions),
				MaxOptions:   int32(m.Limits.MaxOptions),
			},
		})
	}
	return resp, nil
}

// decideInputFromProto rebuilds the Decision tool's input. The JSON fields are
// spliced in as sent (a RawMessage is checked for validity and compacted, never
// decoded), so a number or a key reaches the model as the caller wrote it.
func decideInputFromProto(req *loomcyclepb.DecideRequest) (json.RawMessage, error) {
	type question struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria,omitempty"`
	}
	in := struct {
		Model     string              `json:"model,omitempty"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]question `json:"questions"`
	}{Model: req.GetModel(), State: req.GetStateJson(), Questions: map[string]question{}}
	if len(req.GetStateJson()) > 0 && !json.Valid(req.GetStateJson()) {
		return nil, errors.New("Decide: state_json is not valid JSON")
	}
	for name, q := range req.GetQuestions() {
		if len(q.GetCriteriaJson()) > 0 && !json.Valid(q.GetCriteriaJson()) {
			return nil, errors.New("Decide: question " + strconv.Quote(name) + ": criteria_json is not valid JSON")
		}
		in.Questions[name] = question{Type: q.GetType(), Instructions: q.GetInstructions(), Criteria: q.GetCriteriaJson()}
	}
	return json.Marshal(in)
}

// decideFailureCode maps a failed Decision result to the gRPC code and the
// reason its ErrorInfo carries. The reason is the tool's own code, the same
// string the HTTP body's `code` holds.
func decideFailureCode(res connector.ToolResult) (codes.Code, string) {
	reason := builtin.DecisionFailureCode(res.Text)
	switch reason {
	case builtin.DecisionCodeInvalidInput, decision.CodeBadQuestion, decision.CodeBadOptions,
		decision.CodeTooManyQuestions, decision.CodeModelNotAllowed, decision.CodePromptTooLarge:
		return codes.InvalidArgument, reason
	case builtin.DecisionCodeKeyRestricted:
		return codes.PermissionDenied, reason
	case builtin.DecisionCodeNotConfigured, decision.CodeModelNotFound:
		// Not Unavailable: nothing a retry can change, and a client that retries
		// Unavailable on its own would hammer a deployment that cannot answer
		// until an operator edits its config.
		return codes.FailedPrecondition, reason
	case decision.CodeTimeout:
		return codes.DeadlineExceeded, reason
	case decision.CodeCallFailed:
		return codes.Unavailable, reason
	}
	// No code, or one a later driver added: go by what kind of failure it is.
	if res.ErrorInfo != nil && res.ErrorInfo.Category == tools.CategoryValidation {
		if reason == "" {
			reason = builtin.DecisionCodeInvalidInput
		}
		return codes.InvalidArgument, reason
	}
	if reason == "" {
		reason = decision.CodeCallFailed
	}
	return codes.Unavailable, reason
}

// decideStatus is a decision failure as a gRPC status: the code, the tool's
// text, and a google.rpc.ErrorInfo naming the decision code, so a caller
// branches on the reason rather than on the sentence.
func decideStatus(code codes.Code, reason, msg string, info *tools.ErrorInfo) error {
	st := status.New(code, msg)
	ei := &errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}
	if info != nil && info.Category != "" {
		ei.Metadata = map[string]string{
			"category":     string(info.Category),
			"is_retryable": strconv.FormatBool(info.Retryable),
		}
	}
	if withDetails, err := st.WithDetails(ei); err == nil {
		return withDetails.Err()
	}
	return st.Err()
}
