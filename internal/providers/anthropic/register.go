package anthropic

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the anthropic driver with the RFC BF driver registry.
// Registration records only the factory (no construction, no activation). The
// single canonical dialect is "anthropic-messages" (the Messages API wire
// shape).
func init() {
	providers.RegisterDriver("anthropic", []string{"anthropic-messages"}, newFromOptions)
}

// newFromOptions builds an anthropic Driver from the registry DriverOptions —
// the config-driven construction the resolver uses (the RFC BF replacement for
// the pre-registry hardcoded anthropic.New(...)). A config-declared api_key_env
// re-points tenant/user credential resolution via SetKeyEnvName, so a custom-id
// anthropic provider resolves overrides under the SAME var toDriverOptions read
// the host key from (defaults to ANTHROPIC_API_KEY). The API version is a
// compile-time const, so there are no consumable driver options today.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "anthropic", o.Options)
	return d, nil
}
