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

// TestPricing_CostSecondsPerMinute — an audio row is priced per minute of
// audio (seconds/60 × per_minute), with the same currency and unpriced
// semantics as a token row.
func TestPricing_CostSecondsPerMinute(t *testing.T) {
	p := PricingConfig{
		Currency: "USD",
		Models: map[string]ModelPrice{
			"openai/whisper-1":    {PerMinute: 0.006},
			"speaches/large-v3":   {PerMinute: 0, Currency: "EUR"},
			"openai/gpt-4o-audio": {Input: 2.5, PerMinute: 0.01, Currency: "GBP"},
		},
	}

	// 90 s at $0.006/min = $0.009.
	cost, cur, ok := p.CostSeconds("openai", "whisper-1", 90)
	if !ok || cur != "USD" || !approx(cost, 0.009) {
		t.Errorf("priced = (%v, %q, %v), want (0.009, USD, true)", cost, cur, ok)
	}

	// Per-model currency overrides the table default.
	if cost, cur, ok := p.CostSeconds("openai", "gpt-4o-audio", 60); !ok || cur != "GBP" || !approx(cost, 0.01) {
		t.Errorf("currency override = (%v, %q, %v), want (0.01, GBP, true)", cost, cur, ok)
	}

	// A listed self-hosted model at zero is a genuine zero cost, not unpriced.
	if cost, cur, ok := p.CostSeconds("speaches", "large-v3", 600); !ok || cur != "EUR" || cost != 0 {
		t.Errorf("zero-priced = (%v, %q, %v), want (0, EUR, true)", cost, cur, ok)
	}

	// Absent pair, and an empty table, are unpriced.
	if cost, cur, ok := p.CostSeconds("openai", "whisper-9", 60); ok || cur != "" || cost != 0 {
		t.Errorf("unpriced = (%v, %q, %v), want (0, \"\", false)", cost, cur, ok)
	}
	if _, _, ok := (PricingConfig{}).CostSeconds("openai", "whisper-1", 60); ok {
		t.Error("empty table must not price anything")
	}

	// The token path ignores per_minute: a model priced for both bills tokens
	// by the token prices only.
	if cost, _, _ := p.Cost("openai", "gpt-4o-audio", 1_000_000, 0, 0, 0); !approx(cost, 2.5) {
		t.Errorf("token cost = %v, want 2.5 (per_minute must not leak into Cost)", cost)
	}
}

// TestPricing_PerMinuteReadFromYAML — the unit is read from the pricing block.
func TestPricing_PerMinuteReadFromYAML(t *testing.T) {
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(`
defaults: { provider: anthropic, model: x }
pricing:
  models:
    "openai/whisper-1": { per_minute: 0.006 }
`)})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Pricing.Models["openai/whisper-1"].PerMinute; got != 0.006 {
		t.Errorf("per_minute = %v, want 0.006", got)
	}
}

// TestPricing_PerMillionChars — a text-to-speech row is priced per 1M input
// characters (chars/1e6 × per_million_chars), with the same currency and
// unpriced semantics as a token row.
func TestPricing_PerMillionChars(t *testing.T) {
	p := PricingConfig{
		Currency: "USD",
		Models: map[string]ModelPrice{
			"openai/tts-1":             {PerMillionChars: 15},
			"speech-local/piper-uk":    {Currency: "EUR"},
			"openai/gpt-4o-mini-tts":   {Input: 0.6, PerMinute: 0.015, PerMillionChars: 12, Currency: "GBP"},
			"phonon-local/phonon-31m":  {PerMinute: 0.5},
			"untagged-currency/tts-hd": {PerMillionChars: 30},
		},
	}

	// 4 380 characters at $15 per 1M = $0.0657.
	cost, cur, ok := p.CostChars("openai", "tts-1", 4380)
	if !ok || cur != "USD" || !approx(cost, 0.0657) {
		t.Errorf("priced = (%v, %q, %v), want (0.0657, USD, true)", cost, cur, ok)
	}

	// Per-model currency overrides the table default.
	if cost, cur, ok := p.CostChars("openai", "gpt-4o-mini-tts", 1_000_000); !ok || cur != "GBP" || !approx(cost, 12) {
		t.Errorf("currency override = (%v, %q, %v), want (12, GBP, true)", cost, cur, ok)
	}

	// A listed self-hosted model with no per_million_chars is a genuine zero.
	if cost, cur, ok := p.CostChars("speech-local", "piper-uk", 100000); !ok || cur != "EUR" || cost != 0 {
		t.Errorf("zero-priced = (%v, %q, %v), want (0, EUR, true)", cost, cur, ok)
	}
	// A model priced only per minute costs nothing per character.
	if cost, _, ok := p.CostChars("phonon-local", "phonon-31m", 100000); !ok || cost != 0 {
		t.Errorf("per-minute-only by chars = (%v, %v), want (0, true)", cost, ok)
	}

	// Absent pair, and an empty table, are unpriced.
	if cost, cur, ok := p.CostChars("openai", "tts-9", 100); ok || cur != "" || cost != 0 {
		t.Errorf("unpriced = (%v, %q, %v), want (0, \"\", false)", cost, cur, ok)
	}
	if _, _, ok := (PricingConfig{}).CostChars("openai", "tts-1", 100); ok {
		t.Error("empty table must not price anything")
	}

	// per_million_chars leaks into neither the token nor the seconds path.
	if cost, _, _ := p.Cost("openai", "gpt-4o-mini-tts", 1_000_000, 0, 0, 0); !approx(cost, 0.6) {
		t.Errorf("token cost = %v, want 0.6 (per_million_chars must not leak into Cost)", cost)
	}
	if cost, _, _ := p.CostSeconds("openai", "gpt-4o-mini-tts", 60); !approx(cost, 0.015) {
		t.Errorf("seconds cost = %v, want 0.015 (per_million_chars must not leak into CostSeconds)", cost)
	}
}

// TestPricing_CostCharsCurrencyMatchesCost — the three cost paths share one
// lookup, so whatever currency Cost resolves for a pair, CostChars and
// CostSeconds resolve the same (per-model, table default, then USD).
func TestPricing_CostCharsCurrencyMatchesCost(t *testing.T) {
	for _, p := range []PricingConfig{
		{Models: map[string]ModelPrice{"a/b": {PerMillionChars: 1}}},
		{Currency: "EUR", Models: map[string]ModelPrice{"a/b": {PerMillionChars: 1}}},
		{Currency: "EUR", Models: map[string]ModelPrice{"a/b": {PerMillionChars: 1, Currency: "JPY"}}},
		{Currency: "EUR"},
	} {
		_, want, wantOK := p.Cost("a", "b", 1, 0, 0, 0)
		_, got, gotOK := p.CostChars("a", "b", 1)
		_, sec, secOK := p.CostSeconds("a", "b", 1)
		if got != want || gotOK != wantOK || sec != want || secOK != wantOK {
			t.Errorf("table %+v: Cost (%q, %v), CostChars (%q, %v), CostSeconds (%q, %v) — want all equal", p, want, wantOK, got, gotOK, sec, secOK)
		}
	}
}

// TestPricing_PerMillionCharsReadFromYAML — the unit is read from the pricing
// block.
func TestPricing_PerMillionCharsReadFromYAML(t *testing.T) {
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(`
defaults: { provider: anthropic, model: x }
pricing:
  models:
    "openai/tts-1": { per_million_chars: 15 }
`)})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Pricing.Models["openai/tts-1"].PerMillionChars; got != 15 {
		t.Errorf("per_million_chars = %v, want 15", got)
	}
}
