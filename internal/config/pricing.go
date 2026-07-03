package config

// pricing.go — RFC AV: the operator-owned price table + per-call cost math.
//
// loomcycle owns pricing (RFC AV decision O1): it already resolves
// (provider, model) per run, so the prices live next to the data. Cost is
// materialized at write time from the table current at that moment — a later
// price edit never rewrites history. A provider/gateway-reported cost, when
// present, overrides the table (handled by the caller, not here).

// PricingConfig is the `pricing:` config block: a default currency + a map of
// "provider/model" → per-1M-token unit prices.
type PricingConfig struct {
	// Currency is the default ISO currency for every model without its own
	// override (e.g. "USD"). Empty defaults to "USD" at compute time.
	Currency string `yaml:"currency,omitempty"`
	// Models maps a "provider/model" key (the concrete served pair, aliases
	// already resolved) to its unit prices.
	Models map[string]ModelPrice `yaml:"models,omitempty"`
}

// ModelPrice is one model's per-1M-token prices, one per token bucket (each is
// billed at a different rate). A per-model Currency overrides the table default.
type ModelPrice struct {
	Input      float64 `yaml:"input"`       // $ per 1M input tokens
	Output     float64 `yaml:"output"`      // $ per 1M output tokens
	CacheWrite float64 `yaml:"cache_write"` // $ per 1M cache-creation tokens
	CacheRead  float64 `yaml:"cache_read"`  // $ per 1M cache-read tokens
	Currency   string  `yaml:"currency,omitempty"`
}

// Cost returns the money cost of a call's token usage under this table, the
// currency, and whether the (provider, model) pair was priced. Prices are per
// 1M tokens. ok=false ⇒ unpriced (pair absent from the table) → the caller
// records a NULL cost + a pricing_missing signal, never a guess. A found pair
// with all-zero prices (or zero tokens) is a genuine zero cost and returns
// (0, currency, true) — distinct from unpriced.
func (p PricingConfig) Cost(provider, model string, in, out, cacheCreation, cacheRead int) (float64, string, bool) {
	if len(p.Models) == 0 {
		return 0, "", false
	}
	mp, ok := p.Models[provider+"/"+model]
	if !ok {
		return 0, "", false
	}
	currency := mp.Currency
	if currency == "" {
		currency = p.Currency
	}
	if currency == "" {
		currency = "USD"
	}
	cost := (float64(in)*mp.Input +
		float64(out)*mp.Output +
		float64(cacheCreation)*mp.CacheWrite +
		float64(cacheRead)*mp.CacheRead) / 1e6
	return cost, currency, true
}
