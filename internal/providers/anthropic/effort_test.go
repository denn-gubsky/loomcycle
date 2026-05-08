package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Anthropic's effort translation is the most-tested driver path
// because the mapping is asymmetric (low maps to "skip thinking"),
// model-gated (haiku gets nothing regardless of effort), and the
// budget can clamp against MaxTokens. These tests pin all three.

func TestEffortTranslation_SonnetMediumIncludesThinking(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:   "medium",
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	thinking, ok := w["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block on sonnet/medium, got body: %s", body)
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", thinking["type"])
	}
	if budget, _ := thinking["budget_tokens"].(float64); int(budget) != 2048 {
		t.Errorf("thinking.budget_tokens = %v, want 2048", thinking["budget_tokens"])
	}
}

func TestEffortTranslation_OpusHighUses8192Budget(t *testing.T) {
	// Pass MaxTokens=16384 so the 8192 budget fits without
	// clamping. (Default MaxTokens=8192 would force a clamp to
	// max_tokens-1024 to leave response room — exercised in
	// TestEffortTranslation_BudgetClampsAgainstMaxTokens below.)
	body, err := buildRequestBody(providers.Request{
		Model:     "claude-opus-4-7",
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:    "high",
		MaxTokens: 16384,
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	thinking, ok := w["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block on opus/high")
	}
	if budget, _ := thinking["budget_tokens"].(float64); int(budget) != 8192 {
		t.Errorf("thinking.budget_tokens = %v, want 8192", thinking["budget_tokens"])
	}
}

func TestEffortTranslation_LowSkipsThinking(t *testing.T) {
	// Low effort = "answer fast, don't reason" — wire shape is
	// "no thinking field." Distinct from "no effort declared."
	body, err := buildRequestBody(providers.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:   "low",
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	if _, has := w["thinking"]; has {
		t.Errorf("low effort should skip thinking field, got: %v", w["thinking"])
	}
}

func TestEffortTranslation_HaikuGetsNoThinkingEvenAtHigh(t *testing.T) {
	// Haiku 4.5 does not support extended thinking. The driver
	// must drop the thinking field regardless of effort to avoid
	// a 400 from the API.
	body, err := buildRequestBody(providers.Request{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:   "high",
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	if _, has := w["thinking"]; has {
		t.Errorf("haiku should never get thinking field, got: %v", w["thinking"])
	}
}

func TestEffortTranslation_NoEffortMeansNoThinking(t *testing.T) {
	// Default behaviour for agents that don't declare effort: no
	// thinking field. Backward-compat with v0.7+ PR1 / PR2 yamls.
	body, err := buildRequestBody(providers.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		// Effort intentionally empty.
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	if _, has := w["thinking"]; has {
		t.Errorf("missing effort should skip thinking, got: %v", w["thinking"])
	}
}

func TestEffortTranslation_BudgetClampsAgainstMaxTokens(t *testing.T) {
	// If max_tokens is small (operator picked a tight cap), the
	// thinking budget must stay below it — Anthropic rejects
	// budget ≥ max_tokens. We halve to fit. Below the 1024
	// minimum the field is dropped entirely.
	body, err := buildRequestBody(providers.Request{
		Model:     "claude-sonnet-4-6",
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:    "high",
		MaxTokens: 4096, // half of high's default 8192
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	thinking, _ := w["thinking"].(map[string]any)
	if thinking == nil {
		t.Fatal("expected thinking block (budget should clamp not drop)")
	}
	budget, _ := thinking["budget_tokens"].(float64)
	if int(budget) >= 4096 {
		t.Errorf("budget = %v, must be < max_tokens=4096", budget)
	}
}

func TestEffortTranslation_TinyMaxTokensDropsThinking(t *testing.T) {
	// MaxTokens below the 1024 minimum means there's no valid
	// budget — driver drops the thinking field rather than send
	// an invalid request.
	body, err := buildRequestBody(providers.Request{
		Model:     "claude-sonnet-4-6",
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "x"}}}},
		Effort:    "medium",
		MaxTokens: 1024, // budget would clamp to 512, below the 1024 min → drop
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w map[string]any
	_ = json.Unmarshal(body, &w)
	if _, has := w["thinking"]; has {
		t.Errorf("budget below 1024 minimum should drop thinking, got: %v", w["thinking"])
	}
}
