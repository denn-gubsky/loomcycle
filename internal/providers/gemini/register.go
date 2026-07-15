package gemini

import "github.com/denn-gubsky/loomcycle/internal/providers"

// init registers the gemini driver with the RFC BF driver registry.
// Registration records only the factory. The single canonical dialect is
// "gemini-v1beta" (the generateContent /v1beta wire shape); a Vertex AI gateway
// reuses it via a base_url override.
func init() {
	providers.RegisterDriver("gemini", []string{"gemini-v1beta"}, newFromOptions)
}

// newFromOptions builds a gemini Driver from the registry DriverOptions. P1
// equivalent of cmd/loomcycle/main.go's hardcoded gemini.New(...); NOT yet on
// the hot path (P2 wires the registry into the resolver). Gemini's key env var
// is fixed (GEMINI_API_KEY, no SetKeyEnvName) and it has no consumable driver
// options today.
func newFromOptions(o providers.DriverOptions) (providers.Provider, error) {
	d := New(o.APIKey, o.BaseURL, o.StreamOpts, nil)
	if o.ID != "" {
		d.id = o.ID
	}
	d.capsPatch = o.Capabilities
	providers.WarnUnknownOptions(o.Logf, "gemini", o.Options)
	return d, nil
}
