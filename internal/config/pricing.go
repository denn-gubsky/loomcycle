package config

// pricing.go — RFC AV: the operator-owned price table + per-call cost math.
//
// loomcycle owns pricing (RFC AV decision O1): it already resolves
// (provider, model) per run, so the prices live next to the data. Cost is
// materialized at write time from the table current at that moment — a later
// price edit never rewrites history. A provider/gateway-reported cost, when
// present, overrides the table (handled by the caller, not here).

// PricingConfig is the `pricing:` config block: a default currency + a map of
// "provider/model" → unit prices. Chat, decision and embedder models are priced
// per 1M tokens (input / output / cache_write / cache_read); speech-to-text
// models are priced per minute of audio (per_minute), billed by the second.
// One model may carry both units; each ledger row is costed by the unit it
// measures (Cost for token rows, CostSeconds for audio rows).
//
//	pricing:
//	  currency: USD
//	  models:
//	    "anthropic/claude-sonnet-4-6": { input: 3, output: 15, cache_write: 3.75, cache_read: 0.3 }
//	    "openai/whisper-1":            { per_minute: 0.006 }
type PricingConfig struct {
	// Currency is the default ISO currency for every model without its own
	// override (e.g. "USD"). Empty defaults to "USD" at compute time.
	Currency string `yaml:"currency,omitempty"`
	// Models maps a "provider/model" key (the concrete served pair, aliases
	// already resolved) to its unit prices.
	Models map[string]ModelPrice `yaml:"models,omitempty"`
}

// ModelPrice is one model's unit prices: per-1M-token prices, one per token
// bucket (each is billed at a different rate), and a per-minute audio price for
// speech-to-text models. A per-model Currency overrides the table default.
type ModelPrice struct {
	Input      float64 `yaml:"input"`       // $ per 1M input tokens
	Output     float64 `yaml:"output"`      // $ per 1M output tokens
	CacheWrite float64 `yaml:"cache_write"` // $ per 1M cache-creation tokens
	CacheRead  float64 `yaml:"cache_read"`  // $ per 1M cache-read tokens
	PerMinute  float64 `yaml:"per_minute"`  // $ per minute of audio (speech-to-text)
	Currency   string  `yaml:"currency,omitempty"`
}

// Cost returns the money cost of a call's token usage under this table, the
// currency, and whether the (provider, model) pair was priced. Prices are per
// 1M tokens. ok=false ⇒ unpriced (pair absent from the table) → the caller
// records a NULL cost + a pricing_missing signal, never a guess. A found pair
// with all-zero prices (or zero tokens) is a genuine zero cost and returns
// (0, currency, true) — distinct from unpriced.
func (p PricingConfig) Cost(provider, model string, in, out, cacheCreation, cacheRead int) (float64, string, bool) {
	mp, currency, ok := p.lookup(provider, model)
	if !ok {
		return 0, "", false
	}
	cost := (float64(in)*mp.Input +
		float64(out)*mp.Output +
		float64(cacheCreation)*mp.CacheWrite +
		float64(cacheRead)*mp.CacheRead) / 1e6
	return cost, currency, true
}

// CostSeconds returns the money cost of seconds of audio under this table:
// seconds/60 × per_minute. Currency and the unpriced (ok=false) signal follow
// Cost exactly: an absent pair is unpriced, a listed pair with no per_minute
// is a genuine zero.
func (p PricingConfig) CostSeconds(provider, model string, seconds float64) (float64, string, bool) {
	mp, currency, ok := p.lookup(provider, model)
	if !ok {
		return 0, "", false
	}
	return seconds / 60 * mp.PerMinute, currency, true
}

// lookup finds the pair's prices and its currency (per-model, else the table
// default, else USD).
func (p PricingConfig) lookup(provider, model string) (ModelPrice, string, bool) {
	mp, ok := p.Models[provider+"/"+model]
	if !ok {
		return ModelPrice{}, "", false
	}
	currency := mp.Currency
	if currency == "" {
		currency = p.Currency
	}
	if currency == "" {
		currency = "USD"
	}
	return mp, currency, true
}
