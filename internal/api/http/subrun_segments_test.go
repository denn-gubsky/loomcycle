package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// composeSubRunSegments carries the property Decision 3 of the team-workflow
// design rests on: a per-state system prompt is an EXTRA segment, so one
// AgentDef serves N differently-roled states without a clone per role AND
// without losing the prompt cache on its base prompt.

func roles(segs []loop.PromptSegment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.Role)
	}
	return out
}

func TestComposeSubRunSegments_ExtraSystemAppendsAfterTheAgentsOwn(t *testing.T) {
	segs := composeSubRunSegments("You are a code reviewer.", "Judge only exploitability.", "the diff")

	if got := roles(segs); len(got) != 3 || got[0] != "system" || got[1] != "system" || got[2] != "user" {
		t.Fatalf("roles = %v, want [system system user]", got)
	}
	if segs[0].Content[0].Text != "You are a code reviewer." {
		t.Errorf("the agent's own prompt must come FIRST; got %q", segs[0].Content[0].Text)
	}
	if segs[1].Content[0].Text != "Judge only exploitability." {
		t.Errorf("the state's role must come SECOND; got %q", segs[1].Content[0].Text)
	}
	if segs[2].Content[0].Text != "the diff" {
		t.Errorf("user segment = %q, want the prompt", segs[2].Content[0].Text)
	}
}

// The load-bearing one. If the varying per-state segment were marked cacheable
// it would push the cache breakpoint past per-node text, so N states sharing an
// agent would each miss the cache — which is half the reason the design stores
// prompts on the node instead of cloning the agent.
func TestComposeSubRunSegments_OnlyTheAgentsBasePromptIsCacheable(t *testing.T) {
	segs := composeSubRunSegments("base", "per-node role", "input")

	if !segs[0].Content[0].Cacheable {
		t.Error("the agent's base system prompt lost its cache_control — every state sharing this agent now pays full prompt cost")
	}
	if segs[1].Content[0].Cacheable {
		t.Error("the per-state system segment is cacheable; it VARIES per state, so caching it moves the breakpoint past per-node text and defeats the shared prefix")
	}
}

// A caller must not be able to replace the identity of the agent it spawns, only
// add to it. Every non-team caller passes "" and must be unaffected.
func TestComposeSubRunSegments_NoExtraLeavesTheShapeUnchanged(t *testing.T) {
	segs := composeSubRunSegments("You are a code reviewer.", "", "the diff")

	if got := roles(segs); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("roles = %v, want [system user] — an empty extra must add nothing", got)
	}
	if !segs[0].Content[0].Cacheable {
		t.Error("base prompt should still be cacheable")
	}
}

func TestComposeSubRunSegments_AgentWithNoPromptOfItsOwnStillGetsTheStatesRole(t *testing.T) {
	segs := composeSubRunSegments("", "Judge only exploitability.", "the diff")

	if got := roles(segs); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("roles = %v, want [system user]", got)
	}
	if segs[0].Content[0].Text != "Judge only exploitability." {
		t.Errorf("system segment = %q, want the state's role", segs[0].Content[0].Text)
	}
	if segs[0].Content[0].Cacheable {
		t.Error("a per-state role must not become cacheable just because the agent has no prompt of its own")
	}
}

func TestComposeSubRunSegments_BothSystemSegmentsAreTrustedText(t *testing.T) {
	segs := composeSubRunSegments("base", "per-node role", "input")
	for i, seg := range segs {
		if got := seg.Content[0].Type; got != "trusted-text" {
			t.Errorf("segment %d type = %q, want trusted-text", i, got)
		}
	}
}
