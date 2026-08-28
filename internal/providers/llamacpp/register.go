package llamacpp

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the llamacpp driver. llama-server speaks the OpenAI Chat
// Completions wire shape, so its canonical dialect is "openai-chat" — a
// distinct DRIVER name from openai, so no collision.
func init() {
	providers.RegisterDriver("llamacpp", []string{"openai-chat"}, newFromOptions)
}

// newFromOptions builds a llamacpp Driver from the registry DriverOptions.
// New() already points the inner key resolution at LLAMACPP_API_KEY; a
// config-declared api_key_env re-points it via SetKeyEnvName.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "llamacpp", o.Options)
	return d, nil
}
