package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
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
