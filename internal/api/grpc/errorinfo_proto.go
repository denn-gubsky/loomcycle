package grpc

import (
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// errorInfoToProto renders a failure's structure as the proto ErrorInfo. One
// function for both places gRPC carries it — a streamed Event and a substrate
// RPC's response — so the two cannot say different things about one failure.
//
// Nil when there is nothing to say: no category and no call format. A failure
// that is not classified yet but carries a call format sends that alone, with
// an empty category, rather than an invented one.
func errorInfoToProto(info *tools.ErrorInfo) *loomcyclepb.ErrorInfo {
	if info == nil || (info.Category == "" && info.CallFormat == nil) {
		return nil
	}
	ei := &loomcyclepb.ErrorInfo{
		Category:    string(info.Category),
		IsRetryable: info.Retryable,
		Description: info.Description,
	}
	// Only on a genuinely retryable failure, and only when there is a real
	// hint: a zero would read as "retry immediately", the opposite of "wait
	// as you judge best".
	if info.Retryable && info.RetryAfter != nil && *info.RetryAfter > 0 {
		ms := info.RetryAfter.Milliseconds()
		ei.RetryAfterMs = &ms
	}
	if cf := info.CallFormat; cf != nil {
		pcf := &loomcyclepb.CallFormat{
			Tool:        cf.Tool,
			Op:          cf.Op,
			ExampleJson: []byte(cf.Example),
			Operations:  cf.Operations,
		}
		if cf.Reference != nil {
			pcf.Reference = &loomcyclepb.CallRef{Tool: cf.Reference.Tool, InputJson: []byte(cf.Reference.Input)}
		}
		ei.CallFormat = pcf
	}
	return ei
}
