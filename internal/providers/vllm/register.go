package vllm

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the vllm driver. vLLM speaks the OpenAI Chat Completions wire
// shape, so its canonical dialect is "openai-chat" — the same dialect string
// the openai driver registers, but a distinct DRIVER name, so no collision.
func init() {
	providers.RegisterDriver("vllm", []string{"openai-chat"}, newFromOptions)
}

// newFromOptions builds a vllm Driver from the registry DriverOptions. New()
// already points the inner key resolution at VLLM_API_KEY; a config-declared
// api_key_env re-points it via SetKeyEnvName.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	// vLLM's per-request tuning (extra_body) is not modelled as driver options
	// yet; surface any configured options as an advisory warning rather than
	// silently dropping them.
	providers.WarnUnknownOptions(o.Logf, "vllm", o.Options)
	return d, nil
}
