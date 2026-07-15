package ollama

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the ollama driver with the RFC BF driver registry.
// Registration records only the factory. The single canonical dialect is
// "ollama-chat" (the /api/chat NDJSON wire shape). ONE driver serves BOTH the
// hosted "ollama" and local-network "ollama-local" provider ids — they share
// the wire shape and differ only by base_url + auth (derived from the id), so
// the config declares two `providers:` entries (different ids) that both name
// driver "ollama".
func init() {
	providers.RegisterDriver("ollama", []string{"ollama-chat"}, newFromOptions)
}

// newFromOptions builds an ollama Driver from the registry DriverOptions — the
// config-driven construction the resolver uses (the RFC BF replacement for the
// pre-registry hardcoded ollama.New(...).WithNumCtx().WithNumGpu()). The
// provider id (DriverOptions.ID → New's providerID) selects the default auth +
// KeyEnvName behaviour: "ollama-local" is keyless, "ollama" uses OLLAMA_API_KEY.
// A config-declared api_key_env re-points that via SetKeyEnvName (so a custom-id
// ollama provider is keyable under its own var). num_ctx / num_gpu come from the
// options map.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	// New defaults providerID to "ollama" when o.ID == "".
	d := New(o.ID, o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.KeyEnvName != "" {
		d.SetKeyEnvName(o.KeyEnvName)
	}
	d.capsPatch = o.Capabilities
	if n, ok := providers.IntOption(o.Options, "num_ctx"); ok {
		d.WithNumCtx(n)
	}
	if n, ok := providers.IntOption(o.Options, "num_gpu"); ok {
		d.WithNumGpu(n)
	}
	providers.WarnUnknownOptions(o.Logf, "ollama", o.Options, "num_ctx", "num_gpu")
	return d, nil
}
