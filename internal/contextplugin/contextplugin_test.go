package contextplugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func boolPtr(b bool) *bool { return &b }

// TestRedactPlugin_ScrubsOutboundText pins the core behavior: an injected
// secret VALUE (Tier-A) and a pattern-shaped secret (Tier-B) are masked in the
// outbound system + message text.
func TestRedactPlugin_ScrubsOutboundText(t *testing.T) {
	chain, err := Build(
		[]config.ContextPluginSpec{{Name: "redact"}},
		map[string]string{"ACME_API_TOKEN": "supersecretvalue123"},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain len = %d, want 1", len(chain))
	}

	system := []providers.ContentBlock{{Type: "text", Text: "system: token is supersecretvalue123"}}
	msgs := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{
			{Type: "text", Text: "here is an openai key sk-abcdefghijklmnop1234 to use"},
		}},
	}

	gotSys, gotMsgs, err := Apply(context.Background(), chain, system, msgs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Tier-A exact value masked.
	if got := gotSys[0].Text; got == system[0].Text || containsSecret(got, "supersecretvalue123") {
		t.Errorf("system block not redacted: %q", got)
	}
	// Tier-B pattern (sk-…) masked.
	if got := gotMsgs[0].Content[0].Text; containsSecret(got, "sk-abcdefghijklmnop1234") {
		t.Errorf("message block not redacted: %q", got)
	}
}

// TestRedactPlugin_DoesNotMutateInput is the outbound-copy guarantee: the
// caller's slices/blocks are untouched after Transform.
func TestRedactPlugin_DoesNotMutateInput(t *testing.T) {
	chain, _ := Build([]config.ContextPluginSpec{{Name: "redact"}}, map[string]string{"K": "topsecretvalue999"})

	origText := "leak topsecretvalue999 here"
	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: origText}}}}

	_, gotMsgs, err := Apply(context.Background(), chain, nil, msgs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Input unchanged.
	if msgs[0].Content[0].Text != origText {
		t.Errorf("input mutated: %q", msgs[0].Content[0].Text)
	}
	// Output redacted (a new slice/block).
	if containsSecret(gotMsgs[0].Content[0].Text, "topsecretvalue999") {
		t.Errorf("output not redacted: %q", gotMsgs[0].Content[0].Text)
	}
}

// TestRedactPlugin_PreservesCacheableAndUnmatched: blocks with no secret pass
// through unchanged (shared), and Cacheable markers survive on redacted blocks.
func TestRedactPlugin_PreservesCacheableAndUnmatched(t *testing.T) {
	chain, _ := Build([]config.ContextPluginSpec{{Name: "redact"}}, map[string]string{"K": "abceasysecret123"})
	msgs := []providers.Message{{Role: "user", Content: []providers.ContentBlock{
		{Type: "text", Text: "clean text, nothing here", Cacheable: true},
		{Type: "text", Text: "secret abceasysecret123", Cacheable: true},
	}}}
	_, gotMsgs, _ := Apply(context.Background(), chain, nil, msgs)
	blocks := gotMsgs[0].Content
	if blocks[0].Text != "clean text, nothing here" {
		t.Errorf("clean block changed: %q", blocks[0].Text)
	}
	if !blocks[0].Cacheable || !blocks[1].Cacheable {
		t.Errorf("Cacheable lost: %+v", blocks)
	}
	if containsSecret(blocks[1].Text, "abceasysecret123") {
		t.Errorf("secret block not redacted: %q", blocks[1].Text)
	}
}

// TestRedactPlugin_ToolInputOptIn: tool_use input is only scrubbed when
// redact_tool_input is set.
func TestRedactPlugin_ToolInputOptIn(t *testing.T) {
	secret := map[string]string{"K": "toolsecretvalue42"}
	in := func() []providers.Message {
		return []providers.Message{{Role: "assistant", Content: []providers.ContentBlock{
			{Type: "tool_use", ToolName: "Bash", ToolInput: json.RawMessage(`{"cmd":"echo toolsecretvalue42"}`)},
		}}}
	}

	// Default: tool input NOT redacted.
	off, _ := Build([]config.ContextPluginSpec{{Name: "redact"}}, secret)
	_, gotOff, _ := Apply(context.Background(), off, nil, in())
	if !containsSecret(string(gotOff[0].Content[0].ToolInput), "toolsecretvalue42") {
		t.Errorf("tool input redacted without opt-in: %s", gotOff[0].Content[0].ToolInput)
	}

	// Opt-in: tool input redacted.
	on, _ := Build([]config.ContextPluginSpec{{Name: "redact", RedactToolInput: boolPtr(true)}}, secret)
	_, gotOn, _ := Apply(context.Background(), on, nil, in())
	if containsSecret(string(gotOn[0].Content[0].ToolInput), "toolsecretvalue42") {
		t.Errorf("tool input not redacted with opt-in: %s", gotOn[0].Content[0].ToolInput)
	}
}

// TestBuild_DisabledAndUnknown pins the registry guards.
func TestBuild_DisabledAndUnknown(t *testing.T) {
	// Disabled spec → skipped → nil chain.
	if c, err := Build([]config.ContextPluginSpec{{Name: "redact", Enabled: boolPtr(false)}}, nil); err != nil || c != nil {
		t.Errorf("disabled spec: chain=%v err=%v, want nil/nil", c, err)
	}
	// Unknown name → error.
	if _, err := Build([]config.ContextPluginSpec{{Name: "nope"}}, nil); err == nil {
		t.Error("unknown plugin: want error, got nil")
	}
	// Empty specs → nil chain.
	if c, err := Build(nil, nil); err != nil || c != nil {
		t.Errorf("empty specs: chain=%v err=%v, want nil/nil", c, err)
	}
}

func containsSecret(s, secret string) bool {
	return secret != "" && strings.Contains(s, secret)
}
