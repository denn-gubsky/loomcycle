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

// newFromOptions builds a mock Driver from the registry DriverOptions. P1
// equivalent of cmd/loomcycle/main.go's hardcoded mockprov.New(); NOT yet on
// the hot path (P2 wires the registry into the resolver). The mock ignores
// APIKey/BaseURL (no real HTTP); its behaviour comes from the LOOMCYCLE_MOCK_*
// env that New() reads.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New()
	if o.ID != "" {
		d.id = o.ID
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "mock", o.Options)
	return d, nil
}
