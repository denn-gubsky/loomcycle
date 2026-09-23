package anthropic

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The family table decides three things the API answers with a 400, so the
// negatives matter as much as the positives: a pre-4.7 model classified as
// adaptive-only would lose the thinking budget it still accepts.
func TestAnthropicModelRules_Families(t *testing.T) {
	for _, tc := range []struct {
		model                  string
		adaptiveOnly, noForced bool
	}{
		{"claude-opus-5-5", true, true},
		{"claude-fable-5-1", true, true},
		{"claude-mythos-5-1", true, true},
		{"claude-fable-5", true, false},
		{"claude-opus-5", true, false},
		{"claude-opus-4-8", true, false},
		{"claude-opus-4-7", true, false},
		{"claude-sonnet-5", true, false},
		{"anthropic.claude-opus-4-7", true, false}, // platform prefix
		{"claude-opus-4-6", false, false},
		{"claude-sonnet-4-6", false, false},
		{"claude-opus-4-5-20251101", false, false},
		{"claude-sonnet-4-5", false, false},
		{"claude-haiku-4-5", false, false},
		{"some-unknown-model", false, false},
	} {
		got := anthropicModelRules(tc.model)
		if got.adaptiveOnly != tc.adaptiveOnly || got.noForcedChoice != tc.noForced {
			t.Errorf("%s: adaptiveOnly=%v noForcedChoice=%v, want %v %v",
				tc.model, got.adaptiveOnly, got.noForcedChoice, tc.adaptiveOnly, tc.noForced)
		}
	}
}

// On the adaptive-only models a thinking budget is a 400, so effort must reach
// the wire as output_config.effort with adaptive thinking — never a budget.
func TestEffortTranslation_AdaptiveModelsGetAdaptiveThinkingAndEffort(t *testing.T) {
	for _, model := range []string{"claude-opus-4-7", "claude-opus-5", "claude-sonnet-5", "claude-opus-5-5", "claude-fable-5-1"} {
		for _, effort := range []string{"medium", "high"} {
			req := baseReq()
			req.Model, req.Effort, req.MaxTokens = model, effort, 16384
			body := bodyOf(t, req)
			if !jsonEqual(body["thinking"], map[string]any{"type": "adaptive"}) {
				t.Errorf("%s/%s: thinking = %v, want {type: adaptive} with no budget", model, effort, body["thinking"])
			}
			if !jsonEqual(body["output_config"], map[string]any{"effort": effort}) {
				t.Errorf("%s/%s: output_config = %v, want effort %q", model, effort, body["output_config"], effort)
			}
		}
	}
}

// low keeps its "answer fast" meaning: effort low, no thinking switched on.
func TestEffortTranslation_AdaptiveLowSendsEffortWithoutThinking(t *testing.T) {
	req := baseReq()
	req.Model, req.Effort = "claude-opus-4-8", "low"
	body := bodyOf(t, req)
	if _, ok := body["thinking"]; ok {
		t.Errorf("low effort switched thinking on: %v", body["thinking"])
	}
	if !jsonEqual(body["output_config"], map[string]any{"effort": "low"}) {
		t.Errorf("output_config = %v, want effort low", body["output_config"])
	}
}

// No effort declared must leave the body exactly as before on every model.
func TestEffortTranslation_NoEffortSendsNoOutputConfig(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-4-6"} {
		req := baseReq()
		req.Model = model
		body := bodyOf(t, req)
		if _, ok := body["output_config"]; ok {
			t.Errorf("%s: output_config sent with no effort: %v", model, body["output_config"])
		}
		if _, ok := body["thinking"]; ok {
			t.Errorf("%s: thinking sent with no effort: %v", model, body["thinking"])
		}
	}
}

// The adaptive-only models reject non-default sampling even without thinking;
// the older models keep receiving it.
func TestRequestBody_AdaptiveModelsDropSampling(t *testing.T) {
	temp, topP, topK := 0.2, 0.9, 40
	for _, tc := range []struct {
		model string
		kept  bool
	}{{"claude-opus-4-7", false}, {"claude-fable-5-1", false}, {"claude-sonnet-4-6", true}} {
		req := baseReq()
		req.Model, req.Temperature, req.TopP, req.TopK = tc.model, &temp, &topP, &topK
		body := bodyOf(t, req)
		for _, k := range []string{"temperature", "top_p", "top_k"} {
			if _, present := body[k]; present != tc.kept {
				t.Errorf("%s: %s present=%v, want %v", tc.model, k, present, tc.kept)
			}
		}
	}
}

// Opus 5.5 / Fable 5.1 refuse a forced choice outright. The stateful loop
// forces emit_state on every call, so sending it failed every such run.
func TestToolChoice_AnthropicDropsForcingOnModelsThatRefuseIt(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1"} {
		for _, mode := range []providers.ToolChoice{
			{Mode: providers.ToolChoiceTool, Name: "emit_state"},
			{Mode: providers.ToolChoiceRequired},
		} {
			req := baseReq()
			req.Model, req.ToolChoice = model, mode
			if got, present := bodyOf(t, req)["tool_choice"]; present {
				t.Errorf("%s: forced tool_choice %v reached the wire", model, got)
			}
		}
		req := baseReq()
		req.Model, req.ToolChoice = model, providers.ToolChoice{Mode: providers.ToolChoiceNone}
		if got := bodyOf(t, req)["tool_choice"]; !jsonEqual(got, map[string]any{"type": "none"}) {
			t.Errorf("%s: tool_choice none = %v, want it preserved", model, got)
		}
	}
}

// Adaptive thinking accepts a forced choice, so an adaptive model that allows
// forcing keeps it even with effort set — dropping it there would lose a
// guarantee for nothing.
func TestToolChoice_AnthropicKeepsForcingUnderAdaptiveThinking(t *testing.T) {
	req := baseReq()
	req.Model, req.Effort, req.MaxTokens = "claude-opus-5", "high", 16384
	req.ToolChoice = providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	body := bodyOf(t, req)
	if !jsonEqual(body["thinking"], map[string]any{"type": "adaptive"}) {
		t.Fatalf("fixture did not engage adaptive thinking: %v", body["thinking"])
	}
	if !jsonEqual(body["tool_choice"], map[string]any{"type": "tool", "name": "emit_state"}) {
		t.Errorf("tool_choice = %v, want the forced choice kept", body["tool_choice"])
	}
}
