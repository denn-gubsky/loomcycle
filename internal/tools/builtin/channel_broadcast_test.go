package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Channel.broadcast — the symmetric fan-OUT to await's fan-in: publish one
// payload to N channels in a single call. These fail on a tree without the
// op (unknown op).

// broadcastFixture: c1/c2/c3 are publish+subscribe globals; "readonly" is
// subscribe-only (publish denied) for the atomic-ACL test; "_system/sig" is
// declared but reserved.
func broadcastFixture(t *testing.T) (*Channel, context.Context, func()) {
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
	ctx := tools.WithAgentName(context.Background(), "orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"c1", "c2", "c3"},
		Subscribe: []string{"c1", "c2", "c3", "readonly"},
		Channels: map[string]tools.ChannelDef{
			"c1":          {Name: "c1", Scope: "global", Semantic: "queue"},
			"c2":          {Name: "c2", Scope: "global", Semantic: "queue"},
			"c3":          {Name: "c3", Scope: "global", Semantic: "queue"},
			"readonly":    {Name: "readonly", Scope: "global", Semantic: "queue"},
			"_system/sig": {Name: "_system/sig", Scope: "global", Semantic: "broadcast"},
		},
	})
	return tool, ctx, func() { _ = s.Close() }
}

func peekCount(t *testing.T, tool *Channel, ctx context.Context, channel string) int {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"peek","channel":"`+channel+`","from_cursor":"cur_0","max_messages":10}`))
	if res.IsError {
		t.Fatalf("peek %s: %s", channel, res.Text)
	}
	msgs, _ := decodeResult(t, res.Text)["messages"].([]any)
	return len(msgs)
}

func TestChannelBroadcast_FansOutToAll(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","c2","c3"],"value":{"go":1}}`))
	if res.IsError {
		t.Fatalf("broadcast: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["published"] != float64(3) {
		t.Errorf("published = %v, want 3", out["published"])
	}
	if out["failed"] != float64(0) {
		t.Errorf("failed = %v, want 0", out["failed"])
	}
	// Each channel actually received the payload.
	for _, ch := range []string{"c1", "c2", "c3"} {
		if n := peekCount(t, tool, ctx, ch); n != 1 {
			t.Errorf("%s has %d messages, want 1", ch, n)
		}
	}
}

func TestChannelBroadcast_DedupesChannels(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","c1","c2"],"value":{"x":1}}`))
	if res.IsError {
		t.Fatalf("broadcast: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["published"] != float64(2) {
		t.Errorf("published = %v, want 2 (c1 deduped)", out["published"])
	}
	if n := peekCount(t, tool, ctx, "c1"); n != 1 {
		t.Errorf("c1 got %d messages, want 1 (dedup must not double-publish)", n)
	}
}

// The headline symmetric-to-await test: a denied channel refuses the WHOLE
// op, and NOTHING is published (atomic ACL pre-flight, no partial broadcast).
func TestChannelBroadcast_AtomicACLRefusal(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	// "readonly" is subscribe-only — not in the publish allowlist.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","readonly"],"value":{"x":1}}`))
	if !res.IsError {
		t.Fatalf("broadcast over a non-publishable channel must refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "publish") {
		t.Errorf("refusal should mention publish ACL; got %s", res.Text)
	}
	// c1 must NOT have received the payload — the op is atomic at the ACL
	// pre-flight, so a denial on ANY channel publishes to NONE.
	if n := peekCount(t, tool, ctx, "c1"); n != 0 {
		t.Errorf("c1 got %d messages — broadcast must be all-or-nothing on ACL denial", n)
	}
}

func TestChannelBroadcast_SystemChannelRefused(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","_system/sig"],"value":{"x":1}}`))
	if !res.IsError {
		t.Fatalf("broadcast to a _system/ channel must refuse; got %s", res.Text)
	}
	if n := peekCount(t, tool, ctx, "c1"); n != 0 {
		t.Errorf("c1 got %d messages — system-channel denial must abort the whole op", n)
	}
}

func TestChannelBroadcast_InvalidValueRefused(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	// Missing value.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","c2"]}`))
	if !res.IsError {
		t.Fatalf("broadcast without value must refuse; got %s", res.Text)
	}
	if n := peekCount(t, tool, ctx, "c1"); n != 0 {
		t.Errorf("c1 got %d messages — a bad payload must publish to none", n)
	}
}

func TestChannelBroadcast_EmptyChannelsRefused(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":[],"value":{"x":1}}`))
	if !res.IsError {
		t.Fatalf("broadcast with empty channels must refuse; got %s", res.Text)
	}
}

func TestChannelBroadcast_Deferred(t *testing.T) {
	tool, ctx, cleanup := broadcastFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"broadcast","channels":["c1","c2"],"value":{"x":1},"deliver_at":"2099-01-01T00:00:00Z"}`))
	if res.IsError {
		t.Fatalf("broadcast (deferred): %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want 2", out["results"])
	}
	for _, r := range results {
		m := r.(map[string]any)
		if _, ok := m["visible_at"].(string); !ok {
			t.Errorf("deferred result missing visible_at: %v", m)
		}
	}
	// Not visible yet — peek (which respects visibility) sees nothing.
	if n := peekCount(t, tool, ctx, "c1"); n != 0 {
		t.Errorf("c1 has %d visible messages, want 0 (deferred to 2099)", n)
	}
}

func TestChannelBroadcast_InSchemaEnum(t *testing.T) {
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(channelInputSchema), &schema); err != nil {
		t.Fatalf("channelInputSchema invalid JSON: %v", err)
	}
	found := false
	for _, op := range schema.Properties.Op.Enum {
		if op == "broadcast" {
			found = true
		}
	}
	if !found {
		t.Errorf("op enum %v missing \"broadcast\"", schema.Properties.Op.Enum)
	}
}
