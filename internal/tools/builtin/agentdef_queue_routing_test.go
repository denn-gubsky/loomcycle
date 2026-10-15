package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// The `routing:` block (RFC EI P4) round-trips through AgentDef create/fork,
// is content-identifying, and follows the routing mode: queue-aware routing
// chooses among a tier's candidates, so a pin has none to choose from.

const queueAwarePinRefusal = "routing.queue_aware chooses among a tier's candidates"

func TestAgentDef_RoutingBlockRoundTripsAndIsHashed(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	got := storedRouting(t, tool, ctx, `{"op":"create","name":"busy","overlay":{"tier":"middle","system_prompt":"s","routing":{"queue_aware":true,"switch_after_ms":15000}}}`)
	if got.Routing == nil || !got.Routing.QueueAware || got.Routing.SwitchAfterMS != 15000 {
		t.Fatalf("stored routing = %+v, want queue_aware with 15000", got.Routing)
	}

	// A fork that only changes the routing block is a new version, not a
	// dedup against the active row.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"busy","overlay":{"routing":{"queue_aware":true,"switch_after_ms":5000}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if deduped, _ := decodeResult(t, res.Text)["deduped"].(bool); deduped {
		t.Fatalf("a fork that changes only routing deduped against the active row: %s", res.Text)
	}
	// A fork that does not name routing keeps its parent's — the active
	// version, which a fork does not replace.
	got = storedRouting(t, tool, ctx, `{"op":"fork","name":"busy","overlay":{"effort":"low"}}`)
	if got.Routing == nil || !got.Routing.QueueAware || got.Routing.SwitchAfterMS != 15000 {
		t.Errorf("a fork that does not name routing changed it: %+v", got.Routing)
	}
}

// A fork that pins a model drops the parent's routing block, as it drops the
// parent's tier.
func TestAgentDefFork_PinOverlayClearsQueueAwareRouting(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	storedRouting(t, tool, ctx, `{"op":"create","name":"busy","overlay":{"tier":"middle","system_prompt":"s","routing":{"queue_aware":true}}}`)
	got := storedRouting(t, tool, ctx, `{"op":"fork","name":"busy","overlay":{"provider":"ollama-local","model":"qwen3:32b"}}`)
	if got.Routing != nil {
		t.Errorf("pinned fork kept routing %+v", got.Routing)
	}
}

// Turning queue-aware routing on for a pinned agent is refused — on create,
// and on a fork whose overlay alone is fine but whose parent is pinned.
func TestAgentDef_QueueAwareRoutingOnAPinIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	for _, call := range []string{
		`{"op":"create","name":"pinned","overlay":{"provider":"anthropic","model":"claude-haiku-4-5","routing":{"queue_aware":true}}}`,
		// "researcher" is a static agent pinned to anthropic/claude-haiku-4-5.
		`{"op":"fork","name":"researcher","overlay":{"routing":{"queue_aware":true}}}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(call))
		if !res.IsError || !strings.Contains(res.Text, queueAwarePinRefusal) {
			t.Errorf("%s: got %s, want a refusal naming routing.queue_aware", call, res.Text)
		}
	}
	if _, err := tool.Store.AgentDefGetActive(ctx, "", "pinned"); err == nil {
		t.Error("a refused create still stored a row")
	}
}
