package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// channelFixture builds a Channel tool over an in-memory SQLite store
// + a fresh Bus. The returned ctx has a sensible agent name + user_id
// + a default policy granting both publish and subscribe on
// "findings" (agent-scoped, queue, no TTL) and "alerts" (global,
// broadcast, 1h TTL). Tests override the policy when they want to
// exercise refusals.
func channelFixture(t *testing.T) (*Channel, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	tool := &Channel{
		Store:         s,
		Bus:           channels.NewBus(),
		MaxValueBytes: 65536,
		LongPollCapMS: 30000,
	}
	ctx := tools.WithAgentName(context.Background(), "researcher")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"findings", "alerts"},
		Subscribe: []string{"findings", "alerts", "findings/*"},
		Channels: map[string]tools.ChannelDef{
			"findings":       {Name: "findings", Scope: "agent", MaxMessages: 1000, Semantic: "queue"},
			"alerts":         {Name: "alerts", Scope: "global", DefaultTTL: 3600, Semantic: "broadcast"},
			"findings/alpha": {Name: "findings/alpha", Scope: "agent", Semantic: "queue"},
		},
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestChannelTool_PublishSubscribeRoundTrip(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		input := `{"op":"publish","channel":"findings","value":{"i":` + intToStr(i) + `}}`
		res, _ := tool.Execute(ctx, json.RawMessage(input))
		if res.IsError {
			t.Fatalf("publish %d: %s", i, res.Text)
		}
		time.Sleep(time.Microsecond)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"findings","max_messages":10}`))
	if res.IsError {
		t.Fatalf("subscribe: %s", res.Text)
	}
	// Check basic envelope structure — three messages + next_cursor.
	got := decodeResult(t, res.Text)
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Errorf("got %d msgs, want 3", len(msgs))
	}
	if next, _ := got["next_cursor"].(string); next == "" {
		t.Errorf("next_cursor empty in %s", res.Text)
	}
}

func TestChannelTool_PublishRefusedWithoutACL(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	// Replace policy: agent may subscribe but NOT publish to findings.
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{}, // no publish allowed
		Subscribe: []string{"findings"},
		Channels: map[string]tools.ChannelDef{
			"findings": {Name: "findings", Scope: "agent"},
		},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{"x":1}}`))
	if !res.IsError {
		t.Fatalf("publish should be refused; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "publish") {
		t.Errorf("refusal should mention publish; got %s", res.Text)
	}
}

func TestChannelTool_UnknownChannelRefused(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"nonexistent","value":{}}`))
	if !res.IsError {
		t.Fatal("publish to undeclared channel must be refused")
	}
	if !strings.Contains(res.Text, "not declared") {
		t.Errorf("refusal should mention 'not declared'; got %s", res.Text)
	}
}

func TestChannelTool_WildcardAllowlist(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	// findings/* is in subscribe allowlist, and findings/alpha is a
	// declared channel. Subscribe should succeed.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"findings/alpha","max_messages":10}`))
	if res.IsError {
		t.Errorf("subscribe to findings/alpha should match findings/* wildcard; got %s", res.Text)
	}
}

func TestChannelTool_GlobalScopeChannel(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	// Publish to global-scope alerts. Even without a user_id on the
	// ctx, global publish should succeed (scope_id = "").
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"alerts","value":{"level":"warn"}}`))
	if res.IsError {
		t.Fatalf("global publish: %s", res.Text)
	}
	// Subscribe should return that message.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"alerts","max_messages":10}`))
	got := decodeResult(t, res.Text)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("global subscribe got %d msgs, want 1", len(msgs))
	}
}

func TestChannelTool_AckMissingCursorErrors(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"ack","channel":"findings"}`))
	if !res.IsError {
		t.Error("ack without cursor should error")
	}
}

func TestChannelTool_PeekDoesNotAdvance(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{"x":1}}`))
	// Peek (no auto-ack).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"peek","channel":"findings","max_messages":10}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	// Subscribe should STILL see the message (peek didn't advance).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"findings","max_messages":10}`))
	got := decodeResult(t, res.Text)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("after peek, subscribe got %d msgs, want 1 (peek must not advance)", len(msgs))
	}
}

func TestChannelTool_ListChannelsReportsPolicy(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list_channels"}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(res.Text, `"publish"`) || !strings.Contains(res.Text, `"subscribe"`) {
		t.Errorf("list_channels should report publish + subscribe; got %s", res.Text)
	}
}

func TestChannelTool_InvalidJSONValueErrors(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	// value is a malformed JSON literal (unquoted identifier)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":not_json}`))
	if !res.IsError {
		t.Error("invalid value json should error")
	}
}

func TestChannelTool_MaxValueBytesEnforced(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	tool.MaxValueBytes = 32
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{"x":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
	if !res.IsError {
		t.Error("oversized payload should error")
	}
	if !strings.Contains(res.Text, "exceeds max") {
		t.Errorf("refusal should mention 'exceeds max'; got %s", res.Text)
	}
}

// ---- helpers ----

func decodeResult(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode result %q: %v", body, err)
	}
	return out
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	out := []byte{}
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}

// ---- v0.8.4 typed-audit-event tests ----

// captureEvents returns an EventEmitter that records every event
// into a slice so tests can assert on the structured payload.
func captureEvents() (tools.EventEmitterFunc, *[]providers.Event) {
	var got []providers.Event
	emit := func(ev providers.Event) { got = append(got, ev) }
	return emit, &got
}

