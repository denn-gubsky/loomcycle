package openai

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestBuildRequestBody_MaxTokensSpellingByModel is the regression for the OpenAI
// reasoning-model 400: o-series / GPT-5 models REJECT max_tokens and require
// max_completion_tokens, while chat models (gpt-4o) and DeepSeek's OpenAI-compat
// surface keep max_tokens. The driver previously always sent max_tokens → any
// reasoning model with an explicit max_tokens 400'd on the first call.
func TestBuildRequestBody_MaxTokensSpellingByModel(t *testing.T) {
	cases := []struct {
		model     string
		reasoning bool // expect max_completion_tokens (else max_tokens)
	}{
		{"o1", true}, {"o3", true}, {"o3-mini", true}, {"o4-mini", true},
		{"gpt-5", true}, {"gpt-5.4", true}, {"gpt-5.4-mini", true},
		{"gpt-4o", false}, {"gpt-4o-mini", false}, {"gpt-4-turbo", false},
		// DeepSeek shares this driver and MUST keep max_tokens.
		{"deepseek-v4-pro", false}, {"deepseek-reasoner", false},
	}
	for _, c := range cases {
		body, err := buildRequestBody(providers.Request{
			Model:     c.model,
			MaxTokens: 1234,
			Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		})
		if err != nil {
			t.Fatalf("%s: buildRequestBody: %v", c.model, err)
		}
		var parsed struct {
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.model, err)
		}
		if c.reasoning {
			if parsed.MaxCompletionTokens != 1234 || parsed.MaxTokens != 0 {
				t.Errorf("%s (reasoning): max_tokens=%d max_completion_tokens=%d, want completion=1234 + no max_tokens",
					c.model, parsed.MaxTokens, parsed.MaxCompletionTokens)
			}
		} else {
			if parsed.MaxTokens != 1234 || parsed.MaxCompletionTokens != 0 {
				t.Errorf("%s (chat): max_tokens=%d max_completion_tokens=%d, want max_tokens=1234 + no completion field",
					c.model, parsed.MaxTokens, parsed.MaxCompletionTokens)
			}
		}
	}
}
