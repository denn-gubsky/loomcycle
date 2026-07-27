package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestAgentDefTool_InternalRoundTripsCreateAndFork.
//
// A new yaml field on config.AgentDef that has no home in the persisted overlay
// comes back at its ZERO value on the next resolve, and this class of bug has
// shipped three times already (the F14 channels/evaluation_scopes/interruption
// closure, the F40 *_def_scopes closure, and again in v1.34.0). For `internal:`
// the failure is quiet and expensive: a maintenance agent forked to widen a
// grant would come back NOT internal, its sessions would re-enter consolidation,
// and the pipeline would resume consuming its own output with nothing in the
// logs to say so.
//
// Fork is asserted as well as create, because fork merges an overlay onto a
// PARENT definition — a field present on create and absent from applyOverlay
// passes a create-only test and still loses the marker on every fork.
func TestAgentDefTool_InternalRoundTripsCreateAndFork(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = tools.WithAgentTools(ctx, []string{"*"})

	create := `{"op":"create","name":"maintenance-agent","promote":true,"overlay":{
		"system_prompt":"extract facts",
		"tools":["Memory"],
		"internal":true
	}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(create)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "maintenance-agent")
	if !ok {
		t.Fatal("resolve: maintenance-agent not found after create+promote")
	}
	if !def.Internal {
		t.Error("internal did not round-trip create → persist → resolve")
	}

	// A fork that says nothing about `internal:` INHERITS it from the parent.
	// Losing it here is the live-consequence case: an operator forking the
	// extractor to retune its prompt would silently re-admit every extractor
	// session to consolidation.
	fork := `{"op":"fork","name":"maintenance-agent","promote":true,"overlay":{"system_prompt":"extract facts, tersely"}}`
	if res, _ := tool.Execute(ctx, json.RawMessage(fork)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forked, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "maintenance-agent")
	if !ok {
		t.Fatal("resolve: maintenance-agent not found after fork+promote")
	}
	if !forked.Internal {
		t.Error("internal was lost on fork — a fork that does not mention the field must inherit it")
	}
	if forked.SystemPrompt != "extract facts, tersely" {
		t.Errorf("fork did not apply its own overlay: system_prompt = %q", forked.SystemPrompt)
	}
}

// TestAgentContent_InternalIsContentIdentifying pins the hash decision.
//
// `internal:` is HASHED, unlike the *_def_scopes gates and the `skills:`
// allowlist. Those are AUTHORITY — what the agent is permitted to do — and are
// excluded so an authority change does not mint a new content hash. This is
// BEHAVIOUR: it changes what the runtime does with every run's session, and two
// defs that differ in it are not the same agent. Same reasoning as
// memory_consolidation and unbounded_iterations.
//
// The second half is the byte-stability guarantee: a def that never sets it must
// hash exactly as it did before the field existed, which `omitempty` on a false
// bool gives us. Without that, adding the field would silently invalidate every
// recorded verify-or-fork hash in every deployment.
func TestAgentContent_InternalIsContentIdentifying(t *testing.T) {
	base := agents.AgentContent{Name: "a", SystemPrompt: "p", Tools: []string{"Memory"}}
	marked := base
	marked.Internal = true

	baseHash := agents.Sign(base)
	markedHash := agents.Sign(marked)
	if baseHash == markedHash {
		t.Error("flipping internal did not change content_sha256 — it is behaviour-bearing and must be hashed")
	}

	// Byte-stability: an unmarked agent hashes identically whether or not the
	// field exists, because a false bool with omitempty emits no key.
	again := agents.Sign(agents.AgentContent{Name: "a", SystemPrompt: "p", Tools: []string{"Memory"}})
	if again != baseHash {
		t.Errorf("an unmarked agent's hash is not stable: %s vs %s", again, baseHash)
	}
}