// TestChannelTool_PublishEmitsTypedEvent pins that a successful
// Channel.publish surfaces EventChannelPublish with the full
// structured payload (channel id, scope, message_id, byte size,
// truncated preview).
func TestChannelTool_PublishEmitsTypedEvent(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	emit, got := captureEvents()
	ctx = tools.WithEventEmitter(ctx, emit)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{"i":42}}`))
	if res.IsError {
		t.Fatalf("publish: %s", res.Text)
	}

	var pubs []providers.Event
	for _, ev := range *got {
		if ev.Type == providers.EventChannelPublish {
			pubs = append(pubs, ev)
		}
	}
	if len(pubs) != 1 {
		t.Fatalf("got %d EventChannelPublish, want 1", len(pubs))
	}
	info := pubs[0].Channel
	if info == nil {
		t.Fatal("Event.Channel nil; want non-nil ChannelEventInfo")
	}
	if info.Channel != "findings" {
		t.Errorf("Channel = %q, want findings", info.Channel)
	}
	if info.MessageID == "" {
		t.Error("MessageID empty")
	}
	if info.Scope != "agent" {
		t.Errorf("Scope = %q, want agent", info.Scope)
	}
	if info.ScopeID != "researcher" {
		t.Errorf("ScopeID = %q, want researcher (the fixture's agent name)", info.ScopeID)
	}
	if info.PayloadBytes == 0 {
		t.Error("PayloadBytes = 0")
	}
	if !strings.Contains(info.PayloadPreview, `"i":42`) {
		t.Errorf("PayloadPreview = %q, want contains `\"i\":42`", info.PayloadPreview)
	}
	if info.DroppedOldest != 0 {
		t.Errorf("DroppedOldest = %d, want 0 on the first publish", info.DroppedOldest)
	}
}

// TestChannelTool_SubscribeEmitsOneDeliveryPerMessage pins that
// each message in a returned batch surfaces its own
// EventChannelDelivery, in order, with Cursor populated.
func TestChannelTool_SubscribeEmitsOneDeliveryPerMessage(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	emit, got := captureEvents()
	ctx = tools.WithEventEmitter(ctx, emit)

	for i := 0; i < 3; i++ {
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{"i":`+intToStr(i)+`}}`))
		time.Sleep(time.Microsecond)
	}

	// Reset captured events so the subscribe phase is clean.
	*got = nil

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"findings","max_messages":10}`))
	if res.IsError {
		t.Fatalf("subscribe: %s", res.Text)
	}

	var deliveries []providers.Event
	for _, ev := range *got {
		if ev.Type == providers.EventChannelDelivery {
			deliveries = append(deliveries, ev)
		}
	}
	if len(deliveries) != 3 {
		t.Fatalf("got %d EventChannelDelivery, want 3 (one per message)", len(deliveries))
	}
	for i, ev := range deliveries {
		if ev.Channel == nil {
			t.Fatalf("delivery[%d].Channel nil", i)
		}
		if ev.Channel.MessageID == "" {
			t.Errorf("delivery[%d].MessageID empty", i)
		}
		if ev.Channel.Cursor != ev.Channel.MessageID {
			t.Errorf("delivery[%d].Cursor = %q, want MessageID %q (cursor = id of THIS msg)",
				i, ev.Channel.Cursor, ev.Channel.MessageID)
		}
		if ev.Channel.Channel != "findings" {
			t.Errorf("delivery[%d].Channel = %q, want findings", i, ev.Channel.Channel)
		}
		if i > 0 && deliveries[i].Channel.MessageID <= deliveries[i-1].Channel.MessageID {
			t.Errorf("delivery[%d].MessageID not strictly increasing: %q vs %q",
				i, deliveries[i-1].Channel.MessageID, deliveries[i].Channel.MessageID)
		}
	}
}

// TestChannelTool_NoEmitterIsSilent pins that the no-ctx-emitter
// path is a no-op — tools called without WithEventEmitter still
// work correctly, just without surfacing typed events.
func TestChannelTool_NoEmitterIsSilent(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	// No WithEventEmitter applied — fixture ctx has no emitter.

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":{}}`))
	if res.IsError {
		t.Errorf("publish with no emitter should still succeed: %s", res.Text)
	}
}

// TestChannelTool_PayloadPreviewTruncatedAt200Chars pins that
// large payloads don't blow up the SSE wire — the preview is
// capped at 200 chars + an ellipsis suffix.
func TestChannelTool_PayloadPreviewTruncatedAt200Chars(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	emit, got := captureEvents()
	ctx = tools.WithEventEmitter(ctx, emit)

	// Construct a payload whose JSON-encoded form is well over 200 chars.
	big := strings.Repeat("a", 500)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"findings","value":"`+big+`"}`))
	if res.IsError {
		t.Fatalf("publish: %s", res.Text)
	}

	var info *providers.ChannelEventInfo
	for _, ev := range *got {
		if ev.Type == providers.EventChannelPublish {
			info = ev.Channel
			break
		}
	}
	if info == nil {
		t.Fatal("no EventChannelPublish captured")
	}
	if !strings.HasSuffix(info.PayloadPreview, "…") {
		t.Errorf("PayloadPreview should end with ellipsis when truncated; got %q", info.PayloadPreview[len(info.PayloadPreview)-10:])
	}
	// 200 chars + UTF-8 ellipsis (3 bytes). The rune count is 201; byte
	// length is 203. Either property pins the cap; we go with byte
	// length for simplicity since that's what len() returns.
	if len(info.PayloadPreview) > 203 {
		t.Errorf("PayloadPreview length = %d, want <= 203 (200 chars + 3-byte ellipsis)", len(info.PayloadPreview))
	}
	// PayloadBytes should reflect the FULL untruncated payload size.
	if info.PayloadBytes < 500 {
		t.Errorf("PayloadBytes = %d, want >= 500 (full payload size, not truncated)", info.PayloadBytes)
	}
}
