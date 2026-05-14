// Package cost computes a rough USD estimate per RunResult based on
// token usage and a per-(provider, model) rate card. The rate card is
// intentionally coarse — we don't have wire-accurate provider pricing
// and the bench just needs ballpark numbers to enforce --budget caps
// and produce a "this sweep cost $X" footer in the matrix.
package cost

import (
	"strings"

	"github.com/denn-gubsky/loomcycle/bench/internal/runner"
)

// Rates is the per-million-token price for a model.
type Rates struct {
	InputPerMTok  float64 // USD per 1M input tokens
	OutputPerMTok float64
}

// Of returns the rate-card entry for (provider, model). Falls back to
// a conservative default ($1/Mtok in, $3/Mtok out) when unknown so the
// budget cap can't be silently bypassed by a new model we haven't
// priced.
func Of(provider, model string) Rates {
	key := strings.ToLower(provider + ":" + model)
	if r, ok := rateCard[key]; ok {
		return r
	}
	// Provider-level fallback before the universal default.
	switch provider {
	case "anthropic":
		// Sonnet-class default; haiku-class is cheaper but we're
		// conservative.
		return Rates{InputPerMTok: 3.00, OutputPerMTok: 15.00}
	case "deepseek":
		return Rates{InputPerMTok: 0.30, OutputPerMTok: 1.20}
	case "gemini":
		return Rates{InputPerMTok: 0.30, OutputPerMTok: 2.50}
	case "ollama-cloud":
		return Rates{InputPerMTok: 0.50, OutputPerMTok: 2.00}
	case "ollama-desktop":
		// Self-hosted = no marginal $ cost beyond electricity. We
		// score $0 here so local models don't dominate the budget cap.
		return Rates{InputPerMTok: 0, OutputPerMTok: 0}
	}
	return Rates{InputPerMTok: 1.00, OutputPerMTok: 3.00}
}

// EstimateUSD scores one RunResult in USD. Reads inputTokens +
// outputTokens from result.Usage; returns 0 when usage was not
// reported (some providers omit usage for streamed responses).
func EstimateUSD(provider, model string, result runner.RunResult) float64 {
	if result.Usage == nil {
		return 0
	}
	r := Of(provider, model)
	in := float64(result.Usage.InputTokens) / 1_000_000.0
	out := float64(result.Usage.OutputTokens) / 1_000_000.0
	return in*r.InputPerMTok + out*r.OutputPerMTok
}

// rateCard is a small hand-curated table. Add specific entries when a
// model's price diverges substantially from its provider default.
//
// Prices: USD per 1M tokens. Last verified 2026-05-14. Drift
// expected; the budget cap is what enforces the floor.
var rateCard = map[string]Rates{
	"anthropic:claude-haiku-4-5":  {InputPerMTok: 1.00, OutputPerMTok: 5.00},
	"anthropic:claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00},
	"anthropic:claude-opus-4-7":   {InputPerMTok: 15.00, OutputPerMTok: 75.00},
	"deepseek:deepseek-chat":      {InputPerMTok: 0.27, OutputPerMTok: 1.10},
	"deepseek:deepseek-reasoner":  {InputPerMTok: 0.55, OutputPerMTok: 2.20},
	"gemini:gemini-2.0-flash":     {InputPerMTok: 0.10, OutputPerMTok: 0.40},
	"gemini:gemini-2.5-flash":     {InputPerMTok: 0.30, OutputPerMTok: 2.50},
	"gemini:gemini-2.5-pro":       {InputPerMTok: 1.25, OutputPerMTok: 10.00},
}
