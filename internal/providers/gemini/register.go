package gemini

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the gemini driver with the RFC BF driver registry.
// Registration records only the factory. The single canonical dialect is
// "gemini-v1beta" (the generateContent /v1beta wire shape); a Vertex AI gateway
// reuses it via a base_url override.
func init() {
	providers.RegisterDriver("gemini", []string{"gemini-v1beta"}, newFromOptions)
}

// newFromOptions builds a gemini Driver from the registry DriverOptions — the
// config-driven construction the resolver uses (the RFC BF replacement for the
// pre-registry hardcoded gemini.New(...)). A config-declared api_key_env
// re-points tenant/user credential resolution via SetKeyEnvName, so a custom-id
// gemini provider resolves overrides under the SAME var toDriverOptions read the
// host key from (defaults to GEMINI_API_KEY). It has no consumable driver
// options today.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "gemini", o.Options)
	return d, nil
}
