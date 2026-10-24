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
		// Haiku 5.x joined the adaptive-only family; Bedrock ids carry a
		// geography and/or anthropic. prefix and sometimes a -v1:0 suffix.
		{"claude-haiku-5-5", true, false},
		{"eu.anthropic.claude-haiku-5-5", true, false},
		{"global.anthropic.claude-haiku-5-5", true, false},
		{"anthropic.claude-haiku-5-5-v1:0", true, false},
		{"us.anthropic.claude-haiku-5-6-v1:0", true, false}, // a later 5.x minor
		{"claude-sonnet-5-5", true, true},
		{"eu.anthropic.claude-sonnet-5-5", true, true},
		{"anthropic.claude-sonnet-5-5-v1:0", true, true},
		{"anthropic.claude-haiku-4-5-20251001-v1:0", false, false},
		{"claude-3-5-haiku-20241022", false, false},
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

// Claude Haiku 5.5 (direct or on Bedrock) answers any non-default temperature /
// top_p / top_k with a 400 ("`temperature` is deprecated for this model"), so
// an agent with sampling set failed every call. Those knobs must never reach
// the wire there, while the 4.x models keep receiving them — including an
// explicit temperature 0, which is deterministic and not the same as unset.
func TestRequestBody_DeprecatedSamplingNeverReachesTheWire(t *testing.T) {
	temp, topP, topK := 0.7, 0.9, 40
	for _, model := range []string{
		"claude-haiku-5-5",
		"eu.anthropic.claude-haiku-5-5",
		"anthropic.claude-haiku-5-5-v1:0",
		"eu.anthropic.claude-sonnet-5-5",
		"claude-sonnet-5-5",
	} {
		req := baseReq()
		req.Model, req.Temperature, req.TopP, req.TopK = model, &temp, &topP, &topK
		body := bodyOf(t, req)
		for _, k := range []string{"temperature", "top_p", "top_k"} {
			if v, present := body[k]; present {
				t.Errorf("%s: %s=%v reached the wire, want it omitted", model, k, v)
			}
		}
	}

	zero := 0.0
	for _, model := range []string{"claude-haiku-4-5", "anthropic.claude-haiku-4-5-20251001-v1:0", "claude-sonnet-4-6"} {
		req := baseReq()
		req.Model, req.Temperature = model, &zero
		got, present := bodyOf(t, req)["temperature"]
		if !present || got != 0.0 {
			t.Errorf("%s: temperature = %v (present=%v), want an explicit 0 kept", model, got, present)
		}
	}
}

