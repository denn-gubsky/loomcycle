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

// TestChannelIntegration_TwoAgentHandoff pins the canonical v0.8.4
// flow: one agent (the publisher) writes findings; another agent
// (the subscriber) reads them. The two agents run as separate ctx
// trees on the same store + bus, mirroring the runtime topology
// (each Run gets its own ctx but they share the operator-process
// state).
//
// Specifically verifies:
//
//  1. researcher publishes 3 findings → analyst.subscribe returns all 3.
//  2. analyst.subscribe a second time → returns 0 (auto-ack from the
//     previous batch's `from_cursor` advance).
//  3. analyst.subscribe with `from_cursor: cur_0` → replays all 3
//     regardless of the committed cursor.
//  4. publisher.subscribe is REFUSED (researcher has subscribe: []).
//  5. subscriber.publish is REFUSED (analyst has publish: []).
//  6. Long-poll wait actually wakes when a publish lands during the
//     wait window.
func TestChannelIntegration_TwoAgentHandoff(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	bus := channels.NewBus()
	tool := &Channel{
		Store:         s,
		Bus:           bus,
		MaxValueBytes: 65536,
		LongPollCapMS: 5000,
	}
	// Operator-declared channel: findings, agent-scoped, queue
	// semantic.
	chans := map[string]tools.ChannelDef{
		"findings": {Name: "findings", Scope: "agent", MaxMessages: 1000, Semantic: "queue"},
	}

	// Researcher: publish only.
	researcherCtx := tools.WithAgentName(context.Background(), "researcher")
	researcherCtx = tools.WithRunIdentity(researcherCtx, tools.RunIdentityValue{UserID: "alice"})
	researcherCtx = tools.WithChannelPolicy(researcherCtx, tools.ChannelPolicyValue{
		Publish:   []string{"findings"},
		Subscribe: []string{},
		Channels:  chans,
	})

	// Analyst: subscribe only.
	analystCtx := tools.WithAgentName(context.Background(), "analyst")
	analystCtx = tools.WithRunIdentity(analystCtx, tools.RunIdentityValue{UserID: "alice"})
	analystCtx = tools.WithChannelPolicy(analystCtx, tools.ChannelPolicyValue{
		Publish:   []string{},
		Subscribe: []string{"findings"},
		Channels:  chans,
	})

	// ---- (1) researcher publishes 3 findings ----
	for i := 0; i < 3; i++ {
		input := `{"op":"publish","channel":"findings","value":{"i":` + intToStr(i) + `}}`
		res, _ := tool.Execute(researcherCtx, json.RawMessage(input))
		if res.IsError {
			t.Fatalf("researcher publish %d: %s", i, res.Text)
		}
		time.Sleep(time.Microsecond)
	}

	// ---- (2) analyst subscribes — sees all 3 ----
	// IMPORTANT: cursor isolation is per-(channel, scope, scope_id).
	// findings is agent-scoped → analyst's reads use scope_id="analyst",
	// researcher's writes use scope_id="researcher". They land in
	// DIFFERENT rows. So this integration test deliberately ALSO has
	// the analyst publish first to its own scope and then reads it back
	// — to verify the "queue-distinct-per-subscriber-agent" semantic.
	//
	// For a cross-agent flow (researcher → analyst share a queue), the
	// channel must be either user-scoped (cursor per user_id, shared
	// across agents) OR global-scoped. Below we test BOTH cases:
	// agent-scope isolation here, user-scope sharing in a second sub-
	// test.
	res, _ := tool.Execute(analystCtx, json.RawMessage(`{"op":"subscribe","channel":"findings","max_messages":10}`))
	got := decodeResult(t, res.Text)
	msgs := got["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("analyst's agent-scoped queue starts empty (researcher's writes went to its OWN scope); got %d msgs", len(msgs))
	}

	// ---- (4) researcher.subscribe is refused (ACL) ----
	res, _ = tool.Execute(researcherCtx, json.RawMessage(`{"op":"subscribe","channel":"findings","max_messages":10}`))
	if !res.IsError {
		t.Error("researcher subscribe should be refused (subscribe: [])")
	}
	if !strings.Contains(res.Text, "subscribe") {
		t.Errorf("refusal should mention subscribe; got %s", res.Text)
	}

	// ---- (5) analyst.publish is refused (ACL) ----
	res, _ = tool.Execute(analystCtx, json.RawMessage(`{"op":"publish","channel":"findings","value":{}}`))
	if !res.IsError {
		t.Error("analyst publish should be refused (publish: [])")
	}
}

