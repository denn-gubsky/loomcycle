package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/errkind"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func sampleCallFormat() *errkind.CallFormat {
	return &errkind.CallFormat{
		Tool: "Document", Op: "create_chunk",
		Example:   json.RawMessage(`{"op":"create_chunk","title":"T"}`),
		Reference: &errkind.CallRef{Tool: "Context", Input: json.RawMessage(`{"op":"help","topic":"Document/create_chunk"}`)},
	}
}

// A failed tool call's frame carries its correct call format on the stream,
// as the MCP and HTTP surfaces do.
func TestEventToProto_AToolResultCarriesTheCallFormat(t *testing.T) {
	out := eventToProto(providers.Event{
		Type:    providers.EventToolResult,
		Text:    `{"isError":true,"error":"unknown argument"}`,
		IsError: true,
		ErrorInfo: &errkind.Info{
			Category: errkind.CategoryValidation, Description: "Pass only this tool's own arguments.",
			CallFormat: sampleCallFormat(),
		},
	})
	cf := out.GetErrorInfo().GetCallFormat()
	if out.GetErrorInfo().GetCategory() != "validation" || cf == nil {
		t.Fatalf("error_info = %+v; want the validation category and a call format", out.GetErrorInfo())
	}
	if cf.GetTool() != "Document" || cf.GetOp() != "create_chunk" ||
		string(cf.GetExampleJson()) != `{"op":"create_chunk","title":"T"}` ||
		cf.GetReference().GetTool() != "Context" || string(cf.GetReference().GetInputJson()) != `{"op":"help","topic":"Document/create_chunk"}` {
		t.Errorf("call_format = %+v", cf)
	}
}

// A failure not classified yet but carrying a call format sends that alone,
// with an empty category rather than an invented one; a failure with neither
// sends no ErrorInfo at all.
func TestErrorInfoToProto_CallFormatAloneAndNothing(t *testing.T) {
	only := errorInfoToProto(&tools.ErrorInfo{CallFormat: sampleCallFormat()})
	if only == nil || only.GetCategory() != "" || only.GetCallFormat() == nil {
		t.Errorf("call format alone = %+v", only)
	}
	if got := errorInfoToProto(&tools.ErrorInfo{}); got != nil {
		t.Errorf("an empty info produced %+v", got)
	}
	if got := errorInfoToProto(nil); got != nil {
		t.Errorf("nil produced %+v", got)
	}
}

// A substrate RPC's refusal carries its structure on the response, which the
// connector result used to lose at exactly this seam.
func TestDispatchSubstrateRPC_ARefusalCarriesItsErrorInfo(t *testing.T) {
	s := &Server{connector: &mockConnector{}}
	resp, err := s.dispatchSubstrateRPC(context.Background(), "Document",
		&loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"create_chunk"}`)},
		func(context.Context, json.RawMessage) (connector.ToolResult, error) {
			return connector.ToolResult{
				Text: `Document: unknown argument "text"`, IsError: true,
				ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation, CallFormat: sampleCallFormat()},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetIsError() || resp.GetErrorInfo().GetCategory() != "validation" || resp.GetErrorInfo().GetCallFormat() == nil {
		t.Errorf("response = %+v; want the refusal's error_info", resp)
	}

	ok, err := s.dispatchSubstrateRPC(context.Background(), "Document",
		&loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"get_document"}`)},
		func(context.Context, json.RawMessage) (connector.ToolResult, error) {
			return connector.ToolResult{Text: `{"id":"d1"}`}, nil
		})
	if err != nil || ok.GetErrorInfo() != nil {
		t.Errorf("a success carries error_info: %+v, %v", ok, err)
	}
}