// The chat bundle's shape (effort medium + temperature 0.7) on Haiku 5.5:
// effort reaches the wire as adaptive thinking + output_config.effort, and the
// temperature is dropped rather than failing the call.
func TestRequestBody_Haiku5EffortWithTemperatureSendsAdaptiveEffortOnly(t *testing.T) {
	temp := 0.7
	req := baseReq()
	req.Model, req.Effort, req.Temperature = "eu.anthropic.claude-haiku-5-5", "medium", &temp
	body := bodyOf(t, req)
	if _, present := body["temperature"]; present {
		t.Errorf("temperature reached the wire: %v", body["temperature"])
	}
	if !jsonEqual(body["thinking"], map[string]any{"type": "adaptive"}) {
		t.Errorf("thinking = %v, want {type: adaptive} with no budget", body["thinking"])
	}
	if !jsonEqual(body["output_config"], map[string]any{"effort": "medium"}) {
		t.Errorf("output_config = %v, want effort medium", body["output_config"])
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

// Sonnet 5.5 answers a forced tool_choice with a 400, directly and on Bedrock,
// so it is dropped as on the other models that refuse it. Sonnet 5 and Haiku
// 5.5 accept forcing and must keep it.
func TestToolChoice_Sonnet55DropsForcingOtherFiveXModelsKeepIt(t *testing.T) {
	forced := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	for _, model := range []string{"claude-sonnet-5-5", "eu.anthropic.claude-sonnet-5-5", "anthropic.claude-sonnet-5-5-v1:0"} {
		for _, mode := range []providers.ToolChoice{forced, {Mode: providers.ToolChoiceRequired}} {
			req := baseReq()
			req.Model, req.ToolChoice = model, mode
			if got, present := bodyOf(t, req)["tool_choice"]; present {
				t.Errorf("%s: forced tool_choice %v reached the wire", model, got)
			}
		}
	}
	for _, model := range []string{"claude-sonnet-5", "eu.anthropic.claude-haiku-5-5"} {
		req := baseReq()
		req.Model, req.ToolChoice = model, forced
		if got := bodyOf(t, req)["tool_choice"]; !jsonEqual(got, map[string]any{"type": "tool", "name": "emit_state"}) {
			t.Errorf("%s: tool_choice = %v, want the forced choice kept", model, got)
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

// On Bedrock, Claude Sonnet 5 accepts a forced tool_choice only with thinking
// explicitly disabled (it thinks adaptively when the field is omitted). The
// forced choice is kept and thinking is turned off for that call; the direct
// API and Vertex need no change.
func TestToolChoice_BedrockSonnet5ForcedChoiceDisablesThinking(t *testing.T) {
	for _, model := range []string{
		"anthropic.claude-sonnet-5",
		"eu.anthropic.claude-sonnet-5",
		"us.anthropic.claude-sonnet-5-v1:0",
		"global.anthropic.claude-sonnet-5-20260101-v1:0",
	} {
		for _, effort := range []string{"", "low", "high"} {
			for _, mode := range []providers.ToolChoice{
				{Mode: providers.ToolChoiceTool, Name: "emit_state"},
				{Mode: providers.ToolChoiceRequired},
			} {
				req := baseReq()
				req.Model, req.Effort, req.ToolChoice, req.MaxTokens = model, effort, mode, 16384
				body := bodyOf(t, req)
				if _, present := body["tool_choice"]; !present {
					t.Errorf("%s/%q/%v: forced tool_choice dropped, want it kept", model, effort, mode.Mode)
				}
				if !jsonEqual(body["thinking"], map[string]any{"type": "disabled"}) {
					t.Errorf("%s/%q/%v: thinking = %v, want {type: disabled}", model, effort, mode.Mode, body["thinking"])
				}
			}
		}
	}
}

// Guards: a call that does not force a tool keeps adaptive thinking and effort
// on Bedrock Sonnet 5; the direct-API Sonnet 5 keeps both under a forced
// choice; Sonnet 5.5 drops the forced choice instead; Haiku 5.5 keeps it with
// its adaptive thinking.
func TestToolChoice_ThinkingDisabledOnlyForBedrockSonnet5Forced(t *testing.T) {
	forced := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
	adaptive := map[string]any{"type": "adaptive"}
	for _, tc := range []struct {
		model        string
		tc           providers.ToolChoice
		wantThinking any
		wantForced   bool
	}{
		{"eu.anthropic.claude-sonnet-5", providers.ToolChoice{}, adaptive, false},
		{"eu.anthropic.claude-sonnet-5", providers.ToolChoice{Mode: providers.ToolChoiceNone}, adaptive, false},
		{"claude-sonnet-5", forced, adaptive, true},
		{"eu.anthropic.claude-sonnet-5-5", forced, adaptive, false},
		{"eu.anthropic.claude-haiku-5-5", forced, adaptive, true},
	} {
		req := baseReq()
		req.Model, req.Effort, req.ToolChoice, req.MaxTokens = tc.model, "high", tc.tc, 16384
		body := bodyOf(t, req)
		if !jsonEqual(body["thinking"], tc.wantThinking) {
			t.Errorf("%s/%v: thinking = %v, want %v", tc.model, tc.tc.Mode, body["thinking"], tc.wantThinking)
		}
		if !jsonEqual(body["output_config"], map[string]any{"effort": "high"}) {
			t.Errorf("%s/%v: output_config = %v, want effort high", tc.model, tc.tc.Mode, body["output_config"])
		}
		got, present := body["tool_choice"]
		forcedSent := present && !jsonEqual(got, map[string]any{"type": "none"})
		if forcedSent != tc.wantForced {
			t.Errorf("%s/%v: forced tool_choice sent = %v (%v), want %v", tc.model, tc.tc.Mode, forcedSent, got, tc.wantForced)
		}
	}
}
