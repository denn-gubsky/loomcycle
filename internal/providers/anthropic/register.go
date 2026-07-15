package anthropic

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the anthropic driver with the RFC BF driver registry.
// Registration records only the factory (no construction, no activation). The
// single canonical dialect is "anthropic-messages" (the Messages API wire
// shape).
func init() {
	providers.RegisterDriver("anthropic", []string{"anthropic-messages"}, newFromOptions)
}

// newFromOptions builds an anthropic Driver from the registry DriverOptions.
// P1 equivalent of cmd/loomcycle/main.go's hardcoded anthropic.New(...); NOT
// yet on the hot path (P2 wires the registry into the resolver). Anthropic's
// key env var is fixed (ANTHROPIC_API_KEY) — the driver exposes no SetKeyEnvName
// — so KeyEnvName is not forwarded. The API version is a compile-time const, so
// there are no consumable driver options today.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "anthropic", o.Options)
	return d, nil
}
