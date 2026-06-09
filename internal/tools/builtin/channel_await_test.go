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

// RFC S / F35 — Channel.await fan-in barrier. These tests fail on main
// (await is an unknown op there).

// awaitFixture builds a Channel tool over in-memory SQLite + a fresh Bus
// with three GLOBAL channels c1/c2/c3 the agent may publish + subscribe,
// plus "secret" which it may publish but NOT subscribe (ACL test).
func awaitFixture(t *testing.T) (*Channel, context.Context, func()) {
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
	ctx := tools.WithAgentName(context.Background(), "consolidator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"c1", "c2", "c3", "secret"},
		Subscribe: []string{"c1", "c2", "c3"},
		Channels: map[string]tools.ChannelDef{
			"c1":     {Name: "c1", Scope: "global", Semantic: "queue"},
			"c2":     {Name: "c2", Scope: "global", Semantic: "queue"},
			"c3":     {Name: "c3", Scope: "global", Semantic: "queue"},
			"secret": {Name: "secret", Scope: "global", Semantic: "queue"},
		},
	})
	return tool, ctx, func() { _ = s.Close() }
}

func mustPublish(t *testing.T, tool *Channel, ctx context.Context, channel string) {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"publish","channel":"`+channel+`","value":{"src":"`+channel+`"}}`))
	if res.IsError {
		t.Fatalf("publish %s: %s", channel, res.Text)
	}
}

func TestChannelAwait_AtLeastNSynchronous(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	mustPublish(t, tool, ctx, "c1")
	mustPublish(t, tool, ctx, "c2")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"await","channels":["c1","c2","c3"],"mode":"at_least","n":2}`))
	if res.IsError {
		t.Fatalf("await: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["satisfied"] != true {
		t.Errorf("satisfied = %v, want true", out["satisfied"])
	}
	if out["timed_out"] != false {
		t.Errorf("timed_out = %v, want false", out["timed_out"])
	}
	if tm, _ := out["total_messages"].(float64); tm < 2 {
		t.Errorf("total_messages = %v, want >= 2", out["total_messages"])
	}
	fired, _ := out["fired"].([]any)
	if len(fired) != 2 {
		t.Errorf("fired = %v, want 2 channels", fired)
	}
}

func TestChannelAwait_AnyLongPollWake(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()

	type result struct {
		out     map[string]any
		isError bool
		text    string
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"await","channels":["c1","c2"],"mode":"any","wait_ms":5000}`))
		r := result{isError: res.IsError, text: res.Text, elapsed: time.Since(start)}
		if !res.IsError {
			r.out = decodeResult(t, res.Text)
		}
		done <- r
	}()

	// Publish ~50ms in — the await should wake and return well under the
	// 5s budget.
	time.Sleep(50 * time.Millisecond)
	mustPublish(t, tool, ctx, "c2")

	select {
	case r := <-done:
		if r.isError {
			t.Fatalf("await: %s", r.text)
		}
		if r.out["satisfied"] != true {
			t.Errorf("satisfied = %v, want true", r.out["satisfied"])
		}
		if r.elapsed > 3*time.Second {
			t.Errorf("await took %v — should wake on publish, not wait the full 5s", r.elapsed)
		}
		fired, _ := r.out["fired"].([]any)
		if len(fired) != 1 || fired[0] != "c2" {
			t.Errorf("fired = %v, want [c2]", fired)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("await did not return within 6s — wake likely missed")
	}
}

func TestChannelAwait_AllTimeoutPartial(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	// Only 2 of the 3 channels get a message; mode=all can't be satisfied.
	mustPublish(t, tool, ctx, "c1")
	mustPublish(t, tool, ctx, "c2")

	start := time.Now()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"await","channels":["c1","c2","c3"],"mode":"all","wait_ms":200}`))
	elapsed := time.Since(start)
	if res.IsError {
		t.Fatalf("await must NOT error on timeout: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["satisfied"] != false {
		t.Errorf("satisfied = %v, want false", out["satisfied"])
	}
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v, want true", out["timed_out"])
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned in %v — should have waited ~200ms for the timeout", elapsed)
	}
	// Partial results: c1 + c2 fired, c3 empty.
	results, _ := out["results"].(map[string]any)
	if results == nil || results["c1"] == nil || results["c3"] == nil {
		t.Fatalf("results missing channels: %v", out["results"])
	}
	c3 := results["c3"].(map[string]any)
	if msgs, _ := c3["messages"].([]any); len(msgs) != 0 {
		t.Errorf("c3 should have 0 messages, got %v", msgs)
	}
}

func TestChannelAwait_ACLRefusedOnUnsubscribedChannel(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	// "secret" is publish-only for this agent — await (subscribe-side) refuses.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"await","channels":["c1","secret"],"mode":"any"}`))
	if !res.IsError {
		t.Fatalf("await over an unsubscribed channel must be refused; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "subscribe") {
		t.Errorf("refusal should mention subscribe ACL; got %s", res.Text)
	}
}

func TestChannelAwait_NonCommitting(t *testing.T) {
	tool, ctx, cleanup := awaitFixture(t)
	defer cleanup()
	mustPublish(t, tool, ctx, "c1")

	// await detects the message...
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"await","channels":["c1"],"mode":"any"}`))
	if res.IsError {
		t.Fatalf("await: %s", res.Text)
	}
	if decodeResult(t, res.Text)["satisfied"] != true {
		t.Fatal("await should be satisfied after a publish")
	}

	// ...but does NOT commit, so a subsequent subscribe still sees it.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"subscribe","channel":"c1","max_messages":10}`))
	if res.IsError {
		t.Fatalf("subscribe: %s", res.Text)
	}
	msgs, _ := decodeResult(t, res.Text)["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("subscribe after await got %d messages, want 1 (await must be non-committing)", len(msgs))
	}
}

func TestChannelAwait_InSchemaEnum(t *testing.T) {
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
		if op == "await" {
			found = true
		}
	}
	if !found {
		t.Errorf("op enum %v missing \"await\"", schema.Properties.Op.Enum)
	}
}
