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
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Calc serves the Calc RPC (RFC GY P4a): the Calc tool outside a run, the twin
// of POST /v1/_calc. input_json and result_json are the tool's own JSON, so a
// new tool field needs no proto change. The run-rate charge and the refusal
// of `from` / `to` are the connector's (shared with HTTP and MCP); this maps a
// refusal to a status by its category.
func (s *Server) Calc(ctx context.Context, req *loomcyclepb.CalcRequest) (*loomcyclepb.CalcResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	in := req.GetInputJson()
	if len(in) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Calc: empty input_json")
	}
	if !json.Valid(in) {
		return nil, status.Error(codes.InvalidArgument, "Calc: input_json is not valid JSON")
	}
	res, err := s.connector.Calc(ctx, json.RawMessage(in))
	if err != nil {
		if errors.Is(err, runner.ErrRateLimited) {
			return nil, mapRunnerErr(err)
		}
		return nil, status.Errorf(codes.Internal, "Calc: %v", err)
	}
	if res.IsError {
		return nil, calcRefusalStatus(res)
	}
	return &loomcyclepb.CalcResponse{ResultJson: []byte(res.Text)}, nil
}

// calcRefusalStatus is a Calc refusal as a gRPC status: the code by the
// refusal's category, the detail as the message, and a google.rpc.ErrorInfo
// whose reason is the tool's error code and whose metadata carries the
// category, the hint and the position, as the HTTP body does.
func calcRefusalStatus(res connector.ToolResult) error {
	var refusal struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
		Pos    *int   `json:"pos"`
	}
	msg, reason := res.Text, "tool_refused"
	meta := map[string]string{}
	if json.Unmarshal([]byte(res.Text), &refusal) == nil && refusal.Error != "" {
		msg, reason = refusal.Detail, refusal.Error
		if refusal.Hint != "" {
			meta["hint"] = refusal.Hint
		}
		if refusal.Pos != nil {
			meta["pos"] = strconv.Itoa(*refusal.Pos)
		}
	}
	code := codes.FailedPrecondition
	if info := res.ErrorInfo; info != nil && info.Category != "" {
		meta["category"] = string(info.Category)
		meta["is_retryable"] = strconv.FormatBool(info.Retryable)
		switch info.Category {
		case tools.CategoryValidation:
			code = codes.InvalidArgument
		case tools.CategoryTransient:
			code = codes.Unavailable
		}
	}
	st := status.New(code, msg)
	if withDetails, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain, Metadata: meta}); err == nil {
		return withDetails.Err()
	}
	return st.Err()
}
