package deepseek

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the deepseek driver with the RFC BF driver registry.
// Registration records only the factory. DeepSeek speaks the OpenAI Chat
// Completions wire shape, so its canonical dialect is "openai-chat" (same
// dialect string the openai driver registers — they are distinct DRIVER names,
// so there is no collision).
func init() {
	providers.RegisterDriver("deepseek", []string{"openai-chat"}, newFromOptions)
}

// newFromOptions builds a deepseek Driver from the registry DriverOptions — the
// config-driven construction the resolver uses (the RFC BF replacement for the
// pre-registry hardcoded deepseek.New(...)). New() already points the inner key
// resolution at DEEPSEEK_API_KEY; a config-declared api_key_env re-points it via
// SetKeyEnvName.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "deepseek", o.Options)
	return d, nil
}
