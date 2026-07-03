package config

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPricing_Cost(t *testing.T) {
	p := PricingConfig{
		Currency: "USD",
		Models: map[string]ModelPrice{
			// $ per 1M tokens.
			"anthropic/claude-opus-4-8": {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.5},
			"local/free-model":          {Input: 0, Output: 0, Currency: "EUR"},
		},
	}

	// Priced pair: cost = Σ(tokens × price)/1e6.
	// 1000*15 + 500*75 + 200*18.75 + 100*1.5 = 15000+37500+3750+150 = 56400 /1e6.
	cost, cur, ok := p.Cost("anthropic", "claude-opus-4-8", 1000, 500, 200, 100)
	if !ok || cur != "USD" || !approx(cost, 0.0564) {
		t.Errorf("priced = (%v, %q, %v), want (0.0564, USD, true)", cost, cur, ok)
	}

	// Unknown (provider, model) → unpriced: ok=false, empty currency (→ NULL cost).
	if cost, cur, ok := p.Cost("openai", "gpt-9", 100, 100, 0, 0); ok || cur != "" || cost != 0 {
		t.Errorf("unpriced = (%v, %q, %v), want (0, \"\", false)", cost, cur, ok)
	}

	// Found pair, all-zero prices → a GENUINE zero cost (ok=true) with its own
	// currency — distinct from unpriced.
	if cost, cur, ok := p.Cost("local", "free-model", 999, 999, 0, 0); !ok || cur != "EUR" || cost != 0 {
		t.Errorf("zero-priced = (%v, %q, %v), want (0, EUR, true)", cost, cur, ok)
	}

	// Empty table → everything unpriced.
	if _, _, ok := (PricingConfig{}).Cost("anthropic", "claude-opus-4-8", 1, 1, 0, 0); ok {
		t.Errorf("empty table must not price anything")
	}

	// Per-model currency overrides the table default; absent → table default →
	// "USD" fallback.
	p2 := PricingConfig{Models: map[string]ModelPrice{"x/y": {Input: 1}}}
	if _, cur, _ := p2.Cost("x", "y", 1_000_000, 0, 0, 0); cur != "USD" {
		t.Errorf("currency fallback = %q, want USD", cur)
	}
}
