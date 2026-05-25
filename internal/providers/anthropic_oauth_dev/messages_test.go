package anthropic_oauth_dev

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestAdaptSystemForOAuth_PrependsClaudeCodeIdentity pins the
// Pi-equivalent Phase B finding: under OAuth, every Messages API call
// must lead the system blocks with the verbatim Claude Code identity
// text. Without it Anthropic returns "messages: Input should be a
// valid array" (a misleading error surfacing the broader OAuth
// validator failure).
func TestAdaptSystemForOAuth_PrependsClaudeCodeIdentity(t *testing.T) {
	in := []providers.ContentBlock{
		{Type: "text", Text: "You are a helper.", Cacheable: true},
	}
	out := adaptSystemForOAuth(in)
	if len(out) != 2 {
		t.Fatalf("got %d blocks, want 2 (identity + operator)", len(out))
	}
	if out[0].Type != "text" || out[0].Text != claudeCodeIdentityText {
		t.Errorf("block[0] should be Claude Code identity, got %+v", out[0])
	}
	if !out[0].Cacheable {
		t.Errorf("identity block should be Cacheable=true for prompt-cache hit")
	}
	if out[1].Text != "You are a helper." {
		t.Errorf("operator system text lost: %+v", out[1])
	}
	if !out[1].Cacheable {
		t.Errorf("operator block's Cacheable hint lost in copy")
	}
}

// TestAdaptSystemForOAuth_HandlesEmptyInput: an agent without a
// system_prompt still needs the identity block — OAuth validator
// rejects requests with no system at all.
func TestAdaptSystemForOAuth_HandlesEmptyInput(t *testing.T) {
	out := adaptSystemForOAuth(nil)
	if len(out) != 1 {
		t.Fatalf("got %d blocks, want 1 (identity only)", len(out))
	}
	if out[0].Text != claudeCodeIdentityText {
		t.Errorf("identity block text wrong: %q", out[0].Text)
	}
}

// TestAdaptSystemForOAuth_DoesNotMutateInput: the loop may pass the
// same Request to multiple providers in one iteration; mutating
// req.System would corrupt the other provider's call.
func TestAdaptSystemForOAuth_DoesNotMutateInput(t *testing.T) {
	in := []providers.ContentBlock{{Type: "text", Text: "x"}}
	_ = adaptSystemForOAuth(in)
	if len(in) != 1 {
		t.Errorf("input mutated: %d blocks (want 1)", len(in))
	}
	if in[0].Text != "x" {
		t.Errorf("input[0].Text mutated: %q", in[0].Text)
	}
}
