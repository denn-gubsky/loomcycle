package config

import (
	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
)

// defaultProvidersLayer is the embedded default-providers layer — the SOLE source
// of the built-in providers after RFC BF P3 (which deleted the hardcoded
// validation floor). Both the server (cmd/loomcycle/main.go) and the CLI
// (internal/cli.loadLayeredConfig) prepend it, so a config that references a
// built-in provider (anthropic/mock/deepseek/…) is only valid in the presence of
// this layer.
func defaultProvidersLayer() Layer {
	return Layer{Name: "providers.default", Data: embedded.DefaultProviders()}
}

// withDefaultProviders prepends the embedded default-providers layer to layers,
// mirroring the real server/CLI assembly. Tests that load a built-in-referencing
// config without their own providers: block use this so provider references
// validate (RFC BF P3 removed the hardcoded floor that used to make them valid).
func withDefaultProviders(layers ...Layer) []Layer {
	return append([]Layer{defaultProvidersLayer()}, layers...)
}

// loadWithDefaults loads a single config file WITH the embedded default-providers
// layer prepended — exactly as the server (cmd/loomcycle) and CLI
// (internal/cli.loadLayeredConfig) assemble it. Replaces bare Load(path) in tests
// whose fixture references built-in providers, which after RFC BF P3 are only
// valid when that layer supplies them.
func loadWithDefaults(path string) (*Config, error) {
	return LoadLayers(withDefaultProviders(Layer{Name: path})...)
}
