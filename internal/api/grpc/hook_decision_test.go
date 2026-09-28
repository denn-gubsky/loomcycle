package grpc

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
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

// A hook's hold reaches gRPC clients naming the hook, so they can tell it from
// a hold review arming took.
func TestEventToProto_ARunHeldByAHookNamesIt(t *testing.T) {
	out := eventToProto(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: 1, Round: 1, HeldBy: "ops/hold"}})
	if out.GetAwaitingReview().GetHeldBy() != "ops/hold" {
		t.Errorf("proto = %+v", out.GetAwaitingReview())
	}
}

// A channel hook's decision names the message it decided on, over gRPC too.
func TestEventToProto_CarriesAChannelHookDecision(t *testing.T) {
	hd := eventToProto(providers.Event{Type: providers.EventHookDecision, HookDecision: &providers.HookDecisionInfo{
		Hook: "channel:inbox/screen", Phase: "channel_publish", Channel: "inbox", MessageID: "m1", Decision: "drop", Reason: "spam",
	}}).GetHookDecision()
	if hd.GetChannel() != "inbox" || hd.GetMessageId() != "m1" || hd.GetDecision() != "drop" || hd.GetReason() != "spam" {
		t.Fatalf("got %+v", hd)
	}
}

// Every field of the hook_decision payload is on every transport: the proto
// message, the TS adapter's interface and the Python adapter's dataclass. The
// fields are read from the Go struct, so a field added there fails here until
// each transport carries it — a transport that lacks one delivers the event
// with that part silently missing.
func TestHookDecision_ThreeWayDrift(t *testing.T) {
	var fields []string
	rt := reflect.TypeOf(providers.HookDecisionInfo{})
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		fields = append(fields, name)
	}
	if len(fields) < 9 {
		t.Fatalf("read %d fields from HookDecisionInfo", len(fields))
	}
	for _, tc := range []struct {
		what, path, start, end string
		spell                  func(string) []string
	}{
		{"the proto", "../../../proto/loomcycle.proto", "message HookDecision {", "\n}",
			func(f string) []string { return []string{" " + f + " = "} }},
		{"the TS adapter", "../../../adapters/ts/src/types.ts", "export interface HookDecisionInfo {", "\n}",
			func(f string) []string { return []string{" " + f + ":", " " + f + "?:"} }},
		{"the Python dataclass", "../../../adapters/python/loomcycle/events.py", "class HookDecision:", "\n\n\n",
			func(f string) []string { return []string{" " + f + ":"} }},
		{"the Python decode", "../../../adapters/python/loomcycle/events.py", "hd = HookDecision(", ")",
			func(f string) []string { return []string{f + "=h." + f} }},
	} {
		b, err := os.ReadFile(tc.path)
		if err != nil {
			t.Skipf("%s not readable from here: %v", tc.path, err)
		}
		src := string(b)
		i := strings.Index(src, tc.start)
		if i < 0 {
			t.Fatalf("%s: %q not found", tc.what, tc.start)
		}
		blk := src[i:]
		if j := strings.Index(blk, tc.end); j >= 0 {
			blk = blk[:j]
		}
		for _, f := range fields {
			found := false
			for _, s := range tc.spell(f) {
				found = found || strings.Contains(blk, s)
			}
			if !found {
				t.Errorf("%s does not carry hook_decision.%s", tc.what, f)
			}
		}
	}
}
