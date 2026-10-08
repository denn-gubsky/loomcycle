package grpc

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Decide RPC's behaviour behind real authentication is tested in
// internal/api/decidetest. This is the one mapping those tests cannot reach.

// TestDecideFailureCode_ARefusalThatIsNotTheToolsOwn — a failed result whose
// text carries no decision code was refused before the tool ran. Classified as
// the caller's input it is InvalidArgument, never an Unavailable a client
// would retry; with nothing to go on it is a failed call.
func TestDecideFailureCode_ARefusalThatIsNotTheToolsOwn(t *testing.T) {
	for _, c := range []struct {
		name   string
		res    connector.ToolResult
		code   codes.Code
		reason string
	}{
		{"an unknown argument", connector.ToolResult{IsError: true, Text: `unknown field "op"`,
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation}}, codes.InvalidArgument, "invalid_input"},
		{"unclassified", connector.ToolResult{IsError: true, Text: "something else"}, codes.Unavailable, "call_failed"},
		{"a code this table does not know", connector.ToolResult{IsError: true, Text: "Decision: brand_new: x"}, codes.Unavailable, "brand_new"},
		{"a new code for the caller's own mistake", connector.ToolResult{IsError: true, Text: "Decision: brand_new: x",
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation}}, codes.InvalidArgument, "brand_new"},
	} {
		if code, reason := decideFailureCode(c.res); code != c.code || reason != c.reason {
			t.Errorf("%s: (%s, %q), want (%s, %q)", c.name, code, reason, c.code, c.reason)
		}
	}
}
