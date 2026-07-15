package mock

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the mock driver with the RFC BF driver registry. Registration
// records only the factory — it neither reads the LOOMCYCLE_MOCK_* env vars nor
// enables the provider (that stays behind the resolver's LOOMCYCLE_MOCK_ENABLED
// gate in cmd/loomcycle). The canonical dialect is "mock" (no real wire). Only
// the base "mock" driver is registered; the mock-stable variant remains a
// resolver-side wrapper, not a config-declared driver.
func init() {
	providers.RegisterDriver("mock", []string{"mock"}, newFromOptions)
}

// newFromOptions builds a mock Driver from the registry DriverOptions — the
// config-driven construction the resolver uses (the RFC BF replacement for the
// pre-registry hardcoded mockprov.New()). The mock ignores APIKey/BaseURL (no
// real HTTP); its behaviour comes from the LOOMCYCLE_MOCK_* env that New() reads,
// plus the `stable` option below.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New()
	if o.ID != "" {
		d.id = o.ID
	}
	// RFC BF P2a — a `stable: true` option builds the failure-injection-off variant
	// (the pre-P2a "mock-stable"): 429/500 rates pinned to zero regardless of the
	// LOOMCYCLE_MOCK_* env, latency knobs still apply. Behaviour matches
	// NewStableProvider() but with the id set from DriverOptions, so the config can
	// declare `mock-stable: {driver: mock, options: {stable: true}}`.
	if stable, _ := providers.BoolOption(o.Options, "stable"); stable {
		d.rate429 = 0
		d.rate500 = 0
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "mock", o.Options, "stable")
	return d, nil
}
