package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// A definition routes by a provider/model pin OR by a tier, never both. These
// tests pin how AgentDef create/fork keep it one choice: the live incident was
// a fork that added provider:ollama-local + a model to a tier:middle agent,
// kept the tier, and fell back to a paid provider the author meant to leave.

const pinTierRefusal = "cannot set both explicit provider/model pin and tier (pick one)"

// storedRouting forks/creates through the tool and returns the stored row's
// routing fields as the runtime reads them back (lookup.AgentFromDefRow).
func storedRouting(t *testing.T, tool *AgentDef, ctx context.Context, call string) config.AgentDef {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(call))
	if res.IsError {
		t.Fatalf("%s: %s", call, res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet(%s): %v", defID, err)
	}
	def, ok := lookup.AgentFromDefRow(row)
	if !ok {
		t.Fatalf("AgentFromDefRow(%s) failed: %s", defID, row.Definition)
	}
	return def
}

func TestAgentDefFork_PinOverlayClearsParentTier(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	storedRouting(t, tool, ctx, `{"op":"create","name":"walker","overlay":{"tier":"middle","system_prompt":"walk"}}`)
	got := storedRouting(t, tool, ctx, `{"op":"fork","name":"walker","overlay":{"provider":"ollama-local","model":"qwen3:32b"}}`)

	if got.Tier != "" {
		t.Errorf("forked row kept tier %q beside the pin — the tier would still win at resolution", got.Tier)
	}
	if got.Provider != "ollama-local" || got.Model != "qwen3:32b" {
		t.Errorf("forked row pin = (%q, %q), want (ollama-local, qwen3:32b)", got.Provider, got.Model)
	}
}

// A model alone is a pin too: it must clear the tier the same way.
func TestAgentDefFork_ModelOnlyOverlayClearsParentTier(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	storedRouting(t, tool, ctx, `{"op":"create","name":"walker","overlay":{"tier":"middle","effort":"high"}}`)
	got := storedRouting(t, tool, ctx, `{"op":"fork","name":"walker","overlay":{"model":"local-medium"}}`)

	if got.Tier != "" || got.Model != "local-medium" {
		t.Errorf("forked row (model %q, tier %q), want (local-medium, \"\")", got.Model, got.Tier)
	}
	// effort qualifies either mode and survives the switch.
	if got.Effort != "high" {
		t.Errorf("effort = %q, want high (routing switch must not touch effort)", got.Effort)
	}
}

func TestAgentDefFork_TierOverlayClearsParentPin(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// "researcher" is a static agent pinned to anthropic/claude-haiku-4-5.
	got := storedRouting(t, tool, ctx, `{"op":"fork","name":"researcher","overlay":{"tier":"low"}}`)

	if got.Provider != "" || got.Model != "" {
		t.Errorf("forked row kept pin (%q, %q) beside tier — the pin would win at resolution", got.Provider, got.Model)
	}
	if got.Tier != "low" {
		t.Errorf("tier = %q, want low", got.Tier)
	}
}

// A tier-routed STATIC agent can be pinned by a fork: the bootstrapped lineage
// root carries the tier, and the fork's pin replaces it.
func TestAgentDefFork_StaticTierAgentForkedToPinIsPinned(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Agents["planner"] = config.AgentDef{
		Tier:         "middle",
		SystemPrompt: "plan",
		Tools:        []string{"Read"},
	}

	got := storedRouting(t, tool, ctx, `{"op":"fork","name":"planner","overlay":{"provider":"ollama-local","model":"qwen3:32b"}}`)

	if got.Tier != "" || got.Provider != "ollama-local" || got.Model != "qwen3:32b" {
		t.Errorf("fork of tier static = (provider %q, model %q, tier %q), want (ollama-local, qwen3:32b, \"\")",
			got.Provider, got.Model, got.Tier)
	}
}

func TestAgentDefFork_OverlayWithPinAndTierIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	for _, ov := range []string{
		`{"provider":"ollama-local","tier":"middle"}`,
		`{"model":"qwen3:32b","tier":"middle"}`,
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":`+ov+`}`))
		if !res.IsError {
			t.Errorf("fork overlay %s: want refusal, got %s", ov, res.Text)
			continue
		}
		if !strings.Contains(res.Text, pinTierRefusal) {
			t.Errorf("fork overlay %s: refusal %q does not carry the yaml wording", ov, res.Text)
		}
	}
}

func TestAgentDefCreate_PinAndTierIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"both","overlay":{"provider":"anthropic","model":"claude-haiku-4-5","tier":"middle"}}`))
	if !res.IsError {
		t.Fatalf("create with pin + tier: want refusal, got %s", res.Text)
	}
	if !strings.Contains(res.Text, pinTierRefusal) {
		t.Errorf("refusal %q does not carry the yaml wording", res.Text)
	}
	if _, err := tool.Store.AgentDefGetActive(ctx, "", "both"); err == nil {
		t.Error("a refused create still stored a row")
	}
}
