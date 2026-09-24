package grpc

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A hook's decision reaches gRPC clients with its payload, and only on its own
// event type.
func TestEventToProto_CarriesTheHookDecision(t *testing.T) {
	out := eventToProto(providers.Event{Type: providers.EventHookDecision, HookDecision: &providers.HookDecisionInfo{
		Hook: "sec/pin", Phase: "pre", ToolUseID: "c1", ToolName: "WebFetch",
		Decision: "rewrite_input", UpdatedInput: json.RawMessage(`{"url":"https://safe/"}`),
	}})
	hd := out.GetHookDecision()
	if out.GetType() != "hook_decision" || hd.GetHook() != "sec/pin" || hd.GetDecision() != "rewrite_input" ||
		string(hd.GetUpdatedInput()) != `{"url":"https://safe/"}` || hd.GetToolUseId() != "c1" {
		t.Errorf("proto = %+v", out)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "x"}).GetHookDecision() != nil {
		t.Error("a text frame carries a hook decision")
	}
}