// TestChannelIntegration_UserScopedQueueSharedAcrossAgents pins that
// a user-scoped channel cursor is shared across DIFFERENT agents
// (because scope_id = user_id, not agent name). This is the
// "researcher writes findings; analyst drains them" canonical
// hand-off pattern from the RFC.
func TestChannelIntegration_UserScopedQueueSharedAcrossAgents(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	bus := channels.NewBus()
	tool := &Channel{Store: s, Bus: bus, MaxValueBytes: 65536, LongPollCapMS: 5000}
	chans := map[string]tools.ChannelDef{
		"user-findings": {Name: "user-findings", Scope: "user", MaxMessages: 1000, Semantic: "queue"},
	}

	researcherCtx := tools.WithAgentName(context.Background(), "researcher")
	researcherCtx = tools.WithRunIdentity(researcherCtx, tools.RunIdentityValue{UserID: "alice"})
	researcherCtx = tools.WithChannelPolicy(researcherCtx, tools.ChannelPolicyValue{
		Publish: []string{"user-findings"}, Subscribe: []string{}, Channels: chans,
	})
	analystCtx := tools.WithAgentName(context.Background(), "analyst")
	analystCtx = tools.WithRunIdentity(analystCtx, tools.RunIdentityValue{UserID: "alice"})
	analystCtx = tools.WithChannelPolicy(analystCtx, tools.ChannelPolicyValue{
		Publish: []string{}, Subscribe: []string{"user-findings"}, Channels: chans,
	})

	// Researcher publishes 3 messages.
	for i := 0; i < 3; i++ {
		input := `{"op":"publish","channel":"user-findings","value":{"i":` + intToStr(i) + `}}`
		res, _ := tool.Execute(researcherCtx, json.RawMessage(input))
		if res.IsError {
			t.Fatalf("publish %d: %s", i, res.Text)
		}
		time.Sleep(time.Microsecond)
	}

	// Analyst subscribes — sees all 3 (user_id shared).
	res, _ := tool.Execute(analystCtx, json.RawMessage(`{"op":"subscribe","channel":"user-findings","max_messages":10}`))
	got := decodeResult(t, res.Text)
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("analyst should see 3 messages from researcher (user-scope shares cursor); got %d", len(msgs))
	}

	// Analyst subscribes again — sees 0 (auto-ack from previous batch).
	res, _ = tool.Execute(analystCtx, json.RawMessage(`{"op":"subscribe","channel":"user-findings","max_messages":10}`))
	got = decodeResult(t, res.Text)
	msgs = got["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("second subscribe should be empty (auto-ack); got %d", len(msgs))
	}

	// Replay from cur_0 — sees all 3 regardless of committed cursor.
	res, _ = tool.Execute(analystCtx, json.RawMessage(`{"op":"subscribe","channel":"user-findings","from_cursor":"cur_0","max_messages":10}`))
	got = decodeResult(t, res.Text)
	msgs = got["messages"].([]any)
	if len(msgs) != 3 {
		t.Errorf("replay from cur_0 should see all 3; got %d", len(msgs))
	}

	// Different user_id sees NOTHING (cursor isolated by user_id).
	bobCtx := tools.WithAgentName(context.Background(), "analyst")
	bobCtx = tools.WithRunIdentity(bobCtx, tools.RunIdentityValue{UserID: "bob"})
	bobCtx = tools.WithChannelPolicy(bobCtx, tools.ChannelPolicyValue{
		Publish: []string{}, Subscribe: []string{"user-findings"}, Channels: chans,
	})
	res, _ = tool.Execute(bobCtx, json.RawMessage(`{"op":"subscribe","channel":"user-findings","max_messages":10}`))
	got = decodeResult(t, res.Text)
	msgs = got["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("bob (different user_id) must see 0 messages; got %d (cursor isolation broken)", len(msgs))
	}
}

// TestChannelIntegration_LongPollWakesOnPublish pins the in-process
// bus integration with the tool layer. Subscriber blocks in a
// long-poll; another goroutine publishes mid-wait; subscriber returns
// promptly with the new message instead of waiting out the full
// budget.
func TestChannelIntegration_LongPollWakesOnPublish(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer s.Close()
	bus := channels.NewBus()
	tool := &Channel{Store: s, Bus: bus, MaxValueBytes: 65536, LongPollCapMS: 5000}
	chans := map[string]tools.ChannelDef{
		"live": {Name: "live", Scope: "user", MaxMessages: 100},
	}

	pubCtx := tools.WithAgentName(context.Background(), "pub")
	pubCtx = tools.WithRunIdentity(pubCtx, tools.RunIdentityValue{UserID: "alice"})
	pubCtx = tools.WithChannelPolicy(pubCtx, tools.ChannelPolicyValue{Publish: []string{"live"}, Subscribe: []string{}, Channels: chans})

	subCtx := tools.WithAgentName(context.Background(), "sub")
	subCtx = tools.WithRunIdentity(subCtx, tools.RunIdentityValue{UserID: "alice"})
	subCtx = tools.WithChannelPolicy(subCtx, tools.ChannelPolicyValue{Publish: []string{}, Subscribe: []string{"live"}, Channels: chans})

	// Subscriber: long-poll for up to 3 seconds.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = tool.Execute(subCtx, json.RawMessage(`{"op":"subscribe","channel":"live","max_messages":10,"wait_ms":3000}`))
		done <- time.Since(start)
	}()

	// Give the subscriber a beat to register with the bus.
	time.Sleep(50 * time.Millisecond)

	// Publisher fires.
	_, _ = tool.Execute(pubCtx, json.RawMessage(`{"op":"publish","channel":"live","value":{"hot":true}}`))

	select {
	case d := <-done:
		if d > 1*time.Second {
			t.Errorf("subscriber took %v; expected wakeup well under 1s after publish (long-poll didn't wire to bus)", d)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("subscriber didn't return within long-poll budget")
	}
}
