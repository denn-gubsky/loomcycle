package openai

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the openai driver with the RFC BF driver registry. Registration
// only records the factory — it constructs nothing and enables nothing (the
// operator's `providers:` config + resolver decide activation). The single
// canonical dialect is "openai-chat" (Chat Completions wire shape); any
// OpenAI-compatible endpoint reuses it via a base_url override.
func init() {
	providers.RegisterDriver("openai", []string{"openai-chat"}, newFromOptions)
}

// newFromOptions builds an openai Driver from the registry DriverOptions. It is
// the P1 equivalent of cmd/loomcycle/main.go's hardcoded openai.New(...) — NOT
// yet on the hot path (P2 wires the registry into the resolver). It exists so
// P2 is a clean flip and the seam is testable now.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	// The openai driver has no driver-specific options today; surface any that
	// were configured as an advisory warning rather than silently dropping them.
	providers.WarnUnknownOptions(o.Logf, "openai", o.Options)
	return d, nil
}
