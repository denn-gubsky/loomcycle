package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// holdFixture is channelFixture's twin with ONE difference that is the whole
// point: `gate` is declared hold. `open` is the control — same shape, no hold —
// so every assertion below can be read as "held, unlike this".
func holdFixture(t *testing.T) (*Channel, context.Context, func()) {
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
		Publish:   []string{"gate", "open"},
		Subscribe: []string{"gate", "open"},
		Channels: map[string]tools.ChannelDef{
			"gate": {Name: "gate", Scope: "agent", Semantic: "queue", Hold: true},
			"open": {Name: "open", Scope: "agent", Semantic: "queue"},
		},
	})
	return tool, ctx, func() { _ = s.Close() }
}

func publishTo(t *testing.T, tool *Channel, ctx context.Context, channel string, i int) map[string]any {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"publish","channel":"`+channel+`","value":{"i":`+intToStr(i)+`}}`))
	if res.IsError {
		t.Fatalf("publish to %s: %s", channel, res.Text)
	}
	// Distinct message ids are minted from the clock; keep them ordered.
	time.Sleep(time.Microsecond)
	return decodeResult(t, res.Text)
}

func subscribeCount(t *testing.T, tool *Channel, ctx context.Context, channel string) int {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"subscribe","channel":"`+channel+`","max_messages":50}`))
	if res.IsError {
		t.Fatalf("subscribe %s: %s", channel, res.Text)
	}
	got := decodeResult(t, res.Text)
	msgs, _ := got["messages"].([]any)
	return len(msgs)
}

// A publish to a held channel is STORED and REPORTED, but not delivered — the
// whole contract in one test. The control channel proves the fixture would
// otherwise deliver.
func TestChannelHold_PublishStoresButDoesNotDeliver(t *testing.T) {
	tool, ctx, cleanup := holdFixture(t)
	defer cleanup()

	got := publishTo(t, tool, ctx, "gate", 1)
	if got["message_id"] == "" {
		t.Errorf("held publish should still report a message_id: %v", got)
	}
	if held, _ := got["held"].(bool); !held {
		t.Errorf("held publish must report held:true, got %v", got)
	}
	if _, ok := got["visible_at"]; ok {
		t.Errorf("a held publish must not promise a visible_at: %v", got)
	}
	if n := subscribeCount(t, tool, ctx, "gate"); n != 0 {
		t.Errorf("held channel delivered %d messages, want 0", n)
	}

	// Control: the identical publish on a channel with no hold arrives.
	publishTo(t, tool, ctx, "open", 1)
	if n := subscribeCount(t, tool, ctx, "open"); n != 1 {
		t.Errorf("control channel delivered %d messages, want 1", n)
	}
}

// Release hands over the OLDEST first, one at a time by default, and reports
// what is left — the single-step property the hold exists for.
func TestChannelHold_ReleaseHandsOverOldestFirst(t *testing.T) {
	tool, ctx, cleanup := holdFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		publishTo(t, tool, ctx, "gate", i)
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"release","channel":"gate"}`))
	if res.IsError {
		t.Fatalf("release: %s", res.Text)
	}
	got := decodeResult(t, res.Text)
	if n, _ := got["released_count"].(float64); n != 1 {
		t.Errorf("default release count = %v, want 1", got["released_count"])
	}
	if n, _ := got["still_held"].(float64); n != 2 {
		t.Errorf("still_held = %v, want 2", got["still_held"])
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"gate","max_messages":50}`))
	if res.IsError {
		t.Fatalf("subscribe: %s", res.Text)
	}
	sub := decodeResult(t, res.Text)
	msgs, _ := sub["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("after one release, delivered %d messages, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	val, _ := first["value"].(map[string]any)
	if i, _ := val["i"].(float64); i != 0 {
		t.Errorf("released message carries i=%v, want the OLDEST (0)", val["i"])
	}

	// Releasing the rest drains the queue.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"release","channel":"gate","count":5}`))
	if res.IsError {
		t.Fatalf("release rest: %s", res.Text)
	}
	got = decodeResult(t, res.Text)
	if n, _ := got["released_count"].(float64); n != 2 {
		t.Errorf("released %v, want the 2 remaining", got["released_count"])
	}
	if n, _ := got["still_held"].(float64); n != 0 {
		t.Errorf("still_held = %v, want 0", got["still_held"])
	}
	if n := subscribeCount(t, tool, ctx, "gate"); n != 2 {
		t.Errorf("delivered %d messages after draining, want the 2 released", n)
	}
}

// Release is gated by the PUBLISH allowlist: it completes a publish, so a
// subscribe-only agent may not make a held message arrive.
func TestChannelHold_ReleaseNeedsPublishNotSubscribe(t *testing.T) {
	tool, ctx, cleanup := holdFixture(t)
	defer cleanup()
	publishTo(t, tool, ctx, "gate", 1)

	readOnly := tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"open"},
		Subscribe: []string{"gate"},
		Channels: map[string]tools.ChannelDef{
			"gate": {Name: "gate", Scope: "agent", Hold: true},
			"open": {Name: "open", Scope: "agent"},
		},
	})
	res, _ := tool.Execute(readOnly, json.RawMessage(`{"op":"release","channel":"gate"}`))
	if !res.IsError {
		t.Fatalf("release with subscribe-only ACL should be refused; got %s", res.Text)
	}
	if n := subscribeCount(t, tool, ctx, "gate"); n != 0 {
		t.Errorf("refused release still delivered %d messages", n)
	}
}

// Releasing a channel with nothing held is an answer, not an error.
func TestChannelHold_ReleaseOnEmptyQueueReportsZero(t *testing.T) {
	tool, ctx, cleanup := holdFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"release","channel":"gate","count":3}`))
	if res.IsError {
		t.Fatalf("release on an empty hold queue should not error: %s", res.Text)
	}
	got := decodeResult(t, res.Text)
	if n, _ := got["released_count"].(float64); n != 0 {
		t.Errorf("released_count = %v, want 0", got["released_count"])
	}
	if _, ok := got["released"].([]any); !ok {
		t.Errorf("released must be [] not null: %s", res.Text)
	}
}

// A held message still honours its TTL: holding is not a way to outlive the
// retention the publisher declared. The one test here that sleeps — the TTL
// floor is one second, and faking it would fake the very filter under test.
func TestChannelHold_ExpiredHeldMessageIsNeverReleased(t *testing.T) {
	tool, ctx, cleanup := holdFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"gate","ttl":1,"value":{"i":0}}`))
	if res.IsError {
		t.Fatalf("publish: %s", res.Text)
	}
	time.Sleep(1100 * time.Millisecond)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"release","channel":"gate","count":10}`))
	if res.IsError {
		t.Fatalf("release: %s", res.Text)
	}
	got := decodeResult(t, res.Text)
	if n, _ := got["released_count"].(float64); n != 0 {
		t.Errorf("released %v expired held message(s), want 0", got["released_count"])
	}
	if n, _ := got["still_held"].(float64); n != 0 {
		t.Errorf("still_held counts an expired message: %v", got["still_held"])
	}
	if n := subscribeCount(t, tool, ctx, "gate"); n != 0 {
		t.Errorf("expired held message was delivered (%d)", n)
	}
}
